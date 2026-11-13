//go:build windows

package probe

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本文件回答 P0a 的第 6 个问题：依赖清单。
//
// 需要弄清三件事：
//   1. 是否需要管理员权限（§9.5：绝不自行提权启动平台）
//   2. 有哪些辅助进程（四个客户端都是 Electron/Chromium 多进程）
//   3. 依赖哪些运行时（.NET / VC++ / WebView2）
//
// 判定原则：能从文件系统和进程表直接看到的就是实测；
// 需要实际运行才能知道的（比如某功能是否真的需要管理员）标 unverified。
// 不做「看到 exe 有 manifest 就断言需要管理员」这种推断。

// probeDeps 探测依赖情况。
func probeDeps(ip InstallProbe, procs []ProcInfo, host HostInfo) DepsProbe {
	var dp DepsProbe
	dp.State = Unverified

	if ip.State == Unavailable {
		dp.State = Unavailable
		dp.Reason = "客户端未定位到安装或未运行，依赖清单无法探测"
		return dp
	}

	// ---- 辅助进程 ----
	//
	// 按安装目录归属统计，而不是按 exe 名——Electron 的渲染/GPU/utility
	// 子进程共用同一个 exe 名，按名字统计会漏。
	root := filepath.Dir(ip.ExePath)
	if ip.ExePath != "" && root != "." {
		owned := procsUnderImage(procs, root)
		dp.HelperProcesses = helperProcessNames(owned)
	}
	if len(dp.HelperProcesses) == 0 {
		dp.HelperProcesses = nil
	}

	// ---- 管理员权限 ----
	//
	// 只能给出「当前进程不是管理员」这一事实（host.Elevated），
	// 以及「客户端进程本身是否以更高权限运行」的观察。
	// 客户端是否要求管理员启动，需要实际以普通权限启动一次才能确认，
	// 探针不重启用户的客户端，所以标 unverified。
	dp.AdminRequired = false
	notes := []string{
		"当前探针进程 Elevated=" + boolStr(host.Elevated) + "，完整性级别=" + host.IntegrityLevel,
		"§9.5 要求绝不以管理员身份启动平台，故未尝试提权验证客户端是否需要管理员",
	}

	// 若客户端进程的 token 打不开，通常说明它以更高权限运行，
	// 这会影响 UIA 读取（UIPI）。这在本机是实测到的现象。
	if elevatedHint, ok := clientElevationHint(ip.ProcessIDs); ok {
		notes = append(notes, elevatedHint)
		if strings.Contains(elevatedHint, "更高权限") {
			dp.AdminRequired = true
		}
	}

	// ---- 运行时依赖 ----
	dp.RuntimeDeps = detectRuntimeDeps(ip.ExePath, host)

	dp.Notes = notes
	dp.State = Unverified
	dp.Reason = "辅助进程与运行时依赖已按文件系统/进程表实测；" +
		"「客户端是否必须管理员启动」需实际以普通权限启动才能确认，未验证"
	return dp
}

// clientElevationHint 检查能否打开客户端进程的 token。
//
// 打不开（ACCESS_DENIED）不等于对方是管理员——也可能是保护进程。
// 但这是唯一能在不提权前提下获得的线索，所以如实记录现象，措辞谨慎。
func clientElevationHint(pids []uint32) (string, bool) {
	if len(pids) == 0 {
		return "", false
	}
	denied := 0
	checked := 0
	for _, pid := range pids {
		if pid <= 4 {
			continue
		}
		checked++
		if !canOpenProcessToken(pid) {
			denied++
		}
	}
	if checked == 0 {
		return "", false
	}
	if denied == 0 {
		return "全部 " + itoa(checked) + " 个客户端进程的 token 均可读取（同权限级别）", true
	}
	return itoa(denied) + "/" + itoa(checked) + " 个客户端进程的 token 打不开（ACCESS_DENIED）；" +
		"通常表示该进程以更高权限或受保护方式运行，会影响 UIA 读取（UIPI）", true
}

// detectRuntimeDeps 检查常见运行时是否安装。
//
// 只查已知路径，不扫盘（§9.5）。
func detectRuntimeDeps(exePath string, host HostInfo) []string {
	var deps []string

	// .NET：本机实测存在的版本由 host.DotnetRuntimes 提供。
	if len(host.DotnetRuntimes) > 0 {
		deps = append(deps, ".NET 运行时："+strings.Join(host.DotnetRuntimes, ", "))
	} else {
		deps = append(deps, ".NET 运行时：未在 C:\\Program Files\\dotnet 下发现")
	}

	// .NET Framework
	for _, p := range []string{
		`C:\Windows\Microsoft.NET\Framework64\v4.0.30319`,
		`C:\Windows\Microsoft.NET\Framework\v4.0.30319`,
	} {
		if isDir(p) {
			deps = append(deps, ".NET Framework 4.x："+p)
			break
		}
	}

	// WebView2（Electron 不用，但部分客户端的内嵌页面用）
	for _, p := range []string{
		`C:\Program Files (x86)\Microsoft\EdgeWebView\Application`,
		`C:\Program Files\Microsoft\EdgeWebView\Application`,
	} {
		if isDir(p) {
			entries, err := os.ReadDir(p)
			if err == nil && len(entries) > 0 {
				var vers []string
				for _, e := range entries {
					if e.IsDir() {
						vers = append(vers, e.Name())
					}
				}
				if len(vers) > 0 {
					sort.Strings(vers)
					deps = append(deps, "WebView2 Runtime："+strings.Join(vers, ", "))
				}
			}
			break
		}
	}

	// VC++ 运行库
	for _, p := range []string{
		`C:\Windows\System32\vcruntime140.dll`,
		`C:\Windows\System32\msvcp140.dll`,
	} {
		if isFile(p) {
			deps = append(deps, "VC++ 运行库："+filepath.Base(p)+" 存在")
		}
	}

	// Electron 自带的 Chromium 版本（从 exe 版本资源取，已实测豆包 exe 报 147.0.7727.149）
	if exePath != "" {
		if fv := FileVersion(exePath); fv.ProductVersion != "" {
			deps = append(deps, "主程序 ProductVersion："+fv.ProductVersion+
				"（Electron 客户端此处常为 Chromium 版本，不等于应用版本）")
		}
	}

	return deps
}

// boolStr 把 bool 转成中文可读值。
func boolStr(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
