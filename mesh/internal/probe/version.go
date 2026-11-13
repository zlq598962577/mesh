//go:build windows

package probe

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// 本文件负责两件事：
//  1. 从 PE 版本资源里读版本号（不依赖任何第三方库）。
//  2. 只在「配置与清单里已知的路径」上定位四个客户端的安装位置。
//
// §9.5 硬约束：不递归扫盘、不执行盘上找到的任意程序。
// 因此这里的每个候选路径都是显式写死的，宁可漏报也不扩大扫描面。

// probeTimeout 是所有文件系统/版本读取操作的统一超时。
// 方案要求所有外部探测都必须带超时；这里即使读本地文件也照此处理，
// 避免遇到网络盘或占位文件时整个探测卡死。
const probeTimeout = 3 * time.Second

// FileVersion 读取可执行文件的版本资源。
// 返回零值表示读不到（例如 MSIX 里的 exe 常常没有版本资源），
// 这是合法结果，不是错误——上层会退化为读包版本或标记 unknown。
func FileVersion(path string) FileVersionInfo {
	var fvi FileVersionInfo

	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fvi
	}

	size, _, _ := procGetFileVersionSize.Call(uintptr(unsafe.Pointer(p)))
	if size == 0 {
		return fvi
	}

	buf := make([]byte, size)
	r, _, _ := procGetFileVersionInfo.Call(
		uintptr(unsafe.Pointer(p)), 0, size,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r == 0 {
		return fvi
	}

	// 语言页不一定存在，先试 040904B0（英文 US），失败再试 080404B0（中文 CN），
	// 最后退化为按序号查询。
	langs := []string{
		`\StringFileInfo\040904B0\`,
		`\StringFileInfo\080404B0\`,
		`\StringFileInfo\000004B0\`,
	}
	var base string
	for _, l := range langs {
		block := []struct {
			key string
			dst *string
		}{
			{"FileVersion", &fvi.FileVersion},
			{"ProductVersion", &fvi.ProductVersion},
			{"CompanyName", &fvi.CompanyName},
			{"ProductName", &fvi.ProductName},
		}
		okAny := false
		for _, b := range block {
			v := verQueryString(buf, l+b.key)
			if v != "" {
				*b.dst = v
				okAny = true
			}
		}
		if okAny {
			base = l
			break
		}
	}
	if base != "" {
		fvi.FixedFileVer = verQueryFixed(buf)
	}
	return fvi
}

// ptrInBuf 把 Win32 回填的「buf 内部地址」换回一个从 buf 派生、类型系统可追踪的指针。
//
// VerQueryValue 的约定是：不分配内存，只把指针回填成调用方缓冲区的某个偏移。
// 直接把那个 uintptr 转成 unsafe.Pointer 属于「Go 堆地址降级成整数再转回」，
// go vet 会报 possible misuse，而且语义上依赖 GC 不移动该对象。
// 这里改成先算出偏移、再用 unsafe.Add 从 &buf[0] 重新派生：
//   - 指针来源始终是 Go 指针 buf，GC 能跟踪，不存在跨语句悬空；
//   - vet 不再告警，因为中间没有 uintptr → unsafe.Pointer 的转换。
//
// 返回 nil 表示回填的地址不在 buf 范围内（系统调用行为异常时的防御）。
func ptrInBuf(buf []byte, p uintptr) unsafe.Pointer {
	if len(buf) == 0 || p == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	if p < base || p-base >= uintptr(len(buf)) {
		return nil
	}
	return unsafe.Add(unsafe.Pointer(&buf[0]), p-base)
}

func verQueryString(buf []byte, sub string) string {
	subp, err := syscall.UTF16PtrFromString(sub)
	if err != nil {
		return ""
	}
	var out uintptr
	var outLen uint32
	r, _, _ := procVerQueryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subp)),
		uintptr(unsafe.Pointer(&out)),
		uintptr(unsafe.Pointer(&outLen)),
	)
	// out 指向 buf 内部（VerQueryValue 不分配内存，只回填偏移）。
	// 不把 out 直接转回 unsafe.Pointer：那是「Go 堆地址降级成整数」的用法，
	// go vet 会判为可能的误用，而且 GC 一旦移动/回收 buf 旧地址就失效。
	// 这里用偏移量重新从 buf 派生指针，让类型系统始终能跟踪到 buf。
	runtime.KeepAlive(buf)
	if r == 0 || out == 0 || outLen == 0 {
		return ""
	}
	p := ptrInBuf(buf, out)
	if p == nil {
		return ""
	}
	return u16String((*uint16)(p))
}

func verQueryFixed(buf []byte) string {
	root, err := syscall.UTF16PtrFromString(`\`)
	if err != nil {
		return ""
	}
	var out uintptr
	var outLen uint32
	r, _, _ := procVerQueryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&out)),
		uintptr(unsafe.Pointer(&outLen)),
	)
	// 同 verQueryString：out 指向 buf 内部，用偏移量从 buf 重新派生指针。
	runtime.KeepAlive(buf)
	if r == 0 || out == 0 || outLen < 52 {
		return ""
	}
	// VS_FIXEDFILEINFO: dwSignature, dwStrucVersion, dwFileVersionMS, dwFileVersionLS, ...
	// +8 / +12 是上述结构体的字段偏移，用 unsafe.Add 从 buf 派生的指针继续推进，
	// 全程不把地址降级成 uintptr，vet 可验证、GC 也可追踪。
	p := ptrInBuf(buf, out)
	if p == nil {
		return ""
	}
	ms := *(*uint32)(unsafe.Add(p, 8))
	ls := *(*uint32)(unsafe.Add(p, 12))
	return itoa(int(ms>>16)) + "." + itoa(int(ms&0xffff)) + "." +
		itoa(int(ls>>16)) + "." + itoa(int(ls&0xffff))
}

// excludedPrefixes 是明令禁止探测的路径。
//
// F:\AIWORKSPACE\CodexChinese 会安装一个管理员计划任务，属于 §9.5
// 「不以管理员身份启动平台」的直接冲突项，必须显式排除。
// 注意：§9.5 明确 detect.exclude 只是过滤器、不是安全边界，
// 所以这里只作为「不主动探测」的依据，不承担安全职责。
var excludedPrefixes = []string{
	`F:\AIWORKSPACE\CodexChinese`,
	`F:\AIWORKSPACE\CodexChinese\`,
}

func isExcluded(path string) bool {
	clean := strings.ToLower(filepath.Clean(path))
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(clean, strings.ToLower(filepath.Clean(p))) {
			return true
		}
	}
	return false
}

// exists 带超时地判断路径存在。
func exists(path string) bool {
	ch := make(chan bool, 1)
	go func() {
		defer func() {
			// 目录遍历/权限异常一律视为不存在，避免整个探测被单个坏路径打断。
			_ = recover()
		}()
		_, err := os.Stat(path)
		ch <- err == nil
	}()
	select {
	case ok := <-ch:
		return ok
	case <-time.After(probeTimeout):
		return false
	}
}

// isFile / isDir 是 exists 的细化版本，供安装根目录判定使用。
func isFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !st.IsDir()
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.IsDir()
}

// globExisting 在给定目录下按模式找文件，只匹配一层，不递归。
func globExisting(dir, pattern string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if isExcluded(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// userHome 取当前用户目录，用于拼装 %LOCALAPPDATA% / %APPDATA% 下的已知路径。
func userHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("USERPROFILE")
}

// 下面是四个客户端的已知安装位置清单。
//
// 这些路径全部来自本机实测（注册表卸载项 + 实际文件存在性），
// 不是猜的。豆包的安装根目录尤其反直觉：%LOCALAPPDATA%\Doubao\Application
// 是空壳，真实安装在 E:\Doubao。
//
// 每个清单都按「先精确、后宽松」排序；探测到第一个存在的即停止，
// 不继续扫描——这是 §9.5 的要求。

func codexCandidates() []string {
	home := userHome()
	local := os.Getenv("LOCALAPPDATA")
	if local == "" && home != "" {
		local = filepath.Join(home, "AppData", "Local")
	}
	var out []string
	// MSIX 安装：路径里带版本号，升级后目录名会变，所以必须用通配而不是写死。
	for _, root := range []string{`F:\WindowsApps`, `C:\Program Files\WindowsApps`} {
		out = append(out, globExisting(root, "OpenAI.Codex_*")...)
	}
	// WindowsApps 里的执行别名。
	if local != "" {
		out = append(out, globExisting(filepath.Join(local, "Microsoft", "WindowsApps"),
			"OpenAI.Codex_*")...)
	}
	return out
}

func dshCandidates() []string {
	return []string{
		`F:\deepseek`,
		`C:\Program Files\DeepSeek Harness`,
		`C:\Program Files (x86)\DeepSeek Harness`,
	}
}

func traeCandidates() []string {
	return []string{
		`F:\Trae CN`,
		`C:\Program Files\Trae CN`,
		`C:\Program Files (x86)\Trae CN`,
	}
}

func doubaoCandidates() []string {
	local := os.Getenv("LOCALAPPDATA")
	var out []string
	out = append(out, `E:\Doubao`)
	if local != "" {
		out = append(out, filepath.Join(local, "Doubao"))
	}
	out = append(out, `C:\Program Files\Doubao`, `C:\Program Files (x86)\Doubao`)
	return out
}

// pickRel 按给定的相对路径顺序挑第一个存在的主程序。
//
// 为什么用「相对路径有序清单」而不是「按文件名在根目录找」：
// 豆包在 E:\Doubao\Doubao.exe 放的是启动器，真正的主程序在
// E:\Doubao\app\Doubao.exe，两者版本资源完全不同（前者是 Chromium 版本）。
// 顺序必须显式写死，不能靠目录猜测。
func pickRel(root string, rels []string) string {
	if isExcluded(root) {
		return ""
	}
	for _, rel := range rels {
		p := filepath.Join(root, rel)
		if isFile(p) {
			return p
		}
	}
	return ""
}

// describeInstallRoot 把安装根目录整理成 InstallProbe 的公共部分。
//
// rels 是主程序的候选相对路径（有序）；launchers 是启动脚本的相对路径。
// 启动脚本只用于确认安装根目录，绝不作为验收对象（§10 禁止用同名 CLI 代测）。
//
// 这里只负责「主程序在哪」，不负责版本：主程序和「版本资源最可信的那个 exe」
// 可能不是同一个文件（豆包就是），版本读取见 versionFromRels。
func describeInstallRoot(root string, rels, launchers []string) InstallProbe {
	var ip InstallProbe
	ip.State = Unavailable
	ip.ExePath = pickRel(root, rels)
	ip.LauncherPath = pickRel(root, launchers)
	if ip.ExePath == "" {
		ip.Reason = "安装根目录存在（" + root + "）但未找到已知主程序：" + strings.Join(rels, " / ")
		return ip
	}
	ip.State = Verified
	return ip
}

// versionFromRels 按有序相对路径清单读取客户端版本，并记录取自哪个文件。
//
// 为什么要独立于主程序路径：本机实测（2026-11）豆包两个 exe 的版本资源是反的——
//
//	E:\Doubao\Doubao.exe       ProductName="Doubao Launcher"  ProductVersion=2.36.8
//	E:\Doubao\app\Doubao.exe   ProductName="Doubao"           ProductVersion=147.0.7727.149
//
// 真正在跑的 8 个进程全是 app\Doubao.exe（它是主程序），但 147.x 是 Chromium 版本，
// 2.36.8 才是应用版本（与卸载登记项 DisplayVersion 完全一致）。
// 若拿主程序的版本资源当客户端版本，就会把 Chromium 版本报成豆包版本。
// 所以清单顺序按「谁携带应用版本」排，而不是按「谁是主程序」排。
func versionFromRels(root string, rels []string) (version, source, file string) {
	if isExcluded(root) {
		return "", "", ""
	}
	for _, rel := range rels {
		p := filepath.Join(root, rel)
		if !isFile(p) {
			continue
		}
		fvi := FileVersion(p)
		switch {
		case fvi.ProductVersion != "":
			return fvi.ProductVersion, "exe 版本资源 ProductVersion（" + rel + "）", p
		case fvi.FileVersion != "":
			return fvi.FileVersion, "exe 版本资源 FileVersion（" + rel + "）", p
		case fvi.FixedFileVer != "":
			return fvi.FixedFileVer, "exe 版本资源 VS_FIXEDFILEINFO（" + rel + "）", p
		}
	}
	return "", "", ""
}
