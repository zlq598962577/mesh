package app

// dispatcher：组装层的派发 worker（P0 串行实现，README 已知限制第 5 条的落地）。
//
// 职责链（方案 §5.6）：
//
//	领取投递（ClaimDelivery 原子领取 + 建 Run）
//	  → 建立/复用引擎会话（每 bot 一个专用会话）
//	  → 驱动引擎 Send（desktop 三段协议）
//	  → 读引擎事件并"归位"（WorkID/RunID/Seq 由本层补齐——适配器无法知道
//	    当前 Run，这是 common.go 明确留给组装层的职责）
//	  → 事件落库（AppendEvent，UI 断线补齐用）+ SSE 扇出
//	  → Run 收尾（FinishRun 释放名额）
//
// P0 的诚实边界：单 worker 串行领取——一次只处理一条投递，Send 返回即收尾。
// §5.6 的"跨 Work 公平轮转"由调度器支持，但串行 worker 用不到它；并发派发
// 属于 P1。失败后的重试由 store 层的 attempt/next_attempt_at 机制自动排队
// （环境恢复后自动成功），而"不核实就盲目重发"仍被 desktop.Sender 从构造上
// 封死——重试的是"尚未发生的投递"，不是"可能已送达的注入"。
//
// handle 内部真实流程（P0a 实测验证过）：Codex 缩在托盘时 LocateClientWindow
// 如实报"进程在运行但窗口不可见"，投递按 attempt 重试，用户还原窗口后下一轮
// 轮询自动送达。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"mesh/internal/adapters"
	"mesh/internal/contract"
	"mesh/internal/orchestrator"
	"mesh/internal/store"
)

// pickEngine 查询某 bot 的引擎；未注入（适配器缺失）返回错误而不是占位引擎，
// 派发层需要"缺失即失败"的语义，占位引擎的诚实报错留给 UI 层呈现。
func (d *dispatcher) pickEngine(botID string) (contract.Engine, error) {
	eng, err := d.mux.pick(botID)
	if err != nil {
		return nil, err
	}
	if _, missing := eng.(unavailableEngine); missing {
		return nil, &EngineMissingError{Adapter: botID}
	}
	return eng, nil
}

type dispatcher struct {
	st     *store.Store
	orch   *orchestrator.Orchestrator
	mux    *engineMux
	bridge *webBridge
	cfg    Config
	bots   []contract.BotSpec

	kick chan struct{}

	sessMu   sync.Mutex
	sessions map[string]contract.Session // botID → 已开的引擎会话

	runMu     sync.Mutex
	currentRun map[string]string // botID → 当前 RunID（事件归位用）
	runSeq    map[string]int64  // RunID → 已用最大 seq
}

func newDispatcher(st *store.Store, orch *orchestrator.Orchestrator, mux *engineMux, bridge *webBridge, cfg Config, bots []contract.BotSpec) *dispatcher {
	return &dispatcher{
		st: st, orch: orch, mux: mux, bridge: bridge, cfg: cfg, bots: bots,
		kick:      make(chan struct{}, 1),
		sessions:  map[string]contract.Session{},
		currentRun: map[string]string{},
		runSeq:    map[string]int64{},
	}
}

// Kick 让领取循环立即醒来（用户刚发消息时不用等轮询间隔）。
func (d *dispatcher) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Start 启动派发循环，阻塞到 ctx 取消。
func (d *dispatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.shutdownSessions()
			return
		case <-d.kick:
		case <-ticker.C:
		}
		for {
			more, err := d.step(ctx)
			if err != nil {
				log.Printf("[mesh] 派发: %v", err)
			}
			if !more {
				break
			}
		}
	}
}

// step 领取并处理一条投递。返回 false 表示队列已空。
func (d *dispatcher) step(ctx context.Context) (bool, error) {
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	dl, err := d.st.ClaimDelivery(ctx, time.Now(), runID)
	if err != nil {
		return false, fmt.Errorf("领取投递失败: %w", err)
	}
	if dl == nil {
		return false, nil
	}
	d.handle(ctx, dl, runID)
	return true, nil
}

// handle 执行一条投递的完整生命周期。
func (d *dispatcher) handle(ctx context.Context, dl *store.Delivery, runID string) {
	msg, err := d.st.GetMessage(ctx, dl.MessageID)
	if err != nil {
		d.failDelivery(ctx, dl, runID, fmt.Sprintf("投递的消息读不到: %v", err))
		return
	}
	work, err := d.st.GetWork(ctx, dl.WorkID)
	if err != nil {
		d.failDelivery(ctx, dl, runID, fmt.Sprintf("投递挂在读不到的委托 %s 上: %v", dl.WorkID, err))
		return
	}

	ticket, err := d.orch.StartRun(work.ID, dl.BotID, runID)
	if err != nil {
		d.closeRun(ctx, runID)
		d.failDelivery(ctx, dl, runID, fmt.Sprintf("执行名额不可得: %v", err))
		return
	}

	// 推进状态机 queued → executing（§5.7）。之前漏了这一步：投递 done、
	// 引擎跑完，Work 却永远停在"排队中"，用户以为什么都没发生。
	if err := d.orch.MarkExecuting(ctx, work.ID); err != nil {
		log.Printf("[mesh] 派发: 推进 %s 到 executing 失败: %v", work.ID, err)
	}

	d.runMu.Lock()
	d.currentRun[dl.BotID] = runID
	d.runSeq[runID] = 0
	d.runMu.Unlock()

	eng, err := d.pickEngine(dl.BotID)
	if err != nil {
		d.orch.FinishRun(ticket)
		d.closeRun(ctx, runID)
		d.failDelivery(ctx, dl, runID, err.Error())
		return
	}
	// 会话先行：事件通道必须从"本次 Send 用的那个会话"上取。
	// 顺序反了（先取通道后开会话）会读到旧会话的已关闭通道，一个事件都收不到。
	sess, err := d.ensureSession(ctx, dl.BotID)
	if err != nil {
		d.orch.FinishRun(ticket)
		d.closeRun(ctx, runID)
		d.failDelivery(ctx, dl, runID, fmt.Sprintf("引擎会话打开失败: %v", err))
		return
	}
	// 事件归位与落库的 reader：适配器 Events 关闭 = 会话终结，reader 退出。
	evCh := eng.Events(sess)
	go d.readEvents(evCh, work.ID, dl.BotID, runID)

	turn, sendErr := eng.Send(ctx, sess, msg)
	d.orch.FinishRun(ticket)

	// Run 收尾落库：runs 表的 status 不能永远停在 running（§5.6 重启恢复
	// 以 running 为"被中断"的依据）。Send 出错即 failed，成功即 done。
	runStatus := "done"
	if sendErr != nil {
		runStatus = "failed"
	}
	if err := d.st.FinishRun(ctx, runID, runStatus, time.Now()); err != nil {
		log.Printf("[mesh] 派发: Run 收尾落库失败: %v", err)
	}

	// 失败不手动重试，但也不阻止重试：MarkDeliveryFailed 会在 attempt 未达
	// 上限时自动重新入队（next_attempt_at），环境恢复（如用户把最小化到
	// 托盘的客户端还原出来）后派发循环会自动重试成功。桌面客户端没有幂等
	// 键，"盲目立即重发"仍被 desktop.Sender 层从构造上封死。
	//
	// 两种失败必须分开处理（2026-11-14 @codex 事故的第二个根因：此前一律
	// MarkDeliveryDone，"确定没送达"的失败也失去了重试机会）：
	//   - turn == nil：引擎确认没往客户端写任何东西（窗口没了/锁屏/取锁失败/
	//     核实结论 NotVerified）。重发是安全的，按普通失败走重试队列。
	//   - turn != nil：注入已执行但无法核实是否送达。绝不自动重发——重复提交
	//     的风险大于收益；投递标 done，并把"请到客户端确认"推给用户。
	if sendErr != nil {
		errText := fmt.Sprintf("引擎发送返回错误（结果未确认或未送达）：%v", sendErr)
		d.emitRunError(ctx, work.ID, runID, dl.BotID, errText) // 之前漏了扇出：浏览器永远看不到发送失败
	}
	if sendErr != nil && turn == nil {
		status, ferr := d.st.MarkDeliveryFailed(ctx, dl.ID,
			fmt.Sprintf("引擎发送失败（确认未写入客户端，可安全重试）：%v", sendErr), time.Now())
		if ferr != nil {
			log.Printf("[mesh] 派发: 标记投递失败状态时出错: %v", ferr)
		}
		if status == store.DeliveryFailed {
			d.notifyDeliveryFailed(dl)
		}
	} else {
		if err := d.st.MarkDeliveryDone(ctx, dl.ID, time.Now()); err != nil {
			log.Printf("[mesh] 派发: 标记投递完成失败: %v", err)
		}
		if sendErr != nil {
			// 注入已执行但结果未知：绝不自动重发，把核实责任交给用户。
			d.bridge.PostSystemNotice(ctx, work.RoomID, fmt.Sprintf(
				"已尝试把消息发送给 %s，但无法核实客户端是否收到（核实路径不可用）。请到客户端输入区确认是否已发出；平台不会自动重发，也不会重复提交。",
				dl.BotID))
		}
	}
	d.runMu.Lock()
	delete(d.currentRun, dl.BotID)
	d.runMu.Unlock()
}

// closeRun 在 Send 之前的早退路径上关闭 Run 行。
// 此前这些路径只调 orch.FinishRun（释放内存名额），runs 表的行永远停在
// running——重启恢复把它们误判成"上次崩溃的现场"（2026-11-14 事故留下的
// 5 条僵尸 running）。失败语义：Run 确实结束且以失败告终。
func (d *dispatcher) closeRun(ctx context.Context, runID string) {
	if err := d.st.FinishRun(ctx, runID, store.RunFailed, time.Now()); err != nil {
		log.Printf("[mesh] 派发: Run 行收尾失败(run=%s): %v", runID, err)
	}
}

// notifyDeliveryFailed 投递重试超限时把原因推给用户，不能静默停在"排队中"。
func (d *dispatcher) notifyDeliveryFailed(dl *store.Delivery) {
	reason := ""
	if dl != nil && dl.ID != "" {
		reason = fmt.Sprintf("投递 %s 重试 %d 次后放弃", dl.ID, store.MaxDeliveryAttempt)
	}
	// last_error 里的具体原因 MarkDeliveryFailed 已写入；通知里引用失败原因。
	d.bridge.PostSystemNotice(context.Background(), dl.RoomID,
		fmt.Sprintf("%s：%s。请按提示处理客户端（如把窗口还原到桌面）后重新发送一条消息；已失败的投递不会自动恢复。", reason, d.lastDeliveryError(dl.ID)))
}

// lastDeliveryError 读取投递的 last_error 给通知用；读不到时如实说明。
func (d *dispatcher) lastDeliveryError(deliveryID string) string {
	errText, err := d.st.DeliveryLastError(context.Background(), deliveryID)
	if err != nil || errText == "" {
		return "具体原因见服务端日志"
	}
	return errText
}

// ensureSession 打开（或复用）一个 bot 的引擎会话。
//
// 复用前检查会话是否已关闭：CLI 引擎每轮 Send 以 final 收尾（Emitter 约定
// final 会关事件通道），旧会话的 emitter 已死——复用它等于把回复全部丢弃
//（2026-11-14「DSH 发消息没用」的第三个根因）。检测到关闭就重建。
func (d *dispatcher) ensureSession(ctx context.Context, botID string) (contract.Session, error) {
	d.sessMu.Lock()
	defer d.sessMu.Unlock()
	if s, ok := d.sessions[botID]; ok && !sessionClosed(s) {
		return s, nil
	}
	// 已关闭或不存在：丢弃缓存，走重建。
	delete(d.sessions, botID)

	var spec contract.BotSpec
	for _, b := range d.bots {
		if b.ID == botID {
			spec = b
			break
		}
	}
	if spec.ID == "" {
		return nil, fmt.Errorf("app: bot %q 不在已加载列表中", botID)
	}
	eng, err := d.pickEngine(botID)
	if err != nil {
		return nil, err
	}
	ws := contract.Workspace{Mode: "private", Dir: d.cfg.BotWorkspace(botID)}
	s, err := eng.Open(ctx, spec, ws)
	if err != nil {
		return nil, err
	}
	d.sessions[botID] = s
	return s, nil
}

// sessionClosed 判断引擎会话是否已终结（final 已发 / Close 已调）。
//
// 派发器经 pickEngine 拿到的是裸引擎，Open 返回的是 *adapters.SessionHandle
// 本身（不经 mux 包装）——之前的类型断言只认 *muxSession，永远失败，导致
// 已销毁的旧会话被当成"活着"复用，Send 报"会话不存在"（2026-11-14 用户
// 实测事故第二根因）。两种形态都认，杜绝再犯。
func sessionClosed(s contract.Session) bool {
	switch sh := s.(type) {
	case *adapters.SessionHandle:
		return sh.Closed()
	case *muxSession:
		if inner, ok := sh.inner.(*adapters.SessionHandle); ok {
			return inner.Closed()
		}
	}
	return false
}

// eventsFor 已废弃删除：事件通道必须从"本次 Send 用的那个会话"上取，
// 从缓存取会话再取通道会读到已关闭的旧通道（见 handle 内的顺序注释）。

// readEvents 把适配器事件归位（WorkID/RunID/Seq）后落库、扇出到 SSE，
// 并把回复呈现进聊天流。
//
// 重复输出的根因（2026-11-14 用户实测）：CLI 通道的 output 事件已经被前端
// 渲染成一条流式消息（final 时原位转正），后端再 PostBotMessage 合成一条
// 完整正文 = 同样的内容出现两次。呈现口径修正：
//   - 有 output 事件的 Run（流式通道）：前端流式节点在 final 时转正，后端
//     不再合成——回复以流式节点为准；
//   - 无 output 事件的 Run（桌面适配器等只发 final 的通道）：后端合成一条，
//     用户至少能看到结论；
//   - error 恒即时进聊天流（失败必须可见）。
// final 后按既定语义结束本 reader；有审查者的 Work 不自动完成
//（CompleteWork 会拒绝），停在 executing 等审查。
func (d *dispatcher) readEvents(ch <-chan contract.Event, workID, botID, runID string) {
	var streamed bool
	for ev := range ch {
		ev.WorkID = workID
		ev.RunID = runID
		ev.BotID = botID
		ev.Seq = d.nextSeq(runID)
		if ev.TS.IsZero() {
			ev.TS = time.Now()
		}
		if ev.SchemaVersion == 0 {
			ev.SchemaVersion = contract.SchemaVersion
		}
		if err := d.st.AppendEvent(context.Background(), ev); err != nil {
			log.Printf("[mesh] 派发: 事件落库失败(run=%s seq=%d): %v", runID, ev.Seq, err)
		}
		d.bridge.fanout(ev)

		switch ev.Kind {
		case contract.EventOutput:
			streamed = true
		case contract.EventError:
			d.bridge.PostBotMessage(context.Background(), botID, "出错了："+ev.Text)
		case contract.EventFinal:
			if !streamed {
				// 无流式输出的通道：合成一条结论，保证用户能看到结果。
				text := ""
				if ev.Final != nil {
					text = strings.TrimSpace(strings.Join([]string{ev.Final.Summary, finalText(ev.Final)}, " — "))
				}
				if text != "" {
					d.bridge.PostBotMessage(context.Background(), botID, text)
				}
			}
			if err := d.orch.CompleteWork(context.Background(), workID); err != nil && !errors.Is(err, orchestrator.ErrWorkState) {
				log.Printf("[mesh] 派发: 完成 %s 失败: %v", workID, err)
			}
			return // final 是会话终结语义（适配器层约定），事件流结束
		}
	}
}

// finalText 从 FinalInfo 提取给用户看的正文（无流式输出的通道用）。
func finalText(fi *contract.FinalInfo) string {
	if fi == nil {
		return ""
	}
	return fi.SuccessSignal
}

// nextSeq 每 Run 单调递增的事件序号（§4.3：seq ≥ 1）。
func (d *dispatcher) nextSeq(runID string) int64 {
	d.runMu.Lock()
	defer d.runMu.Unlock()
	d.runSeq[runID]++
	return d.runSeq[runID]
}

// failDelivery 把投递标记失败并产生用户可见的错误事件。
// 重试上限内的失败自动重新入队（next_attempt_at）；超限后由
// notifyDeliveryFailed 把原因推给用户，不允许停在"排队中"无人知晓。
// 每次失败（含 Send 之前的早退，如客户端未运行、会话打不开）都必须立刻在
// 聊天流可见：此前早退只写服务端日志，界面停在排队状态毫无动静——
// 2026-11-14 @codex 发消息后界面无反馈的直接原因。
func (d *dispatcher) failDelivery(ctx context.Context, dl *store.Delivery, runID, reason string) {
	log.Printf("[mesh] 派发: 投递 %s 失败: %s", dl.ID, reason)
	status, err := d.st.MarkDeliveryFailed(context.Background(), dl.ID, reason, time.Now())
	if err != nil {
		log.Printf("[mesh] 派发: 标记投递失败状态时出错: %v", err)
		return
	}
	var text string
	if status == store.DeliveryFailed {
		text = fmt.Sprintf("发送给 %s 失败，已重试 %d 次仍未成功，不再自动重试：%s",
			dl.BotID, store.MaxDeliveryAttempt, reason)
	} else {
		text = fmt.Sprintf("发送给 %s 失败（第 %d/%d 次尝试），稍后自动重试：%s",
			dl.BotID, dl.Attempt, store.MaxDeliveryAttempt, reason)
	}
	d.emitRunError(ctx, dl.WorkID, runID, dl.BotID, text)
	// 错误同时进聊天流：只进 SSE 的话，浏览器没开着或刷新后就"消失"了。
	d.bridge.PostBotMessage(ctx, dl.BotID, "发送失败："+text)
	if status == store.DeliveryFailed {
		d.notifyDeliveryFailed(dl)
	}
}

// emitRunError 把一次派发失败写成 error 事件：落库 + SSE 扇出。
// 与 readEvents 的口径一致——用户等不了聚合，失败必须即时可见。
func (d *dispatcher) emitRunError(ctx context.Context, workID, runID, botID, text string) {
	ev := contract.Event{
		EventID: fmt.Sprintf("%s-err", runID), WorkID: workID, RunID: runID,
		BotID: botID, Seq: d.nextSeq(runID), Kind: contract.EventError,
		Text: text, TS: time.Now(), SchemaVersion: contract.SchemaVersion,
	}
	if err := d.st.AppendEvent(ctx, ev); err != nil {
		log.Printf("[mesh] 派发: 错误事件落库失败: %v", err)
	}
	d.bridge.fanout(ev)
}

// shutdownSessions 进程退出时释放全部引擎会话（只断平台连接，不动客户端窗口）。
func (d *dispatcher) shutdownSessions() {
	d.sessMu.Lock()
	defer d.sessMu.Unlock()
	for botID, s := range d.sessions {
		eng, err := d.mux.pick(botID)
		if err != nil {
			continue
		}
		if _, missing := eng.(unavailableEngine); missing {
			continue
		}
		if err := eng.Close(s); err != nil {
			log.Printf("[mesh] 派发: 关闭 %s 会话失败: %v", botID, err)
		}
	}
}
