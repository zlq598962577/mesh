//go:build windows

package probe

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// 本文件是整个探测包的 Win32 底座。硬性约束：
//   - 只用 syscall.NewLazyDLL 动态绑定，不用 cgo、不用 golang.org/x/sys，
//     保证 CGO_ENABLED=0 能编译、单文件分发不被破坏（§11 第 684 行）。
//   - 任何跨进程操作都只读取「标识与状态」，不读取凭据正文（§9.4）。
//   - 不调用需要管理员权限的 API；权限不足时返回错误，由上层降级为 unknown。

var (
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modVersion  = syscall.NewLazyDLL("version.dll")
	modShcore   = syscall.NewLazyDLL("shcore.dll")
	modAdvapi32 = syscall.NewLazyDLL("advapi32.dll")
)

var (
	procOpenProcess                 = modKernel32.NewProc("OpenProcess")
	procCloseHandle                 = modKernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW  = modKernel32.NewProc("QueryFullProcessImageNameW")
	procProcessIdToSessionId        = modKernel32.NewProc("ProcessIdToSessionId")
	procGetCurrentProcessId         = modKernel32.NewProc("GetCurrentProcessId")
	procGetCurrentThreadId          = modKernel32.NewProc("GetCurrentThreadId")
	procWTSGetActiveConsoleSessionI = modKernel32.NewProc("WTSGetActiveConsoleSessionId")
)

var (
	procEnumWindows              = modUser32.NewProc("EnumWindows")
	procGetWindowTextW           = modUser32.NewProc("GetWindowTextW")
	procGetClassNameW            = modUser32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = modUser32.NewProc("IsWindowVisible")
	procIsIconic                 = modUser32.NewProc("IsIconic")
	procGetProcessWindowStation  = modUser32.NewProc("GetProcessWindowStation")
	procGetThreadDesktop         = modUser32.NewProc("GetThreadDesktop")
	procGetUserObjectInformation = modUser32.NewProc("GetUserObjectInformationW")
	procOpenInputDesktop         = modUser32.NewProc("OpenInputDesktop")
	procCloseDesktop             = modUser32.NewProc("CloseDesktop")
	procEnumDisplayMonitors      = modUser32.NewProc("EnumDisplayMonitors")
	procGetDpiForSystem          = modUser32.NewProc("GetDpiForSystem")
	procIsProcessDPIAware        = modUser32.NewProc("IsProcessDPIAware")
	procGetForegroundWindow      = modUser32.NewProc("GetForegroundWindow")
)

var (
	procGetDpiForMonitor = modShcore.NewProc("GetDpiForMonitor")
)

var (
	procOpenProcessToken    = modAdvapi32.NewProc("OpenProcessToken")
	procGetTokenInformation = modAdvapi32.NewProc("GetTokenInformation")
	procGetSidSubAuthority  = modAdvapi32.NewProc("GetSidSubAuthority")
	procGetSidSubAuthCount  = modAdvapi32.NewProc("GetSidSubAuthorityCount")
	procGetFileVersionSize  = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfo  = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValue       = modVersion.NewProc("VerQueryValueW")
)

const (
	processQueryLimitedInformation = 0x1000
	tokenQuery                     = 0x0008
	tokenIntegrityLevel            = 25 // TOKEN_INFORMATION_CLASS.TokenIntegrityLevel
	tokenElevation                 = 20 // TOKEN_INFORMATION_CLASS.TokenElevation
)

// utoa 是 UTF-16 转换的薄封装，避免各处重复写 unsafe。
func u16String(p *uint16) string {
	if p == nil {
		return ""
	}
	return syscall.UTF16ToString((*[1 << 15]uint16)(unsafe.Pointer(p))[:])
}

func lastErr(op string) error {
	err := syscall.GetLastError()
	if err == nil || err == syscall.Errno(0) {
		return fmt.Errorf("%s 失败（未取到错误码）", op)
	}
	return fmt.Errorf("%s 失败：%v", op, err)
}

// ImagePathOfPID 通过 OpenProcess + QueryFullProcessImageNameW 取进程映像路径。
//
// 这里刻意不用 Get-Process 之类的封装：实测目标客户端的窗口 PID 在
// PowerShell 的 Get-Process 里取不到，但 P/Invoke 可以取到。
// 权限不足（ACCESS_DENIED）是合法结果，返回错误由上层记 unknown，
// 因为 §9.5 不允许为此提权。
func ImagePathOfPID(pid uint32) (string, error) {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return "", lastErr("OpenProcess(pid=" + itoa(int(pid)) + ")")
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		h, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return "", lastErr("QueryFullProcessImageNameW(pid=" + itoa(int(pid)) + ")")
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// EnumTopWindows 枚举当前桌面上的所有顶层窗口。
// 只用 EnumWindows，不做全盘/全桌面控件扫描，符合 §9.5「不递归扫盘」。
func EnumTopWindows() []WindowInfo {
	var out []WindowInfo

	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

		title := windowText(hwnd)
		class := windowClass(hwnd)
		if title == "" && class == "" {
			return 1 // 继续枚举
		}

		vis, _, _ := procIsWindowVisible.Call(hwnd)
		ico, _, _ := procIsIconic.Call(hwnd)

		out = append(out, WindowInfo{
			HWND:      uint64(hwnd),
			PID:       pid,
			Title:     title,
			Class:     class,
			Visible:   vis != 0,
			Minimized: ico != 0,
		})
		return 1
	})

	procEnumWindows.Call(cb, 0)
	return out
}

func windowText(hwnd uintptr) string {
	buf := make([]uint16, 512)
	r, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:r])
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	r, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:r])
}

// CurrentSessionID 返回本进程所在会话 id。
func CurrentSessionID() (uint32, error) {
	var sid uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(SelfPID()), uintptr(unsafe.Pointer(&sid)))
	if r == 0 {
		return 0, lastErr("ProcessIdToSessionId")
	}
	return sid, nil
}

// SessionIDOfPID 返回目标进程所在会话 id。
// 会话不同意味着 UIA 与窗口操作根本跨不过去，必须先排除这一条再谈控件可达性。
func SessionIDOfPID(pid uint32) (uint32, error) {
	var sid uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(pid), uintptr(unsafe.Pointer(&sid)))
	if r == 0 {
		return 0, lastErr("ProcessIdToSessionId(pid=" + itoa(int(pid)) + ")")
	}
	return sid, nil
}

// ActiveConsoleSessionID 返回当前物理控制台的会话 id。
func ActiveConsoleSessionID() uint32 {
	r, _, _ := procWTSGetActiveConsoleSessionI.Call()
	return uint32(r)
}

// SelfPID 返回本进程 pid。
func SelfPID() uint32 {
	r, _, _ := procGetCurrentProcessId.Call()
	return uint32(r)
}

// WindowStationAndDesktop 返回本进程所在的窗口站与桌面名。
// 若窗口站在非 WinSta0 或桌面不是 Default/input desktop，
// 那么 UIA 看不到用户桌面上的控件是环境事实，不是客户端缺陷。
func WindowStationAndDesktop() (station, desktop string) {
	station = userObjectName(0 /* UOI_NAME */, true)
	desktop = userObjectName(0, false)
	return station, desktop
}

func userObjectName(_ int, isStation bool) string {
	var h uintptr
	if isStation {
		h, _, _ = procGetProcessWindowStation.Call()
	} else {
		tid, _, _ := procGetCurrentThreadId.Call()
		h, _, _ = procGetThreadDesktop.Call(tid)
	}
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	var need uint32
	r, _, _ := procGetUserObjectInformation.Call(
		h, 2, /* UOI_NAME */
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
		uintptr(unsafe.Pointer(&need)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// InputDesktopOpenable 判断当前是否能打开输入桌面。
// 打不开通常意味着锁屏或正处于 RDP 断开状态——这是 §4.2.2 第 221 行
// 要求「停止派发并显示原因」的直接判据。
func InputDesktopOpenable() bool {
	const desktopReadObjects = 0x0001
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return false
	}
	procCloseDesktop.Call(h)
	return true
}

// MonitorCount 返回显示器数量。多屏行为必须实测，不能假设。
func MonitorCount() int {
	count := 0
	cb := syscall.NewCallback(func(_ uintptr, _ uintptr, _ uintptr, _ uintptr) uintptr {
		count++
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return count
}

// PrimaryDPI 返回系统 DPI。96 = 100% 缩放。
func PrimaryDPI() uint32 {
	r, _, _ := procGetDpiForSystem.Call()
	return uint32(r)
}

// ProcessDPIAware 返回本进程是否声明了 DPI 感知。
// 非 DPI 感知进程在非 100% 缩放下读到的坐标会被系统虚拟化，
// 这是「不许盲点坐标」的另一条理由（§11 第 667 行）。
func ProcessDPIAware() bool {
	r, _, _ := procIsProcessDPIAware.Call()
	return r != 0
}

// TokenFacts 汇总本进程的令牌客观事实。
type TokenFacts struct {
	Elevated       bool
	IntegrityLevel string
	IntegrityRID   uint32
}

// CurrentTokenFacts 读取本进程的提权状态与完整性级别。
//
// 这条信息用于自证：探测必须在与用户相同的普通权限下进行（§9.5 不以管理员启动）。
// 之前曾错误地把「UIA 拿不到控件」归因于低完整性级别，实测本进程为 Medium，
// 因此该归因不成立——这段注释保留下来，防止后续再次犯同样的误判。
func CurrentTokenFacts() TokenFacts {
	var tf TokenFacts

	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(SelfPID()))
	if h == 0 {
		return tf
	}
	defer procCloseHandle.Call(h)

	var token uintptr
	r, _, _ := procOpenProcessToken.Call(h, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return tf
	}
	defer procCloseHandle.Call(token)

	// TokenElevation -> TOKEN_ELEVATION{DWORD TokenIsElevated}
	var elev [4]byte
	var retLen uint32
	r, _, _ = procGetTokenInformation.Call(
		token, tokenElevation,
		uintptr(unsafe.Pointer(&elev[0])), 4,
		uintptr(unsafe.Pointer(&retLen)),
	)
	if r != 0 {
		tf.Elevated = *(*uint32)(unsafe.Pointer(&elev[0])) != 0
	}

	// TokenIntegrityLevel -> TOKEN_MANDATORY_LABEL{SID_AND_ATTRIBUTES{PSID, DWORD}}
	buf := make([]byte, 64)
	r, _, _ = procGetTokenInformation.Call(
		token, tokenIntegrityLevel,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&retLen)),
	)
	// GetTokenInformation 把 SID 直接写进调用方缓冲区 buf，
	// 回填的 PSID 指向 buf 内部而不是独立分配的内存。
	// 所以后面两个 GetSidSubAuthority* 返回的指针也落在 buf 内：
	// 它们看着像「系统调用返回的原生地址」，实际指向 Go 堆，属于必须从 buf
	// 重新派生的一类。直接用 uintptr → unsafe.Pointer 会让 vet 报警，
	// 且语义上依赖 GC 不移动该对象，这里统一走 ptrInBuf 换算偏移。
	if r == 0 {
		return tf
	}
	// sid 是 buf 内部的 PSID（指向 SID 结构体首字节）。
	sid := *(*uintptr)(unsafe.Pointer(&buf[0]))
	if sid == 0 {
		return tf
	}
	cntPtr, _, _ := procGetSidSubAuthCount.Call(sid)
	if cntPtr == 0 {
		return tf
	}
	cntP := ptrInBuf(buf, cntPtr)
	if cntP == nil {
		return tf
	}
	cnt := *(*uint8)(cntP)
	if cnt == 0 {
		return tf
	}
	subPtr, _, _ := procGetSidSubAuthority.Call(sid, uintptr(cnt-1))
	if subPtr == 0 {
		return tf
	}
	subP := ptrInBuf(buf, subPtr)
	if subP == nil {
		return tf
	}
	tf.IntegrityRID = *(*uint32)(subP)
	// buf 全程由 sid/cntP/subP 派生自同一底层数组，KeepAlive 让 GC 明确
	// 不能在这一段之前回收它（ptrInBuf 已保证指针来源可追踪，这里再兜一层）。
	runtime.KeepAlive(buf)
	switch {
	case tf.IntegrityRID >= 0x4000:
		tf.IntegrityLevel = "System"
	case tf.IntegrityRID >= 0x3000:
		tf.IntegrityLevel = "High"
	case tf.IntegrityRID >= 0x2000:
		tf.IntegrityLevel = "Medium"
	case tf.IntegrityRID >= 0x1000:
		tf.IntegrityLevel = "Low"
	default:
		tf.IntegrityLevel = "Untrusted"
	}
	return tf
}

// foregroundHWND 返回当前前台窗口句柄，没有则返回 0。
//
// 只读，不改变焦点。探针绝不主动抢焦点（§4.2.2 line 215）。
func foregroundHWND() uint64 {
	h, _, _ := procGetForegroundWindow.Call()
	return uint64(h)
}

// canOpenProcessToken 判断能否打开指定进程的令牌。//
// 用途仅限「记录现象」：打不开通常意味着对方以更高权限或受保护方式运行，
// 这会影响 UIA 读取（UIPI）。但不能反过来断定对方是管理员，
// 所以调用方只能据此提示风险，不能据此下结论。
func canOpenProcessToken(pid uint32) bool {
	if pid <= 4 {
		return false
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)

	var token uintptr
	r, _, _ := procOpenProcessToken.Call(h, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return false
	}
	procCloseHandle.Call(token)
	return true
}

// itoa 避免为了一个整数转换到处引入 strconv。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
