//go:build windows

package desktop

import (
	"context"
	"errors"
	"time"
)

// 本文件只做一件事：把"发一次按键"这件事包装成和发送文本同样安全的原语。
//
// 为什么需要它：Cancel（停止生成）在桌面客户端上唯一可用的手段就是发 Esc，
// 而 keybd_event 是**全局**注入——它不认窗口，只认当前焦点。所以"按键"和
// "往窗口里打字"是同一类危险操作，必须共用同一套焦点纪律和同一把锁。
// 适配器层不允许自己调 keybd_event 重造这套逻辑（task-6 硬约束）。

// PressKeyOptions 控制 PressKey 的聚焦行为。
type PressKeyOptions struct {
	// RestoreMinimized 允许把最小化的窗口恢复出来再按键。
	//
	// 默认（false）遇到最小化直接报错，理由与 FocusOptions 完全一致：
	// 把窗口弹出来是用户可见的干扰动作，是否允许应由上层按策略决定。
	RestoreMinimized bool

	// FocusTimeout 是等待窗口真正成为前台的上限。0 表示用默认值。
	FocusTimeout time.Duration
}

// PressKey 向目标窗口发送一次完整按键（按下 + 抬起）。
//
// 严格按发送协议的四步走，缺一不可：
//
//  1. 取锁（调用方已持有则复用，见下）
//  2. 快照当前前台窗口
//  3. EnsureForeground 聚焦，并在按键前**再复核一次**前台窗口
//  4. RestoreForeground 把焦点还给用户
//
// 为什么第 3 步要复核两次：EnsureForeground 返回 nil 只证明"那一刻"窗口是
// 前台。从那一刻到 keybd_event 真正执行之间还有时间，用户完全可能已经切走。
// 此时 Esc 会落到用户正在打字的窗口里——如果用户正在编辑文档，Esc 可能取消
// 他的输入；在浏览器里可能中断页面加载。这和"把字打到错误窗口"是同一类事故，
// 方案 §4.2.2 明令禁止。所以宁可这次取消不生效，也绝不盲发。
//
// 锁的处理有一个必须说清的点：globalLock 不可重入。如果调用方已经持有锁
// （例如它刚做完一段注入，正处于同一个段落式持有区间内），这里再 Acquire
// 会自己等自己，直到超时。所以先查 Held()，已持有就不再取、也不释放。
// 这让 PressKey 在两种调用方式下都正确：独占调用，或嵌进调用方已有的锁区间。
//
// 返回 error 时**不保证**按键没有发出（keybd_event 没有返回值），只保证
// "我们不会在未确认焦点的情况下发"。这个区别由适配器层用
// "已请求未证实 / 未发送" 两态如实表达，见 adapters 包。
func PressKey(ctx context.Context, hwnd uintptr, vk uint16, opts PressKeyOptions) error {
	if ctx == nil {
		return errors.New("desktop: PressKey 需要非 nil 的 context")
	}
	if hwnd == 0 {
		return &FocusError{Kind: FocusWindowGone, Detail: "窗口句柄为 0"}
	}
	if vk == 0 {
		return errors.New("desktop: 虚拟键码为 0")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 锁：已持有就复用，未持有就自己取并负责释放。
	release := func() {}
	if !Held() {
		rel, err := AcquireDesktop(ctx)
		if err != nil {
			return err
		}
		release = rel
	}
	defer release()

	// 先快照再聚焦：顺序反了就把"我们自己刚抢来的焦点"当成用户的原焦点，
	// 恢复时等于什么都没恢复。
	fg := SaveForeground()
	defer func() {
		// 恢复焦点是尽力而为的收尾动作，失败不该让已经发出的按键变成错误。
		// Restore 内部对 ctx 已取消、窗口已销毁、用户已自己切走三种情况都有保护。
		_ = RestoreForeground(ctx, fg)
	}()

	if err := EnsureForeground(ctx, hwnd, FocusOptions{
		RestoreMinimized: opts.RestoreMinimized,
		Timeout:          opts.FocusTimeout,
	}); err != nil {
		return err
	}

	// 最后一次复核，理由见函数文档第 3 步。
	if foregroundWindow() != hwnd {
		return &FocusError{
			Kind:   FocusNotForeground,
			HWND:   hwnd,
			Detail: "按键前目标窗口已不在前台，已中止（避免按键落到其他窗口）",
		}
	}

	// 按下与抬起紧邻，不留间隔：这与 ctrlV 的写法一致，实测可用。
	// 留间隔只会扩大"焦点被切走"的窗口，不会提高可靠性。
	keyPress(vk, false)
	keyPress(vk, true)
	return nil
}

// PressEscape 向目标窗口发送一次 Esc。
//
// 单独导出的理由：Esc 是桌面客户端上"停止当前生成"的唯一键盘手段，四个适配器
// 的 Cancel 都要用它。让调用方自己传 vkEscape 常量，就得把这个常量也导出，
// 而"导出虚拟键码表"会鼓励上层去拼组合键——那正是本包想避免的事。
func PressEscape(ctx context.Context, hwnd uintptr) error {
	return PressKey(ctx, hwnd, vkEscape, PressKeyOptions{})
}
