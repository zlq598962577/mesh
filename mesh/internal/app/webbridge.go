package app

// webBridge 把 store / orchestrator 的能力桥接成 web 包要的五个窄接口
// （StateSource / EventStream / CostSource / ControlSink / MessageSink）。
//
// web 包不 import orchestrator / store（task-5 规格），所以这层胶水在 app。
// 每个实现都遵守同一类诚实规则：不知道的显示"未知"，没发生的绝不伪造。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mesh/internal/contract"
	"mesh/internal/orchestrator"
	"mesh/internal/store"
	"mesh/internal/web"
)

// ringCap 是内存消息环的容量。P0 的聊天历史只在内存里展示（重启后只显示
// 新消息）——messages 表里有落库数据，但存储层没有时间线查询方法，
// 为不临时刻意加宽存储层接口，P0 如实接受这个限制（README 记录）。
const ringCap = 200

type webBridge struct {
	st   *store.Store
	orch *orchestrator.Orchestrator
	cfg  Config
	bots []contract.BotSpec
	mux  *engineMux
	disp *dispatcher

	mu       sync.Mutex
	ring     []web.MessageView
	subs     map[chan contract.Event]struct{}
	rowidCur int64 // EventsAfterRowid 的游标推进水位（Replay 用独立游标，见 Replay 注释）
}

func newWebBridge(st *store.Store, orch *orchestrator.Orchestrator, cfg Config, bots []contract.BotSpec, mux *engineMux) *webBridge {
	return &webBridge{
		st:   st,
		orch: orch,
		cfg:  cfg,
		bots: bots,
		mux:  mux,
		subs: make(map[chan contract.Event]struct{}),
	}
}

// ---------------- StateSource ----------------

func (b *webBridge) Snapshot(ctx context.Context) (web.Snapshot, error) {
	snap := web.Snapshot{
		Session: web.SessionView{RoomID: b.cfg.RoomID},
	}

	works, err := b.st.ListWorks(ctx, b.cfg.RoomID, []contract.WorkState{
		contract.WorkQueued, contract.WorkExecuting, contract.WorkReviewing,
		contract.WorkPaused, contract.WorkWaitingUser,
	})
	if err != nil {
		return snap, fmt.Errorf("app: 读取进行中委托失败: %w", err)
	}
	unsettled := map[string]bool{}
	for _, eff := range b.orch.UnsettledEffects() {
		unsettled[eff.Key.WorkID] = true
	}
	if len(works) > 0 {
		w := works[0] // 单聊天框 P0：一次只突出最早一个未完成委托（§5.4 当前 Work）
		snap.Current = &web.WorkView{
			WorkID: w.ID, Goal: w.Goal, State: string(w.State),
			Executor:  web.RoleView{Name: w.Assign.Executor, Version: w.Assign.Version},
			Reviewer:  web.RoleView{Name: w.Assign.Reviewer, Version: w.Assign.Version},
			Rounds:    w.ReviewRounds, MaxRounds: contract.MaxAutoReviewRounds,
			StopUnconfirmed: unsettled[w.ID],
			StopDetail:      stopDetail(w.ID, unsettled),
		}
		pend, err := b.st.PendingFor(ctx, w.ID)
		if err != nil {
			return snap, fmt.Errorf("app: 读取待处理项失败: %w", err)
		}
		for _, p := range pend {
			snap.Pending = append(snap.Pending, web.PendingView{
				ID: p.ID, Kind: string(p.Kind), WorkID: p.WorkID,
				Title:   pendingTitle(p),
				Detail:  "尚无外部副作用，可以安全撤销；已发出的动作不在列表里",
				Options: []string{"撤销"},
			})
		}
	}

	// 能力受限（三类"未知"之三）：适配器未接入的 bot 逐个如实列出。
	for _, bs := range b.bots {
		eng, err := b.muxEngine(bs.ID)
		if err == nil && eng != nil {
			continue
		}
		snap.Capabilities = append(snap.Capabilities, web.CapView{
			Scope: bs.ID, Label: "适配器未接入",
			Detail: "该 bot 的客户端适配器尚未实现，委托会停留在队列里",
		})
	}

	// bot 状态行。P0 只区分"有在办委托的执行者 = 思考中"与"空闲"；
	// 等授权/操作浏览器等细粒度状态依赖引擎事件流，适配器落地后接入（README 记录）。
	for _, bs := range b.bots {
		status := "空闲"
		detail := ""
		if snap.Current != nil && snap.Current.Executor.Name == bs.ID {
			status = "思考中"
			detail = "该 bot 是当前委托的执行者"
		}
		snap.Bots = append(snap.Bots, web.BotView{
			ID: bs.ID, Name: bs.Name, Adapter: bs.Engine.Adapter,
			Status: status, StatusDetail: detail,
		})
	}

	b.mu.Lock()
	snap.Messages = append([]web.MessageView(nil), b.ring...)
	b.mu.Unlock()
	return snap, nil
}

func stopDetail(workID string, unsettled map[string]bool) string {
	if unsettled[workID] {
		return "已有停止请求，但客户端界面不可读，无法确认是否已停（停止待确认）"
	}
	return ""
}

func pendingTitle(p orchestrator.Pending) string {
	if p.Kind == orchestrator.PendingApproval {
		return fmt.Sprintf("等待 %s 处理的审批", p.BotID)
	}
	return fmt.Sprintf("尚未投递给 %s 的消息", p.BotID)
}

func (b *webBridge) muxEngine(botID string) (contract.Engine, error) {
	b.mux.mu.RLock()
	eng := b.mux.engines[botID]
	b.mux.mu.RUnlock()
	if eng == nil {
		return nil, &EngineMissingError{Adapter: botID}
	}
	return eng, nil
}

// ---------------- MessageSink ----------------

func (b *webBridge) SendMessage(ctx context.Context, req web.SendRequest) (web.MessageView, error) {
	if strings.TrimSpace(req.Text) == "" {
		return web.MessageView{}, errors.New("app: 消息内容不能为空")
	}
	// 界面指定的分工与聊天里写的指令走同一条路：并进文本让编排层按 §12 的
	// 优先级解析，产生同一份分工变更记录（方案 §12 的硬要求）。
	text := req.Text
	if req.Executor != "" {
		if err := sanitizeBotID(req.Executor); err != nil {
			return web.MessageView{}, fmt.Errorf("app: 执行者 id 非法: %w", err)
		}
		text += fmt.Sprintf("\n改由 %s 执行", req.Executor)
	}
	if req.Reviewer != "" {
		if err := sanitizeBotID(req.Reviewer); err != nil {
			return web.MessageView{}, fmt.Errorf("app: 审查者 id 非法: %w", err)
		}
		text += fmt.Sprintf("\n改由 %s 审查", req.Reviewer)
	}

	now := time.Now()
	msg := contract.Message{
		ID:              newMessageID(now),
		RoomID:          b.cfg.RoomID,
		ClientMessageID: req.ClientMessageID,
		FromID:          "user",
		FromKind:        contract.FromUser,
		Kind:            contract.MsgChat,
		Text:            text,
		Mentions:        mentionsIn(text, b.bots),
		CreatedAt:       now,
	}
	targets, notice, err := b.orch.OnUserMessage(ctx, msg)
	if err != nil {
		return web.MessageView{}, fmt.Errorf("app: 编排层拒绝该消息: %w", err)
	}

	// 委托创建（§5.6 顺序：先建 Work 再产生投递）。消息无 Work 且路由出了
	// 执行目标时，把这条消息立为一个新委托：执行者取界面选择或第一个 @ 目标，
	// 审查者取界面选择（可选）。Run 必须挂在 Work 下（ClaimDelivery 只领
	// 有 work_id 的投递），不建 Work 的消息永远派不出去。
	executor := req.Executor
	if executor == "" && len(targets) > 0 {
		executor = targets[0].BotID
	}
	if executor != "" && msg.WorkID == "" {
		if err := sanitizeBotID(executor); err != nil {
			return web.MessageView{}, fmt.Errorf("app: 执行者 id 非法: %w", err)
		}
		w, werr := b.createDelegation(ctx, msg, executor, req.Reviewer)
		if werr != nil {
			return web.MessageView{}, werr
		}
		msg.WorkID = w.ID
	}

	// 落库 + 建投递（提交成功后才算送达，§5.6）。targets 去重由 CreateMessage 做。
	if err := b.st.CreateMessage(ctx, &msg, targetsToIDs(targets)); err != nil {
		return web.MessageView{}, fmt.Errorf("app: 消息落库失败: %w", err)
	}
	if notice != "" {
		b.pushView(b.noticeView(notice, now))
	}
	b.disp.Kick() // 投递已入队，唤醒派发循环立即领取
	return b.pushView(b.userView(msg)), nil
}

// createDelegation 把一条用户消息立为一个新委托（v1 分工：执行者 + 可选审查者）。
func (b *webBridge) createDelegation(ctx context.Context, msg contract.Message, executor, reviewer string) (contract.Work, error) {
	now := time.Now()
	assign := contract.RoleAssignment{
		Version: 1, Executor: executor, Reviewer: reviewer,
		Source: contract.SourceUserChat, At: now,
	}
	w := contract.Work{
		ID:        fmt.Sprintf("work-%s", msg.ID),
		RoomID:    msg.RoomID,
		ThreadID:  msg.ThreadID,
		Goal:      msg.Text,
		State:     contract.WorkQueued,
		Assign:    assign,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := b.st.SaveWork(ctx, w); err != nil {
		return contract.Work{}, fmt.Errorf("app: 创建委托失败: %w", err)
	}
	if err := b.st.SaveAssignment(ctx, w.ID, assign); err != nil {
		return contract.Work{}, fmt.Errorf("app: 记录委托分工失败: %w", err)
	}
	return w, nil
}

func targetsToIDs(targets []orchestrator.WakeTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.BotID)
	}
	return out
}

// mentionPattern 提取 "@词" 形式的提及（词 = 字母数字下划线连字符）。
var mentionPattern = regexp.MustCompile(`@([a-zA-Z0-9_\-]+)`)

// mentionsIn 从文本提取 @ 提及并只保留已加载的 bot。
//
// 为什么在组装层做：编排层的路由只认 msg.Mentions 字段，而它自己的分工解析
// 有"文本里认人"的兜底、路由没有——两条路径不一致。消息入口统一解析提及，
// 让 @ 提及在路由与分工解析里行为一致。
func mentionsIn(text string, bots []contract.BotSpec) []string {
	matches := mentionPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(bots))
	for _, b := range bots {
		known[strings.ToLower(b.ID)] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, m := range matches {
		id := strings.ToLower(m[1])
		if _, ok := known[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ---------------- ControlSink ----------------

// Control 把界面动作翻译成编排层的控制指令文本（§5.8 控制动词），
// 复用同一条解析与护栏路径，保证界面与聊天产生一致的行为与记录。
func (b *webBridge) Control(ctx context.Context, workID string, req web.ControlRequest) (string, error) {
	var text string
	switch req.Action {
	case "pause":
		// 译文必须是 orchestrator.controlCommands 命令表里的整句（"暂停"），
		// 不能写成"暂停这件事"——命令表只做整句匹配，防误停（§5.8）。
		text = "暂停"
	case "resume":
		text = "继续"
	case "stop":
		text = "停止"
	case "replace":
		if err := sanitizeBotID(req.Executor); err != nil {
			return "", fmt.Errorf("app: 新执行者 id 非法: %w", err)
		}
		text = "改由 " + req.Executor + " 执行"
	case "approval":
		return b.resolveApproval(ctx, req)
	case "dismiss_pending":
		if req.PendingID == "" {
			return "", errors.New("app: dismiss_pending 需要 pendingId")
		}
		if err := b.st.RevokePending(ctx, req.PendingID, "用户在界面撤销"); err != nil {
			return "", err
		}
		return "已撤销（该项尚无外部副作用）", nil
	default:
		return "", fmt.Errorf("app: 未知控制动作 %q", req.Action)
	}

	now := time.Now()
	msg := contract.Message{
		ID: newMessageID(now), RoomID: b.cfg.RoomID, WorkID: workID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
		Text: text, CreatedAt: now,
	}
	_, notice, err := b.orch.OnUserMessage(ctx, msg)
	if err != nil {
		return "", fmt.Errorf("app: 控制指令未生效: %w", err)
	}
	if err := b.st.CreateMessage(ctx, &msg, nil); err != nil {
		return "", fmt.Errorf("app: 控制消息落库失败: %w", err)
	}
	if notice == "" {
		notice = "已执行：" + text
	}
	b.pushView(b.noticeView(notice, now))
	return notice, nil
}

// resolveApproval 处理界面审批。P0 如实口径：平台无法替客户端点"允许"，
// 除非适配器声明了可桥接；当前没有可桥接的适配器，一律引导用户去客户端。
func (b *webBridge) resolveApproval(ctx context.Context, req web.ControlRequest) (string, error) {
	if req.ApprovalID == "" {
		return "", errors.New("app: approval 需要 approvalId")
	}
	note := "请在客户端窗口内确认该授权；本机平台不代客户端点\"允许\"（§9.3：语义与目标无法验证时不桥接）"
	b.pushView(b.noticeView(note, time.Now()))
	return note, nil
}

// ---------------- EventStream ----------------

// Replay 用 store.EventsAfterRowid 的全局 rowid 游标做断线补齐。
// cursor 语义 = "这个 rowid 之前（含）的我都见过了"。
func (b *webBridge) Replay(ctx context.Context, cursor int64, limit int) ([]contract.Event, error) {
	evs, next, err := b.st.EventsAfterRowid(ctx, cursor, limit)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if next > b.rowidCur {
		b.rowidCur = next
	}
	b.mu.Unlock()
	return evs, nil
}

// Subscribe 加入实时扇出。P0 没有引擎事件源（适配器未接入），通道平时安静，
// web 层的 SSE keepalive 负责连接保活；事件源接入后 pumpEvents 会向这里投递。
func (b *webBridge) Subscribe(ctx context.Context) (<-chan contract.Event, error) {
	ch := make(chan contract.Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// fanout 向订阅者投递事件；慢订阅者直接跳过（非阻塞 send），
// 绝不让一个卡住的浏览器拖住事件生产方（web.EventStream 的契约）。
func (b *webBridge) fanout(ev contract.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// PostSystemNotice 把平台侧的失败/提示写进房间消息流：落库（重启前的排障
// 还能查到）+ 内存环（下次快照可见）+ SSE error 事件（已打开的页面立即
// 显示并触发快照刷新）。没有这条通道时，投递失败只进 events 表和日志，
// 用户看到的是"排队中"永远不动——这是 2026-11-14 @codex 无反应事故的直接
// 原因之一，此方法是它的修复面。
func (b *webBridge) PostSystemNotice(ctx context.Context, roomID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	now := time.Now()
	msg := contract.Message{
		ID:        newMessageID(now),
		RoomID:    roomID,
		FromID:    "mesh",
		FromKind:  contract.FromSystem,
		Kind:      contract.MsgNotice,
		Text:      text,
		CreatedAt: now,
	}
	if err := b.st.CreateMessage(ctx, &msg, nil); err != nil {
		// 落库失败不拦通知本身：环 + SSE 至少保证当次会话可见。
		fmt.Fprintf(os.Stderr, "webbridge: 系统通知落库失败: %v\n", err)
	}
	b.pushView(b.noticeView(text, now))
	b.fanout(contract.Event{
		EventID:       fmt.Sprintf("notice-%d", now.UnixNano()),
		Kind:          contract.EventError,
		Text:          text,
		TS:            now,
		SchemaVersion: contract.SchemaVersion,
	})
}

// ---------------- CostSource ----------------

func (b *webBridge) Cost(ctx context.Context, dim string) (web.CostReport, error) {
	var buckets []store.CostBucket
	var err error
	switch dim {
	case "bot":
		buckets, err = b.st.UsageByBot(ctx, time.Time{})
	case "room":
		buckets, err = b.st.UsageByRoom(ctx, time.Time{})
	case "day":
		buckets, err = b.st.UsageByDay(ctx, time.Time{})
	default:
		return web.CostReport{}, fmt.Errorf("app: 未知成本维度 %q（可选 bot / room / day）", dim)
	}
	if err != nil {
		return web.CostReport{}, err
	}

	rep := web.CostReport{UnknownReason: "桌面客户端不上报 token 用量，这部分调用的金额与 token 均为未知（§9.6）"}
	// 币种下标而不是指针副本：多币种交错到达时按"最后一个 append 的行"更新
	// 会把 A 币种的累加值写进 B 币种的行（钱相关的代码不许这种隐晦写法）。
	seenCur := map[string]int{}
	for _, bk := range buckets {
		row := web.CostRow{
			Key:                bk.Key,
			Label:              bucketLabel(dim, bk.Key),
			Calls:              int64(bk.Summary.Calls),
			UnknownAmountCalls: int64(bk.Summary.UnknownAmountCalls),
			UnknownReason:      rep.UnknownReason,
			Note:               bucketNote(bk.Summary),
			CallsWithTokens:    int64(bk.Summary.CallsWithTokens),
			TokensIn:           bk.Summary.TokensIn,
			TokensOut:          bk.Summary.TokensOut,
		}
		for _, cur := range bk.Summary.ByCurrency {
			idx, ok := seenCur[cur.Currency]
			if !ok {
				rep.ByCurrency = append(rep.ByCurrency, web.Money{Currency: cur.Currency})
				idx = len(rep.ByCurrency) - 1
				seenCur[cur.Currency] = idx
			}
			rep.ByCurrency[idx].AmountMicros += cur.AmountMicros
		}
		rep.Calls += int64(bk.Summary.Calls)
		rep.UnknownAmountCalls += int64(bk.Summary.UnknownAmountCalls)
		rep.ReservedCalls += int64(bk.Summary.ReservedCalls)
		rep.CallsWithTokens += int64(bk.Summary.CallsWithTokens)
		rep.TokensIn += bk.Summary.TokensIn
		rep.TokensOut += bk.Summary.TokensOut
		rep.Rows = append(rep.Rows, row)
	}
	sort.Slice(rep.Rows, func(i, j int) bool { return rep.Rows[i].Key < rep.Rows[j].Key })

	// 限额如实展示：桌面客户端不上报用量 → token/金额硬顶无法由本机强制（§9.6）。
	rep.Limits = append(rep.Limits, web.CostLimit{
		Label: "Token / 金额硬顶",
		Limit: "未知",
		Note:  "四个桌面客户端都不上报逐次用量，本机平台无法把这些上限当硬线卡；护栏只按步数与时长生效",
	})
	return rep, nil
}

func bucketLabel(dim, key string) string {
	if key == "" {
		if dim == "bot" {
			return "（未关联到 bot 的调用）"
		}
		if dim == "room" {
			return "（未关联到房间的调用）"
		}
		return "（未关联的调用）"
	}
	return key
}

func bucketNote(s store.UsageSummary) string {
	if s.UnknownAmountCalls == 0 {
		return ""
	}
	return fmt.Sprintf("另有 %d 次调用金额未知，未计入上方金额", s.UnknownAmountCalls)
}

// ---------------- 消息环 ----------------

func (b *webBridge) userView(m contract.Message) web.MessageView {
	return web.MessageView{
		ID: m.ID, Author: "你", AuthorKind: "user", Kind: string(m.Kind),
		Text: m.Text, CreatedAtMs: m.CreatedAt.UnixMilli(),
	}
}

// PostBotMessage 把引擎产出的回复写进聊天环并落库为房间消息。
//
// 之前回复只进 SSE 增量：浏览器没开着或刷新后，回复就"消失"了——用户看到
// 的就是"发消息没用"。这里双写：内存环（Snapshot 立即可见）+ messages 表
// （刷新后 SaveMessage 兜底？P0 环形缓冲的已知限制仍在，环丢即丢，如实）。
// 落库失败不阻塞呈现：环里已经有了，数据库少一条 bot 回复不影响交互。
func (b *webBridge) PostBotMessage(ctx context.Context, botID, text string) {
	now := time.Now()
	m := contract.Message{
		ID: newMessageID(now), RoomID: b.cfg.RoomID,
		FromID: botID, FromKind: contract.FromBot, Kind: contract.MsgResult,
		Text: text, CreatedAt: now,
	}
	if err := b.st.SaveMessage(ctx, m); err != nil {
		log.Printf("[mesh] bridge: bot 回复落库失败(bot=%s): %v", botID, err)
	}
	b.pushView(web.MessageView{
		ID: m.ID, Author: b.botName(botID), AuthorKind: "bot", Kind: string(contract.MsgResult),
		Text: text, CreatedAtMs: now.UnixMilli(),
	})
}

// botName bot 的显示名；找不到时回退 id（不编名字）。
func (b *webBridge) botName(botID string) string {
	for _, bs := range b.bots {
		if bs.ID == botID {
			return bs.Name
		}
	}
	return botID
}

func (b *webBridge) noticeView(text string, at time.Time) web.MessageView {
	return web.MessageView{
		ID: newMessageID(at), Author: "mesh", AuthorKind: "system", Kind: string(contract.MsgNotice),
		Text: text, CreatedAtMs: at.UnixMilli(),
	}
}

func (b *webBridge) pushView(v web.MessageView) web.MessageView {
	b.mu.Lock()
	b.ring = append(b.ring, v)
	if len(b.ring) > ringCap {
		b.ring = b.ring[len(b.ring)-ringCap:]
	}
	b.mu.Unlock()
	return v
}

// msgSeq 用原子计数：SendMessage/Control 从并发的 HTTP handler goroutine 调用，
// 普通自增是数据竞争（-race 因 cgo 红线不可用，更要从写法上消除而不是靠运气）。
var msgSeq atomic.Int64

func newMessageID(at time.Time) string {
	return fmt.Sprintf("m-%d-%d", at.UnixMilli(), msgSeq.Add(1))
}
