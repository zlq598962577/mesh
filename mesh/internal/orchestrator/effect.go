package orchestrator

import (
	"sync"
	"time"

	"mesh/internal/contract"
)

// 外部副作用的五态（方案 §5.8）。
//
// 平台能确认的只有自己发出的那一步：prepared（已登记、还没发出去）、
// executing（已发出、客户端在跑）、succeeded / failed（拿到了明确回执）。
// 一旦失去观测（客户端窗口被关、进程被杀、超时无回音），只能是 unknown。
//
// 硬规则：unknown 永远不许被当成成功。用户点"停止"之后，如果平台无法确认
// 客户端内部真的停了，状态必须停在 unknown 并带上「停止待确认」，不能写"已停止"。

// EffectKey 一次外部动作的身份：属于哪个 Work 的哪个动作。
type EffectKey struct {
	WorkID string
	Step   string
}

// Effect 一个外部动作的当前状态。
type Effect struct {
	Key       EffectKey
	State     contract.OperationState
	Reason    string
	UpdatedAt time.Time
}

// StopPendingNotice 停止请求无法确认时给用户看的文案。
const StopPendingNotice = "停止待确认：请求已经发出，但我无法确认客户端内部是否真的停下。请到该客户端窗口确认。"

// StopDoneNotice 只有拿到明确回执才允许说这句。
const StopDoneNotice = "已停止。"

// effectLog 记录外部动作状态。纯内存：进程重启后一律从 unknown 重新开始，
// 不假设"上次应该成功了"。
type effectLog struct {
	mu   sync.Mutex
	seen map[EffectKey]Effect
}

func newEffectLog() *effectLog {
	return &effectLog{seen: map[EffectKey]Effect{}}
}

// Begin 登记一个即将发生的动作，状态 prepared。
func (l *effectLog) Begin(k EffectKey, now time.Time) {
	l.set(k, contract.OpPrepared, "", now)
}

// Executing 动作已经发出去了，但结果未知。
func (l *effectLog) Executing(k EffectKey, now time.Time) {
	l.set(k, contract.OpExecuting, "", now)
}

// Succeed 拿到明确成功回执。
func (l *effectLog) Succeed(k EffectKey, now time.Time) {
	l.set(k, contract.OpSucceeded, "", now)
}

// Fail 拿到明确失败回执。
func (l *effectLog) Fail(k EffectKey, reason string, now time.Time) {
	l.set(k, contract.OpFailed, reason, now)
}

// Lose 失去观测：只能标 unknown，不许猜。
func (l *effectLog) Lose(k EffectKey, reason string, now time.Time) {
	l.set(k, contract.OpUnknown, reason, now)
}

// RequestStop 处理停止请求，返回应对外宣告的状态与文案。
//
// 只有 prepared 能确定地停下（还没发出去）；succeeded / failed 已经结束，
// 停止无意义；executing 和 unknown 都无法确认，统一落到 unknown + 停止待确认。
func (l *effectLog) RequestStop(k EffectKey, now time.Time) (contract.OperationState, string) {
	l.mu.Lock()
	cur, ok := l.seen[k]
	l.mu.Unlock()

	state := contract.OpUnknown
	if ok {
		state = cur.State
	}

	switch state {
	case contract.OpPrepared:
		l.set(k, contract.OpFailed, "已取消（尚未发出）", now)
		return contract.OpFailed, StopDoneNotice
	case contract.OpSucceeded, contract.OpFailed:
		l.set(k, state, "已经结束，无需停止", now)
		return state, "这个动作已经结束了，停止对它没有作用。"
	default:
		// executing / unknown / 从未登记：都没法确认。
		l.set(k, contract.OpUnknown, "停止待确认", now)
		return contract.OpUnknown, StopPendingNotice
	}
}

// Get 读一个动作的状态。
func (l *effectLog) Get(k EffectKey) (Effect, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.seen[k]
	return e, ok
}

// Pending 列出所有还没落定的动作（unknown / executing / prepared）。
// 用于重启后向用户交代"这些动作不知道成没成"。
func (l *effectLog) Pending() []Effect {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Effect
	for _, e := range l.seen {
		switch e.State {
		case contract.OpPrepared, contract.OpExecuting, contract.OpUnknown:
			out = append(out, e)
		}
	}
	return out
}

// Forget 清掉一个 Work 的全部动作记录（Work 终结后调用）。
func (l *effectLog) Forget(workID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.seen {
		if k.WorkID == workID {
			delete(l.seen, k)
		}
	}
}

// ForWork 列出某个 Work 的全部动作（停止时逐个请求停下用）。
func (l *effectLog) ForWork(workID string) []Effect {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Effect
	for k, e := range l.seen {
		if k.WorkID == workID {
			out = append(out, e)
		}
	}
	return out
}

func (l *effectLog) set(k EffectKey, state contract.OperationState, reason string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[k] = Effect{Key: k, State: state, Reason: reason, UpdatedAt: now}
}

// ---- 对外接口：调用方在真正驱动引擎时逐步上报 ----
//
// 这些方法必须在每个外部动作的前后各调一次，否则本层无法判断"停止是否已确认"。
// 顺序就是 §5.8 的生命周期：Begin → Executing → Succeeded / Failed，或 Lose → unknown。

// BeginEffect 登记一个即将发生的外部动作（prepared）。
func (o *Orchestrator) BeginEffect(workID, step string) {
	o.effects.Begin(EffectKey{WorkID: workID, Step: step}, o.now())
}

// EffectExecuting 动作已发出，正在客户端里跑。
func (o *Orchestrator) EffectExecuting(workID, step string) {
	o.effects.Executing(EffectKey{WorkID: workID, Step: step}, o.now())
}

// EffectSucceeded 拿到明确成功回执。
func (o *Orchestrator) EffectSucceeded(workID, step string) {
	o.effects.Succeed(EffectKey{WorkID: workID, Step: step}, o.now())
}

// EffectFailed 拿到明确失败回执。
func (o *Orchestrator) EffectFailed(workID, step, reason string) {
	o.effects.Fail(EffectKey{WorkID: workID, Step: step}, reason, o.now())
}

// EffectUnknown 失去观测。永远不要用 Succeeded 代替这个。
func (o *Orchestrator) EffectUnknown(workID, step, reason string) {
	o.effects.Lose(EffectKey{WorkID: workID, Step: step}, reason, o.now())
}

// StopEffect 请求停止一个外部动作，返回应展示给用户的状态与文案。
//
// 调用方拿到 OpUnknown 时必须显示 StopPendingNotice，不许说"已停止"。
func (o *Orchestrator) StopEffect(workID, step string) (contract.OperationState, string) {
	return o.effects.RequestStop(EffectKey{WorkID: workID, Step: step}, o.now())
}

// UnsettledEffects 还没落定的外部动作。重启后要拿它向用户交代。
func (o *Orchestrator) UnsettledEffects() []Effect {
	return o.effects.Pending()
}
