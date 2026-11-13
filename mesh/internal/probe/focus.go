//go:build windows

package probe

// 本文件回答 P0a 的第 5 个问题：焦点与锁屏。
//
// 需要分别验证的条件（§4.2.2 line 221 要求逐项单独测）：
//   最小化、锁屏、多显示器、DPI 变化、焦点抢占。
//
// 为什么必须分开：这五种情况下的失败原因完全不同，处理动作也不同。
//   - 最小化：窗口还在，但渲染可能暂停，控件可能不在树上 → 可恢复
//   - 锁屏：整个输入桌面不可用，任何 UI 操作都不该继续 → 暂停派发
//   - 多显示器：窗口可能在另一块屏上，坐标与焦点归属变化 → 需重新定位
//   - DPI 变化：坐标与截图缩放比例改变，点击会偏 → 必须重算
//   - 焦点抢占：会打断用户正在做的事 → §4.2.2 line 215 明确禁止
//
// 探针的职责是把这些条件「测出来并如实描述」，而不是承诺「我们能处理」。
// 方案 §12 line 697 明确：能否不抢焦点、最小化运行，由 P0a 实测决定，
// 不提前承诺。所以这里只报现象，不下保证。

// probeFocus 汇总焦点相关的条件探测。
func probeFocus(host HostInfo, w WindowInfo, hasWindow bool) FocusProbe {
	var fp FocusProbe
	fp.State = Unavailable

	// ---- 最小化 ----
	fp.Minimized = probeMinimized(w, hasWindow)

	// ---- 锁屏 ----
	fp.LockedScreen = probeLocked(host)

	// ---- 多显示器 ----
	fp.MultiMonitor = probeMultiMonitor(host)

	// ---- DPI ----
	fp.DPIChange = probeDPI(host)

	// ---- 焦点抢占 ----
	fp.FocusSteal = probeFocusSteal(w, hasWindow)

	// 汇总状态：任一项实测到「当前不满足可操作条件」就整体标 unverified，
	// 因为在这些条件下不能声称链路可用。
	all := []ConditionProbe{
		fp.Minimized, fp.LockedScreen, fp.MultiMonitor,
		fp.DPIChange, fp.FocusSteal,
	}
	unavailable := 0
	for _, c := range all {
		if c.State == Unavailable {
			unavailable++
		}
	}
	switch {
	case unavailable == len(all):
		fp.State = Unavailable
		fp.Reason = "五项焦点条件均无法探测"
	case unavailable > 0:
		fp.State = Unverified
		fp.Reason = "部分条件无法探测（" + itoa(unavailable) + "/" + itoa(len(all)) + "），" +
			"不能据此声称这些条件下可用"
	default:
		fp.State = Unverified
		fp.Reason = "五项条件均已观测，但「能否在这些条件下稳定操作」需真实收发验证；" +
			"方案 §12 line 697 要求由实测决定，不提前承诺"
	}
	return fp
}

// probeMinimized 观测窗口当前是否最小化。
//
// 只报「现在是最小化/不是」，不报「最小化时还能不能操作」——
// 后者需要真的在最小化状态下发一条消息，探针不做这件事。
func probeMinimized(w WindowInfo, hasWindow bool) ConditionProbe {
	if !hasWindow {
		return ConditionProbe{
			State:  Unavailable,
			Reason: "未找到该客户端主窗口，无法观测最小化状态",
		}
	}
	if w.Minimized {
		return ConditionProbe{
			State:    Unverified,
			Observed: "当前处于最小化状态",
			Behavior: "Electron 窗口最小化后渲染可能暂停，控件树可能不可读；" +
				"能否在最小化下发送未验证，不得承诺后台无窗口运行（§4.2.2 line 221）",
			Reason: "观测到最小化，但未在最小化状态下实测收发",
		}
	}
	return ConditionProbe{
		State:    Unverified,
		Observed: "当前未最小化",
		Behavior: "最小化后的行为需把窗口最小化再测，探针不改变用户窗口状态",
		Reason:   "仅在当前（非最小化）状态下观测，最小化场景未验证",
	}
}

// probeLocked 判断当前是否处于锁屏/安全桌面。
//
// 判据：能否打开输入桌面（OpenInputDesktop）。锁屏时输入桌面被安全桌面
// 占据，普通进程打不开。这是最可靠的程序化判据，不依赖截图。
func probeLocked(host HostInfo) ConditionProbe {
	if host.InputDesktopOK {
		return ConditionProbe{
			State:    Unverified,
			Observed: "输入桌面可打开（当前未锁屏或已解锁）",
			Behavior: "锁屏期间不得继续 UI 操作；恢复后必须先核对会话再继续（§10.1 line 644）",
			Reason:   "当前未锁屏，锁屏场景本身未实测",
		}
	}
	return ConditionProbe{
		State:    Verified,
		Observed: "输入桌面不可打开（疑似锁屏或处于安全桌面）",
		Behavior: "此状态下停止派发并显示原因，绝不盲点坐标（§4.2.2 line 221）",
		Reason:   "已通过 OpenInputDesktop 失败确认输入桌面不可用",
	}
}

// probeMultiMonitor 观测显示器数量。
func probeMultiMonitor(host HostInfo) ConditionProbe {
	if host.MonitorCount <= 0 {
		return ConditionProbe{
			State:  Unavailable,
			Reason: "EnumDisplayMonitors 未返回任何显示器",
		}
	}
	if host.MonitorCount == 1 {
		return ConditionProbe{
			State:    Unverified,
			Observed: "单显示器（1 块，主屏 DPI=" + u32str(host.PrimaryDPI) + "）",
			Behavior: "多显示器场景无法在本机复现，跨屏定位与焦点归属未验证",
			Reason:   "本机仅 1 块显示器，多屏场景需在双屏机器上另测",
		}
	}
	return ConditionProbe{
		State:    Unverified,
		Observed: "多显示器（" + itoa(host.MonitorCount) + " 块）",
		Behavior: "需按显示器分别计算窗口位置与 DPI；跨屏场景未做真实收发验证",
		Reason:   "已观测到多屏，但跨屏操作未验证",
	}
}

// probeDPI 观测 DPI 相关事实。
func probeDPI(host HostInfo) ConditionProbe {
	if host.PrimaryDPI == 0 {
		return ConditionProbe{
			State:  Unavailable,
			Reason: "无法读取系统 DPI",
		}
	}
	aware := "进程未声明 DPI 感知（坐标会被系统缩放，点击位置需换算）"
	if host.DPIAware {
		aware = "进程已声明 DPI 感知（坐标为物理像素）"
	}
	return ConditionProbe{
		State:    Unverified,
		Observed: "系统 DPI=" + u32str(host.PrimaryDPI) + "；" + aware,
		Behavior: "DPI 变化（拖到不同缩放比例的显示器、用户改缩放）会使既有坐标失效，" +
			"必须在每次操作前重新计算（§11 line 667 禁止盲点坐标）",
		Reason: "已观测当前 DPI，但 DPI 动态变化的场景未实测",
	}
}

// probeFocusSteal 观测焦点抢占的现状。
//
// 探针绝不主动抢焦点（§4.2.2 line 215），所以这里只能报「窗口当前是否在前台」
// 这一客观事实，不能报「我们能否在不抢焦点的情况下操作」。
func probeFocusSteal(w WindowInfo, hasWindow bool) ConditionProbe {
	if !hasWindow {
		return ConditionProbe{
			State:  Unavailable,
			Reason: "未找到该客户端主窗口，无法观测焦点状态",
		}
	}
	if foregroundHWND() == w.HWND {
		return ConditionProbe{
			State:    Unverified,
			Observed: "该客户端窗口当前在前台",
			Behavior: "前台时可直接操作，但用户可能正在使用；必须先检查用户是否在输入（§4.2.2 line 215）",
			Reason:   "观测到前台状态，但「用户是否正在使用」未判定",
		}
	}
	return ConditionProbe{
		State:    Unverified,
		Observed: "该客户端窗口当前不在前台",
		Behavior: "不在前台时若要用键盘输入必须抢焦点，会打断用户；" +
			"§4.2.2 line 215 要求检测到用户冲突就暂停并保留草稿，不抢焦点",
		Reason: "观测到非前台状态；「能否不抢焦点完成输入」未验证",
	}
}

// u32str 把 uint32 转成十进制字符串。
func u32str(v uint32) string {
	return itoa(int(v))
}
