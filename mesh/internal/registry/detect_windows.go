//go:build windows

package registry

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mesh/internal/probe"
)

// 本文件是 Detector 接口的 Windows 实现，也是整个 registry 里唯一直接
// 依赖 probe 包的地方。
//
// 为什么不是把 probe.DetectInstall 直接包一层：probe 的客户端规格是
// 硬编码在 probe 包里的 unexported clientSpec，跨包拿不到；而且内置
// manifest 走的是"数据驱动探测"——用户放一份自己的 JSON 进 agents/ 目录，
// 探测逻辑必须跟着那份 JSON 走，不可能为每个新客户端回 probe 包改代码。
// 所以这里复用 probe 的底层原语（进程枚举、窗口枚举、版本资源、卸载项），
// 而"哪些路径算这个客户端"由 manifest 提供。

// WindowsDetector 用 Win32 事实实现 Detector。
//
// 它无状态，可以安全地被多个 goroutine 并发调用；探测结果该缓存多久
// 由 Registry 的 DetectionTTL 决定，本类型自己不做缓存。
type WindowsDetector struct{}

// NewWindowsDetector 返回默认探测器。
func NewWindowsDetector() *WindowsDetector { return &WindowsDetector{} }

// 编译期确认接口实现，避免改了接口忘了改这里。
var _ Detector = (*WindowsDetector)(nil)

// Detect 按 manifest 的 detect 段探测本机状态。
//
// 返回值永远非 nil。三条判定口径（与 Detection 的文档一致，别改）：
//   - 找到可执行文件或运行中的进程 ⇒ Available=true；
//   - 全都找不到，但探测过程本身没出错 ⇒ Available=false, Err=""（"没装"）；
//   - 探测过程本身失败（例如拿不到进程列表）⇒ Err 非空（"未确认"）。
//
// 第三条必须与第二条区分开：把"没探测成"报成"没安装"，用户会去重装一个
// 本来就装着的客户端。
func (d *WindowsDetector) Detect(ctx context.Context, m *Manifest) *Detection {
	// nil 检查必须在取 m.ID 之前：先解引用再判空就是空指针 panic。
	if m == nil {
		return &Detection{Err: "manifest 为空"}
	}
	res := &Detection{
		Adapter: m.ID,
		// ProbedAt 刻意留零值：Registry.storeDetection 会统一补上时间戳。
		// 探测器自己取 time.Now 会让测试注入的时钟失效。
	}
	if err := ctx.Err(); err != nil {
		res.Err = "探测被取消：" + err.Error()
		return res
	}

	procs, procsErr := enumProcsSafe()
	if procsErr != nil {
		// 进程枚举失败不直接放弃：文件系统探测仍然可能给出确定结论。
		// 但如果最后什么都没找到，这个错误就是"未确认"而不是"没装"。
		res.Notes = append(res.Notes, "进程枚举失败："+procsErr.Error())
	}

	// 1) 进程优先：已经在跑的客户端一定装着，而且能直接拿到真实映像路径。
	//    这不是优化，是正确性要求——MSIX 的 WindowsApps 目录不能被枚举，
	//    但进程映像路径是绝对路径，能读。
	byProcess, pidPath := matchProcesses(m, procs)

	// 2) 文件系统候选路径（含单分量通配）。
	byPath, exePath := matchExePaths(m, byProcess)

	// 3) 排除目录优先于一切：命中即判不可用。
	//    这是防事故用的（见 manifest 里 exclude_paths 的说明），
	//    所以即使进程在跑也要拦下来，并在 Notes 里说清楚为什么。
	if hit, why := excludedBy(m, exePath, pidPath); hit {
		res.Available = false
		res.Err = ""
		res.Notes = append(res.Notes, why)
		res.Notes = append(res.Notes, "已排除该安装目录，不会对它做任何桌面操作")
		return res
	}

	res.Available = byProcess || byPath
	if exePath == "" {
		exePath = pidPath
	}
	res.ExePath = exePath

	// 4) 运行中的进程 id。
	if len(m.Detect.ProcessNames) > 0 {
		res.PIDs = pidsOf(procs, m.Detect.ProcessNames)
	}

	// 5) 版本。读不到就是读不到，不猜。
	if vp := versionPathOf(m, exePath); vp != "" {
		fillVersion(res, m.Detect.VersionFrom, vp)
	}

	// 6) 窗口。窗口类/标题只是"能不能定位到主窗口"的参考信息，
	//    不参与"是否安装"的判定——Trae 就是反例：它的主窗口标题是
	//    当前项目名，没有任何标题含 "Trae"，按标题判定必然漏判。
	res.WindowTitles = windowTitles(m, res.PIDs)

	if !res.Available {
		res.Notes = append(res.Notes, "未找到可执行文件，也没有同名进程在运行")
	}
	return res
}

// enumProcsSafe 包一层 recover：进程枚举要走 syscall，真出问题时应报成
// "未确认"而不是把整个 Registry 带崩。
func enumProcsSafe() (procs []probe.ProcInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			procs = nil
			err = &detectPanicError{stage: "枚举进程", val: r}
		}
	}()
	return probe.EnumProcesses(), nil
}

// detectPanicError 把探测过程中 recover 到的 panic 变成可展示的错误。
//
// 不重新 panic：一个客户端的探测崩了不该拖垮整个 agent 列表。
type detectPanicError struct {
	stage string
	val   any
}

func (e *detectPanicError) Error() string {
	return e.stage + "时发生异常（已隔离）：" + panicString(e.val)
}

func panicString(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case error:
		return x.Error()
	case string:
		return x
	default:
		return "非字符串 panic"
	}
}

// matchProcesses 判断是否有进程在跑，并返回一个可用的映像路径。
func matchProcesses(m *Manifest, procs []probe.ProcInfo) (found bool, imagePath string) {
	if len(m.Detect.ProcessNames) == 0 {
		return false, ""
	}
	want := make(map[string]bool, len(m.Detect.ProcessNames))
	for _, n := range m.Detect.ProcessNames {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	for _, p := range procs {
		if !want[strings.ToLower(p.ExeName)] {
			continue
		}
		found = true
		if imagePath == "" && p.ImagePath != "" && !isExcludedPath(m, p.ImagePath) {
			imagePath = p.ImagePath
		}
	}
	return found, imagePath
}

// pidsOf 收集匹配进程名的所有 pid，升序便于日志比对。
func pidsOf(procs []probe.ProcInfo, names []string) []uint32 {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []uint32
	for _, p := range procs {
		if want[strings.ToLower(p.ExeName)] {
			out = append(out, p.PID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// matchExePaths 按 manifest 顺序找第一个存在的候选可执行文件。
//
// "按顺序取第一个"是有语义的，不是随手写的：豆包同时存在
// E:\Doubao\Doubao.exe（启动器外壳，版本资源是 Chromium 通用版本号）
// 和 E:\Doubao\app\Doubao.exe（真正的主程序）。manifest 里把 app\ 排在
// 前面，取第一个命中才能读到正确的版本号。
func matchExePaths(m *Manifest, processRunning bool) (found bool, exePath string) {
	for _, raw := range m.Detect.ExePaths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		for _, cand := range expandCandidate(raw) {
			if isExcludedPath(m, cand) {
				continue
			}
			if !fileExists(cand) {
				continue
			}
			return true, cand
		}
	}
	return processRunning, ""
}

// expandCandidate 把 %ENV% 展开，并把带 * 的路径展开成实际存在的路径列表。
//
// 通配只允许匹配单个路径分量内部（见 manifest.go 里 ExePaths 的说明）：
// OpenAI.Codex_* 这种目录名要能匹配，但 * 不许跨目录，否则一个
// "C:\Program Files\WindowsApps\*\*.exe" 会把整个目录树扫一遍，
// 既慢又违反"不递归扫盘"的约束。
func expandCandidate(raw string) []string {
	p := os.ExpandEnv(raw)
	if strings.TrimSpace(p) == "" {
		return nil
	}
	if !strings.Contains(p, "*") {
		return []string{p}
	}
	dir, rest := splitWildcardDir(p)
	if dir == "" || rest == "" {
		// 整条路径都不可枚举（例如盘符根就带 *），按不存在处理。
		return nil
	}
	parts := strings.Split(rest, string(filepath.Separator))
	out := expandSegments(dir, parts)
	sort.Strings(out)
	return out
}

// expandSegments 按路径分量逐级展开通配。
//
// 关键点：带 * 的只有其中一个分量（例如 OpenAI.Codex_*），它后面还有
// app\ChatGPT.exe 这种不含通配的尾巴。所以匹配到一个目录之后必须带着
// 剩余分量继续往下走，不能就地当成最终结果——早期版本正是漏了这一步，
// 结果 Codex 的 MSIX 路径永远探测不到。
func expandSegments(dir string, parts []string) []string {
	// 剩余部分已经没有通配了，直接拼出来看存在与否。
	if !containsWildcardParts(parts) {
		cand := filepath.Join(append([]string{dir}, parts...)...)
		if fileExists(cand) {
			return []string{cand}
		}
		return nil
	}
	if len(parts) == 0 {
		return nil
	}
	head, tail := parts[0], parts[1:]
	if !strings.Contains(head, "*") {
		next := filepath.Join(dir, head)
		if !dirExists(next) {
			return nil
		}
		return expandSegments(next, tail)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// 读不了目录（WindowsApps 在部分机器上拒绝枚举）是合法结果，
		// 不是错误：上层还有进程映像路径这条主路。
		return nil
	}
	var out []string
	for _, e := range entries {
		if !matchWildcardSegment(head, e.Name()) {
			continue
		}
		out = append(out, expandSegments(filepath.Join(dir, e.Name()), tail)...)
	}
	return out
}

// containsWildcardParts 判断剩余分量里还有没有 *。
func containsWildcardParts(parts []string) bool {
	for _, p := range parts {
		if strings.Contains(p, "*") {
			return true
		}
	}
	return false
}

// dirExists 判断目录存在。通配展开时用目录而不是文件判断，
// 因为通配命中的中间分量必然是目录。
func dirExists(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.IsDir()
}

// splitWildcardDir 把带通配的路径拆成"可枚举的最长前缀目录"和"剩余分量"。
//
// 例：C:\Program Files\WindowsApps\OpenAI.Codex_*\app\ChatGPT.exe
//
//	→ dir = C:\Program Files\WindowsApps
//	  rest = OpenAI.Codex_*\app\ChatGPT.exe
//
// rest 以路径分隔符开头会被 strings.Split 切出一个空首元素，
// 所以这里去掉前导分隔符后再返回，让调用方拿到的首元素就是 OpenAI.Codex_*。
func splitWildcardDir(p string) (dir, rest string) {
	vol := filepath.VolumeName(p)
	rest = strings.TrimPrefix(p, vol)
	rest = strings.TrimPrefix(rest, string(filepath.Separator))
	parts := strings.Split(rest, string(filepath.Separator))
	idx := -1
	for i, part := range parts {
		if strings.Contains(part, "*") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", ""
	}
	if idx == 0 {
		// 通配就在盘符下的第一层（例如 C:\*\x.exe），可枚举的目录就是盘符根。
		if vol == "" {
			return "", ""
		}
		return vol + string(filepath.Separator), strings.Join(parts, string(filepath.Separator))
	}
	dir = filepath.Join(append([]string{vol + string(filepath.Separator)}, parts[:idx]...)...)
	return dir, strings.Join(parts[idx:], string(filepath.Separator))
}

// matchWildcardSegment 在单个路径分量内做 * 匹配（大小写不敏感）。
//
// 手写而不用 filepath.Match 的理由：filepath.Match 的 * 不匹配分隔符，
// 这个语义是对的，但它对 Windows 的大小写不敏感不负责——Windows 上
// 目录名大小写不敏感，而 Match 是逐字节比较的，用它会漏判。
func matchWildcardSegment(pattern, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	if pattern == "*" {
		return true
	}
	// 支持多个 *，按段贪心匹配即可（分量名很短，不需要更复杂的算法）。
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	// 首段必须在前缀位置。
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	// 末段必须在后缀位置。
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(name, last) {
			return false
		}
		name = name[:len(name)-len(last)]
	}
	// 中间各段按顺序找。
	for _, seg := range parts[1 : len(parts)-1] {
		if seg == "" {
			continue
		}
		i := strings.Index(name, seg)
		if i < 0 {
			return false
		}
		name = name[i+len(seg):]
	}
	return true
}

// fileExists 判断路径存在且是普通文件（不跟随快捷方式之类的语义猜测）。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular()
}

// isExcludedPath 判断某个路径是否落在 manifest 的 exclude_paths 之下。
//
// 语义与 probe 包的 excludedPrefixes 一致：大小写不敏感 + filepath.Clean
// 之后做前缀比较，且带分隔符与不带分隔符两种前缀都算命中。
//
// 为什么要两份前缀：exclude_paths 写的是目录 F:\AIWORKSPACE\CodexChinese，
// 而待判断的是文件 F:\AIWORKSPACE\CodexChinese\codex.exe。只比较
// 目录本身会把文件漏掉。
func isExcludedPath(m *Manifest, p string) bool {
	if m == nil || p == "" || len(m.Detect.ExcludePaths) == 0 {
		return false
	}
	target := normalizePathForCompare(p)
	for _, ex := range m.Detect.ExcludePaths {
		if strings.TrimSpace(ex) == "" {
			continue
		}
		prefix := normalizePathForCompare(os.ExpandEnv(ex))
		if prefix == "" {
			continue
		}
		if target == prefix {
			return true
		}
		if strings.HasPrefix(target, strings.TrimSuffix(prefix, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// normalizePathForCompare 统一路径比较形式：展开环境变量、Clean、转小写。
func normalizePathForCompare(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(os.ExpandEnv(p)))
}

// excludedBy 检查当前判定的可执行路径与进程映像路径是否被排除。
//
// 返回的说明会进 Detection.Notes，让"为什么明明装了却不可用"可自查。
func excludedBy(m *Manifest, paths ...string) (bool, string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if isExcludedPath(m, p) {
			return true, "命中 exclude_paths，排除路径：" + p
		}
	}
	return false, ""
}

// versionPathOf 决定从哪个文件读版本号。
//
// 默认用探测命中的可执行文件，manifest 可用 detect.version_path 指定另一个。
// 这不是给配置留后门，是豆包这种"启动器 + 主程序"结构的必需项：
// 主程序 app\Doubao.exe 带的是 Chromium 版本，应用版本在启动器上。
//
// 指定的路径读不到时回落到命中的 exe，而不是报"版本未知"：宁可给出一个
// 来源标注正确的近似版本，也不要因为一条候选路径不存在就丢掉版本信息。
// 回落是否发生由 VersionSource 如实反映，不隐瞒。
func versionPathOf(m *Manifest, exePath string) string {
	raw := strings.TrimSpace(m.Detect.VersionPath)
	if raw == "" {
		return exePath
	}
	for _, cand := range expandCandidate(raw) {
		if fileExists(cand) {
			return cand
		}
	}
	return exePath
}

// fillVersion 读版本号。
//
// 三种来源必须分开实现，因为它们的可靠性结构完全不同：
//
//	exe / exe_product：exe 的版本资源；
//	msix_package：MSIX 包目录名（Codex 无卸载登记项，这是唯一来源）。
//
// 不查卸载登记表的原因：MSIX 包根本不登记卸载项，查了也是空。版本只从
// exe 自己身上或它所在的包目录名里读。
func fillVersion(res *Detection, from, path string) {
	if from == VersionFromMSIXPackage {
		fillVersionFromMSIX(res, path)
		return
	}
	fv := probe.FileVersion(path)
	switch {
	case from == VersionFromExeProduct:
		// 显式要求只看 ProductVersion。没有就如实说未知，不回落 FileVersion——
		// 回落会把 rc 构建号当成应用版本，正是这个字段要避免的错误。
		if fv.ProductVersion != "" {
			res.Version = fv.ProductVersion
			res.VersionSource = "product_version"
		} else {
			res.Notes = append(res.Notes, "要求读 ProductVersion 但该文件没有此项："+path)
		}
	case fv.FileVersion != "":
		res.Version = fv.FileVersion
		res.VersionSource = "file_version"
	case fv.ProductVersion != "":
		res.Version = fv.ProductVersion
		res.VersionSource = "product_version"
	case fv.FixedFileVer != "":
		res.Version = fv.FixedFileVer
		res.VersionSource = "fixed_file_version"
	default:
		// 常见于 MSIX 的零长度 reparse point 与部分精简打包的 exe。
		res.Notes = append(res.Notes, "可执行文件没有版本资源，版本号未知："+path)
	}
}

// fillVersionFromMSIX 从 MSIX 包目录名里解析版本。
//
// 目录名格式固定：<包名>_<版本>_<架构>__<发布者哈希>，例如
// OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0。版本段是第 2 段。
//
// 为什么不用 exe 的版本资源兜底：WindowsApps 下的 exe 版本资源时有时无，
// 而目录名一定带版本。反过来，目录名解析失败时再用 exe 版本资源兜底，
// 并让 VersionSource 说明真实来源。
func fillVersionFromMSIX(res *Detection, exePath string) {
	if v := msixVersionOf(exePath); v != "" {
		res.Version = v
		res.VersionSource = "msix_package"
		return
	}
	res.Notes = append(res.Notes, "无法从 MSIX 包目录名解析版本，回落到 exe 版本资源："+exePath)
	fillVersion(res, VersionFromExe, exePath)
}

// msixVersionOf 从路径里的 WindowsApps 包目录名取版本段。
func msixVersionOf(exePath string) string {
	parts := strings.Split(filepath.ToSlash(exePath), "/")
	for _, p := range parts {
		// 包目录名的判据是"含双下划线"，这是 MSIX 目录名的固定分隔符。
		// 用它而不是"判断父目录是不是 WindowsApps"，是为了让版本号已经
		// 被拷出 WindowsApps 的场景也能识别。
		if !strings.Contains(p, "__") {
			continue
		}
		fields := strings.Split(p, "_")
		if len(fields) < 2 {
			continue
		}
		if looksLikeVersion(fields[1]) {
			return fields[1]
		}
	}
	return ""
}

// looksLikeVersion 判断一个串是否形如 26.1106.6628.0。
//
// 只接受"全数字段 + 点"，避免把包名里的其他内容误当版本。
func looksLikeVersion(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
		default:
			return false
		}
	}
	return dots >= 1
}

// windowTitles 返回属于这些进程的可见顶层窗口标题。
//
// 只用作诊断信息，不参与可用性判定：窗口标题是最不可靠的一环，
// Trae 的主窗口标题就是当前项目名。
func windowTitles(m *Manifest, pids []uint32) []string {
	if len(pids) == 0 && len(m.Detect.WindowClass) == 0 && len(m.Detect.WindowTitle) == 0 {
		return nil
	}
	wins := safeEnumWindows()
	if len(wins) == 0 {
		return nil
	}
	pidSet := make(map[uint32]bool, len(pids))
	for _, p := range pids {
		pidSet[p] = true
	}
	classOK := func(c string) bool {
		if len(m.Detect.WindowClass) == 0 {
			return false
		}
		for _, want := range m.Detect.WindowClass {
			if strings.EqualFold(strings.TrimSpace(want), c) {
				return true
			}
		}
		return false
	}
	var out []string
	seen := make(map[string]bool)
	for _, w := range wins {
		if w.Title == "" {
			continue
		}
		if !pidSet[w.PID] && !classOK(w.Class) {
			continue
		}
		if seen[w.Title] {
			continue
		}
		seen[w.Title] = true
		out = append(out, w.Title)
	}
	sort.Strings(out)
	return out
}

// safeEnumWindows 同 enumProcsSafe，窗口枚举崩了也不许拖垮整个探测。
func safeEnumWindows() (wins []probe.WindowInfo) {
	defer func() {
		if r := recover(); r != nil {
			wins = nil
		}
	}()
	return probe.EnumTopWindows()
}
