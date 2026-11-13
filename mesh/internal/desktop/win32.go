//go:build windows

// Package desktop 提供所有桌面客户端适配器共用的底层原语：全局桌面操作锁、焦点管理、
// 文本注入与三阶段发送协议。设计依据为方案 §4.2.2 / §9.2 / §12。
//
// 全包硬性约束：
//   - 只用 Go 标准库 + syscall/unsafe 直调 Win32；不引入 golang.org/x/sys，不用 cgo，
//     必须 CGO_ENABLED=0 可编译。
//   - 所有等待都必须响应 context 超时，不允许裸 Sleep 轮询到天荒地老。
//   - 绝不关闭用户自己的窗口；本包只释放自己分配的句柄与内存。
//
// 重要环境说明：本机当前没有 Go 工具链，本包代码未经编译、未经运行测试。
// 详见同目录 README.md 的「未验证事项」一节。
package desktop

import (
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------------------
// DLL 句柄
// ---------------------------------------------------------------------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
)

// ---------------------------------------------------------------------------
// 函数声明
// ---------------------------------------------------------------------------

var (
	// 窗口查找与状态
	procFindWindowW            = user32.NewProc("FindWindowW")
	procGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procIsIconic               = user32.NewProc("IsIconic")
	procIsWindow               = user32.NewProc("IsWindow")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procGetWindowTextW         = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW   = user32.NewProc("GetWindowTextLengthW")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procShowWindow             = user32.NewProc("ShowWindow")
	procSendMessageW           = user32.NewProc("SendMessageW")

	// 焦点抢占辅助。Windows 有前台锁（foreground lock），
	// 后台进程直接 SetForegroundWindow 经常静默失败，标准绕法是把本线程
	// 附加到当前前台窗口的输入队列上再调用。见 forceForeground。
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	procAttachThreadInput  = user32.NewProc("AttachThreadInput")

	// 剪贴板
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	// 全局内存（剪贴板写入用）
	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalFree   = kernel32.NewProc("GlobalFree")

	// 键盘注入
	procKeybdEvent = user32.NewProc("keybd_event")

	// 锁屏检测
	procOpenInputDesktop          = user32.NewProc("OpenInputDesktop")
	procCloseDesktop              = user32.NewProc("CloseDesktop")
	procGetUserObjectInformationW = user32.NewProc("GetUserObjectInformationW")

	// 时间
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
	procSleep          = kernel32.NewProc("Sleep")
)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

const (
	swShow    = 5
	swRestore = 9

	wmSetText = 0x000C
	wmPaste   = 0x0302

	cfUnicodeText = 13

	gmemMoveable = 0x0002

	vkReturn  = 0x0D
	vkControl = 0x11
	vkV       = 0x56
	vkEscape  = 0x1B

	keyeventfKeyUp = 0x0002

	desktopReadObjects   = 0x0001
	desktopSwitchDesktop = 0x0080
	uoiName              = 2
)

// ---------------------------------------------------------------------------
// 最薄封装。只做调用与错误整理，不含业务判断。
// ---------------------------------------------------------------------------

// tickCount64 返回系统启动以来的毫秒数。用于测量耗时，不用于绝对时间。
func tickCount64() uint64 {
	r, _, _ := procGetTickCount64.Call()
	return uint64(r)
}

// rawSleep 硬阻塞指定毫秒。
//
// 为什么保留它：正常等待一律走 sleepCtx（响应 context），但有一处例外——
// 剪贴板恢复必须在 ctx 已经取消之后仍然完成，否则用户的剪贴板内容会被我们
// 永久吞掉。那个场景需要一个不理会 ctx 的有界延迟，就是这里。
// 除该场景外禁止直接调用 rawSleep。
func rawSleep(ms uint32) {
	procSleep.Call(uintptr(ms))
}

// foregroundWindow 返回当前前台窗口句柄，没有则返回 0。
func foregroundWindow() uintptr {
	r, _, _ := procGetForegroundWindow.Call()
	return r
}

// setForegroundWindow 直接调用 SetForegroundWindow，返回值表示系统是否同意。
func setForegroundWindow(hwnd uintptr) bool {
	r, _, _ := procSetForegroundWindow.Call(hwnd)
	return r != 0
}

// isWindow 判断句柄是否仍是一个有效窗口。
func isWindow(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(hwnd)
	return r != 0
}

// isIconic 判断窗口是否被最小化。
func isIconic(hwnd uintptr) bool {
	r, _, _ := procIsIconic.Call(hwnd)
	return r != 0
}

// isWindowVisible 判断窗口是否可见（WS_VISIBLE）。
func isWindowVisible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

// showWindow 包装 ShowWindow。
func showWindow(hwnd uintptr, cmd uint32) bool {
	r, _, _ := procShowWindow.Call(hwnd, uintptr(cmd))
	return r != 0
}

// windowText 读取窗口标题。
//
// 标题为空和句柄无效都会得到空串——两者对我们都不是致命错误，调用方用
// isWindow 另行判断句柄有效性。
func windowText(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	r, _, _ := procGetWindowTextW.Call(
		hwnd,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:r])
}

// windowPID 返回窗口所属进程 ID。
func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// findWindow 按类名 + 标题精确查找顶层窗口。两者都允许为空。
func findWindow(className, title string) (uintptr, error) {
	var cls, ttl *uint16
	if className != "" {
		p, err := syscall.UTF16PtrFromString(className)
		if err != nil {
			return 0, err
		}
		cls = p
	}
	if title != "" {
		p, err := syscall.UTF16PtrFromString(title)
		if err != nil {
			return 0, err
		}
		ttl = p
	}
	r, _, err := procFindWindowW.Call(
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(ttl)),
	)
	if r == 0 {
		return 0, err
	}
	return r, nil
}

// sendMessage 同步发送窗口消息并返回 LRESULT。
//
// 注意：SendMessage 会阻塞到目标窗口处理完消息。如果目标进程卡死，这里会
// 一直挂着，且没有超时参数可用（SendMessageTimeoutW 才有，但本包目前不声明
// 它，因为我们的用法只在已知会立即返回的控件直设路径上）。因此 sendMessage
// 只允许在持有全局桌面锁的短临界区内调用，并且调用方必须自己保证目标进程
// 是活的——focus 预检里的 isWindow 只验证窗口存在，不验证进程响应。
func sendMessage(hwnd uintptr, msg uint32, wparam, lparam uintptr) (uintptr, error) {
	r, _, err := procSendMessageW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r, err
}

// setWindowText 用 WM_SETTEXT 直设窗口文本。
//
// 这条路只对标准 Win32 编辑控件（EDIT / RichEdit 的默认行为）有效；现代
// Electron / 自绘控件会忽略它。所以它只是 inject.go 里的降级直设路径，
// 不是首选；首选是 UIA ValuePattern（由适配器层实现 TextTarget）。
func setWindowText(hwnd uintptr, text string) error {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	r, callErr := sendMessage(hwnd, wmSetText, 0, uintptr(unsafe.Pointer(p)))
	if r == 0 {
		return callErr
	}
	return nil
}

// currentThreadID 返回调用方线程 ID。
func currentThreadID() uintptr {
	r, _, _ := procGetCurrentThreadID.Call()
	return r
}

// threadIDOfWindow 返回拥有指定窗口的线程 ID。
func threadIDOfWindow(hwnd uintptr) uintptr {
	var pid uint32
	r, _, _ := procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return r
}

// attachThreadInput 把本线程的输入队列附加到目标线程，或解除附加。
func attachThreadInput(ownThread, targetThread uintptr, doAttach bool) bool {
	var v uintptr
	if doAttach {
		v = 1
	}
	r, _, _ := procAttachThreadInput.Call(ownThread, targetThread, v)
	return r != 0
}

// keyPress 用 keybd_event 产生一次按键的按下或抬起。
//
// 为什么用 keybd_event 而不是 SendInput：SendInput 需要精确构造 INPUT 结构
// （含 MOUSEINPUT 大小的联合体，amd64 上 sizeof(INPUT)=40，偏移算错会静默
// 失败）。本机没有 Go 工具链，无法编译、更无法运行验证，选择零结构体布局
// 风险的 keybd_event 更稳妥。keybd_event 在文档里被标记为"已被 SendInput
// 取代"，但它至今在所有支持的 Windows 版本上仍然可用。
// 若将来换成 SendInput，必须补上 INPUT 结构体尺寸的运行时断言（unsafe.Sizeof
// 必须等于 40），否则失败会完全静默。
func keyPress(vk uint16, up bool) {
	var flags uintptr
	if up {
		flags = keyeventfKeyUp
	}
	procKeybdEvent.Call(uintptr(vk), 0, flags, 0)
}

// ctrlV 发送一次 Ctrl+V 组合键。
func ctrlV() {
	keyPress(vkControl, false)
	keyPress(vkV, false)
	keyPress(vkV, true)
	keyPress(vkControl, true)
}

// openClipboard 尝试打开剪贴板。失败通常是因为别的进程正占着它。
func openClipboard(owner uintptr) bool {
	r, _, _ := procOpenClipboard.Call(owner)
	return r != 0
}

// closeClipboard 关闭剪贴板。
func closeClipboard() bool {
	r, _, _ := procCloseClipboard.Call()
	return r != 0
}

// emptyClipboard 清空剪贴板并把所有权交给当前打开者。
func emptyClipboard() bool {
	r, _, _ := procEmptyClipboard.Call()
	return r != 0
}

// clipboardSequenceNumber 返回剪贴板修改序号。用于判断"我们写完之后，
// 有没有别人又改了剪贴板"——如果改过，我们就不该恢复旧内容，否则会覆盖
// 用户刚复制的东西。
func clipboardSequenceNumber() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// clipboardHasFormat 判断剪贴板当前是否含指定格式。
func clipboardHasFormat(format uint32) bool {
	r, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format))
	return r != 0
}

// clipboardData 取得指定格式的剪贴板数据句柄。调用前剪贴板必须是打开状态。
func clipboardData(format uint32) uintptr {
	r, _, _ := procGetClipboardData.Call(uintptr(format))
	return r
}

// clipboardSetData 把一块全局内存交给剪贴板。
//
// 成功后内存所有权归系统，调用方绝对不能再 GlobalFree，否则剪贴板里会留下
// 悬空指针。失败时调用方必须自己 GlobalFree。
func clipboardSetData(format uint32, hMem uintptr) bool {
	r, _, _ := procSetClipboardData.Call(uintptr(format), hMem)
	return r != 0
}

// globalAlloc 分配可移动全局内存。
func globalAlloc(size int) (uintptr, error) {
	r, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(size))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

// lockGlobalMem 锁定全局内存，返回可写指针。
//
// 注意名字不能叫 globalLock：lock.go 里的 globalLock 是桌面操作锁，
// 两者同名会让本文件编译不过（锁变量不是函数）。
//
// 关于这里的 uintptr → unsafe.Pointer 转换（go vet 会报
// "possible misuse of unsafe.Pointer"，这是本包唯一保留的告警）：
//   - r 直接来自紧邻的 procGlobalLock.Call 返回值，没有做任何指针运算，
//     也没有在中间再分配内存，中间只有一次 nil 判断（判断不改变值）；
//   - 它指向的是 GlobalAlloc 分配的系统内存，不是 Go 堆对象，
//     因此不存在 GC 在转换窗口内移动对象的问题——vet 的担忧在此不成立；
//   - 唯一能消掉告警的写法是 *(*unsafe.Pointer)(unsafe.Pointer(&r))
//     这种"借变量地址重新解释"的技巧，语义完全一样，但纯粹为了糊住
//     linter，反而把"这是个句柄"的意图藏起来了，所以不采用。
//
// 结论：保留直白写法，在 README 记录该告警的理由。
func lockGlobalMem(hMem uintptr) (unsafe.Pointer, error) {
	r, _, err := procGlobalLock.Call(hMem)
	if r == 0 {
		return nil, err
	}
	return unsafe.Pointer(r), nil //nolint:govet // 理由见上：系统句柄，非 Go 堆指针
}

// globalUnlock 解锁全局内存。
func globalUnlock(hMem uintptr) {
	procGlobalUnlock.Call(hMem)
}

// globalFree 释放全局内存。
func globalFree(hMem uintptr) {
	procGlobalFree.Call(hMem)
}

// readClipboardText 读取剪贴板里的 Unicode 文本。
// 调用前剪贴板必须是打开状态，且已确认含 CF_UNICODETEXT。
func readClipboardText() (string, error) {
	h := clipboardData(cfUnicodeText)
	if h == 0 {
		return "", syscall.EINVAL
	}
	p, err := lockGlobalMem(h)
	if err != nil {
		return "", err
	}
	defer globalUnlock(h)
	return utf16PtrToString((*uint16)(p)), nil
}

// writeClipboardText 把文本写入已打开且已清空的剪贴板。
func writeClipboardText(text string) error {
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	hMem, err := globalAlloc(len(utf16) * 2)
	if err != nil {
		return err
	}
	p, err := lockGlobalMem(hMem)
	if err != nil {
		globalFree(hMem)
		return err
	}
	dst := unsafe.Slice((*uint16)(p), len(utf16))
	copy(dst, utf16)
	globalUnlock(hMem)

	if !clipboardSetData(cfUnicodeText, hMem) {
		// 交给系统失败，内存还是我们的，必须自己释放，否则泄漏。
		globalFree(hMem)
		return syscall.EINVAL
	}
	// 从这里起内存归系统所有，禁止 GlobalFree。
	return nil
}

// utf16PtrToString 把以 NUL 结尾的 UTF-16 指针转成 Go 字符串。
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var n int
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		n++
		if n > 1<<20 { // 1M 个 UTF-16 码元上限，防御异常数据导致越界扫描
			break
		}
	}
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// inputDesktopName 返回当前输入桌面的名字。
//
// 锁屏时输入桌面会变成 "Winlogon"（或其它非 "Default" 的名字）。
// 失败时返回错误——调用方必须把它当作"不能确定"处理，而不是"没锁屏"。
func inputDesktopName() (string, error) {
	h, _, err := procOpenInputDesktop.Call(
		0,
		0,
		uintptr(desktopReadObjects|desktopSwitchDesktop),
	)
	if h == 0 {
		return "", err
	}
	defer procCloseDesktop.Call(h)

	buf := make([]uint16, 256)
	var needed uint32
	r, _, callErr := procGetUserObjectInformationW.Call(
		h,
		uintptr(uoiName),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
		uintptr(unsafe.Pointer(&needed)),
	)
	if r == 0 {
		return "", callErr
	}
	return syscall.UTF16ToString(buf), nil
}
