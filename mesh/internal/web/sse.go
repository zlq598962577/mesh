package web

// SSE（Server-Sent Events）实现。
//
// 为什么是 SSE 而不是 WebSocket（§6 的选择，也是本包的设计前提）：
//   - 只需要单向推送（服务端 → 浏览器）。浏览器的输入走普通 POST，不需要双向通道。
//   - SSE 是 http.ResponseWriter 上一个死循环，没有额外依赖、没有第三方库、
//     不需要处理握手/掩码/分片，也不引入 cgo。
//   - 断线补齐天然可行：浏览器自带重连，我们只用 ?cursor=N 让他接着说
//     "我已经处理到 N 了"，服务端从 N+1 补读（与 events(run_id, seq) 索引对齐）。
//   - WebSocket 需要自己实现重连 + 补读协议，收益为零。
//
// 三条硬性要求（写在 COORDINATION.md 里，改代码时不许违反）：
//  1. 慢消费者只能被丢弃，绝不允许阻塞全局广播。事件生产方（引擎输出）的
//     进度不能因为某个卡住的浏览器标签页而停住。
//  2. 每 15–30 秒必须有保活。中间代理/浏览器会掐掉长期静默的连接。
//     同时发 `: keepalive` 注释行和具名 `ping` 事件——前端两种都监听。
//  3. Shutdown 必须主动掐断所有 SSE 连接。http.Server.Shutdown 会等
//     所有连接结束，而 SSE 连接是长连接，不主动关就等不到优雅退出。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"mesh/internal/contract"
)

const (
	// sseBuffer 是单个连接的发送缓冲深度。
	//
	// 取 64：够吸收引擎一次突发输出（流式 output 会成批到达），又足够小，
	// 让"卡住的标签页"在几百毫秒内被判定为慢消费者而不是吃掉内存。
	sseBuffer = 64
	// ssePingInterval 保活间隔。前端没有任何超时假设，但代理通常在 30–60 秒
	// 静默后断开，20 秒留足余量。
	ssePingInterval = 20 * time.Second
	// sseWriteTimeout 单帧写超时。写不动就是客户端死了，别拖着。
	sseWriteTimeout = 10 * time.Second
	// resubscribeDelay 实时订阅断开后的重连间隔（防止热循环）。
	resubscribeDelay = time.Second
)

// sseHub 是事件扇出中心：一个实时事件源，N 个浏览器连接。
type sseHub struct {
	mu     sync.Mutex
	subs   map[*sseSubscriber]struct{}
	closed bool
}

func newSSEHub() *sseHub {
	return &sseHub{subs: make(map[*sseSubscriber]struct{})}
}

// sseSubscriber 是一个浏览器连接的接收端。
type sseSubscriber struct {
	ch     chan contract.Event
	hub    *sseHub
	closed bool
	once   sync.Once
}

// subscribe 注册一个新连接。
func (h *sseHub) subscribe() *sseSubscriber {
	sub := &sseSubscriber{ch: make(chan contract.Event, sseBuffer), hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		// 已经关服：返回一个已关闭的通道，让调用方立刻收尾。
		close(sub.ch)
		sub.closed = true
		return sub
	}
	h.subs[sub] = struct{}{}
	return sub
}

// unsubscribe 注销连接。可重复调用（sync.Once 保证只关一次通道）。
func (h *sseHub) unsubscribe(sub *sseSubscriber) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.close()
}

// close 关闭单个订阅者。调用方必须已经把它从 hub 里摘掉，或者持有 hub 锁。
func (s *sseSubscriber) close() {
	s.once.Do(func() {
		s.closed = true
		close(s.ch)
	})
}

// broadcast 扇出一条事件。
//
// 绝不阻塞：通道满了说明这个消费者跟不上，直接摘掉它。
// 浏览器会自己重连并带 cursor 补读，所以丢弃是安全的——这正是断线补齐存在的理由。
func (h *sseHub) broadcast(ev contract.Event) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	var slow []*sseSubscriber
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			slow = append(slow, sub)
		}
	}
	for _, sub := range slow {
		delete(h.subs, sub)
	}
	h.mu.Unlock()

	// 摘掉之后在锁外关通道：close 会让 handler 的 select 立刻醒来收尾。
	for _, sub := range slow {
		sub.close()
	}
}

// closeAll 掐断所有连接，用于 Shutdown。
func (h *sseHub) closeAll() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*sseSubscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*sseSubscriber]struct{})
	h.mu.Unlock()

	for _, sub := range subs {
		sub.close()
	}
}

// count 返回当前连接数，仅用于日志与自检。
func (h *sseHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// ------------------------------------------------------------------ *
// 实时事件泵
// ------------------------------------------------------------------ *

// pumpEvents 把 EventStream 的实时事件转发进 hub。
//
// 订阅断了就重订阅（引擎/存储层重建、数据库短暂锁定都会造成断流）。
// 退出条件只有 baseCtx 结束——即 Shutdown。
func (s *Server) pumpEvents() {
	for {
		if s.baseCtx.Err() != nil {
			return
		}
		ch, err := s.opts.Events.Subscribe(s.baseCtx)
		if err != nil {
			if s.baseCtx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "web: 订阅实时事件失败，%s 后重试: %v\n", resubscribeDelay, err)
			if !sleepOrDone(s.baseCtx, resubscribeDelay) {
				return
			}
			continue
		}
		s.consumeEvents(ch)
		// 通道被上游关闭：等一小会儿再重订阅，避免热循环。
		if !sleepOrDone(s.baseCtx, resubscribeDelay) {
			return
		}
	}
}

func (s *Server) consumeEvents(ch <-chan contract.Event) {
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.hub.broadcast(ev)
		}
	}
}

// sleepOrDone 等待 d 或 ctx 结束；返回 false 表示 ctx 已结束。
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ------------------------------------------------------------------ *
// GET /api/events
// ------------------------------------------------------------------ *

// handleEvents 是 SSE 端点。
//
// 流程：订阅 hub → 从 cursor 补读历史 → 推一帧完整 state 让前端对齐 → 进入实时循环。
// 先订阅后补读是关键：反过来的话，补读期间产生的事件会永久丢失。
// 代价是补读内容可能与实时流重叠，用 lastSeqByRun 去重即可。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 不该发生：net/http 的 ResponseWriter 在 HTTP/1.1 下都支持 Flush。
		writeJSONError(w, http.StatusInternalServerError, "当前连接不支持流式输出")
		return
	}

	cursor := int64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "cursor 必须是非负整数")
			return
		}
		cursor = n
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// 关掉 nginx 之类中间层的缓冲（本机直连时无影响，但保留无害且明确）。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 先订阅，再补读。顺序不能反。
	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	enc := &sseWriter{w: w, f: flusher, rc: http.NewResponseController(w)}
	// lastSeqByRun 记录每个 Run 已发出的最大 seq，用于补读与实时流之间的去重。
	lastSeqByRun := make(map[string]int64)
	// lastCursor 是回给前端的游标：取所有 seq 的最大值。
	// 前端只有一个 cursor 变量（app.js:135），所以游标必须是全局单调的。
	var lastCursor int64 = cursor

	// 1) 补读历史。cursor == 0（刷新页面）也补读：否则界面要等到下一次输出才有内容。
	if _, err := s.replayEvents(r.Context(), cursor, lastSeqByRun, &lastCursor, enc); err != nil {
		// 头已经发出去了，没法改成错误响应；只能断开让前端重连重试。
		fmt.Fprintf(os.Stderr, "web: 补读事件失败(cursor=%d): %v\n", cursor, err)
		return
	}

	// 2) 推一帧完整快照，让前端对齐（app.js 里 `state` 具名事件 → applySnapshot）。
	s.pushStateFrame(r.Context(), enc)

	// 3) 实时循环。
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.baseCtx.Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				// hub 被关（Shutdown）或本连接被判定为慢消费者。
				return
			}
			if !acceptEvent(lastSeqByRun, ev) {
				continue
			}
			if ev.Seq > lastCursor {
				lastCursor = ev.Seq
			}
			if err := enc.event(ev); err != nil {
				return
			}
		case <-ping.C:
			// 双保险保活：注释行给代理看，具名 ping 给前端看（app.js 监听 'ping'）。
			if err := enc.comment("keepalive"); err != nil {
				return
			}
			if err := enc.named("ping", map[string]any{"ts": time.Now().UnixMilli()}); err != nil {
				return
			}
		}
	}
}

// replayEvents 补读并写出历史事件，返回写出的条数。
//
// 分批读取：一次最多 replayLimit 条，最多 8 批，防止一次重连把几万条事件全灌进浏览器。
// 剩余部分由前端下一次带新 cursor 重连时继续补齐。
func (s *Server) replayEvents(
	ctx context.Context,
	cursor int64,
	lastSeqByRun map[string]int64,
	lastCursor *int64,
	enc *sseWriter,
) (int, error) {
	const maxBatches = 8
	written := 0
	cur := cursor
	for batch := 0; batch < maxBatches; batch++ {
		evs, err := s.opts.Events.Replay(ctx, cur, replayLimit)
		if err != nil {
			return written, err
		}
		if len(evs) == 0 {
			return written, nil
		}
		for _, ev := range evs {
			if ev.Seq > cur {
				cur = ev.Seq
			}
			if !acceptEvent(lastSeqByRun, ev) {
				continue
			}
			if ev.Seq > *lastCursor {
				*lastCursor = ev.Seq
			}
			if err := enc.event(ev); err != nil {
				return written, err
			}
			written++
		}
		if len(evs) < replayLimit {
			return written, nil
		}
	}
	return written, nil
}

// acceptEvent 按 (runID, seq) 去重。seq 必须严格递增才算新事件。
//
// 为什么按 Run 分组而不是用单个全局 seq：契约里 seq 只在单个 Run 内单调
// （contract/event.go）。用 (RunID, Seq) 做键在两种实现下都正确。
func acceptEvent(lastSeqByRun map[string]int64, ev contract.Event) bool {
	if ev.Seq <= 0 {
		// 没有 seq 的事件无法去重，也无法让前端推进游标；直接丢弃。
		return false
	}
	if last, ok := lastSeqByRun[ev.RunID]; ok && ev.Seq <= last {
		return false
	}
	lastSeqByRun[ev.RunID] = ev.Seq
	return true
}

// pushStateFrame 推一帧完整快照。
//
// 失败不致命：前端启动时本来就会 GET /api/state，这一帧只是让"重连后立刻对齐"
// 更快，推不出去就算了。
func (s *Server) pushStateFrame(ctx context.Context, enc *sseWriter) {
	snap, err := s.opts.State.Snapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: SSE 推快照失败: %v\n", err)
		return
	}
	snap.Session.CSRF = s.csrf
	snap.Bots = nonNilBots(snap.Bots)
	snap.Pending = nonNilPending(snap.Pending)
	snap.Capabilities = nonNilCaps(snap.Capabilities)
	snap.Notices = nonNilNotices(snap.Notices)
	snap.Messages = nonNilMessages(snap.Messages)
	if err := enc.named("state", snap); err != nil {
		return
	}
}

// ------------------------------------------------------------------ *
// SSE 帧写出
// ------------------------------------------------------------------ *

// sseWriter 负责把帧写进 ResponseWriter 并 flush。
//
// 每帧写完立刻 Flush：SSE 的语义是"立刻可见"，攒着写会让界面看起来卡住。
type sseWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	rc *http.ResponseController
}

// refreshDeadline 给下一次写设一个超时。
//
// 客户端假死（TCP 零窗口，既不读也不关）时，写会永久阻塞在 Fprintf 里。
// 此时广播侧的"摘掉慢消费者"救不了它——那个 goroutine 不在 select 上等，
// 于是连接、订阅者和 goroutine 一起泄漏。写超时是唯一的出路。
// 超时只覆盖单次写，每帧刷新，健康的长连接不受影响。
func (s *sseWriter) refreshDeadline() {
	if s.rc != nil {
		// 不支持（如 httptest.ResponseRecorder）时返回 ErrNotSupported，忽略即可。
		_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	}
}

// event 写一条具名 `event` 帧（app.js:960 监听 'event'）。
func (s *sseWriter) event(ev contract.Event) error {
	frame := streamEvent{
		EventID:  ev.EventID,
		RunID:    ev.RunID,
		WorkID:   ev.WorkID,
		BotID:    ev.BotID,
		Seq:      ev.Seq,
		Kind:     string(ev.Kind),
		Text:     ev.Text,
		TS:       ev.TS.UnixMilli(),
		Session:  ev.Session,
		Final:    ev.Final,
		Approval: ev.Approval,
	}
	// BotName 留给上层填（这里没有 bot 名字表）；为空时前端退回显示 botId。
	return s.named("event", frame)
}

// named 写一条具名事件帧。data 必须是可 JSON 序列化的值。
func (s *sseWriter) named(name string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		// 序列化失败不能把坏帧发出去，否则前端 JSON.parse 会抛异常并断开连接。
		fmt.Fprintf(os.Stderr, "web: 序列化 SSE 帧 %q 失败: %v\n", name, err)
		return nil
	}
	s.refreshDeadline()
	// 一行一个 data：JSON 里不含裸换行，所以单行 data 是安全的。
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// comment 写一条注释行，用于保活。前端把它当空帧忽略。
func (s *sseWriter) comment(text string) error {
	s.refreshDeadline()
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
