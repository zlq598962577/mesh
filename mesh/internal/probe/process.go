//go:build windows

package probe

import (
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// 本文件枚举进程，用于两件事：
//  1. InstallProbe.ProcessIDs —— §4.2.2 第 213 行要求「发送前重验绑定的进程」，
//     所以必须知道一个客户端到底占了多少进程。
//  2. DepsProbe.HelperProcesses —— 四个客户端都是 Electron/Chromium 多进程
//     （主进程 + GPU + renderer + utility），这是客观依赖，必须如实记录。
//
// 只用标准库 syscall + kernel32 的 Toolhelp 快照，不引入第三方库。

var (
	modKernel32Toolhelp = syscall.NewLazyDLL("kernel32.dll")

	procCreateToolhelp32Snapshot = modKernel32Toolhelp.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = modKernel32Toolhelp.NewProc("Process32FirstW")
	procProcess32NextW           = modKernel32Toolhelp.NewProc("Process32NextW")
)

const (
	th32csSnapProcess = 0x00000002
	invalidHandleVal  = ^uintptr(0)
)

// processEntry32W 对应 PROCESSENTRY32W。
// 字段顺序与填充必须与 Windows 头文件一致，否则读取会错位；
// 这里用显式 pad 字段而不是依赖编译器对齐推断。
type processEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	_pad0               uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [syscall.MAX_PATH]uint16
}

// ProcInfo 是一个进程的客观事实。
type ProcInfo struct {
	PID       uint32 `json:"pid"`
	ParentPID uint32 `json:"parent_pid"`
	ExeName   string `json:"exe_name"`
	ImagePath string `json:"image_path,omitempty"`
	Threads   uint32 `json:"threads"`
}

// EnumProcesses 返回当前所有进程。
// 单个进程取不到镜像路径是常态（权限不足），此时 ImagePath 留空，
// 绝不因此中断整个枚举。
func EnumProcesses() []ProcInfo {
	var out []ProcInfo

	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == invalidHandleVal {
		return nil
	}
	defer procCloseHandle.Call(snap)

	var pe processEntry32W
	pe.dwSize = uint32(unsafe.Sizeof(pe))

	r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for r != 0 {
		pi := ProcInfo{
			PID:       pe.th32ProcessID,
			ParentPID: pe.th32ParentProcessID,
			ExeName:   syscall.UTF16ToString(pe.szExeFile[:]),
			Threads:   pe.cntThreads,
		}
		// 只对有意义的进程取路径：System Idle (0) / System (4) 取不到也无所谓。
		if pi.PID > 4 {
			if p, err := ImagePathOfPID(pi.PID); err == nil {
				pi.ImagePath = p
			}
		}
		out = append(out, pi)

		pe.dwSize = uint32(unsafe.Sizeof(pe))
		r, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	return out
}

// procsUnderImage 找出所有镜像路径落在 root 目录之下的进程。
//
// 为什么按目录而不是按 exe 名匹配：Electron 客户端的所有子进程
// （renderer/gpu/utility）都用同一个 exe 名，按名字匹配会把别的程序算进来；
// 按安装目录归属才是准确的。
func procsUnderImage(all []ProcInfo, root string) []ProcInfo {
	if root == "" {
		return nil
	}
	rootClean := strings.ToLower(filepath.Clean(root))
	var out []ProcInfo
	for _, p := range all {
		if p.ImagePath == "" {
			continue
		}
		img := strings.ToLower(filepath.Clean(p.ImagePath))
		if strings.HasPrefix(img, rootClean) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// helperProcessNames 把进程列表压成「exe 名 × 数量」的可读清单，
// 便于在依赖清单里说明多进程布局，同时避免把 PID 泄漏成噪声。
func helperProcessNames(procs []ProcInfo) []string {
	counts := map[string]int{}
	var order []string
	for _, p := range procs {
		if p.ExeName == "" {
			continue
		}
		if _, seen := counts[p.ExeName]; !seen {
			order = append(order, p.ExeName)
		}
		counts[p.ExeName]++
	}
	sort.Strings(order)
	out := make([]string, 0, len(order))
	for _, n := range order {
		out = append(out, n+" ×"+itoa(counts[n]))
	}
	return out
}
