//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 焦点管理
// ---------------------------------------------------------------------------
//
// 方案 §4.2.2 的核心要求：抢焦点前先记录当前前台窗口，操作后恢复。并且必须
// 能检测"目标窗口不在前台 / 被最小化 / 锁屏"并返回明确错误，而不是把字打到
// 错误窗口里。
//
// 最后半句是本节存在的全部理由。桌面自动化的灾难性故障不是"没发出去"，而是
// "发到了别的地方"——把本该发给 bot 的提示词打进了用户的私人聊天窗口，或者
// 在用户正在编辑的文档里按下 Ctrl+V。这类错误不可撤销，且用户往往不知道发生
// 了什么。所以本包的策略是**失败优先（fail closed）**：任何一项预检不通过就
// 报错退出，绝不"尽力尝试一下"。
//
// 所有函数都假定调用方已经持有全局桌面锁（见 lock.go）。没有锁的焦点操作会
// 和其他 bot 的操作交错，预检与注入之间会出现 TOCTOU 窗口，预检就失去意义。
// ---------------------------------------------------------------------------

// FocusErrorKind 是可机器判定的焦点失败原因。
//
// 用独立的 Kind 而不是靠字符串匹配，是因为上层需要按原因决定行为：锁屏应当
// 暂停整个队列，最小化可以尝试恢复，前台不对应当重新聚焦，"窗口没了"则要
// 重建适配器会话。这些分支不能靠猜错误文案。
type FocusErrorKind string

const (
	// FocusWindowGone 目标窗口句柄已失效（进程退出或被销毁）。
	FocusWindowGone FocusErrorKind = "window_gone"
	// FocusWindowMinimized 目标窗口被最小化。
	FocusWindowMinimized FocusErrorKind = "window_minimized"
	// FocusWindowNotVisible 目标窗口不可见（被隐藏或属于别的虚拟桌面）。
	FocusWindowNotVisible FocusErrorKind = "window_not_visible"
	// FocusScreenLocked 屏幕已锁定，或无法确认未锁定。
	FocusScreenLocked FocusErrorKind = "screen_locked"
	// FocusNotForeground 目标窗口不在前台，且我们没能把它提到前台。
	FocusNotForeground FocusErrorKind = "not_foreground"
	// FocusDenied 系统拒绝了前台切换请求（前台锁）。
	FocusDenied FocusErrorKind = "focus_denied"
)

// FocusError 是焦点预检/聚焦失败的结构化错误。
//
// 每条都带可读的原因说明，因为最终它要展示给用户看：用户需要知道"为什么这个
// bot 卡住了"，而不是看到一个 syscall 错误号。
type FocusError struct {
	Kind   FocusErrorKind
	HWND   uintptr
	Detail string
	Err    error
}

func (e *FocusError) Error() string {
	msg := focusKindText(e.Kind)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += " (" + e.Err.Error() + ")"
	}
	return "desktop: " + msg
}

func (e *FocusError) Unwrap() error { return e.Err }

// focusKindText 把 Kind 翻成面向用户的中文原因。
func focusKindText(k FocusErrorKind) string {
	switch k {
	case FocusWindowGone:
		return "目标窗口已不存在，客户端可能已退出"
	case FocusWindowMinimized:
		return "目标窗口被最小化"
	case FocusWindowNotVisible:
		return "目标窗口不可见"
	case FocusScreenLocked:
		return "屏幕已锁定或无法确认未锁定，此时注入会落到锁屏界面"
	case FocusNotForeground:
		return "目标窗口不在前台，且无法将其置于前台"
	case FocusDenied:
		return "系统拒绝了前台切换请求"
	default:
		return "焦点操作失败"
	}
}

// IsFocusErrorKind 判断 err 是否为指定原因的焦点错误。
func IsFocusErrorKind(err error, kind FocusErrorKind) bool {
	var fe *FocusError
	if errors.As(err, &fe) {
		return fe.Kind == kind
	}
	return false
}

// ---------------------------------------------------------------------------
// 前台窗口保存与恢复
// ---------------------------------------------------------------------------

// Foreground 是一次前台窗口快照。
//
// 为什么用显式句柄而不是包级全局变量：多个 bot 的段落式持有会交错，全局变量
// 会被后一个覆盖，导致 A 操作完把焦点"恢复"到了 B 记录下来的窗口。快照必须
// 跟着调用栈走。
type Foreground struct {
	prev uintptr
	// takenAt 记录快照时间。恢复前用它判断这次操作是否已经拖太久——
	// 如果用户在我们操作期间主动切走了窗口，强行恢复会把焦点从用户手上抢走。
	takenAt time.Time
}

// SaveForeground 记录当前前台窗口。
//
// 返回值永不为 nil。若当时没有前台窗口（罕见，例如会话切换瞬间），prev 为 0，
// 此时 Restore 什么都不做而不是报错——没有可恢复的东西不是错误。
func SaveForeground() *Foreground {
	return &Foreground{
		prev:    foregroundWindow(),
		takenAt: time.Now(),
	}
}

// HWND 返回快照里的窗口句柄，可能为 0。
func (f *Foreground) HWND() uintptr {
	if f == nil {
		return 0
	}
	return f.prev
}

// Restore 把焦点还给快照记录的那个窗口。
//
// 三条保护，缺一不可：
//  1. 快照为空 → 直接返回，没有可恢复的目标。
//  2. 那个窗口已经不存在（用户关掉了）→ 不报错，只是不再恢复。强行对失效句柄
//     调 SetForegroundWindow 是无意义的，而"用户关窗"不是我们的失败。
//  3. 用户已经自己切到了别处 → 不抢。如果当前前台窗口既不是我们操作的目标、
//     也不是快照记录的原窗口，说明用户在这期间主动切换了，此时恢复焦点等于
//     和用户抢键盘。这种情况下我们什么都不做才是对的。
//
// 注意 Restore 绝不关闭任何窗口。本包没有任何 DestroyWindow / WM_CLOSE 调用，
// 这是硬性要求：用户自己开的客户端窗口不归我们管。
func (f *Foreground) Restore(ctx context.Context) error {
	if f == nil || f.prev == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		// ctx 已取消：仍然尽力恢复，因为把焦点留在 bot 窗口上是最坏的结果。
		// 但只做一次非阻塞尝试，不再等。
		if isWindow(f.prev) {
			forceForegroundOnce(f.prev)
		}
		return err
	}
	if !isWindow(f.prev) {
		return nil
	}
	if foregroundWindow() == f.prev {
		return nil // 已经是它了
	}

	// 尝试恢复，但允许失败：恢复焦点是"尽力而为"的收尾动作，失败不应让整个
	// 已成功的注入变成失败。上层拿到错误只需记日志。
	if err := bringToForeground(ctx, f.prev, false); err != nil {
		return err
	}
	return nil
}

// RestoreForeground 是 Foreground.Restore 的函数式别名，nil 安全。
func RestoreForeground(ctx context.Context, f *Foreground) error {
	if f == nil {
		return nil
	}
	return f.Restore(ctx)
}

// ---------------------------------------------------------------------------
// 预检
// ---------------------------------------------------------------------------

// Preflight 在不改变任何窗口状态的前提下检查目标窗口是否适合接收键盘输入。
//
// 检查顺序按"越便宜越先"排列，但顺序也有语义：先确认窗口还在，再谈它长什么样，
// 最后确认锁屏——因为锁屏会同时让所有窗口都不在前台，先报锁屏比先报"不在前台"
// 对用户更有用。
func Preflight(hwnd uintptr) error {
	if hwnd == 0 {
		return &FocusError{Kind: FocusWindowGone, Detail: "窗口句柄为 0"}
	}
	if !isWindow(hwnd) {
		return &FocusError{
			Kind:   FocusWindowGone,
			HWND:   hwnd,
			Detail: "IsWindow 返回 false，窗口可能已被销毁",
		}
	}

	locked, err := screenLocked()
	if err != nil {
		// 无法判定锁屏状态时按"已锁屏"处理（fail closed）。理由是注入到锁屏
		// 界面上的按键会被 Winlogon 接收，这是最坏情况；而误判为锁屏的代价只是
		// 本轮操作推迟。
		return &FocusError{
			Kind:   FocusScreenLocked,
			HWND:   hwnd,
			Detail: "无法确认锁屏状态，按已锁屏处理",
			Err:    err,
		}
	}
	if locked {
		return &FocusError{
			Kind:   FocusScreenLocked,
			HWND:   hwnd,
			Detail: "输入桌面不是 Default，当前处于锁屏或 UAC 安全桌面",
		}
	}

	if isIconic(hwnd) {
		return &FocusError{
			Kind:   FocusWindowMinimized,
			HWND:   hwnd,
			Detail: "IsIconic 返回 true",
		}
	}
	if !isWindowVisible(hwnd) {
		return &FocusError{
			Kind:   FocusWindowNotVisible,
			HWND:   hwnd,
			Detail: "IsWindowVisible 返回 false",
		}
	}
	return nil
}

// screenLocked 检测当前是否锁屏。
//
// 判定方法：打开输入桌面（OpenInputDesktop）并读取它的名字。正常登录会话的
// 输入桌面名为 "Default"；锁屏 / UAC 安全桌面会变成 "Winlogon" 等其它名字。
//
// 为什么不用 WTSQuerySessionInformation 的 WTSSessionInfoEx：那需要 wtsapi32.dll
// 且结构体布局随版本变化，风险大于收益。输入桌面名这条路只用 user32.dll。
//
// 已知局限（必须在 README 里对用户讲清楚）：某些第三方锁屏工具会自己接管输入
// 桌面但名字仍是 "Default"；远程桌面断开时行为也不同于本地锁屏。也就是说本函数
// 能覆盖标准 Win+L 锁屏，但不保证覆盖所有场景。
func screenLocked() (bool, error) {
	name, err := inputDesktopName()
	if err != nil {
		return false, err
	}
	return name != "Default", nil
}

// ---------------------------------------------------------------------------
// 聚焦
// ---------------------------------------------------------------------------

// FocusOptions 控制聚焦行为。
type FocusOptions struct {
	// RestoreMinimized 为 true 时允许把最小化的窗口恢复出来。
	//
	// 默认（false）遇到最小化直接报错。因为"恢复一个窗口"本身就是用户可见的
	// 干扰动作，是否允许应当由上层按策略决定，而不是由底层默默替用户做主。
	RestoreMinimized bool

	// Timeout 是等待窗口真正变成前台的上限。0 表示用 defaultFocusTimeout。
	//
	// 为什么不复用 ctx：SetForegroundWindow 是异步的，系统同意之后窗口成为
	// 前台还有延迟，需要轮询确认。这个轮询需要一个独立的、比整个操作 ctx 更短
	// 的预算，否则一个慢窗口会吃掉整个操作的时限。
	Timeout time.Duration
}

const (
	defaultFocusTimeout = 2 * time.Second
	focusPollInterval   = 25 * time.Millisecond
)

// EnsureForeground 确保目标窗口处于前台，可以被安全注入。
//
// 流程：预检 → 已在前台则直接返回 → 必要时恢复最小化 → 抢焦点 → 轮询确认。
//
// 调用方必须已持有全局桌面锁。
func EnsureForeground(ctx context.Context, hwnd uintptr, opts FocusOptions) error {
	if !Held() {
		// 编程错误保护。检查与使用之间有竞态，但在"自己忘了加锁"这个最常见的
		// 失误上它必然命中，值得保留。
		return &FocusError{
			Kind:   FocusNotForeground,
			HWND:   hwnd,
			Detail: "内部错误：调用 EnsureForeground 时未持有全局桌面锁",
		}
	}

	if err := Preflight(hwnd); err != nil {
		var fe *FocusError
		if errors.As(err, &fe) && fe.Kind == FocusWindowMinimized && opts.RestoreMinimized {
			// 允许恢复：ShowWindow(SW_RESTORE) 之后重新走一遍预检。
			// 恢复动作本身也可能失败（窗口在别的虚拟桌面、被系统禁止），
			// 所以必须复查而不是假定成功。
			showWindow(hwnd, swRestore)
			if err := sleepCtx(ctx, focusPollInterval); err != nil {
				return err
			}
			if err := Preflight(hwnd); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if foregroundWindow() == hwnd {
		return nil
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultFocusTimeout
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := bringToForeground(fctx, hwnd, true); err != nil {
		return err
	}

	// 轮询确认。SetForegroundWindow 成功返回不代表窗口立刻就是前台，
	// 中间隔着一层窗口管理器的异步处理。
	for {
		if foregroundWindow() == hwnd {
			return nil
		}
		if err := sleepCtx(fctx, focusPollInterval); err != nil {
			return &FocusError{
				Kind:   FocusNotForeground,
				HWND:   hwnd,
				Detail: fmt.Sprintf("等待 %s 后目标窗口仍未成为前台窗口", timeout),
				Err:    err,
			}
		}
	}
}

// bringToForeground 尝试把窗口提到前台，带一次重试。
//
// Windows 的前台锁（foreground lock）会拒绝后台进程直接抢焦点：SetForegroundWindow
// 返回非 0 也可能没生效，或者干脆返回失败。标准绕法是把本线程的输入队列附加到
// 当前前台窗口的线程上，此时系统认为我们在代表前台窗口操作，于是放行。
func bringToForeground(ctx context.Context, hwnd uintptr, strict bool) error {
	if forceForegroundOnce(hwnd) {
		return nil
	}
	if err := sleepCtx(ctx, focusPollInterval); err != nil {
		return &FocusError{Kind: FocusDenied, HWND: hwnd, Detail: "等待重试时被取消", Err: err}
	}
	if forceForegroundOnce(hwnd) {
		return nil
	}
	if !strict {
		// 非严格模式（用于恢复原前台窗口）允许静默失败：恢复焦点失败不该
		// 影响已经完成的操作结果。
		return nil
	}
	return &FocusError{
		Kind:   FocusDenied,
		HWND:   hwnd,
		Detail: "SetForegroundWindow 连续两次被系统拒绝（前台锁）",
	}
}

// forceForegroundOnce 做一次附加线程输入 + SetForegroundWindow + 解除附加。
//
// 无论成功与否都必须解除附加：附加期间两个线程共享输入队列，如果泄漏了附加
// 状态，会让目标进程和我们的输入处理互相干扰，是很隐蔽的 bug。
func forceForegroundOnce(hwnd uintptr) bool {
	if hwnd == 0 || !isWindow(hwnd) {
		return false
	}
	fg := foregroundWindow()
	var fgThread uintptr
	if fg != 0 {
		fgThread = threadIDOfWindow(fg)
	}
	own := currentThreadID()

	attached := false
	if fgThread != 0 && fgThread != own {
		attached = attachThreadInput(own, fgThread, true)
	}
	if attached {
		defer attachThreadInput(own, fgThread, false)
	}

	// 最小化的窗口不能直接 SetForegroundWindow，必须先恢复。
	if isIconic(hwnd) {
		showWindow(hwnd, swRestore)
	}

	ok := setForegroundWindow(hwnd)
	if !ok {
		// 部分情况下 SetForegroundWindow 报失败但实际已经生效（系统在返回前
		// 就完成了切换）。以实际状态为准，不迷信返回值。
		return foregroundWindow() == hwnd
	}
	return true
}
