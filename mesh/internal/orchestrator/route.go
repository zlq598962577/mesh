package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mesh/internal/contract"
)

// Orchestrator 编排层。
//
// 只依赖两样东西：contract.Engine（已冻结的引擎契约）和自己定义的 Store 小接口。
// 不操作窗口、不写 SQL、不 import store / desktop / adapters 的具体实现。
type Orchestrator struct {
	store   Store
	engine  contract.Engine
	bots    map[string]contract.BotSpec
	clock   func() time.Time
	guards  *guardState
	effects *effectLog
	sched   *scheduler
}

// New 构造编排层。bots 是房间内可被调度的 bot 规格。
func New(store Store, engine contract.Engine, bots []contract.BotSpec) *Orchestrator {
	m := make(map[string]contract.BotSpec, len(bots))
	for _, b := range bots {
		m[b.ID] = b
	}
	return &Orchestrator{
		store:   store,
		engine:  engine,
		bots:    m,
		guards:  newGuardState(),
		effects: newEffectLog(),
		sched:   newScheduler(),
	}
}

// knownBot 该 bot 是否存在于本房间。
func (o *Orchestrator) knownBot(id string) bool {
	_, ok := o.bots[id]
	return ok
}

// WakeReason 唤醒某人的原因。写进投递文本，便于用户看懂"为什么它醒了"。
type WakeReason string

const (
	WakeNewWork     WakeReason = "new_work"     // 新委托，只唤醒执行者
	WakeReviewReady WakeReason = "review_ready" // 产物已提交，唤醒审查者
	WakeRevise      WakeReason = "revise"       // 要求修改，唤醒执行者
	WakeMention     WakeReason = "mention"      // 被 @，咨询性质
)

// WakeTarget 一次投递目标。
type WakeTarget struct {
	BotID  string
	Reason WakeReason
	Text   string
	// Depth 本次投递在因果链上的深度（交接深度护栏用）。
	Depth int
}

// OnUserMessage 处理一条来自用户的聊天消息：先落分工/控制指令，再算唤醒目标。
//
// 这是聊天控制的唯一入口。调用方（app 层）收到用户消息时应当先调它，再把返回的
// 唤醒目标交给派发器。返回的 notice 非空时也要显示给用户。
//
// 为什么不把这段塞进 Route：Route 是纯路由，对 bot 消息同样适用；用户消息要先
// 改状态（暂停/停止/换人）再路由，顺序不能反 —— 先路由的话，一条"停止"消息
// 会先把执行者唤醒一次，然后才被取消，等于白跑一轮。
func (o *Orchestrator) OnUserMessage(ctx context.Context, m contract.Message) (targets []WakeTarget, notice string, err error) {
	if m.WorkID != "" {
		w, e := o.store.GetWork(ctx, m.WorkID)
		if e != nil && !errors.Is(e, ErrNotFound) {
			return nil, "", e
		}
		if e == nil {
			changed, n, e2 := o.applyInstruction(ctx, &w, m.Text, m.Mentions)
			if e2 != nil {
				return nil, "", e2
			}
			notice = n
			if changed {
				// applyInstruction 内部已经落盘（changeAssignment / pause / resume / cancel
				// 各自保存），这里只补一条用户可见的 notice。
				if notice != "" {
					if err := o.notice(ctx, m, notice); err != nil {
						return nil, notice, err
					}
				}
				// 被暂停或取消的 Work 不再唤醒任何人。
				if w.State == contract.WorkPaused || w.State == contract.WorkCancelled {
					return nil, notice, nil
				}
			}
		}
	}
	targets, err = o.Route(ctx, m)
	return targets, notice, err
}

// Route 决定一条消息应该唤醒谁（§5.4 唤醒路由 / §5.8 分工）。
//
// 规则（与 README 的判定表一致）：
//  1. 新委托只唤醒执行者。审查者不醒，它等产物提交。
//  2. 产物提交只唤醒审查者；没有审查者则 Work 直接完成，不唤醒任何人。
//  3. 审查要求修改只唤醒执行者：返修任务以 MsgTask/MsgChat 形式回到执行者，
//     此时 Work 已从 reviewing 退回 executing，唤醒原因是 revise；审查通过不唤醒任何人。
//  4. 消息里 @ 的人与角色推导出的人取并集去重；@ 只是咨询，不改分工。
//  5. 发送者自己不会被唤醒（避免自激）。
//  6. 不在本房间的 bot 不唤醒，改为一条 notice 告知用户。
func (o *Orchestrator) Route(ctx context.Context, m contract.Message) ([]WakeTarget, error) {
	var (
		work *contract.Work
		err  error
	)
	if m.WorkID != "" {
		w, e := o.store.GetWork(ctx, m.WorkID)
		if e != nil && !errors.Is(e, ErrNotFound) {
			return nil, e
		}
		if e == nil {
			work = &w
		}
	}

	depth := o.depthOf(ctx, m.ID)
	targets := make([]WakeTarget, 0, 3)

	switch {
	case m.Kind == contract.MsgTask || (m.Kind == contract.MsgChat && work != nil &&
		(work.State == contract.WorkQueued || work.State == contract.WorkExecuting)):
		// 委托消息：只有执行者醒。已经在执行中的 Work 又收到委托消息，
		// 说明这是审查退回后的返修任务，唤醒原因是 revise 而不是新委托。
		if work != nil {
			reason, text := WakeNewWork, work.Goal
			if work.State == contract.WorkExecuting {
				reason, text = WakeRevise, m.Text
			}
			targets = append(targets, WakeTarget{
				BotID: work.Assign.Executor, Reason: reason,
				Text: text, Depth: depth,
			})
		}

	case m.Kind == contract.MsgResult:
		// 产物提交：只有审查者醒。
		if work != nil && work.Assign.Reviewer != "" && work.State == contract.WorkReviewing {
			targets = append(targets, WakeTarget{
				BotID: work.Assign.Reviewer, Reason: WakeReviewReady,
				Text: m.Text, Depth: depth,
			})
		}

	case m.Kind == contract.MsgHandoff:
		// 交接：目标就是 ToID，深度继续累加（护栏会拦）。
		if m.ToID != "" {
			targets = append(targets, WakeTarget{
				BotID: m.ToID, Reason: WakeNewWork, Text: m.Text, Depth: depth,
			})
		}
	}

	// @ 目标并集：咨询性质，不改分工。
	for _, id := range dedupeIDs(m.Mentions) {
		targets = append(targets, WakeTarget{BotID: id, Reason: WakeMention, Text: m.Text, Depth: depth})
	}

	kept, err := o.filterTargets(ctx, m, work, targets)
	if err != nil {
		return nil, err
	}

	// 一条"本该有人干活"的消息最后谁也没唤醒，属于静默失败，必须告诉用户（§5.3）。
	// 普通聊天不提示：用户自己说话本来就不一定需要谁醒。
	if len(kept) == 0 && (m.Kind == contract.MsgTask || m.Kind == contract.MsgHandoff) {
		if err := o.notice(ctx, m, summarizeTargets(kept)); err != nil {
			return nil, err
		}
	}
	return kept, nil
}

// filterTargets 去重、剔除发送者与未知 bot、跑护栏。
func (o *Orchestrator) filterTargets(ctx context.Context, m contract.Message, work *contract.Work, in []WakeTarget) ([]WakeTarget, error) {
	out := make([]WakeTarget, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	var unknown []string

	for _, t := range in {
		t.BotID = strings.TrimSpace(t.BotID)
		if t.BotID == "" {
			continue
		}
		if _, dup := seen[t.BotID]; dup {
			continue // 并集去重：同一条消息只唤醒同一个人一次
		}
		seen[t.BotID] = struct{}{}
		if t.BotID == m.FromID {
			continue // 不唤醒发送者自己
		}
		if !o.knownBot(t.BotID) {
			unknown = append(unknown, t.BotID)
			continue
		}
		out = append(out, t)
	}

	if len(unknown) > 0 {
		// 目标不存在：如实告知，不猜一个替身顶上。
		if err := o.notice(ctx, m, fmt.Sprintf("以下对象不在本房间，已忽略：%s", strings.Join(unknown, "、"))); err != nil {
			return nil, err
		}
	}

	// 护栏：去重 / 深度 / 唤醒总数 / 预算 / 冷却。超限的投递被丢弃，但一定留下 notice。
	kept := out[:0]
	for _, t := range out {
		v := o.guards.Check(ctx, o.store, work, m.ID, t)
		if !v.Allow {
			if err := o.notice(ctx, m, v.Notice); err != nil {
				return nil, err
			}
			continue
		}
		kept = append(kept, t)
	}
	return kept, nil
}

// depthOf 沿 ReplyTo 因果链计算当前消息的交接深度（§5.3 深度 ≤6）。
// 链断了就按已知深度算，不猜。
func (o *Orchestrator) depthOf(ctx context.Context, messageID string) int {
	depth := 0
	seen := make(map[string]struct{}, MaxHandoffDepth+2)
	id := messageID
	for id != "" {
		if _, ok := seen[id]; ok {
			break // 环，停止
		}
		seen[id] = struct{}{}
		m, err := o.store.GetMessage(ctx, id)
		if err != nil {
			break
		}
		id = m.ReplyTo
		if id != "" {
			depth++
		}
		if depth > MaxHandoffDepth {
			break
		}
	}
	return depth
}

// notice 产生一条用户可见的系统消息。
// 护栏拦截、分工被忽略、目标不存在等都必须走这里，不允许静默丢弃（§5.3）。
func (o *Orchestrator) notice(ctx context.Context, cause contract.Message, text string) error {
	return o.store.SaveMessage(ctx, contract.Message{
		ID:        o.newID("msg"),
		RoomID:    cause.RoomID,
		ThreadID:  cause.ThreadID,
		WorkID:    cause.WorkID,
		CauseID:   cause.ID,
		FromID:    "system",
		FromKind:  contract.FromSystem,
		Kind:      contract.MsgNotice,
		Text:      text,
		CreatedAt: o.now(),
	})
}

// newID 生成 ID。编排层不引入 uuid 依赖，用时间戳+计数即可（单机单用户，冲突面极小）。
func (o *Orchestrator) newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, o.now().UnixNano(), o.guards.nextSeq())
}
