//go:build windows

package probe

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// 本文件收集探测主机自身的环境事实。
//
// 为什么要单独记录主机信息：P0a 的很多结论是「相对于这台机器的」。
// 例如「UIA 枚举不到控件」这条，如果不记录探测进程的完整性级别、
// 会话 ID、窗口站/桌面、是否可打开输入桌面，事后就无法判断
// 这到底是客户端的问题还是探测环境的问题。本机实测已经证明：
// 连资源管理器都枚举不出按钮，所以是探测侧限制。
//
// 这些字段全部来自系统 API 直接读取，不猜测。

// collectHost 汇总主机环境。
func collectHost() HostInfo {
	var h HostInfo

	h.OSBuild = osBuild()
	h.User = currentUser()

	tf := CurrentTokenFacts()
	h.Elevated = tf.Elevated
	h.IntegrityLevel = tf.IntegrityLevel

	if sid, err := CurrentSessionID(); err == nil {
		h.SessionID = sid
	}
	station, desktop := WindowStationAndDesktop()
	h.WindowStation = station
	h.Desktop = desktop
	h.InputDesktopOK = InputDesktopOpenable()

	h.MonitorCount = MonitorCount()
	h.PrimaryDPI = PrimaryDPI()
	h.DPIAware = ProcessDPIAware()

	h.DotnetRuntimes = dotnetRuntimes()

	h.TempDir = tempDir()
	h.GoVersion = runtime.Version()
	return h
}

// osBuild 读取系统版本号。
//
// 不用 GetVersionEx——它在 Win8.1 之后会对未声明兼容性的进程撒谎。
// 这里读注册表里的真实版本，属于「已知路径」读取（§9.5）。
func osBuild() string {
	v := regStringKnown(
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		"CurrentBuildNumber",
	)
	product := regStringKnown(
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		"ProductName",
	)
	display := regStringKnown(
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		"DisplayVersion",
	)
	switch {
	case v == "" && product == "":
		return "unknown"
	case display != "":
		return strings.TrimSpace(product + " " + v + " (" + display + ")")
	default:
		return strings.TrimSpace(product + " " + v)
	}
}

// currentUser 读取当前用户名。
func currentUser() string {
	for _, k := range []string{"USERDOMAIN", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			if k == "USERDOMAIN" {
				if u := os.Getenv("USERNAME"); u != "" {
					return v + `\` + u
				}
			}
			return v
		}
	}
	return "unknown"
}

// dotnetRuntimes 列出已安装的 .NET 运行时版本。
//
// 只读已知目录，不递归扫盘（§9.5）。
func dotnetRuntimes() []string {
	var out []string
	base := `C:\Program Files\dotnet\shared\Microsoft.NETCore.App`
	entries, err := os.ReadDir(base)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// tempDir 返回探针使用的临时目录，并确保它不在 C 盘（§9.1）。
//
// 方案要求临时目录固定为 F:\AIWORKSPACE\temp\mesh，不允许静默回退到 C 盘。
// 这里如实返回配置值；若不可用，返回带说明的字符串让上层报错，而不是偷偷换路径。
func tempDir() string {
	const want = `F:\AIWORKSPACE\temp\mesh`
	if err := os.MkdirAll(want, 0o755); err == nil {
		return want
	}
	// 不回退到 C 盘：把失败原因带回上层，由它决定是否终止。
	return "不可用（" + want + " 无法创建；按 §9.1 不回退到 C 盘）"
}

// tempDirUsable 判断临时目录是否真的可用。
func tempDirUsable() bool {
	return !strings.HasPrefix(tempDir(), "不可用")
}

// hostBlockers 把主机环境里会阻碍探测的事实转成阻塞项列表。
//
// 这些不是「客户端不可用」，而是「这次探测在这台机器上受限」。
// 两者必须分开记录，否则会把环境限制误报成产品缺陷。
func hostBlockers(h HostInfo) []string {
	var out []string
	if !h.InputDesktopOK {
		out = append(out, "无法打开输入桌面：锁屏或安全桌面期间不能做任何 UI 操作（§4.2.2 line 221）")
	}
	if h.IntegrityLevel != "" && h.IntegrityLevel != "Medium" && h.IntegrityLevel != "High" {
		out = append(out, "探测进程完整性级别为 "+h.IntegrityLevel+
			"，低于常规用户进程，可能影响对客户端的读取")
	}
	if !tempDirUsable() {
		out = append(out, "临时目录不可用："+h.TempDir)
	}
	return out
}
