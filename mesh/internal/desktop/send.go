//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 三阶段发送协议
// ---------------------------------------------------------------------------
//
// 方案 §9.2：发送必须拆成 Prepare / Submit / Verify 三段，且**超时后必须先
// Verify 再决定是否重发**，绝对禁止盲目重发。
//
// ---------------------------------------------------------------------------
// 为什么必须禁止盲目重发
// ---------------------------------------------------------------------------
//
// 桌面客户端没有幂等键。我们按下 Ctrl+V 之后，如果等待超时，无法区分下面两种
// 情况：
//
//	(a) 按键没送达，客户端什么都没收到  → 重发是对的
//	(b) 按键送达了，客户端正在生成       → 重发会让同一句话被提交两次
//
// 情况 (b) 的后果不是"多发一条消息"，而是：客户端可能把第二次提交当成一次新的
// 对话轮次，于是生成两份回答；或者输入框里残留着上一条内容，两次文本被拼接成
// 一段语义错乱的话发给模型。用户看到的是一个精神分裂的 bot，而且很难理解
// 为什么会这样。
//
// 所以规则是硬性的：Submit 返回超时后，**必须先调 Verify**。
//   - Verify 说 Verified    → 已送达，不重发
//   - Verify 说 NotVerified → 确认没送达，可以重发
//   - Verify 说 Unknown     → 不知道，**不重发**，如实上报
//
// ---------------------------------------------------------------------------
// 明令禁止的启发式
// ---------------------------------------------------------------------------
//
// 【禁止】"等待 N 秒，如果这段时间内目标区域没有出现新字符，就认为已经生成
// 完毕"。
//
// 这个启发式在本项目里被明确禁止，任何后续维护者不得以"提高效率"或"改善体验"
// 为由把它加回来。理由是它不是"不够精确"，而是**在原理上无法成立**：
//
//  1. 模型的思考停顿与生成结束在屏幕上不可区分。一个正在推理的模型可能安静
//     十几秒，而一个已经结束的生成可能立刻就安静了。字符静默期这个信号本身
//     不携带"是否结束"的信息，任何阈值都是在猜。
//
//  2. 慢模型 + 长回答必然被误判。回答越长、模型越慢，静默期越长，被误判成
//     "已完成"的概率越高——恰好在我们最需要耐心的时候最容易出错。这个错误
//     方向是系统性的，不能靠调大阈值解决。
//
//  3. 它会把"未知"伪装成"成功"。一旦上层拿到 Success=true，就不会再核实，
//     错误的结果会一路流到用户面前，且没有任何地方留下"这可能是假的"的痕迹。
//     这直接违反本项目"诚实上报未知"的底线（参见 contract.FinalInfo.Unverified
//     与 contract.Usage.Known）。
//
//  4. 它让故障不可观测。如果采用了这个启发式，超时和正常完成会产生完全相同的
//     日志，真实故障率将永远无法从数据中看出来。
//
// 正确的做法只有一个：向客户端要一个**明确的可观测信号**（生成中指示器的出现
// 与消失、停止按钮的可用状态、消息计数的变化等），由适配器层的 Verifier 实现
// 读取。拿不到明确信号时就返回 Unknown，让上层决定是否提示用户"这个结果没被
// 确认"。未知是允许的答案；假装知道不是。
// ---------------------------------------------------------------------------

// Target 是一次发送的目标客户端。
//
// 为什么要把 hwnd 和 TextTarget 打包成一个值类型，而不是让每个阶段都传两个
// 参数：三个阶段必须作用在同一个目标上，参数分开传时调用方很容易在第二阶段
// 传错窗口（尤其是多客户端并存时）。打包之后，"这一轮发送的目标"是一个不可
// 拆散的整体，传错的概率大幅下降。
type Target struct {
	// HWND 是目标顶层窗口句柄。
	HWND uintptr

	// Text 是可选的控件直设能力。为 nil 时注入一律走剪贴板路径。
	Text TextTarget
}

// Valid 报告目标是否至少有一个可用的窗口句柄。
func (t Target) Valid() bool { return t.HWND != 0 }

// VerifyResult 是一次核实的结果。
//
// 三值枚举而不是 bool，理由与 contract.OperationState 里那个 OpUnknown 完全
// 一致：桌面自动化的很多失败是"我们不知道"，把它折叠成 false 会让上层误以为
// "确定没成功"从而安全地重试，而实际上可能已经成功了。三值强制调用方处理
// 中间态。
type VerifyResult string

const (
	// Verified 有明确证据表明文本已被客户端接收。
	Verified VerifyResult = "verified"

	// NotVerified 有明确证据表明文本未被接收，可以安全重发。
	NotVerified VerifyResult = "not_verified"

	// Unknown 无法判断。**此时禁止重发**，必须把不确定性如实上报。
	Unknown VerifyResult = "unknown"
)

// String 便于日志输出。
func (v VerifyResult) String() string { return string(v) }

// IsSuccess 报告是否可确定视为成功。只有 Verified 返回 true。
func (v VerifyResult) IsSuccess() bool { return v == Verified }

// IsRetryable 报告是否允许重发。只有 NotVerified 允许。
//
// 这个方法存在的意义就是把"禁止盲目重发"编码进类型系统：调用方想知道能不能
// 重试时，不该自己去写 `if result != Verified`——那会把 Unknown 也当成可重试，
// 正好踩中本包最想避免的坑。
func (v VerifyResult) IsRetryable() bool { return v == NotVerified }

// SendErrorKind 是可机器判定的发送失败原因。
type SendErrorKind string

const (
	// SendErrPrepare 准备阶段失败，尚未向客户端提交任何内容。
	SendErrPrepare SendErrorKind = "prepare_failed"
	// SendErrSubmit 提交阶段失败，可能已送达也可能没有——必须 Verify。
	SendErrSubmit SendErrorKind = "submit_failed"
	// SendErrVerify 核实阶段本身失败（读不到状态）。
	SendErrVerify SendErrorKind = "verify_failed"
	// SendErrTarget 目标本身无效（句柄为 0）。
	SendErrTarget SendErrorKind = "invalid_target"
)

// SendError 是发送协议的结构化错误。
type SendError struct {
	Kind   SendErrorKind
	Phase  string // "prepare" / "submit" / "verify"
	Result VerifyResult
	Detail string
	Err    error
}

func (e *SendError) Error() string {
	s := fmt.Sprintf("desktop: 发送失败[%s]", e.Phase)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Result != "" {
		s += "（核实结果=" + string(e.Result) + "）"
	}
	if e.Err != nil {
		s += " (" + e.Err.Error() + ")"
	}
	return s
}

func (e *SendError) Unwrap() error { return e.Err }

// IsRetryable 报告这个发送错误是否允许重发。
//
// 只有"确认没送达"才允许。其余一律不允许——尤其是 Submit 超时，
// 必须由调用方先 Verify 拿到 NotVerified。
func (e *SendError) IsRetryable() bool {
	return e.Result == NotVerified
}

// Verifier 由适配器层实现，负责读取客户端状态以核实文本是否被接收。
//
// 实现方约定（重要）：
//   - 只在有**明确证据**时返回 Verified / NotVerified。
//   - 任何不确定（控件读不到、状态在变、刚提交还没来得及渲染）都必须返回
//     Unknown 和 nil 错误，不要为了"给个答案"而猜。
//   - 返回 Unknown 时不要返回错误。Unknown 是一个合法的、诚实的答案，不是
//     异常。返回错误会诱使上层把它当成"核实失败，重试核实"，从而掩盖真相。
type Verifier interface {
	// VerifyText 核实 text 是否已被客户端接收。
	VerifyText(ctx context.Context, target Target, text string) (VerifyResult, error)

	// Ready 报告目标当前是否处于可提交状态（例如生成中不可提交）。
	//
	// 返回 false 表示"现在不能提交"，调用方应当等待而不是硬提交。
	Ready(ctx context.Context, target Target) (bool, error)
}

// Sender 驱动三阶段协议。
//
// 不导出字段，通过 NewSender 构造，保证依赖总是齐全的。
type Sender struct {
	verifier Verifier
	opts     SendOptions
}

// SendOptions 控制协议行为。
type SendOptions struct {
	// SubmitTimeout 是等待"提交动作完成"的上限。
	SubmitTimeout time.Duration

	// VerifyTimeout 是单次核实的上限。
	VerifyTimeout time.Duration

	// ReadyTimeout 是等待客户端进入可提交状态的上限。
	ReadyTimeout time.Duration

	// ReadyPollInterval 是轮询 Ready 的间隔。
	ReadyPollInterval time.Duration

	// MaxResends 是允许的重发次数上限。
	//
	// 允许重发的前提始终是"上一次核实明确返回 NotVerified"。即使如此也要
	// 设上限：如果每次核实都说没送达而客户端其实收到了（Verifier 实现有 bug），
	// 无上限重发会变成刷屏。默认 2 次。想要零重发请用 NewSenderNoResend。
	MaxResends int

	// InjectOptions 透传给注入层。
	InjectOptions InjectOptions
}

const (
	defaultSubmitTimeout     = 10 * time.Second
	defaultVerifyTimeout     = 5 * time.Second
	defaultReadyTimeout      = 30 * time.Second
	defaultReadyPollInterval = 250 * time.Millisecond
	defaultMaxResends        = 2
)

func (o SendOptions) normalize() SendOptions {
	if o.SubmitTimeout <= 0 {
		o.SubmitTimeout = defaultSubmitTimeout
	}
	if o.VerifyTimeout <= 0 {
		o.VerifyTimeout = defaultVerifyTimeout
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.ReadyPollInterval <= 0 {
		o.ReadyPollInterval = defaultReadyPollInterval
	}
	if o.MaxResends < 0 {
		o.MaxResends = 0
	}
	if o.MaxResends == 0 {
		// 0 与"未设置"无法区分，这里统一按默认值处理。想要零重发的调用方
		// 请用 NewSenderNoResend。
		o.MaxResends = defaultMaxResends
	}
	return o
}

// NewSender 构造一个三阶段发送器。
func NewSender(v Verifier, opts SendOptions) (*Sender, error) {
	if v == nil {
		return nil, errors.New("desktop: NewSender 需要非 nil 的 Verifier（无法核实就不能安全发送）")
	}
	return &Sender{verifier: v, opts: opts.normalize()}, nil
}

// NewSenderNoResend 构造一个禁止任何重发的发送器。
//
// 用于对重复提交极其敏感的客户端。它仍然允许提交前的等待与核实，只是永不重发。
func NewSenderNoResend(v Verifier, opts SendOptions) (*Sender, error) {
	s, err := NewSender(v, opts)
	if err != nil {
		return nil, err
	}
	s.opts.MaxResends = 0
	return s, nil
}

// ---------------------------------------------------------------------------
// 阶段 1：Prepare
// ---------------------------------------------------------------------------

// Prepare 把文本放进目标客户端的输入区，但**不提交**。
//
// 这一阶段的目标是把"写文本"和"发送"分开，因为写文本可以安全重试（覆盖式
// 写入不会产生重复提交），而发送不能。Prepare 完成后调用方可以检查输入区
// 内容是否如预期，再决定是否提交。
//
// 前置条件：调用方持有全局桌面锁，且已 EnsureForeground。
// Prepare 内部不取锁——锁的段落边界由调用方掌握（见 README 的时序图）。
func (s *Sender) Prepare(ctx context.Context, target Target, text string) error {
	if !target.Valid() {
		return &SendError{Kind: SendErrTarget, Phase: "prepare", Detail: "目标窗口句柄为 0"}
	}
	if text == "" {
		return &SendError{Kind: SendErrPrepare, Phase: "prepare", Detail: "文本为空", Err: ErrEmptyText}
	}

	// 等客户端进入可提交状态。生成中的客户端会忽略或缓存我们的输入，
	// 这时写进去的文本可能在生成结束后被清掉。
	if err := s.waitReady(ctx, target); err != nil {
		return &SendError{Kind: SendErrPrepare, Phase: "prepare", Detail: "等待客户端就绪失败", Err: err}
	}

	if err := Inject(ctx, target.HWND, target.Text, text, s.opts.InjectOptions); err != nil {
		return &SendError{Kind: SendErrPrepare, Phase: "prepare", Detail: "写入文本失败", Err: err}
	}
	return nil
}

// waitReady 等待客户端进入可提交状态。
func (s *Sender) waitReady(ctx context.Context, target Target) error {
	if s.verifier == nil {
		return nil
	}
	deadline := time.Now().Add(s.opts.ReadyTimeout)
	for {
		ready, err := s.verifier.Ready(ctx, target)
		if err == nil && ready {
			return nil
		}
		// err != nil 不单独处理：读不到状态时继续轮询，最终由下面的超时兜住。
		// 这里不把 err 上抛，是因为"暂时读不到"在窗口重绘期间很常见，
		// 立刻失败会让 Prepare 在正常场景下随机报错。
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("desktop: 等待客户端就绪超时（%s）", s.opts.ReadyTimeout)
		}
		if serr := sleepCtx(ctx, s.opts.ReadyPollInterval); serr != nil {
			return serr
		}
	}
}

// ---------------------------------------------------------------------------
// 阶段 2：Submit
// ---------------------------------------------------------------------------

// Submit 提交已写入的文本（通常是发送回车）。
//
// 关键约定：Submit 返回错误**不代表没送达**。调用方必须随后调用 Verify，
// 而不是直接重发。这一点在函数文档、README 和 Send 的实现里重复强调，
// 因为它是本协议最容易写错的地方。
func (s *Sender) Submit(ctx context.Context, target Target) error {
	if !target.Valid() {
		return &SendError{
			Kind:   SendErrTarget,
			Phase:  "submit",
			Detail: "目标窗口句柄为 0",
			Result: NotVerified, // 没按键，确定没送达
		}
	}

	// 提交前最后一次确认窗口还在前台。如果用户此刻切走了窗口，回车会落到
	// 别的窗口里——那可能是用户正在写的文档，也可能是另一个聊天框。
	if foregroundWindow() != target.HWND {
		return &SendError{
			Kind:   SendErrSubmit,
			Phase:  "submit",
			Detail: "提交前目标窗口已不在前台，已中止（避免回车落入其他窗口）",
			Result: NotVerified, // 我们没按键，确定没送达
		}
	}

	sctx, cancel := context.WithTimeout(ctx, s.opts.SubmitTimeout)
	defer cancel()

	keyPress(vkReturn, false)
	keyPress(vkReturn, true)

	// 等一小会儿让按键有机会被处理。注意这**不是**在判断提交是否成功，
	// 只是让后续 Verify 有机会看到变化。判断成功与否是 Verify 的职责。
	if err := sleepCtx(sctx, defaultPasteSettleDelay); err != nil {
		return &SendError{
			Kind:   SendErrSubmit,
			Phase:  "submit",
			Detail: "等待提交动作时被取消",
			// 已经按下回车了，所以是"可能送达"。
			Result: Unknown,
			Err:    err,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 阶段 3：Verify
// ---------------------------------------------------------------------------

// Verify 核实文本是否已被客户端接收。
//
// 这是整个协议的安全阀。它永远不返回"猜"的结果：拿不到明确证据就返回
// Unknown。上层拿到 Unknown 时应当把结果标记为"未确认"并如实呈现给用户，
// 而不是重发或当作成功。
func (s *Sender) Verify(ctx context.Context, target Target, text string) (VerifyResult, error) {
	if s.verifier == nil {
		// 没有核实能力时只能承认不知道。绝不返回 Verified 来"让流程走完"。
		return Unknown, &SendError{
			Kind:   SendErrVerify,
			Phase:  "verify",
			Detail: "未配置 Verifier，无法核实",
			Result: Unknown,
		}
	}

	vctx, cancel := context.WithTimeout(ctx, s.opts.VerifyTimeout)
	defer cancel()

	result, err := s.verifier.VerifyText(vctx, target, text)
	if err != nil {
		// 核实过程本身出错（比如窗口在核实期间消失了）。这不能推断成
		// "没送达"——那会导致重发。必须降级为 Unknown。
		return Unknown, &SendError{
			Kind:   SendErrVerify,
			Phase:  "verify",
			Detail: "核实过程出错，无法判断是否送达",
			Result: Unknown,
			Err:    err,
		}
	}

	switch result {
	case Verified, NotVerified, Unknown:
		return result, nil
	default:
		// Verifier 返回了未定义的值。按 Unknown 处理，绝不按成功处理。
		return Unknown, &SendError{
			Kind:   SendErrVerify,
			Phase:  "verify",
			Detail: fmt.Sprintf("Verifier 返回了未定义的核实结果 %q", string(result)),
			Result: Unknown,
		}
	}
}

// ---------------------------------------------------------------------------
// 编排：Send
// ---------------------------------------------------------------------------

// SendOutcome 是一轮完整发送的结果。
type SendOutcome struct {
	// Result 是最终的核实结论。
	Result VerifyResult

	// Resends 是实际发生的重发次数。
	Resends int

	// Err 是导致提前终止的错误，可能为 nil。
	Err error
}

// OK 报告是否可确定成功。
func (o SendOutcome) OK() bool { return o.Result == Verified }

// NeedsUserAttention 报告是否应当提示用户"这次没确认成功"。
//
// Unknown 为 true：我们不打算重发，也不能说成功，只能让用户自己看一眼。
// NotVerified 也为 true：确定没送达，需要用户决定是否重来。
func (o SendOutcome) NeedsUserAttention() bool {
	return o.Result == Unknown || o.Result == NotVerified
}

// Send 编排完整的三阶段流程，并严格遵守"核实后再重发"。
//
// 流程：
//
//	循环（最多 1 + MaxResends 次）：
//	  Prepare → Submit → Verify
//	    Verified    → 返回成功
//	    NotVerified → 允许重发，回到循环开头
//	    Unknown     → 立即返回，绝不重发
//
// 任何阶段的错误都会先尝试 Verify 再决定，因为错误不等于没送达。
//
// 注意 Send 内部不获取桌面锁。锁的段落边界由调用方按 README 的时序图掌握：
// Prepare+Submit 是一个段落，Verify 可以另起一个段落（甚至可以不持锁，
// 取决于 Verifier 是否需要读屏）。
func (s *Sender) Send(ctx context.Context, target Target, text string) SendOutcome {
	attempts := 1 + s.opts.MaxResends
	var lastErr error

	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 重发前的退避。走到这里说明上一轮核实明确返回了 NotVerified，
			// 所以重发是安全的。
			if err := sleepCtx(ctx, s.opts.ReadyPollInterval); err != nil {
				return SendOutcome{Result: Unknown, Resends: i - 1, Err: err}
			}
		}

		if err := s.Prepare(ctx, target, text); err != nil {
			// 准备阶段失败：文本按理还没进去。但为了严谨仍然核实一次——
			// Prepare 内部可能已经写了一半（例如剪贴板写入成功但粘贴失败）。
			res, verr := s.Verify(ctx, target, text)
			if res == Verified {
				// 意外：Prepare 报错但内容其实进去了。按成功处理，
				// 不要重发（重发会让输入框出现两份文本）。
				return SendOutcome{Result: Verified, Resends: i}
			}
			lastErr = err
			if res == Unknown {
				return SendOutcome{Result: Unknown, Resends: i, Err: errors.Join(err, verr)}
			}
			// NotVerified：可以安全进入下一轮。
			continue
		}

		if submitErr := s.Submit(ctx, target); submitErr != nil {
			// 提交阶段出错：这是最危险的时刻，因为我们可能已经按下了回车。
			// 必须先核实。
			res, verr := s.Verify(ctx, target, text)
			switch res {
			case Verified:
				return SendOutcome{Result: Verified, Resends: i}
			case Unknown:
				// 不知道送没送到 → 禁止重发，如实上报。
				return SendOutcome{Result: Unknown, Resends: i, Err: errors.Join(submitErr, verr)}
			default: // NotVerified
				lastErr = submitErr
				continue
			}
		}

		res, verr := s.Verify(ctx, target, text)
		switch res {
		case Verified:
			return SendOutcome{Result: Verified, Resends: i}
		case Unknown:
			return SendOutcome{Result: Unknown, Resends: i, Err: verr}
		default: // NotVerified
			lastErr = verr
			continue
		}
	}

	// 重发次数用尽。最后一次核实的结论是 NotVerified，所以确定没送达。
	return SendOutcome{Result: NotVerified, Resends: s.opts.MaxResends, Err: lastErr}
}
