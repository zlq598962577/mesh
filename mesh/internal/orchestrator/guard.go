package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"mesh/internal/contract"
)

// 护栏阈值（§5.3，P0 全部强制）。
const (
	// MaxHandoffDepth 交接因果链最大深度。超过即停，并告知用户。
	MaxHandoffDepth = 6
	// MaxWakeupsPerWork 单个 Work 累计唤醒次数上限。
	MaxWakeupsPerWork = 20
	// CooldownWindow 冷却统计窗口。
	CooldownWindow = 60 * time.Second
	// CooldownCount 窗口内允许的最大唤醒次数。
	CooldownCount = 6
	// CooldownMute 超限后的静音时长。
	CooldownMute = 5 * time.Minute
	// guardKeepAlive 冷却记录保留时长，超过即清理，避免常驻内存无限增长。
	guardKeepAlive = 30 * time.Minute
)

// GuardReason 护栏拦截原因。
type GuardReason string

const (
	GuardOK        GuardReason = ""
	GuardDuplicate GuardReason = "duplicate" // 同一 (message_id, bot_id) 已投递
	GuardDepth     GuardReason = "depth"     // 交接深度超限
	GuardWakeups   GuardReason = "wakeups"   // 唤醒总次数超限
	GuardBudget    GuardReason = "budget"    // 预算耗尽
	GuardCooldown  GuardReason = "cooldown"  // 冷却静音中
)

// GuardVerdict 护栏判定结果。
//
// Notice 为空表示"不需要告知用户"（去重命中属于此类：那条消息已经送到过，
// 用户没有丢东西）。任何"因为超限所以不做了"的判定都必须带 Notice，
// 不允许静默停止（§5.3）。
type GuardVerdict struct {
	Allow  bool
	Reason GuardReason
	Notice string
}

// budgetSpent 单个 Work 的可观测消耗。
type budgetSpent struct {
	tokens      int64
	tokensKnown bool // 是否至少有一次带 Known=true 的用量
	steps       int
	startedAt   time.Time
}

// guardState 护栏的运行时状态。
//
// 全部为内存态：护栏防的是同一进程内的循环风暴，重启后从零开始即可，
// 落库反而会引入"重启后仍被静音"的困惑。
type guardState struct {
	mu           sync.Mutex
	seq          uint64
	wakes        map[string]int          // workID -> 累计唤醒次数
	muted        map[string]time.Time    // botID -> 静音到什么时候
	recent       map[string][]time.Time  // botID -> 最近唤醒时间
	spent        map[string]*budgetSpent // workID -> 消耗
	budgetNotice map[string]bool         // workID -> 是否已就"预算无法精确计量"提示过
}

func newGuardState() *guardState {
	return &guardState{
		wakes:        make(map[string]int),
		muted:        make(map[string]time.Time),
		recent:       make(map[string][]time.Time),
		spent:        make(map[string]*budgetSpent),
		budgetNotice: make(map[string]bool),
	}
}

// nextSeq 单调递增序号，用于拼 ID。
func (g *guardState) nextSeq() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	return g.seq
}

// Check 依次跑五道护栏。任一道拦住即返回 Allow=false。
//
// 顺序有讲究：先做幂等去重（最便宜、且能挡住重复投递造成的假循环），
// 再判结构性上限（深度、唤醒数），最后判资源类（预算、冷却）。
func (g *guardState) Check(ctx context.Context, store Store, work *contract.Work, messageID string, t WakeTarget) GuardVerdict {
	// 护栏一：同一条消息不重复投给同一个 bot（幂等，落库唯一索引保证重启后仍有效）。
	// 这一步要走存储，放在锁外面：存储慢的时候不能把整个路由卡住。
	seen, err := store.MarkInputSeen(ctx, messageID, t.BotID)
	if err != nil {
		// 登记失败时宁可不投，也不要冒重复唤醒的风险。
		return GuardVerdict{Allow: false, Reason: GuardDuplicate,
			Notice: fmt.Sprintf("消息 %s 的投递登记失败，已跳过对 %s 的投递：%v", messageID, t.BotID, err)}
	}
	if seen {
		return GuardVerdict{Allow: false, Reason: GuardDuplicate}
	}

	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	// 护栏二：交接深度。
	if t.Depth > MaxHandoffDepth {
		return GuardVerdict{Allow: false, Reason: GuardDepth,
			Notice: fmt.Sprintf("这次交接已经连续转了 %d 层（上限 %d 层），我先停在这里。要它继续，请直接说明下一步由谁做什么。", t.Depth, MaxHandoffDepth)}
	}

	// 护栏三：单个 Work 的累计唤醒次数。
	key := t.BotID
	if work != nil {
		key = work.ID
	}
	g.wakes[key]++
	if g.wakes[key] > MaxWakeupsPerWork {
		return GuardVerdict{Allow: false, Reason: GuardWakeups,
			Notice: fmt.Sprintf("这个委托已经互相唤醒了 %d 次（上限 %d 次），看着像在原地打转，我先暂停自动流转。请给一句明确的下一步，或者换个执行者。", g.wakes[key], MaxWakeupsPerWork)}
	}

	// 护栏四：冷却 / 静音。
	if until, ok := g.muted[t.BotID]; ok {
		if now.Before(until) {
			return GuardVerdict{Allow: false, Reason: GuardCooldown,
				Notice: fmt.Sprintf("%s 短时间内被反复唤醒，已临时静音到 %s。需要它马上处理，请直接点它的窗口或取消静音。", t.BotID, until.Format("15:04:05"))}
		}
		delete(g.muted, t.BotID)
	}
	times := append(g.recent[t.BotID], now)
	cut := now.Add(-guardKeepAlive)
	kept := times[:0]
	for _, ts := range times {
		if ts.After(cut) {
			kept = append(kept, ts)
		}
	}
	g.recent[t.BotID] = kept
	inWindow := 0
	for _, ts := range kept {
		if ts.After(now.Add(-CooldownWindow)) {
			inWindow++
		}
	}
	if inWindow > CooldownCount {
		g.muted[t.BotID] = now.Add(CooldownMute)
		return GuardVerdict{Allow: false, Reason: GuardCooldown,
			Notice: fmt.Sprintf("%s 在 %s 内被唤醒了 %d 次（上限 %d 次），已临时静音 %s。这通常是两个 bot 在互相推活，请指定一个人收尾。",
				t.BotID, CooldownWindow, inWindow, CooldownCount, CooldownMute)}
	}

	// 护栏五：预算。
	if v := g.checkBudget(work); v != nil {
		return *v
	}
	return GuardVerdict{Allow: true, Reason: GuardOK}
}

// checkBudget 检查 Work 预算。返回 nil 表示放行。
//
// 诚实边界（必须写进 README 和 notice）：四个桌面客户端都不上报 token 用量，
// contract.Usage.Known 为 false 时平台拿不到真实消耗，因此 token 硬上限
// 在这些客户端上**无法真正生效**。这里只对"可观测的部分"生效：
// 已知用量（Builtin 引擎）与每轮步数/时长。不假装能卡住看不见的东西。
func (g *guardState) checkBudget(work *contract.Work) *GuardVerdict {
	if work == nil || work.Budget == nil {
		return nil
	}
	b := work.Budget
	s := g.spent[work.ID]
	if s == nil {
		return nil
	}

	if b.DailyTokens > 0 && s.tokensKnown && s.tokens >= b.DailyTokens {
		return &GuardVerdict{Allow: false, Reason: GuardBudget,
			Notice: fmt.Sprintf("这个委托已用掉 %d tokens，达到今日上限 %d，我停下不再自动流转。要继续请调高预算或明天再来。", s.tokens, b.DailyTokens)}
	}

	if b.MaxStepsPerTurn > 0 && s.steps > b.MaxStepsPerTurn {
		return &GuardVerdict{Allow: false, Reason: GuardBudget,
			Notice: fmt.Sprintf("这个委托单轮步数已到 %d（上限 %d），我停下等你确认是否继续。", s.steps, b.MaxStepsPerTurn)}
	}

	if b.MaxTurnSeconds > 0 && !s.startedAt.IsZero() {
		if time.Since(s.startedAt) > time.Duration(b.MaxTurnSeconds)*time.Second {
			return &GuardVerdict{Allow: false, Reason: GuardBudget,
				Notice: fmt.Sprintf("这个委托单轮已跑 %s（上限 %d 秒），我停下等你确认是否继续。",
					time.Since(s.startedAt).Round(time.Second), b.MaxTurnSeconds)}
		}
	}

	// 预算配了 token 上限但一个 Known 用量都没拿到：如实说清楚不能硬卡。
	if b.DailyTokens > 0 && !s.tokensKnown && !g.budgetNotice[work.ID] {
		g.budgetNotice[work.ID] = true
		return &GuardVerdict{Allow: false, Reason: GuardBudget,
			Notice: "这个客户端不上报 token 用量，我没法把 token 上限当硬线来卡。目前只按步数和时长控制；要精确控制请改用能上报用量的内置引擎。"}
	}
	return nil
}

// RecordUsage 累计一次可观测消耗。Known=false 的用量只用来标记"看不见"。
func (g *guardState) RecordUsage(workID string, u contract.Usage, steps int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.spent[workID]
	if s == nil {
		s = &budgetSpent{startedAt: time.Now()}
		g.spent[workID] = s
	}
	if u.Known {
		s.tokensKnown = true
		s.tokens += u.TokensIn + u.TokensOut
	}
	s.steps += steps
}

// StartTurn 记录一轮开始时间（用于时长上限）。
func (g *guardState) StartTurn(workID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.spent[workID]
	if s == nil {
		s = &budgetSpent{}
		g.spent[workID] = s
	}
	s.startedAt = time.Now()
	s.steps = 0
}

// ForgetWork Work 结束后清理其护栏状态。
func (g *guardState) ForgetWork(workID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.wakes, workID)
	delete(g.spent, workID)
	delete(g.budgetNotice, workID)
}

// Unmute 手动解除某 bot 的静音（用户在 UI 上点"取消静音"）。
func (g *guardState) Unmute(botID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.muted, botID)
	delete(g.recent, botID)
}

// MutedBots 当前静音中的 bot，供 UI 展示。
func (g *guardState) MutedBots() map[string]time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]time.Time, len(g.muted))
	now := time.Now()
	for k, v := range g.muted {
		if now.Before(v) {
			out[k] = v
		}
	}
	return out
}

// summarizeTargets 生成一条给用户看的投递摘要（写进 notice，不做成隐藏行为）。
func summarizeTargets(targets []WakeTarget) string {
	if len(targets) == 0 {
		return "本次没有唤醒任何人"
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s(%s)", t.BotID, t.Reason))
	}
	return "已唤醒：" + strings.Join(parts, "、")
}
