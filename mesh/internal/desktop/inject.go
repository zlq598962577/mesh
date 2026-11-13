//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 文本注入
// ---------------------------------------------------------------------------
//
// 方案 §4.2.2：优先走控件直设（UIA ValuePattern），失败才降级到剪贴板粘贴。
//
// 两条路的取舍：
//
//	控件直设：不碰剪贴板、不依赖焦点、不会把用户剪贴板里的东西挤掉。但要求
//	          目标控件真的实现了 ValuePattern，而很多 Electron / 自绘输入框
//	          没有实现，或者实现了但只读。
//	剪贴板粘贴：通用，任何能接收 Ctrl+V 的输入框都行。代价是要借用用户的
//	          剪贴板——这是有创意的破坏性操作，因为用户可能正复制着重要内容。
//
// 本包的态度是：默认走剪贴板（因为它是唯一通用的路径），但把剪贴板路径写成
// "借用并完整归还"，并且在归还失败时如实上报，而不是假装没事。
//
// UIA 本身不在本包实现：UIAutomationCore 是 COM 接口，纯 syscall 直调需要
// 手工搭 vtable 布局，在没有编译器和运行环境的情况下写出来必然是一堆无法验证
// 的偏移量猜测。所以本包只定义 TextTarget 接口，具体实现在适配器层（那里可以
// 用成熟的 UIA 绑定库，也便于单测）。见 README「UIA 为何不在本层实现」。
// ---------------------------------------------------------------------------

// ErrClipboardBusy 表示剪贴板被其他进程长时间占用，重试耗尽。
//
// 这是"明确失败"而不是"可能已生效"：返回它时我们没有往目标窗口发送任何按键。
var ErrClipboardBusy = errors.New("desktop: 剪贴板被其他进程占用，重试耗尽")

// ErrEmptyText 拒绝注入空文本。
//
// 空文本在剪贴板路径下意味着"清空剪贴板再粘贴空内容"，在直设路径下意味着
// "把输入框清空"——两者都不是调用方想要的效果，几乎必然是上游 bug。
var ErrEmptyText = errors.New("desktop: 拒绝注入空文本")

// TextTarget 是需要焦点无关的控件直设能力。
//
// 只声明这一个方法，是因为本包只关心"把这段文字放进那个控件"。控件怎么定位、
// 用不用 UIA、走 ValuePattern 还是 UIA 的 TextPattern，都是适配器层的自由。
// 本包不引入任何 UIA 类型，也就不把这个包绑死在 Windows 的 COM 世界之外。
//
// 实现方约定：
//   - 返回 nil 表示控件已经确实接受了全部文本，可以认为注入成功。
//   - 返回错误表示**不确定**是否写入成功（例如 COM 调用超时）。调用方不得
//     据此直接重发——必须走 send.go 的 Verify 确认。
type TextTarget interface {
	SetValue(ctx context.Context, text string) error
}

// InjectOptions 控制注入行为。
type InjectOptions struct {
	// KeepClipboard 为 true 时**不**恢复剪贴板原有内容，让它保持我们写入的
	// 文本。
	//
	// 这里用"保持"而不是"恢复"作为开关名，是为了让零值等于安全行为。零值
	// false 表示"恢复"，也就是默认尊重用户剪贴板；只有调用方明确写出
	// KeepClipboard: true 才会牺牲用户剪贴板。Go 选项结构体没法区分"没设置"
	// 和"设成 false"，所以让安全行为占据零值是唯一可靠的做法。
	KeepClipboard bool

	// ClipboardOpenTimeout 是等待剪贴板可用的总时长。
	ClipboardOpenTimeout time.Duration

	// ClipboardRetryInterval 是 OpenClipboard 的重试间隔。
	ClipboardRetryInterval time.Duration

	// PasteSettleDelay 是按下 Ctrl+V 之后等待目标窗口处理粘贴的时间。
	//
	// 这不是"用时间代替验证"——粘贴是否成功由 send.go 的 Verify 负责。
	// 这里等的是"让 Ctrl+V 有机会被处理"，避免我们立刻去做下一件事（比如
	// 恢复剪贴板）时和粘贴动作抢同一个剪贴板。换句话说它保护的是剪贴板的
	// 一致性，不是用来判断成功的。
	PasteSettleDelay time.Duration
}

const (
	defaultClipboardOpenTimeout   = 1500 * time.Millisecond
	defaultClipboardRetryInterval = 40 * time.Millisecond
	defaultPasteSettleDelay       = 120 * time.Millisecond
)

func (o InjectOptions) normalize() InjectOptions {
	if o.ClipboardOpenTimeout <= 0 {
		o.ClipboardOpenTimeout = defaultClipboardOpenTimeout
	}
	if o.ClipboardRetryInterval <= 0 {
		o.ClipboardRetryInterval = defaultClipboardRetryInterval
	}
	if o.PasteSettleDelay <= 0 {
		o.PasteSettleDelay = defaultPasteSettleDelay
	}
	return o
}

// restoreWanted 报告是否需要恢复剪贴板。零值 KeepClipboard=false → 需要恢复。
func (o InjectOptions) restoreWanted() bool {
	return !o.KeepClipboard
}

// Inject 把文本注入目标窗口，自动选择直设或剪贴板路径。
//
// 调用方必须已持有全局桌面锁，且已经通过 EnsureForeground 把窗口置前。
//
// 选择逻辑：给了 target 就先试直设；直设失败（或没给 target）降级到剪贴板。
// 直设失败不算错误——那正是降级路径存在的理由。
func Inject(ctx context.Context, hwnd uintptr, target TextTarget, text string, opts InjectOptions) error {
	if text == "" {
		return ErrEmptyText
	}
	if target != nil {
		if err := target.SetValue(ctx, text); err == nil {
			return nil
		}
		// 直设失败，记录后降级。不把直设错误上抛，因为最终成功与否由
		// 剪贴板路径决定；但也不吞掉——调用方可用 StrictInject 拿到它。
	}
	return InjectViaClipboard(ctx, hwnd, text, opts)
}

// StrictInject 只走直设，绝不碰剪贴板。
//
// 用于"用户明确不希望我们动剪贴板"的场景，或者调用方自己已经验证过控件支持
// ValuePattern。直设失败就直接返回错误。
func StrictInject(ctx context.Context, target TextTarget, text string) error {
	if text == "" {
		return ErrEmptyText
	}
	if target == nil {
		return errors.New("desktop: StrictInject 需要非 nil 的 TextTarget")
	}
	return target.SetValue(ctx, text)
}

// InjectViaClipboard 用剪贴板 + Ctrl+V 把文本注入窗口。
//
// 完整流程：
//
//  1. 打开剪贴板（有界重试）
//  2. 读出原有 CF_UNICODETEXT 内容并备份
//  3. **先注册恢复逻辑**（在改动剪贴板之前，理由见下）
//  4. 清空 + 写入我们的文本，成功后记下此刻序号作为基准
//  5. 重新确认窗口仍在前台，然后发送 Ctrl+V
//  6. 重新打开剪贴板，把备份写回去
//
// 第 3 步的顺序不是风格问题，是正确性问题：EmptyClipboard 与
// SetClipboardData 是两次独立调用，前者成功后者失败时剪贴板已被清空。
// 如果恢复逻辑在写入之后才注册，那条失败路径就没有兜底，用户复制的文本
// 会永久丢失。所以 defer 必须在写入之前注册。
//
// 关于第 6 步的一个重要判断：如果注入期间剪贴板序号变了（用户或别的程序改过
// 剪贴板），我们**不恢复**。因为此时恢复等于用旧内容覆盖用户刚复制的东西，
// 比不恢复更糟。这种情况不算失败，只在上层日志里记一笔。
// 基准取的是"写入之后"的序号，而不是"写入之前 +1"——EmptyClipboard 与
// SetClipboardData 各自都可能推进序号，增量并不稳定。
//
// 恢复必须在 ctx 取消后仍然执行——所以那段用的是 rawSleep 而不是 sleepCtx。
// 让用户的剪贴板内容因为我们的 ctx 超时而永久丢失，是不可接受的副作用。
func InjectViaClipboard(ctx context.Context, hwnd uintptr, text string, opts InjectOptions) error {
	if text == "" {
		return ErrEmptyText
	}
	opts = opts.normalize()

	// 记录原始内容与序号。
	orig, hadText, seqBefore, err := snapshotClipboard(ctx, opts)
	if err != nil {
		return err
	}

	// 恢复逻辑必须在"写入"之前注册。
	//
	// 反例（曾经的真实缺陷）：先 write 再 defer。若 EmptyClipboard 成功但
	// SetClipboardData 失败，用户的剪贴板已经被清空，而我们直接 return 错误，
	// 没有 defer 兜底 —— 用户复制的文本就永久丢了。所以 defer 要提前注册。
	var seqAfterWrite uint32
	wrote := false
	defer func() {
		if !opts.restoreWanted() || !hadText {
			// 原本没有文本内容：什么都不写回。原本是图片等其它格式时，我们
			// 也无法完整重建（见 README 已知局限），此时保持我们写入的文本，
			// 不做假装成功的恢复。
			return
		}
		// 判断此刻的剪贴板是否还该由我们负责：
		//   写入成功 → 序号应与写入后一致，变了说明别人动过，不恢复；
		//   写入失败 → 只要序号已经偏离写入前的值，就说明是我们清空过它，
		//             必须还原，否则用户内容就丢了。
		// 基准取"写入之后"而不是"写入之前 +1"：EmptyClipboard 与
		// SetClipboardData 各自都可能推进序号，增量并不稳定。
		cur := clipboardSequenceNumber()
		if wrote && cur != seqAfterWrite {
			return
		}
		if !wrote && cur == seqBefore {
			return
		}
		// 用 rawSleep：ctx 可能已取消，但剪贴板必须还原。
		rawSleep(uint32(opts.PasteSettleDelay / time.Millisecond))
		restoreClipboard(orig, opts)
	}()

	// 写入我们的文本。
	if err := writeClipboardWithRetry(ctx, text, opts); err != nil {
		return err
	}
	wrote = true
	seqAfterWrite = clipboardSequenceNumber()

	// 确保窗口仍是前台再发按键。这一步不能省：从注入开始到现在虽然只有
	// 几十毫秒，但如果用户恰好切走了窗口，Ctrl+V 就会落到别的窗口里。
	// 这是整个包最危险的瞬间，必须复查。
	if foregroundWindow() != hwnd {
		return &FocusError{
			Kind:   FocusNotForeground,
			HWND:   hwnd,
			Detail: "发送 Ctrl+V 前目标窗口已不在前台，已中止（避免粘贴到其他窗口）",
		}
	}

	ctrlV()

	// 给目标窗口处理粘贴的时间，然后交还给调用方去 Verify。
	// 这里睡的是"让粘贴发生"，不是"判断粘贴成功"。
	if err := sleepCtx(ctx, opts.PasteSettleDelay); err != nil {
		return err
	}
	return nil
}

// snapshotClipboard 备份剪贴板中的文本内容，并记录改动前的剪贴板序号。
//
// 返回 (内容, 是否有文本, 改动前序号, 错误)。
// 没有文本内容不是错误——剪贴板是空的时候 hadText=false。
//
// 序号在这里取而不是在调用点取，是为了让它与"读到的那份内容"来自同一段
// 时间窗口：如果先读内容、再在调用点取序号，中间可能有别的进程改过剪贴板，
// 之后失败路径的"序号是否已偏离"判断就会失准。
func snapshotClipboard(ctx context.Context, opts InjectOptions) (string, bool, uint32, error) {
	seqBefore := clipboardSequenceNumber()

	if err := openClipboardWithRetry(ctx, opts); err != nil {
		return "", false, seqBefore, err
	}
	defer closeClipboard()

	if !clipboardHasFormat(cfUnicodeText) {
		return "", false, seqBefore, nil
	}
	text, err := readClipboardText()
	if err != nil {
		// 读不出来但格式存在（比如超大文本分配失败）。按"无文本"处理：
		// 我们宁可丢失这次恢复机会，也不能因为备份失败而让注入整个失败。
		return "", false, seqBefore, nil
	}
	return text, true, seqBefore, nil
}

// restoreClipboard 把备份内容写回剪贴板。失败只影响"剪贴板被还原"这件事，
// 不产生返回值——调用点在 defer 里，此时已经无法改变主流程的成败。
func restoreClipboard(orig string, opts InjectOptions) {
	// 这里不用 ctx：恢复是收尾动作，必须尽力完成。
	for i := 0; i < 3; i++ {
		if openClipboard(0) {
			emptyClipboard()
			err := writeClipboardText(orig)
			closeClipboard()
			if err == nil {
				return
			}
		}
		rawSleep(uint32(opts.ClipboardRetryInterval / time.Millisecond))
	}
}

// writeClipboardWithRetry 清空剪贴板并写入文本。
func writeClipboardWithRetry(ctx context.Context, text string, opts InjectOptions) error {
	if err := openClipboardWithRetry(ctx, opts); err != nil {
		return err
	}
	defer closeClipboard()

	if !emptyClipboard() {
		return errors.New("desktop: EmptyClipboard 失败")
	}
	if err := writeClipboardText(text); err != nil {
		return fmt.Errorf("desktop: 写入剪贴板失败: %w", err)
	}
	return nil
}

// openClipboardWithRetry 在有界预算内反复尝试打开剪贴板。
//
// 剪贴板是全局独占资源，任何进程打开后没关（或正在处理大数据）都会让我们
// OpenClipboard 失败。正常情况几十毫秒就够，所以默认预算 1.5 秒；超过这个
// 时间基本可以断定是别的进程卡住了剪贴板，再等下去没有意义，应当报
// ErrClipboardBusy 让上层决定（通常是稍后重试整轮操作）。
func openClipboardWithRetry(ctx context.Context, opts InjectOptions) error {
	deadline := time.Now().Add(opts.ClipboardOpenTimeout)
	for {
		if openClipboard(0) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return ErrClipboardBusy
		}
		if err := sleepCtx(ctx, opts.ClipboardRetryInterval); err != nil {
			return err
		}
	}
}
