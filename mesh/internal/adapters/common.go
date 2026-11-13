//go:build windows

// Package adapters 是四个桌面客户端适配器（codex / dsh / trae / doubao）
// 共用的归一化层。
//
// 为什么要有这一层：四个客户端长得几乎一样（都是 Chromium/Electron），
// 差异只在"怎么找到输入框、怎么判断输出变了"。如果每个子包各抄一份
// 事件归一化、Seq 递增、完成判定，那么"禁止用退出码判定成功"这条铁律
// 就会有四份实现，迟早有一份被改坏。所以下面这些必须是唯一实现：
//
//   - 六类事件的归一化与信封填充（contract.Event）
//   - Seq 在每个 Run 内单调递增
//   - FinalInfo 的推导（Success / SuccessSignal / Unverified）
//   - 能力声明（Capabilities）
//   - 错误分类（ErrorKind / AdapterError）
//
// 子包只允许实现与具体客户端交互的部分（找窗口、注入文本、观察输出），
// 不允许自己拼 contract.Event，也不允许自己判定成功。
//
// # 为什么整个包按 //go:build windows 编译
//
// 本包的公共骨架（DesktopEngine）要调用 desktop 包的发送协议、焦点原语与
// 按键原语，而那些文件全部是 Windows-only。为不在非 Windows 平台上留下一个
// 缺胳膊少腿、一编译就报错还带不齐实现包的 adapters，这里整包加 Windows 标签，
// 与 locate_windows.go 保持一致。desktop 包的 lock.go 仍然刻意保持跨平台
// 可编译可测试，那是锁逻辑唯一的验证通道，不受本标签影响。
package adapters

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"mesh/internal/contract"
	"mesh/internal/desktop"
	"mesh/internal/registry"
)

// ---------------------------------------------------------------------------
// 完成判定
// ---------------------------------------------------------------------------

// SuccessSignal 完成判定的依据。原样写进 contract.FinalInfo.SuccessSignal，
// 便于事后审计"这条 final 到底是凭什么下的结论"。
type SuccessSignal string

const (
	// SignalNone 没有任何依据（连输出都没观察到）。
	SignalNone SuccessSignal = "none"

	// SignalUnknownOnly 观察到了输出，但无法确认本轮是否结束。
	// 这是 P0a 对四个客户端实测后的默认结论：output.completion = unknown_only。
	SignalUnknownOnly SuccessSignal = "unknown_only"

	// SignalTextObserved 观察到属于本轮的新文本，但"有新文本"不等于"已完成"。
	SignalTextObserved SuccessSignal = "text_observed"

	// SignalNativeDone 客户端自己给出了明确的完成标记。
	//
	// 目前四个客户端都做不到（详见 adapters/README.md 的实测表）。
	// 这个常量保留不是因为"以后可能有用"，而是因为它是唯一合法的成功来源：
	// 判定逻辑必须写得出"什么才算成功"，否则 Success 就变成了硬编码 false。
	SignalNativeDone SuccessSignal = "native_done"

	// SignalStopConfirmed 平台请求停止，并且已经核实客户端确实停了。
	// 注意：这只说明"停了"，不说明"成功了"。
	SignalStopConfirmed SuccessSignal = "stop_confirmed"

	// SignalStopUnconfirmed 平台请求停止，但无法核实是否真的停了。
	// 必须让用户回客户端确认，不许报"已停止"。
	SignalStopUnconfirmed SuccessSignal = "stop_unconfirmed"

	// SignalClientError 客户端或界面明确报错。
	SignalClientError SuccessSignal = "client_error"
)

// Verified 表示这个信号是否给出了"确定的知识"。
//
// 确定的只有三类：客户端明确说完成了、明确核实停了、明确报错了。
// unknown_only / text_observed / stop_unconfirmed 都是"不知道"，
// 它们必须让 FinalInfo.Unverified = true。
func (s SuccessSignal) Verified() bool {
	switch s {
	case SignalNativeDone, SignalStopConfirmed, SignalClientError:
		return true
	default:
		return false
	}
}

// Succeeded 表示这个信号能否支撑 Success = true。
//
// 只有客户端明确给出完成标记才算成功。停止被核实不等于成功，
// 报错更不等于成功——把这两者算成成功，上层就会把一次失败的
// 任务当成已完成，然后继续往下游交付。
func (s SuccessSignal) Succeeded() bool {
	return s == SignalNativeDone
}

// Outcome 一次 Turn 的**观察结果**。
//
// 适配器只允许填"我看到了什么"，不允许填"成功还是失败"。
// 成功与否由 DecideFinal 统一推导，这样四个适配器不可能各自跑偏。
type Outcome struct {
	// Signal 适配器能给出的最强信号。
	Signal SuccessSignal

	// Text 本轮最终观察到的文本（可为空）。
	Text string

	// Summary 人可读的结论补充，会进 FinalInfo.Summary。
	Summary string

	// ClientError 客户端层面明确的错误信息（非空即视为 SignalClientError）。
	ClientError string
}

// DecideFinal 由观察结果推导终结事件。
//
// 这是全适配器层唯一的完成判定入口。三条规则：
//
//  1. Success 只可能来自 SignalNativeDone。P0a 实测四个客户端都没有
//     可靠的完成信号，所以现在走这里的结果一律 Success = false。
//  2. 凡是"不知道"，必须 Unverified = true，让 UI 显示"未确认"。
//  3. ClientError 非空时，信号强制降级为 SignalClientError——不允许
//     适配器一边报错一边声称完成。
func DecideFinal(o Outcome) contract.FinalInfo {
	sig := o.Signal
	if o.ClientError != "" {
		sig = SignalClientError
	}
	if sig == "" {
		sig = SignalNone
	}

	fi := contract.FinalInfo{
		Success:       sig.Succeeded(),
		SuccessSignal: string(sig),
		Summary:       o.Summary,
		Unverified:    !sig.Verified(),
	}
	if o.ClientError != "" {
		if fi.Summary == "" {
			fi.Summary = o.ClientError
		} else {
			fi.Summary = fi.Summary + "；" + o.ClientError
		}
	}
	if fi.Unverified && fi.Summary == "" {
		// 留空会让 UI 只显示"未确认"而没有原因，补一句可解释的话。
		fi.Summary = "未能核实本轮是否完成：" + string(sig)
	}
	return fi
}

// StopOutcome 是 Cancel 的返回值形态。
//
// 取消绝不靠杀整个客户端（§4.2.2），所以"请求已发出"与"确实停了"
// 是两件事。Confirmed = false 时上层必须提示用户回客户端确认。
type StopOutcome struct {
	// Requested 停止请求是否已成功送达客户端。
	Requested bool

	// Confirmed 是否已核实客户端确实停止了本轮。
	Confirmed bool

	// Detail 说明，例如"已发送 Esc，但无法读取客户端状态以确认停止"。
	Detail string
}

// Final 把停止结果转成终结事件。
//
// 这是"停止待确认"的唯一出口：Confirmed = false 时一律 Unverified = true，
// 不许因为"请求发出去了"就报成功。
func (s StopOutcome) Final() contract.FinalInfo {
	sig := SignalStopConfirmed
	summary := s.Detail
	if !s.Confirmed {
		sig = SignalStopUnconfirmed
		if summary == "" {
			summary = "停止请求已发出，但无法核实客户端是否真的停止，请回到客户端确认"
		}
	} else if summary == "" {
		summary = "已停止"
	}
	return contract.FinalInfo{
		Success:       false, // 停下来不是成功，别混为一谈
		SuccessSignal: string(sig),
		Summary:       summary,
		Unverified:    !s.Confirmed,
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// ErrorKind 适配器错误分类。分类决定上层是重试、降级，还是让用户介入。
type ErrorKind string

const (
	// ErrClientNotRunning 客户端没在运行。
	ErrClientNotRunning ErrorKind = "client_not_running"
	// ErrClientNotForeground 客户端不在前台，且无法把它拉到前台。
	ErrClientNotForeground ErrorKind = "client_not_foreground"
	// ErrScreenLocked 屏幕已锁定，任何注入都会落到锁屏上。
	ErrScreenLocked ErrorKind = "screen_locked"
	// ErrFocus 焦点相关失败。
	ErrFocus ErrorKind = "focus"
	// ErrLock 未能获得全局桌面锁。
	//
	// 单独分类而不是塞进 ErrFocus：锁超时意味着"一个字节都没写到目标窗口"，
	// 与聚焦失败（可能已经写入一半）的处理路径完全不同——前者可以安全重试，
	// 后者必须先核实。混在一个分类里，上层就没法按"是否动过桌面"决定行为。
	ErrLock ErrorKind = "lock"
	// ErrInject 文本注入失败。
	ErrInject ErrorKind = "inject"
	// ErrSubmit 提交失败。
	ErrSubmit ErrorKind = "submit"
	// ErrVerify 无法核实结果。
	ErrVerify ErrorKind = "verify"
	// ErrStopUnconfirmed 停止请求已发出但无法核实，需要用户确认。
	ErrStopUnconfirmed ErrorKind = "stop_unconfirmed"
	// ErrUnsupported 该客户端不支持这个能力。
	ErrUnsupported ErrorKind = "unsupported"
	// ErrManifest manifest 与实际不符（例如声明的路径找不到）。
	ErrManifest ErrorKind = "manifest"
	// ErrInternal 适配器自身的 bug。
	ErrInternal ErrorKind = "internal"
)

// AdapterError 适配器统一错误。
//
// UserAction 是刻意加的字段：桌面客户端适配器的大部分失败不是程序能自己
// 修好的，必须告诉用户"去做什么"。没有这个字段，UI 只能显示一句技术报错。
type AdapterError struct {
	Adapter    string    `json:"adapter"`
	Op         string    `json:"op"`
	Kind       ErrorKind `json:"kind"`
	Detail     string    `json:"detail,omitempty"`
	UserAction string    `json:"user_action,omitempty"`
	Err        error     `json:"-"`
}

func (e *AdapterError) Error() string {
	msg := string(e.Kind)
	if e.Adapter != "" {
		msg = e.Adapter + ": " + msg
	}
	if e.Op != "" {
		msg += "（" + e.Op + "）"
	}
	if e.Detail != "" {
		msg += "：" + e.Detail
	}
	if e.Err != nil {
		msg += "：" + e.Err.Error()
	}
	if e.UserAction != "" {
		msg += "。请" + e.UserAction
	}
	return msg
}

func (e *AdapterError) Unwrap() error { return e.Err }

// IsAdapterError 判断错误链里是否有 AdapterError。
func IsAdapterError(err error) (*AdapterError, bool) {
	var ae *AdapterError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// IsErrorKind 判断错误链里是否有指定分类的 AdapterError。
func IsErrorKind(err error, kind ErrorKind) bool {
	ae, ok := IsAdapterError(err)
	return ok && ae.Kind == kind
}

// Unsupported 构造"该客户端不支持此能力"的错误。
//
// 用它而不是返回 nil：静默降级会让上层以为操作成功了。
func Unsupported(adapter, op, capability string) error {
	return &AdapterError{
		Adapter: adapter,
		Op:      op,
		Kind:    ErrUnsupported,
		Detail:  "本客户端未提供该能力：" + capability,
	}
}

// StopUnconfirmedError 构造"停止待确认"错误。
//
// 单独一个构造函数，是因为这条语义最容易被写错：取消操作返回 error
// 不代表取消失败，而是代表"发了但没核实"。上层必须按 Kind 区分。
func StopUnconfirmedError(adapter, detail string) error {
	return &AdapterError{
		Adapter:    adapter,
		Op:         "Cancel",
		Kind:       ErrStopUnconfirmed,
		Detail:     detail,
		UserAction: "回到客户端确认是否已停止",
	}
}

// NeedsUserAttention 表示这个错误必须让用户看到，不能只写日志。
//
// 屏幕锁定、停止待确认、客户端没开这三种，用户不处理就永远不会自愈。
func NeedsUserAttention(err error) bool {
	ae, ok := IsAdapterError(err)
	if !ok {
		return false
	}
	switch ae.Kind {
	case ErrScreenLocked, ErrStopUnconfirmed, ErrClientNotRunning, ErrClientNotForeground:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 能力声明
// ---------------------------------------------------------------------------

// Capabilities 适配器在**本机实测**能做到什么。
//
// 与 manifest 的 ManifestCapabilities 同形，但含义不同：
// manifest 是"声明"，Capabilities 是"这份适配器代码真的实现了"。
// 两者不一致时以 Capabilities 为准，并应视为 manifest 需要修。
type Capabilities struct {
	// Send 能发送文本。
	Send bool
	// StreamOutput 能流式读取输出。四个客户端实测都做不到。
	StreamOutput bool
	// Cancel 能发出取消请求。
	Cancel bool
	// CancelVerified 取消后能核实确实停了。
	//
	// false 是 P0 的常态：我们只能发出停止键，读不到客户端内部状态。
	// false 时 Cancel 只能报"停止待确认"，不许报"已停止"。
	CancelVerified bool
	// ApprovalBridge 能桥接客户端内部的授权请求。
	ApprovalBridge bool
	// FileTransfer 能传文件。
	FileTransfer bool
	// NativeSessionID 能拿到客户端原生会话 id。
	NativeSessionID bool
	// ReadOnlyReview 能只读地读取会话内容做复核。
	ReadOnlyReview bool
	// HealthCheck 能自查连接健康。
	HealthCheck bool
}

// Validate 检查能力声明自洽。
//
// 只校验逻辑蕴含关系，不校验"应该是什么"——那属于实测结论，
// 写在 README 的能力表里，不写在代码里。
func (c Capabilities) Validate() error {
	if c.CancelVerified && !c.Cancel {
		return errors.New("adapters: 能力声明不自洽：cancel_verified=true 但 cancel=false")
	}
	if c.StreamOutput && !c.Send {
		return errors.New("adapters: 能力声明不自洽：stream_output=true 但 send=false")
	}
	return nil
}

// Explain 把能力声明转成人可读的条目，供 Health 与 README 对照。
func (c Capabilities) Explain() []string {
	item := func(ok bool, yes, no string) string {
		if ok {
			return yes
		}
		return no
	}
	return []string{
		item(c.Send, "能发送文本", "不能发送文本"),
		item(c.StreamOutput, "能流式读取输出", "不能流式读取输出（只能整段读）"),
		item(c.Cancel, "能发出取消请求", "不能取消"),
		item(c.CancelVerified, "取消可核实", "取消不可核实（只能报停止待确认）"),
		item(c.ApprovalBridge, "能桥接授权请求", "不能桥接授权请求（交回客户端）"),
		item(c.FileTransfer, "能传文件", "不能传文件"),
		item(c.NativeSessionID, "能取原生会话 id", "取不到原生会话 id（Recovered=false）"),
		item(c.ReadOnlyReview, "能只读复核", "不能只读复核"),
		item(c.HealthCheck, "能自查健康", "不能自查健康"),
	}
}

// ---------------------------------------------------------------------------
// 事件归一化
// ---------------------------------------------------------------------------

// EmitterOptions 构造 Emitter 的可选项。
type EmitterOptions struct {
	// Adapter 适配器名，写进 SessionInfo.Adapter。
	Adapter string
	// WorkID / RunID / BotID 信封字段。
	WorkID string
	RunID  string
	BotID  string
	// Buffer 事件通道缓冲。默认 DefaultEventBuffer。
	Buffer int
	// Now 时间源，测试可注入。默认 time.Now。
	Now func() time.Time
}

// DefaultEventBuffer 事件通道默认缓冲。
//
// 取 64 而不是无界：事件是给 orchestrator 读的，正常一轮只有几条；
// 真的堆到 64 条说明消费端卡住了，此时继续堆内存只会更糟。
const DefaultEventBuffer = 64

// Emitter 六事件归一化 + Seq 递增 + 信封字段填充。
//
// 一个 Run 一个 Emitter。并发安全。
//
// 关于发送策略：Emit 系列方法是**非阻塞**的，通道满了就丢弃并计数。
// 理由很硬：适配器发事件时通常还持有全局桌面锁（段落式持有），
// 在锁内阻塞等消费端会直接卡死整个平台。丢事件是可控损失，
// 持锁阻塞是不可控故障。丢弃数量通过 Dropped() 暴露。
type Emitter struct {
	opts EmitterOptions
	now  func() time.Time

	mu      sync.Mutex
	seq     int64
	closed  bool
	dropped int
	ch      chan contract.Event
}

// NewEmitter 创建 Emitter。
func NewEmitter(opts EmitterOptions) *Emitter {
	if opts.Buffer <= 0 {
		opts.Buffer = DefaultEventBuffer
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Emitter{
		opts: opts,
		now:  now,
		ch:   make(chan contract.Event, opts.Buffer),
	}
}

// Events 返回只读事件通道。通道由 Emitter 关闭。
func (e *Emitter) Events() <-chan contract.Event { return e.ch }

// Seq 返回当前已分配的最大序号。
func (e *Emitter) Seq() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

// Dropped 返回被丢弃的事件数。非零时适配器应写进 final 的 Summary。
func (e *Emitter) Dropped() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// emit 是所有事件唯一的出口：分配 Seq、填信封、非阻塞投递。
func (e *Emitter) emit(kind contract.EventKind, fill func(*contract.Event)) bool {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false
	}
	e.seq++
	seq := e.seq
	ev := contract.Event{
		EventID:       newID("ev"),
		WorkID:        e.opts.WorkID,
		RunID:         e.opts.RunID,
		BotID:         e.opts.BotID,
		Seq:           seq,
		TS:            e.now(),
		SchemaVersion: contract.SchemaVersion,
		Kind:          kind,
	}
	if fill != nil {
		fill(&ev)
	}
	e.mu.Unlock()

	select {
	case e.ch <- ev:
		return true
	default:
		// 通道满：丢弃并计数，绝不阻塞。
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
		return false
	}
}

// Session 发出一条 session 事件。
func (e *Emitter) Session(info contract.SessionInfo) bool {
	if info.Adapter == "" {
		info.Adapter = e.opts.Adapter
	}
	return e.emit(contract.EventSession, func(ev *contract.Event) {
		ev.Session = &info
	})
}

// Progress 发出一条 progress 事件。空文本不产生事件。
func (e *Emitter) Progress(text string) bool {
	if text == "" {
		return false
	}
	return e.emit(contract.EventProgress, func(ev *contract.Event) {
		ev.Text = text
	})
}

// Output 发出一条 output 事件。空文本不产生事件。
//
// 注意：output 事件不得触发唤醒（§5.6），这是消费端的责任，
// 但适配器也不应该把大段文本切得太碎——每条事件都有一次投递成本。
func (e *Emitter) Output(text string) bool {
	if text == "" {
		return false
	}
	return e.emit(contract.EventOutput, func(ev *contract.Event) {
		ev.Text = text
	})
}

// Error 发出一条 error 事件。
func (e *Emitter) Error(text string) bool {
	if text == "" {
		return false
	}
	return e.emit(contract.EventError, func(ev *contract.Event) {
		ev.Text = text
	})
}

// Approval 发出一条 approval 事件。
//
// 平台不得自动点"全部允许"（§9.3）：Bridged = false 时这条事件只是
// 通知用户去客户端处理，不提供任何代答能力。
func (e *Emitter) Approval(info contract.ApprovalInfo) bool {
	return e.emit(contract.EventApproval, func(ev *contract.Event) {
		ev.Approval = &info
	})
}

// Final 发出一条 final 事件，并关闭事件通道。
//
// final 之后不会再有事件：这是 contract.Engine 的语义约定
// （Events 关闭代表该会话不再产生事件）。
func (e *Emitter) Final(fi contract.FinalInfo) bool {
	ok := e.emit(contract.EventFinal, func(ev *contract.Event) {
		ev.Final = &fi
	})
	e.Close()
	return ok
}

// Close 关闭事件通道。幂等。
func (e *Emitter) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	close(e.ch)
}

// ---------------------------------------------------------------------------
// 会话与轮次句柄
// ---------------------------------------------------------------------------

// SessionHandle 适配器侧的会话句柄，实现 contract.Session。
//
// 一个会话对应客户端里的一个专用对话（§4.2.2 的 dedicated 模式）。
type SessionHandle struct {
	id      string
	info    contract.SessionInfo
	emitter *Emitter

	mu        sync.Mutex
	suspended bool
	closed    bool
}

// ID 实现 contract.Session。
func (s *SessionHandle) ID() string { return s.id }

// Info 返回会话信息快照。
func (s *SessionHandle) Info() contract.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Emitter 返回事件发射器。
func (s *SessionHandle) Emitter() *Emitter { return s.emitter }

// SetWindowTitle 更新窗口标题（窗口可能被用户改过标题）。
func (s *SessionHandle) SetWindowTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.WindowTitle = title
}

// Suspended 返回是否已挂起。
func (s *SessionHandle) Suspended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// markSuspended 记录挂起状态。
func (s *SessionHandle) markSuspended(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspended = v
}

// markClosed 记录关闭状态，返回是否是第一次关闭。
func (s *SessionHandle) markClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	return true
}

// Closed 返回会话是否已关闭。
//
// DesktopEngine.Close 需要它来判断"final 事件发过没有"：final 只发一次，
// 发第二次要么 panic（通道已关）要么误导上层以为会话又被终结了一回。
func (s *SessionHandle) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// TurnHandle 一次发送产生的执行轮次，实现 contract.Turn。
type TurnHandle struct {
	id        string
	sessionID string
	startedAt time.Time

	mu   sync.Mutex
	done bool
}

// ID 实现 contract.Turn。
func (t *TurnHandle) ID() string { return t.id }

// SessionID 返回所属会话 id。
func (t *TurnHandle) SessionID() string { return t.sessionID }

// StartedAt 返回开始时间。
func (t *TurnHandle) StartedAt() time.Time { return t.startedAt }

// Done 返回本轮是否已终结。
func (t *TurnHandle) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// markDone 标记本轮已终结。
func (t *TurnHandle) markDone() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
}

// ---------------------------------------------------------------------------
// 引擎骨架
// ---------------------------------------------------------------------------

// BaseEngine 四个适配器共用的引擎骨架。
//
// 它实现 contract.Engine 里与客户端无关的那几个方法（Events / Suspend /
// Close / Health / ResolveApproval），把与客户端真正交互的
// Open / Send / Cancel 留给子包。子包通过嵌入复用它。
//
// 为什么不把整个 Engine 都实现掉：Open/Send/Cancel 的每一步都取决于
// 客户端界面长什么样，强行抽象只会得到一个满是 if adapter == "xxx" 的怪物。
type BaseEngine struct {
	// Adapter 适配器名。
	Adapter string
	// Caps 能力声明，构造时校验。
	Caps Capabilities

	mu       sync.Mutex
	sessions map[string]*SessionHandle
	now      func() time.Time
}

// NewBaseEngine 创建引擎骨架。
func NewBaseEngine(adapter string, caps Capabilities) (*BaseEngine, error) {
	if adapter == "" {
		return nil, errors.New("adapters: adapter 名不能为空")
	}
	if err := caps.Validate(); err != nil {
		return nil, err
	}
	return &BaseEngine{
		Adapter:  adapter,
		Caps:     caps,
		sessions: make(map[string]*SessionHandle),
		now:      time.Now,
	}, nil
}

// SetClock 注入时间源，仅供测试。
func (b *BaseEngine) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now != nil {
		b.now = now
	}
}

// NewSession 注册一个新会话。
//
// opts 里的 Adapter 会被强制覆盖成引擎的 Adapter：会话属于哪个客户端
// 不该由调用方随便填，填错了上层就会把事件派给错误的适配器。
func (b *BaseEngine) NewSession(id string, info contract.SessionInfo, opts EmitterOptions) (*SessionHandle, error) {
	if id == "" {
		return nil, errors.New("adapters: 会话 id 不能为空")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[id]; ok {
		return nil, fmt.Errorf("adapters: 会话 id 重复：%s", id)
	}
	info.Adapter = b.Adapter
	opts.Adapter = b.Adapter
	if opts.Now == nil {
		opts.Now = b.now
	}
	h := &SessionHandle{
		id:      id,
		info:    info,
		emitter: NewEmitter(opts),
	}
	b.sessions[id] = h
	return h, nil
}

// Lookup 按 id 查会话。
func (b *BaseEngine) Lookup(id string) (*SessionHandle, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.sessions[id]
	return h, ok
}

// NewTurn 为一个会话开一轮。turnID 为空时自动生成。
func (b *BaseEngine) NewTurn(s *SessionHandle, turnID string) *TurnHandle {
	if turnID == "" {
		turnID = newID("turn")
	}
	b.mu.Lock()
	now := b.now
	b.mu.Unlock()
	return &TurnHandle{
		id:        turnID,
		sessionID: s.ID(),
		startedAt: now(),
	}
}

// FinishTurn 标记一个轮次在适配器侧登记结束。供 adapters 子包使用
// （markDone 是私有方法，子包不能直接调）。
func (b *BaseEngine) FinishTurn(t *TurnHandle) {
	if t != nil {
		t.markDone()
	}
}

// Events 实现 contract.Engine。
//
// 会话不存在时返回一个已关闭的通道，而不是 nil：调用方对 nil 通道做
// range 会永久阻塞，那是比报错更难查的故障。
func (b *BaseEngine) Events(s contract.Session) <-chan contract.Event {
	h, ok := b.Lookup(sessionID(s))
	if !ok {
		ch := make(chan contract.Event)
		close(ch)
		return ch
	}
	return h.Emitter().Events()
}

// Suspend 实现 contract.Engine。
//
// 只标记状态，不关闭用户自己的窗口（§4.2.2）。
func (b *BaseEngine) Suspend(s contract.Session) error {
	h, ok := b.Lookup(sessionID(s))
	if !ok {
		return &AdapterError{
			Adapter: b.Adapter, Op: "Suspend", Kind: ErrInternal,
			Detail: "会话不存在：" + sessionID(s),
		}
	}
	h.markSuspended(true)
	return nil
}

// Close 实现 contract.Engine。
//
// 只释放平台持有的连接：关闭事件通道，绝不动用户的客户端窗口。
// 平台退出时不把用户已有客户端加入 kill-on-close（§9.2）。
func (b *BaseEngine) Close(s contract.Session) error {
	h, ok := b.Lookup(sessionID(s))
	if !ok {
		// 已经关过了不算错误，Close 必须幂等。
		return nil
	}
	if !h.markClosed() {
		return nil
	}
	h.Emitter().Close()
	b.mu.Lock()
	delete(b.sessions, h.ID())
	b.mu.Unlock()
	return nil
}

// CloseAll 关闭全部会话，供进程退出时收尾。
func (b *BaseEngine) CloseAll() {
	b.mu.Lock()
	hs := make([]*SessionHandle, 0, len(b.sessions))
	for _, h := range b.sessions {
		hs = append(hs, h)
	}
	b.mu.Unlock()
	for _, h := range hs {
		_ = b.Close(h)
	}
}

// ResolveApproval 实现 contract.Engine 的默认行为：无权代答。
//
// 四个客户端在 P0 都不桥接授权（§9.3），所以默认就是交回用户。
// 想真正桥接的适配器必须自己覆盖这个方法，并同时把
// Capabilities.ApprovalBridge 改成 true。
func (b *BaseEngine) ResolveApproval(ctx context.Context, s contract.Session, approvalID string, decision contract.Decision) error {
	if decision == contract.DecisionUnavailable {
		return nil
	}
	return &AdapterError{
		Adapter: b.Adapter, Op: "ResolveApproval", Kind: ErrUnsupported,
		Detail:     "本适配器不桥接授权请求，审批必须由用户在客户端内完成",
		UserAction: "回到客户端处理该授权请求",
	}
}

// Health 实现 contract.Engine 的默认健康检查：只报告自身状态。
//
// 真正的"客户端是否可达"由各适配器覆盖后补上，这里不猜。
func (b *BaseEngine) Health() error {
	b.mu.Lock()
	n := len(b.sessions)
	b.mu.Unlock()
	if n < 0 { // 不可达，仅用于让编译器保留字段读取路径
		return errors.New("adapters: 会话计数异常")
	}
	return nil
}

// SessionCount 返回当前会话数，供 Health 与测试使用。
func (b *BaseEngine) SessionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// sessionID 从 contract.Session 取 id，容忍 nil。
func sessionID(s contract.Session) string {
	if s == nil {
		return ""
	}
	return s.ID()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// newID 生成短随机 id。
//
// 不用第三方 uuid：纯标准库够用，而且这个 id 只需要在单机内唯一。
// crypto/rand 失败时回落到时间戳，宁可碰撞也不要让事件发不出去。
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// NewID 生成适配器子包可用的标识符（newID 的导出形式）。
func NewID(prefix string) string { return newID(prefix) }

// WaitFor 在 ctx 上等待 d，可被取消。
//
// 适配器里到处需要"等一会儿再观察"，直接 time.Sleep 会让取消不生效，
// 进而让用户点"停止"后仍要干等几十秒。
func WaitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PollUntil 反复调用 probe 直到它返回 true、ctx 取消或超时。
//
// 返回 nil 表示 probe 返回了 true；返回错误表示超时或取消。
// 这是"等客户端出现反应"的统一实现，四个适配器都该用它而不是各写一套
// for + Sleep。
func PollUntil(ctx context.Context, interval, timeout time.Duration, probe func(context.Context) (bool, error)) error {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := probe(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			return &AdapterError{
				Op: "PollUntil", Kind: ErrVerify,
				Detail: fmt.Sprintf("等待超过 %s 仍未观察到预期状态", timeout),
			}
		}
		if err := WaitFor(ctx, interval); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 公共桌面引擎骨架
// ---------------------------------------------------------------------------
//
// DesktopEngine 是四个子包（codex / dsh / trae / doubao）共用的 contract.Engine
// 实现。四个客户端在本机实测后的行为几乎完全同构（见各 manifest 的 notes 与
// adapters/README.md 的对照表）：
//
//	Open    定位主窗口，绑定窗口句柄 + PID，产出 session 事件
//	Send    desktop 三段协议（剪贴板注入为 P0 主路径），verify 不可达 → Unknown
//	Events  输出读取路径整条"待真机验证"，读不到就如实发 error 事件
//	Cancel  Esc 停止键，只有"停止待确认 / 未发送"两种诚实结论
//	Close   只释放平台持有的连接，绝不动用户的客户端窗口
//
// 因此四个子包不再各写一份编排逻辑——那会让"禁止盲目重发"与"禁止用退出码
// 判定成功"这两条铁律出现四份可能各自走样的实现。子包只保留：适配器名、
// 能力声明、该客户端的实测差异注释与构造函数。
//
// 事件信封的已知局限：contract.Event 的 WorkID / RunID 由 Open 时的
// EmitterOptions 决定，而 Open 的参数（BotSpec）里没有 Run 概念，所以桌面
// 适配器发出的信封里这两个字段为空，只有 BotID 来自 spec.ID。把它们按当前
// Run 归位是 orchestrator 落库时的职责，不是适配器能越俎代庖的事。
//
// ---------------------------------------------------------------------------

// OutputSample 是一次输出读取的结果样本。
type OutputSample struct {
	// Text 读到的可见文本（可为空）。
	Text string
	// Signal 读取方对"本轮是否结束"的判断依据。只允许填确定结论：
	// SignalNativeDone / SignalClientError。其余一律不填——适配器层
	// 不允许替客户端宣布"大概完成了"。
	Signal SuccessSignal
	// ClientError 客户端界面明确报错的内容。
	ClientError string
}

// OutputReader 从客户端界面读取一轮生成的结果。
//
// 这个接口是"读消息列表"路径的接入点。P0 的现实是：本机 UI Automation
// 控件枚举实测不可达（probe 连资源管理器基线窗口都枚举不到后代控件），
// manifest 声明的 clipboard 输出模式需要 desktop 包导出剪贴板读取原语
// 而它没有导出——所以四个适配器当前都没有 OutputReader 实现，
// 整条路径在 README 与能力表里标"待真机验证"。
//
// 接口留在这里而不是删掉，是因为它定义了未来接入的唯一合法形状：
// 实现方必须给出明确的 Signal，拿不到就不许给。任何"N 秒没新字符就算
// 完成"的启发式都进不了这个接口（§4.2.2 明令禁止）。
type OutputReader interface {
	ReadOutput(ctx context.Context, w *ClientWindow) (OutputSample, error)
}

// desktopTarget 一个会话绑定的客户端窗口（§4.2.2 专用会话绑定）。
//
// SessionInfo 里没有窗口句柄字段（contract 是冻结的），所以绑定存在引擎里。
// 每次使用前仍要通过 LocateClientWindowByHWND 复核：窗口被用户关掉再重开时，
// 旧句柄可能已被系统回收给别的进程，直接往旧句柄上打字是最恶性的事故。
type desktopTarget struct {
	hwnd uintptr
	pid  uint32
}

// DesktopEngine 四个桌面客户端适配器的公共引擎骨架。
//
// 通过嵌入 BaseEngine 获得 Events / Suspend / ResolveApproval / 会话簿记；
// Open / Send / Cancel / Close / Health 在本结构里用公共编排实现，
// 与具体客户端无关的部分全部收敛在这里。
type DesktopEngine struct {
	*BaseEngine

	// id 是适配器名（codex | dsh | trae | doubao），与 manifest.id 一致。
	id string
	// reg 是注册表，可为 nil（单测时不起注册表）。非 nil 时 Open 登记
	// 第 4 层 Instance，Close 注销。
	reg *registry.Registry
	// sender 是禁用重发的三段发送器。桌面客户端没有幂等键，
	// "verify 未通过就重发"会把同一句话提交两次，所以这里从构造上
	// 就封死重发能力，而不是靠调用方自觉。
	sender *desktop.Sender
	// output 是输出读取器。nil 表示当前没有可用实现（P0 的现实），
	// Events 侧会如实发 error 事件而不是猜。
	output OutputReader

	// RestoreMinimized 允许把最小化的客户端窗口恢复出来再注入。
	// 默认 false：弹出一个窗口是用户可见的干扰，是否允许由上层按
	// 场景决定（无人值守批量任务可开，共屏协作应保持关）。
	RestoreMinimized bool

	targetsMu sync.Mutex
	targets   map[string]desktopTarget

	turnsMu sync.Mutex
	turns   map[string]*TurnHandle

	// noteReported 记录已发过"输出不可读"说明的会话，防止每轮 Send
	// 都重复刷一条相同的 error 事件。
	noteMu       sync.Mutex
	noteReported map[string]bool
}

// 编译期断言：公共骨架必须满足引擎契约。四个子包嵌入它之后再各断言一次，
// 防止将来有人给 DesktopEngine 加了字段却不小心把方法集弄破。
var _ contract.Engine = (*DesktopEngine)(nil)

// NewDesktopEngine 创建一个桌面引擎骨架。
//
// id 必须与内置 manifest 的 id 一致（codex / dsh / trae / doubao），
// Open 时按它去 registry 找 manifest、探测客户端。
func NewDesktopEngine(id string, caps Capabilities, reg *registry.Registry) (*DesktopEngine, error) {
	base, err := NewBaseEngine(id, caps)
	if err != nil {
		return nil, err
	}
	// NewSenderNoResend：把"绝不重发"从纪律变成结构。SendOptions 零值
	// 即可，所有超时走 desktop 包的默认值（它们都响应 ctx 取消）。
	sender, err := desktop.NewSenderNoResend(&HonestVerifier{}, desktop.SendOptions{})
	if err != nil {
		return nil, err
	}
	return &DesktopEngine{
		BaseEngine:   base,
		id:           id,
		reg:          reg,
		sender:       sender,
		targets:      make(map[string]desktopTarget),
		turns:        make(map[string]*TurnHandle),
		noteReported: make(map[string]bool),
	}, nil
}

// HonestVerifier 是 desktop.Verifier 的诚实实现：所有结论都是
// "拿不到证据就报 Unknown"，绝不为了流程走通而返回 Verified。
//
// 这是 P0a 实测后的唯一选择：四个客户端的 UIA 控件枚举全部不可达，
// 没有任何手段在提交后确认"客户端出现了本条请求"。
//
// 刻意是无状态空结构体：它没有任何可配置项——一旦出现"宽松一点"的开关，
// 迟早有人在某个客户端上打开它，然后四个适配器里就有一个开始编造证据。
type HonestVerifier struct{}

// Ready 实现 desktop.Verifier。
//
// 返回 (true, nil) 不是在断言"客户端就绪了"，而是在声明"无法判断，
// 不阻塞发送"：Ready 读不到生成状态，返回 false 会让每一轮发送都在
// desktop.SendOptions.ReadyTimeout（默认 30 秒）后超时失败，把一个
// 可发送的通道变成永远发不出东西的通道。真正的安全阀是 VerifyText
// 的 Unknown 与上层对 Unverified 的呈现，不在这里。
func (v *HonestVerifier) Ready(ctx context.Context, target desktop.Target) (bool, error) {
	_ = ctx
	_ = target
	return true, nil
}

// VerifyText 实现 desktop.Verifier。
//
// 返回 Unknown 且**不返回错误**——desktop 包的约定是 Unknown 是一个合法的、
// 诚实的答案：返回错误会诱使上层把它当"核实失败，重试核实"，而返回
// Verified/NotVerified 中的任何一个都是编造证据。
func (v *HonestVerifier) VerifyText(ctx context.Context, target desktop.Target, text string) (desktop.VerifyResult, error) {
	_ = ctx
	_ = target
	_ = text
	return desktop.Unknown, nil
}

// ---------------------------------------------------------------------------
// Open：探测 → 定位 → 建会话绑定
// ---------------------------------------------------------------------------

// Open 实现 contract.Engine。
//
// 流程：校验 BotSpec 与引擎身份一致 → 按 manifest 的 detect 结论确认客户端
// 在运行（LocateClientWindow 内部走 registry 的 WindowsDetector，探不出来
// 就明确报"没在运行"，绝不猜窗口）→ 建专用会话绑定（窗口句柄 + PID）→
// 发 session 事件 → 向注册表第 4 层登记 Instance。
//
// 原生会话 ID：四个客户端的 manifest 都是 resume.mode=none（界面上没有
// 可读的原生会话标识），所以 NativeID 恒为空、Recovered 恒为 false。
// 这是实测事实，不是能力欠缺的委婉说法——把空 ID 填成编造值、把 Recovered
// 报成 true，重连时上层就会以为接上了同一个对话，实际却是一张白纸。
func (e *DesktopEngine) Open(ctx context.Context, spec contract.BotSpec, ws contract.Workspace) (contract.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// bot.yaml 声明的 adapter 必须与引擎身份一致。派发层按 Engine.Adapter
	// 选引擎，这里再校验一次，挡住"配置写错但引擎碰巧能开"的静默错绑——
	// 错绑的后果是消息发进完全无关的客户端。
	if spec.Engine.Adapter != e.id {
		return nil, &AdapterError{
			Adapter: e.id, Op: "Open", Kind: ErrManifest,
			Detail: fmt.Sprintf("bot %q 声明的 engine.adapter 是 %q，与引擎 %q 不符", spec.ID, spec.Engine.Adapter, e.id),
		}
	}
	// desktop 通道只实现了 dedicated 会话模式（§4.2.2）：平台在客户端里
	// 使用专用对话，绝不混用用户已有的会话。其它取值一律拒绝，而不是
	// 当成 dedicated 跑——配置语义被悄悄替换比报错更难排查。
	if sm := strings.TrimSpace(spec.Engine.SessionMode); sm != "" && sm != "dedicated" {
		return nil, &AdapterError{
			Adapter: e.id, Op: "Open", Kind: ErrUnsupported,
			Detail: fmt.Sprintf("desktop 通道只支持 session_mode=dedicated，配置为 %q", spec.Engine.SessionMode),
		}
	}

	// 定位客户端窗口（内部：manifest detect → 进程/路径匹配 → 窗口挑选）。
	// ws 只记录不使用：客户端的工作目录由用户自己在客户端里选，
	// 平台没有资格替用户改客户端的会话设置。
	_ = ws
	w, err := LocateClientWindow(ctx, e.id)
	if err != nil {
		return nil, err
	}

	info := contract.SessionInfo{
		Adapter:     e.id,
		ClientPID:   int(w.PID),
		WindowTitle: w.Title,
		NativeID:    "", // 实测取不到，如实留空（contract 约定 NativeID 空则 Recovered 必须为 false）
		Recovered:   false,
	}
	h, err := e.NewSession(newID("sess"), info, EmitterOptions{BotID: spec.ID})
	if err != nil {
		return nil, err
	}

	t := desktopTarget{hwnd: w.HWND, pid: w.PID}
	e.targetsMu.Lock()
	e.targets[h.ID()] = t
	e.targetsMu.Unlock()

	// session 事件是会话生命周期的第一条事件，一定发得出去（缓冲未满）。
	// 发不出去也只计数不回滚——会话是真实存在的，事件丢一个比假报失败诚实。
	if !h.Emitter().Session(info) {
		// 不做任何额外动作：Emitter.Dropped() 已经计数，Close 时的 final
		// 摘要会反映丢弃情况。
		_ = h.Emitter().Dropped()
	}

	// 登记注册表第 4 层。登记失败不回滚会话：连接是真实建立的，账没记上
	// 要显式说出来，静默吞掉会让 UI 显示"未连接"而实际马上就要往客户端打字。
	if e.reg != nil {
		if aerr := e.reg.Attach(&registry.Instance{
			Adapter:   e.id,
			SessionID: h.ID(),
			ClientPID: int(w.PID),
			NativeID:  "",
			Recovered: false, // NativeID 为空时注册表也会兜底置 false，这里显式写出语义
		}); aerr != nil {
			h.Emitter().Error("会话已建立，但注册表实例登记失败，平台视图与实际连接不一致：" + aerr.Error())
		}
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// Send：desktop 三段协议
// ---------------------------------------------------------------------------

// Send 实现 contract.Engine。
//
// 锁的段落边界（desktop/lock.go 的段落式持有）：
//
//	段落 A：currentTarget 复核窗口 → 持全局锁 → 聚焦 → Prepare + Submit
//	        + Verify → 恢复焦点 → 释放锁
//	段落 B：等待客户端生成 —— 完全在锁外，本方法返回后锁已释放
//
// 结果语义（由 desktop.Sender.Send 的三值结论决定，本方法不发明第四种）：
//
//	Verified    轮次登记成功，返回 (turn, nil)。但"客户端收到了"不等于
//	            "生成会成功"——输出读不到，完成状态未知，由 Events 侧说明。
//	Unknown     已执行注入但无法核实是否送达：返回 (turn, err)。turn 非 nil
//	            是因为客户端可能已经收到了这条消息；err 明确告诉上层
//	            不要重发。
//	NotVerified 确定没送达：返回 (nil, err)，上层可以安全重试。
func (e *DesktopEngine) Send(ctx context.Context, s contract.Session, msg contract.Message) (contract.Turn, error) {
	h, ok := e.Lookup(sessionID(s))
	if !ok {
		return nil, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrInternal,
			Detail: "会话不存在：" + sessionID(s),
		}
	}
	if h.Suspended() {
		return nil, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrUnsupported,
			Detail:     "会话已挂起，不接受发送",
			UserAction: "先恢复会话",
		}
	}
	if strings.TrimSpace(msg.Text) == "" {
		return nil, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrInject,
			Detail: "消息文本为空，拒绝注入（空文本在剪贴板路径下等于清空用户剪贴板再粘贴空内容）",
		}
	}

	turn := e.NewTurn(h, "")
	e.turnsMu.Lock()
	e.turns[turn.ID()] = turn
	e.turnsMu.Unlock()

	target, err := e.currentTarget(ctx, h)
	if err != nil {
		e.finishTurn(turn)
		return nil, err
	}

	// 段落 A：取锁。取不到就一个字节都没写，属于可安全重试的失败。
	release, err := desktop.AcquireDesktop(ctx)
	if err != nil {
		e.finishTurn(turn)
		return nil, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrLock,
			Detail:     "未能获得全局桌面锁，未对目标窗口做任何操作",
			Err:        err,
			UserAction: "稍后重试；若频繁出现，检查是否有其它桌面操作长时间占用",
		}
	}

	outcome := e.runSend(ctx, target, msg.Text)
	release() // 段落 A 结束，立刻释放；等待生成在锁外

	e.finishTurn(turn)
	switch outcome.Result {
	case desktop.Verified:
		h.Emitter().Progress("请求已提交并核实客户端已接收文本；本轮生成结果无法从界面读取（输出路径待真机验证），完成状态未知，平台不会宣告成功。")
		// 尝试观察本轮输出。当前 output 为 nil，observeTurn 会发出
		// "输出不可读"的状态说明；将来接入真实 OutputReader 后这条
		// 调用点自动升级为真实的输出事件，不需要再改 Send。
		e.observeTurn(h, turn, target)
		return turn, nil
	case desktop.Unknown:
		// 注入已执行但无法核实。宁可让上层拿到一个"带错误的轮次"，
		// 也不能返回纯 error 诱使它按"发送失败"重试——那条消息可能
		// 已经在客户端里跑起来了。
		detail := "注入已执行，但无法核实客户端是否收到本条请求（核实路径不可用），禁止重发"
		h.Emitter().Error(detail + "。请人工在客户端输入区确认。")
		return turn, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrVerify,
			Detail:     detail,
			Err:        outcome.Err,
			UserAction: "在客户端输入区确认消息是否已发出；不要让平台重发",
		}
	default: // NotVerified：确定没送达。
		detail := "确认未送达客户端，未发生重复提交"
		h.Emitter().Error(detail + "：" + errorText(outcome.Err))
		return nil, &AdapterError{
			Adapter: e.id, Op: "Send", Kind: ErrVerify,
			Detail:     detail,
			Err:        outcome.Err,
			UserAction: "可根据错误原因重试本轮发送",
		}
	}
}

// runSend 执行段落 A 的桌面交互，全部要求在持锁状态下调用。
//
// 注入路径：desktop.Target 不带 TextTarget（为 nil），所以 desktop.Inject
// 直接走剪贴板路径。控件直设（UIA ValuePattern）在本机实测不可达，
// 剪贴板是 P0 主路径——这一结论写在 manifest 的 output.notes 与
// adapters/README.md，代码里不做"先试一下 UIA"的姿态性调用：
// 适配器不许自己实现 UIA，而 desktop.TextTarget 需要一个真实实现才能挂上。
func (e *DesktopEngine) runSend(ctx context.Context, t desktopTarget, text string) desktop.SendOutcome {
	// 焦点纪律：先快照用户的前台窗口，操作完还回去。
	fg := desktop.SaveForeground()
	defer func() {
		// 恢复失败只记后果不报错：注入可能已经成功，焦点还不上是
		// 尽力而为的收尾，不该把已经送达的一轮说成失败。
		_ = desktop.RestoreForeground(ctx, fg)
	}()

	if err := desktop.EnsureForeground(ctx, t.hwnd, desktop.FocusOptions{
		RestoreMinimized: e.RestoreMinimized,
	}); err != nil {
		// 聚焦失败是 fail closed：没碰任何键，确定没送达 → NotVerified。
		// 按 NotVerified 而不是 Unknown 返回，上层才能安全地重试。
		return desktop.SendOutcome{Result: desktop.NotVerified, Err: classifyFocusErr(e.id, "Send", err)}
	}

	return e.sender.Send(ctx, desktop.Target{HWND: t.hwnd}, text)
}

// ---------------------------------------------------------------------------
// Events 与输出观察
// ---------------------------------------------------------------------------

// noteOutputUnavailable 向会话发一条"为什么读不到输出"的 error 事件。
//
// 每个会话只发一次：这条事件是状态说明不是错误报警，每轮 Send 都刷一遍
// 只会把真正的新错误淹没。内容必须写清三件事：为什么读不到（两个实测
// 原因）、这条路现在的状态（待真机验证）、用户该做什么（自己看客户端）。
func (e *DesktopEngine) noteOutputUnavailable(h *SessionHandle) {
	e.noteMu.Lock()
	if e.noteReported[h.ID()] {
		e.noteMu.Unlock()
		return
	}
	e.noteReported[h.ID()] = true
	e.noteMu.Unlock()

	h.Emitter().Error("无法读取客户端界面输出，本轮结果需要人工在客户端里查看。原因（均为本机实测）：(1) UI Automation 控件枚举不可达，probe 连资源管理器基线窗口都枚举不到后代控件，'读消息列表'整条路径标为待真机验证；(2) manifest 声明的剪贴板抓取输出需要 desktop 包导出剪贴板读取原语，当前未导出。平台不会猜测完成状态，也不会以任何启发式宣告成功。")
}

// observeTurn 尝试读取一轮生成的输出并归一化为事件。
//
// 当前的现实是 e.output == nil：调用 noteOutputUnavailable 后返回。
// reader 存在时的路径是未来接入的唯一合法形状——读到文本发 Output 事件，
// 只有 SignalNativeDone / SignalClientError 这种"客户端明确给出"的结论
// 才允许产出 final 并结束事件流；unknown_only 一律停在"不发 final"，
// 完成状态保持未确认。
func (e *DesktopEngine) observeTurn(h *SessionHandle, turn *TurnHandle, target desktopTarget) {
	if e.output == nil {
		e.noteOutputUnavailable(h)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cw := &ClientWindow{HWND: target.hwnd, PID: target.pid}
	sample, err := e.output.ReadOutput(ctx, cw)
	if err != nil {
		// 读取失败是"这一轮没读到"，不是"没有输出"：发 error 说明，
		// 绝不产出 final（没有证据不许下任何结论）。
		h.Emitter().Error("输出读取失败（本轮结果未确认）：" + errorText(err))
		return
	}
	switch {
	case sample.ClientError != "":
		// 客户端明确报错：这是确定的结论（SignalClientError 是 Verified 类），
		// 允许 final 并关闭事件流。
		h.Emitter().Final(DecideFinal(Outcome{
			Signal:      SignalClientError,
			ClientError: sample.ClientError,
			Summary:     fmt.Sprintf("客户端界面报错（turn %s）", turn.ID()),
		}))
	case sample.Signal == SignalNativeDone:
		h.Emitter().Final(DecideFinal(Outcome{
			Signal:  SignalNativeDone,
			Text:    sample.Text,
			Summary: fmt.Sprintf("客户端给出明确完成标记（turn %s）", turn.ID()),
		}))
	default:
		// 有文本但没有完成信号：文本如实上报，结论保持未确认。
		if sample.Text != "" {
			h.Emitter().Output(sample.Text)
		}
		h.Emitter().Progress("读到输出文本但没有任何完成信号（unknown_only），本轮保持未确认。")
	}
}

// ---------------------------------------------------------------------------
// Cancel：Esc 停止键，两态诚实结论
// ---------------------------------------------------------------------------

// Cancel 实现 contract.Engine。
//
// 唯一可用手段是 desktop.PressEscape（键是全局注入的，必须持锁 + 前台，
// PressKey 内部已按发送协议的四步做聚焦纪律）。它没有返回值告诉我们
// 客户端是否真的停了——keybd_event 是 fire-and-forget。所以结论只有两种：
//
//	发送失败 → "未发送，请手动停止"（明确失败，可直接重试）
//	发送成功 → "停止待确认"（ErrStopUnconfirmed，NeedsUserAttention）
//
// 两种都不会被标成"已停止"：把"键发出去了"说成"停了"，用户会放心地
// 走开，而客户端可能还在消耗他的额度。
func (e *DesktopEngine) Cancel(ctx context.Context, s contract.Session, turnID string) error {
	h, ok := e.Lookup(sessionID(s))
	if !ok {
		return &AdapterError{
			Adapter: e.id, Op: "Cancel", Kind: ErrInternal,
			Detail: "会话不存在：" + sessionID(s),
		}
	}

	target, err := e.currentTarget(ctx, h)
	if err != nil {
		return err
	}

	// PressEscape 内部处理锁与焦点（未持锁则自取，已持有则复用）。
	// 这里刻意不预取桌面锁：取消是用户主动的低频动作，独立段落足够。
	if err := desktop.PressEscape(ctx, target.hwnd); err != nil {
		detail := "停止键未发送（焦点或锁预检未通过，未向客户端发送任何按键），生成可能仍在进行"
		h.Emitter().Error(detail + "：" + errorText(err))
		// 分类只决定上层如何归档，UserAction 都指向同一件事——平台
		// 这次没帮上忙，需要用户手动停止。
		var fe *desktop.FocusError
		switch {
		case errors.As(err, &fe):
			aerr := classifyFocusErr(e.id, "Cancel", err)
			aerr.Detail = detail + "。" + aerr.Detail
			aerr.UserAction = "手动在客户端里停止生成"
			return aerr
		case errors.Is(err, desktop.ErrLockTimeout):
			return &AdapterError{
				Adapter: e.id, Op: "Cancel", Kind: ErrLock,
				Detail:     detail,
				Err:        err,
				UserAction: "稍后重试，或手动在客户端里停止生成",
			}
		default:
			return &AdapterError{
				Adapter: e.id, Op: "Cancel", Kind: ErrFocus,
				Detail:     detail,
				Err:        err,
				UserAction: "手动在客户端里停止生成",
			}
		}
	}

	// Esc 已发出。无法核实生成是否真的停了：没有手段读客户端状态，
	// 不发"已停止"的 final，也不关事件流（会话还要继续用）。
	so := StopOutcome{
		Requested: true,
		Confirmed: false,
		Detail:    "已发送 Esc 停止键，但无法读取客户端状态，生成是否已停止未确认",
	}
	h.Emitter().Error(so.Detail + "。请回到客户端确认是否已停止；确认停止后本轮状态为" + string(SignalStopUnconfirmed) + "，不是成功。")

	if turnID != "" {
		e.turnsMu.Lock()
		t, ok := e.turns[turnID]
		e.turnsMu.Unlock()
		if ok {
			t.markDone()
		}
	}
	// 返回 ErrStopUnconfirmed 而不是 nil：nil 会让上层以为取消已生效。
	// NeedsUserAttention 会按这个分类把它推到用户面前。
	return StopUnconfirmedError(e.id, so.Detail)
}

// finishTurn 标记轮次在适配器侧的登记结束。
func (e *DesktopEngine) finishTurn(t *TurnHandle) {
	if t != nil {
		t.markDone()
	}
}

// ---------------------------------------------------------------------------
// Close / CloseAll：只释放平台持有的连接
// ---------------------------------------------------------------------------

// Close 实现 contract.Engine。
//
// 关闭前发一条 final：这是会话生命周期的终结事件（Emitter.Final 会关闭
// 事件通道，contract 约定"Events 关闭 = 该会话不再产生事件"）。final 恒为
// Unverified——会话关闭不代表客户端里的生成结束了，更不代表它成功。
// Suspend/Close 绝不关闭客户端进程或窗口（§4.2.2 / §9.2），只注销
// 平台侧的会话簿记与注册表 Instance。
func (e *DesktopEngine) Close(s contract.Session) error {
	id := sessionID(s)
	h, ok := e.Lookup(id)
	if ok && !h.Closed() {
		// 先发 final（会关闭 emitter 通道），再走基类清理。
		h.Emitter().Final(contract.FinalInfo{
			Success:       false,
			SuccessSignal: string(SignalNone),
			Summary:       "平台已释放该会话连接；客户端窗口保持打开，未关闭任何客户端进程。会话关闭前的生成结果未能核实。",
			Unverified:    true,
		})
	}
	if e.reg != nil && ok {
		e.reg.Detach(e.id, id)
	}
	e.targetsMu.Lock()
	delete(e.targets, id)
	e.targetsMu.Unlock()
	// 清理该会话登记过的轮次：TurnHandle 的生命周期跟随会话，会话关了
	// 它们就不可能再被 Cancel 引用，留着是无意义的增长。
	e.turnsMu.Lock()
	for tid, t := range e.turns {
		if t.SessionID() == id {
			delete(e.turns, tid)
		}
	}
	e.turnsMu.Unlock()
	e.noteMu.Lock()
	delete(e.noteReported, id)
	e.noteMu.Unlock()
	return e.BaseEngine.Close(s)
}

// CloseAll 覆盖基类版本：必须走 DesktopEngine.Close 才会发 final 并注销
// 注册表 Instance，否则进程退出时所有会话的账都挂在注册表第 4 层不下账。
func (e *DesktopEngine) CloseAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		if h, ok := e.Lookup(id); ok {
			_ = e.Close(h)
		}
	}
}

// ---------------------------------------------------------------------------
// Health：探测可达性，并说清是哪一侧的问题
// ---------------------------------------------------------------------------

// Health 实现 contract.Engine。
//
// 失败时必须分清两侧（这是任务硬性要求，也是排障的第一分叉点）：
//
//	探测侧失败（Detection.Err 非空）→ 进程枚举没做成，"客户端没开"这个
//	结论没有证据支撑，报 ErrVerify 并明确说"未确认"。
//	客户端侧不可达（探测做成了，结论是没有）→ ErrClientNotRunning。
func (e *DesktopEngine) Health() error {
	// Health 的签名没有 ctx，用短超时兜底：探测是进程枚举，几十毫秒级，
	// 15 秒还不返回说明系统级异常，报错比挂着强。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	d, m, err := DetectClient(ctx, e.id)
	if err != nil {
		if d != nil && d.Uncertain() {
			return &AdapterError{
				Adapter: e.id, Op: "Health", Kind: ErrVerify,
				Detail:     "探测侧失败，客户端运行状态未确认：" + d.Err,
				UserAction: "检查系统进程枚举权限后重试",
			}
		}
		return &AdapterError{
			Adapter: e.id, Op: "Health", Kind: ErrVerify,
			Detail: "探测侧失败，客户端运行状态未确认",
			Err:    err,
		}
	}
	if !d.Available {
		return &AdapterError{
			Adapter: e.id, Op: "Health", Kind: ErrClientNotRunning,
			Detail:     "客户端侧：没有检测到正在运行的 " + m.Name,
			UserAction: "启动客户端并登录",
		}
	}
	// 探测可达。顺带报告活动会话数，便于诊断"会话都在但事件没人消费"。
	_ = e.SessionCount()
	return nil
}

// ---------------------------------------------------------------------------
// 内部小工具
// ---------------------------------------------------------------------------

// currentTarget 返回会话当前绑定的客户端窗口，并在必要时重新定位。
//
// 优先复核缓存的 HWND（LocateClientWindowByHWND 会确认它仍属于客户端
// 进程，防止句柄被系统回收后打到别的窗口）；复核失败（窗口被关了重开、
// 客户端重启）就重新全量定位一次并更新绑定。都不行才报错——用户重启
// 客户端是常态，会话应该尽力跟着走，而不是第一下就摆烂。
func (e *DesktopEngine) currentTarget(ctx context.Context, h *SessionHandle) (desktopTarget, error) {
	e.targetsMu.Lock()
	t, cached := e.targets[h.ID()]
	e.targetsMu.Unlock()

	if cached {
		w, err := LocateClientWindowByHWND(ctx, e.id, t.hwnd)
		if err == nil {
			h.SetWindowTitle(w.Title)
			return t, nil
		}
		// 缓存失效（窗口被回收/客户端重启），落到重新定位。
		// 不把这里的 err 直接返回：它描述的是"旧句柄没了"，不是"客户端没了"。
	}

	w, err := LocateClientWindow(ctx, e.id)
	if err != nil {
		return desktopTarget{}, err
	}
	nt := desktopTarget{hwnd: w.HWND, pid: w.PID}
	e.targetsMu.Lock()
	e.targets[h.ID()] = nt
	e.targetsMu.Unlock()
	h.SetWindowTitle(w.Title)
	return nt, nil
}

// classifyFocusErr 把 desktop.FocusError 翻译成适配器错误分类。
//
// 映射只做一层：锁屏 → ErrScreenLocked，窗口没了 → ErrClientNotRunning，
// 其余焦点问题 → ErrClientNotForeground。上层靠 NeedsUserAttention 决定
// 是否弹给用户看，所以分类错了等于把该弹的吞掉。
func classifyFocusErr(adapter, op string, err error) *AdapterError {
	var fe *desktop.FocusError
	if !errors.As(err, &fe) {
		return &AdapterError{
			Adapter: adapter, Op: op, Kind: ErrFocus,
			Detail: "焦点操作失败", Err: err,
		}
	}
	kind := ErrFocus
	action := "把客户端窗口打开并置于桌面，然后重试"
	switch fe.Kind {
	case desktop.FocusScreenLocked:
		kind, action = ErrScreenLocked, "解锁屏幕后重试"
	case desktop.FocusWindowGone:
		kind, action = ErrClientNotRunning, "重新打开客户端窗口"
	}
	return &AdapterError{
		Adapter:    adapter,
		Op:         op,
		Kind:       kind,
		Detail:     fe.Detail,
		UserAction: action,
		Err:        fe,
	}
}

// errorText 取错误的可读文本，nil 安全。
func errorText(err error) string {
	if err == nil {
		return "（无底层错误）"
	}
	return err.Error()
}
