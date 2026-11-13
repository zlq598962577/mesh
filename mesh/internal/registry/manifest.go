// Package registry 实现四层 agent 注册表（方案 §8.2）。
//
// 四层：Catalog（内置）→ Registry（用户目录）→ Detection（运行时探测缓存）
// → Instance（实际连接实例）。本文件只负责第一层的底座：Tier-1 声明式
// manifest 的结构与解析。
//
// # 关于格式：这里用 JSON，不是方案里的 YAML
//
// 方案 §8.2 写的是"Tier-1 纯 YAML manifest"。本实现改用 JSON，理由是：
// 解析 YAML 必须引入第三方库（gopkg.in/yaml.v3），而本机无法联网拉依赖，
// 且任务硬性要求"纯标准库 + syscall/unsafe，不许 cgo"。用 encoding/json
// 可以零依赖完成同一件事，代价是 manifest 作者要写 JSON 而不是 YAML。
//
// 这个取舍的影响面被限制在"写文件的手感"上，不影响能力：
// 字段集合、语义、四层加载顺序、热增删行为都与方案一致。
//
// 兼容处理：用户目录下 *.json 与 *.yaml 都会被扫描（见 registry.go）。
// *.yaml 里的内容必须是 JSON 语法；如果不是，解析错误会明确写出
// "本实现不解析 YAML"，而不是让人对着一个看不懂的语法错误发呆。
//
// # 错误必须带文件路径与行号
//
// 解析错误（语法/类型）由 encoding/json 给出字节偏移，本文件把它换算成
// 行号与列号。语义错误（缺字段、取值非法）的行号来自一次轻量的字段位置
// 索引（见 indexFieldLines），因此也能定位到具体那一行，而不是只说
// "这个文件有问题"。
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Tier 注册表层级。
//
// Tier-1 是声明式 manifest（纯数据，本文件解析）。
// Tier-2 是 sidecar 进程（stdio JSON-RPC 六方法），刻意不用 Go plugin：
// plugin 对编译器版本、平台、构建标签极其脆弱，跨平台分发基本不可用。
const (
	TierDeclarative = 1
	TierSidecar     = 2
)

// Origin 标明 manifest 来自哪一层，便于 UI 区分"内置"与"用户添加"。
type Origin string

const (
	OriginCatalog Origin = "catalog" // embed.FS 内置
	OriginUser    Origin = "user"    // <data_dir>/agents/
)

// Manifest 一个 agent 的 Tier-1 声明式描述。
//
// 这是"新增一种 agent = 丢一个文件，不重新编译"的载体：字段覆盖
// detect / launch / output / resume / approval / capabilities 六段。
type Manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Tier 只能是 1 或 2。缺省按 1 处理（零值 0 视为未填写）。
	Tier int `json:"tier,omitempty"`

	Detect       DetectSpec           `json:"detect"`
	Launch       LaunchSpec           `json:"launch"`
	Output       OutputSpec           `json:"output"`
	Resume       ResumeSpec           `json:"resume"`
	Approval     ApprovalSpec         `json:"approval"`
	Capabilities ManifestCapabilities `json:"capabilities"`

	// Path 与 Origin 由加载器填写，不来自文件内容。
	Path   string `json:"-"`
	Origin Origin `json:"-"`
}

// DetectSpec 如何判断这个客户端在本机存在。
//
// 探测不启动客户端，只看文件系统与已运行的进程/窗口。所有条件之间是
// "或"的关系：任一命中即认为可用；ExcludePaths 优先于其它条件，
// 命中即判定不可用（用于排除已知有副作用的安装目录）。
type DetectSpec struct {
	// ExePaths 候选可执行文件绝对路径，支持 %ENV% 展开。
	//
	// 路径中允许出现 * 通配（只匹配单个路径分量的部分内容，不跨目录）。
	// 通配是必需的而不是图方便：Codex 以 MSIX 安装，真实目录形如
	// ...\WindowsApps\OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0\app\，
	// 版本段每次升级都会变；写死版本号等于下次升级后探测失效。
	ExePaths []string `json:"exe_paths,omitempty"`
	// ProcessNames 进程名（不含路径），用于判断客户端是否已在运行。
	ProcessNames []string `json:"process_names,omitempty"`
	// WindowClass 窗口类名候选，用于定位主窗口。
	WindowClass []string `json:"window_class,omitempty"`
	// WindowTitle 窗口标题候选（精确或包含匹配，见 match 规则）。
	WindowTitle []string `json:"window_title,omitempty"`
	// ExcludePaths 命中即排除。存在的意义是防事故，不是过滤噪音。
	ExcludePaths []string `json:"exclude_paths,omitempty"`

	// VersionPath 读版本号的目标文件，支持 %ENV% 展开与单分量通配。
	//
	// 空表示"就用探测命中的那个可执行文件"。需要单独指定，是因为
	// "哪个 exe 是客户端"和"哪个 exe 带真实版本号"不一定是同一个：
	// 豆包是启动器 + 主程序结构，主程序 E:\Doubao\app\Doubao.exe 的
	// ProductVersion 147.0.7727.149 其实是 Chromium 版本，真正的应用版本
	// 2.36.8 在根目录启动器 Doubao.exe 上（ProductName=Doubao Launcher）。
	// 把 Chromium 版本当应用版本报给用户，是明确的错误信息。
	VersionPath string `json:"version_path,omitempty"`

	// VersionFrom 版本号的解读方式，取值见 VersionFrom* 常量。
	//
	// 空等价于 VersionFromExe。存在这个字段是因为三种客户端的版本来源
	// 结构完全不同，不能用一个隐式约定覆盖：
	//   - 多数客户端：exe 版本资源；
	//   - Codex：MSIX 安装，无卸载登记项，版本只在带版本号的包目录名里；
	//   - DSH：FileVersion 是 0.2.2-rc.1、ProductVersion 是 0.2.2.0，
	//     两者是不同性质的版本，必须显式选一个。
	VersionFrom string `json:"version_from,omitempty"`

	// Notes 给维护者看的说明，不参与逻辑。
	Notes string `json:"notes,omitempty"`
}

// LaunchSpec 如何启动客户端。
//
// 平台默认不主动启动客户端（用户自己的窗口优先），只有在 bot 需要而
// 客户端未运行时才用这里的配置启动，且只启动、不接管生命周期。
type LaunchSpec struct {
	// ExePath 可执行文件绝对路径，支持 %ENV% 展开。
	ExePath string   `json:"exe_path"`
	Args    []string `json:"args,omitempty"`
	WorkDir string   `json:"work_dir,omitempty"`
	// WindowWaitMS 启动后等待主窗口出现的上限，0 表示用默认值。
	WindowWaitMS int `json:"window_wait_ms,omitempty"`
}

// OutputSpec 如何读取输出与判断完成。
//
// Completion 只有两种合法取值，这是本文件最重要的一个约束：
//   - success_signal：客户端有可靠的完成标志，Success 只能来自它。
//   - unknown_only：没有可靠标志，完成状态一律上报"未确认"。
//
// 不存在第三种取值。方案 §4.2.2 明令"无可靠完成信号不得宣告成功"，
// 也明令禁止用退出码判定成功，所以这里连一个表示"看退出码"的字段都不给。
type OutputSpec struct {
	// Mode 输出读取方式：control（控件直读）| clipboard（剪贴板抓取）。
	Mode string `json:"mode"`
	// Control 控件标识（UIA 名称/AutomationID），Mode=control 时必填。
	Control string `json:"control,omitempty"`
	// Completion 完成判定方式：success_signal | unknown_only。
	Completion string `json:"completion"`
	// SuccessSignal 完成标志的可读描述，Completion=success_signal 时必填。
	// 它同时会被写进 contract.FinalInfo.SuccessSignal，便于事后追溯。
	SuccessSignal string `json:"success_signal,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

// ResumeSpec 专用会话的定位与恢复。
//
// Mode=native_id 表示能从客户端拿到原生会话标识；拿不到时调用方必须
// 置 contract.SessionInfo.Recovered=false，不许假装恢复成功。
type ResumeSpec struct {
	Mode string `json:"mode"` // native_id | none
	// NativeIDSource 原生会话 ID 的来源说明（界面元素/配置项），仅作文档。
	NativeIDSource string `json:"native_id_source,omitempty"`
}

// ApprovalSpec 客户端内部授权请求能否被平台代答（方案 §9.3）。
//
// Bridged=false 时平台无权代答，只能把请求透传给用户，让用户回客户端点。
// 平台不得自动点"全部允许"。
type ApprovalSpec struct {
	Bridged bool   `json:"bridged"`
	Notes   string `json:"notes,omitempty"`
}

// ManifestCapabilities 客户端能力声明。
//
// 这是"声明"，不是"证明"：真实行为以适配器实现与 README 的实测标注为准。
// 声明与实现不一致时，以更保守的一方（即"不能"）为准。
type ManifestCapabilities struct {
	// Send 能否发送一轮请求。
	Send bool `json:"send"`
	// StreamOutput 能否流式读到输出（false 表示只能拿最终文本）。
	StreamOutput bool `json:"stream_output"`
	// Cancel 能否发起取消。
	Cancel bool `json:"cancel"`
	// CancelVerified 取消后能否核实"确实停了"。false 时取消结果只能报
	// "停止待确认"，不许报成功。
	CancelVerified bool `json:"cancel_verified"`
	// ApprovalBridge 能否代答客户端内的审批。
	ApprovalBridge bool `json:"approval_bridge"`
	// FileTransfer 能否把文件交给客户端。
	FileTransfer bool `json:"file_transfer"`
	// NativeSessionID 能否取到原生会话 ID。
	NativeSessionID bool `json:"native_session_id"`
	// ReadOnlyReview 客户端是否有只读模式可供审查者使用。
	ReadOnlyReview bool `json:"read_only_review"`
	// HealthCheck 能否做健康检查。
	HealthCheck bool `json:"health_check"`
}

// 合法的枚举取值。集中定义，避免各处写裸字符串写错。
const (
	OutputModeControl   = "control"
	OutputModeClipboard = "clipboard"
	// OutputModeStdout CLI 通道：headless 子进程的 stdout 就是任务输出流。
	// 该通道下进程退出本身是可靠的完成信号（进程不退出任务就没结束），
	// 与桌面客户端的常驻进程完全不同（P0a 实测：DSH 无 GUI 窗口形态）。
	OutputModeStdout = "stdout"

	CompletionSuccessSignal = "success_signal"
	CompletionUnknownOnly   = "unknown_only"

	ResumeNativeID = "native_id"
	ResumeNone     = "none"
)

// VersionFrom 的合法取值。
const (
	// VersionFromExe 从 exe 的版本资源读（FileVersion 优先，回落 ProductVersion）。
	// 多数客户端用这个。
	VersionFromExe = "exe"

	// VersionFromExeProduct 只读 ProductVersion，不回落 FileVersion。
	//
	// DSH 是必须区分两者的实例：FileVersion=0.2.2-rc.1、ProductVersion=0.2.2.0，
	// 前者是构建流水线号，后者才是用户认的应用版本。默认的"FileVersion 优先"
	// 会把 rc 号当正式版本报出去。
	VersionFromExeProduct = "exe_product"

	// VersionFromMSIXPackage MSIX 包目录名解析。
	//
	// Codex 以 MSIX 安装，三个注册表 hive 都没有卸载登记项，exe 所在的
	// WindowsApps\<包名>_<版本>_<架构>__<发布者哈希>\ 目录名是唯一版本来源。
	VersionFromMSIXPackage = "msix_package"
)

// ManifestError manifest 相关的错误，一律带文件路径与行号。
//
// 行号可能为 0，仅出现在"整个文件级别"的问题上（例如文件读不出来、
// 文件为空）。只要错误能落到某个字段，就一定给出非 0 行号。
type ManifestError struct {
	Path   string // 文件绝对路径
	Line   int    // 1-based，0 表示无法定位到行
	Column int    // 1-based，0 表示未知
	Field  string // 出问题的字段路径，如 capabilities.send
	Msg    string // 人话说明
	Err    error  // 底层错误，可能为 nil
}

func (e *ManifestError) Error() string {
	var b strings.Builder
	b.WriteString("registry: ")
	if e.Path != "" {
		b.WriteString(e.Path)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
			if e.Column > 0 {
				fmt.Fprintf(&b, ":%d", e.Column)
			}
		}
		b.WriteString(": ")
	}
	if e.Field != "" {
		b.WriteString(e.Field)
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *ManifestError) Unwrap() error { return e.Err }

// IsManifestError 判断 err 是否是 manifest 错误，并可取出细节。
func IsManifestError(err error) (*ManifestError, bool) {
	var me *ManifestError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// ParseManifest 从字节解析一个 manifest。
//
// path 只用于错误信息与回填 Manifest.Path，不参与解析。
func ParseManifest(path string, data []byte) (*Manifest, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &ManifestError{Path: path, Msg: "manifest 为空"}
	}

	lines := indexFieldLines(data)

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	// 拒绝未知字段。理由：manifest 是给人手写的，字段名打错时静默忽略
	// 会让人以为配置生效了，实际没生效——这种"看起来成功了"的失败最难查。
	dec.DisallowUnknownFields()

	if err := dec.Decode(&m); err != nil {
		return nil, decodeError(path, data, lines, err)
	}
	// 只允许一个顶层 JSON 对象。多出来的内容通常是复制粘贴留下的残渣。
	if err := ensureEOF(dec); err != nil {
		return nil, &ManifestError{
			Path: path,
			Msg:  "manifest 只能包含一个 JSON 对象，后面还有多余内容",
			Err:  err,
		}
	}

	m.Path = path
	if m.Tier == 0 {
		m.Tier = TierDeclarative
	}
	if err := m.Validate(lines); err != nil {
		if me, ok := IsManifestError(err); ok {
			me.Path = path
			return nil, me
		}
		return nil, err
	}
	return &m, nil
}

// LoadManifestFile 从磁盘读取并解析一个 manifest。
func LoadManifestFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ManifestError{Path: path, Msg: "读取 manifest 失败", Err: err}
	}
	m, err := ParseManifest(path, data)
	if err != nil {
		return nil, err
	}
	// 文件名与 id 不一致时以 id 为准，但记一笔——重复 id 排查时有用。
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base != m.ID {
		m.Description = strings.TrimSpace(m.Description + " [文件名 " + base + " 与 id 不一致]")
	}
	return m, nil
}

// Validate 检查语义约束。lines 用于把错误定位到行，可为 nil。
func (m *Manifest) Validate(lines map[string]int) error {
	fail := func(field, msg string) error {
		line := 0
		if lines != nil {
			line = lines[field]
		}
		return &ManifestError{Field: field, Line: line, Msg: msg}
	}

	if strings.TrimSpace(m.ID) == "" {
		return fail("id", "id 不能为空")
	}
	if !validID(m.ID) {
		return fail("id", "id 只能由小写字母、数字、- 和 _ 组成")
	}
	if strings.TrimSpace(m.Name) == "" {
		return fail("name", "name 不能为空")
	}
	if m.Tier != TierDeclarative && m.Tier != TierSidecar {
		return fail("tier", fmt.Sprintf("tier 只能是 %d 或 %d，实际 %d",
			TierDeclarative, TierSidecar, m.Tier))
	}
	if m.Tier == TierSidecar {
		// Tier-2 的 sidecar 配置不在本文件解析（见 sidecar.go 的说明）。
		// 这里只要求它至少声明了探测方式，否则四层加载会无处安放。
		if len(m.Detect.ExePaths) == 0 && len(m.Detect.ProcessNames) == 0 {
			return fail("detect", "Tier-2 manifest 至少要声明 exe_paths 或 process_names")
		}
		return nil
	}

	// ---- Tier-1 的必填项 ----
	if len(m.Detect.ExePaths) == 0 && len(m.Detect.ProcessNames) == 0 {
		return fail("detect", "至少要声明 exe_paths 或 process_names，否则无法探测")
	}
	if strings.TrimSpace(m.Launch.ExePath) == "" {
		return fail("launch.exe_path", "launch.exe_path 不能为空")
	}
	if m.Launch.WindowWaitMS < 0 {
		return fail("launch.window_wait_ms", "window_wait_ms 不能为负")
	}

	// ---- 版本来源 ----
	switch m.Detect.VersionFrom {
	case "", VersionFromExe, VersionFromExeProduct, VersionFromMSIXPackage:
	default:
		return fail("detect.version_from", fmt.Sprintf(
			"version_from 只能是 %q、%q 或 %q，实际 %q",
			VersionFromExe, VersionFromExeProduct, VersionFromMSIXPackage, m.Detect.VersionFrom))
	}
	if m.Detect.VersionFrom == VersionFromMSIXPackage && len(m.Detect.ExePaths) == 0 {
		return fail("detect.version_from",
			"version_from=msix_package 时必须给出 exe_paths：包目录名只能从已命中的路径反推")
	}

	switch m.Output.Mode {
	case OutputModeControl:
		if strings.TrimSpace(m.Output.Control) == "" {
			return fail("output.control", "mode=control 时必须给出 control 标识")
		}
	case OutputModeClipboard:
		// 剪贴板路径不需要 control 标识。
	case OutputModeStdout:
		// CLI 通道：stdout 流不需要 control 标识。
	default:
		return fail("output.mode", fmt.Sprintf("output.mode 只能是 %q、%q 或 %q，实际 %q",
			OutputModeControl, OutputModeClipboard, OutputModeStdout, m.Output.Mode))
	}

	switch m.Output.Completion {
	case CompletionSuccessSignal:
		if strings.TrimSpace(m.Output.SuccessSignal) == "" {
			return fail("output.success_signal",
				"completion=success_signal 时必须描述成功信号，否则无法据此判定完成")
		}
	case CompletionUnknownOnly:
		// 明确声明"没有可靠完成信号"。此时 success_signal 若被填写则自相矛盾。
		if strings.TrimSpace(m.Output.SuccessSignal) != "" {
			return fail("output.success_signal",
				"completion=unknown_only 时不能声明 success_signal，两者矛盾")
		}
	default:
		return fail("output.completion", fmt.Sprintf("output.completion 只能是 %q 或 %q，实际 %q",
			CompletionSuccessSignal, CompletionUnknownOnly, m.Output.Completion))
	}

	switch m.Resume.Mode {
	case ResumeNativeID:
		if strings.TrimSpace(m.Resume.NativeIDSource) == "" {
			return fail("resume.native_id_source",
				"mode=native_id 时必须说明原生会话 ID 的来源；取不到时调用方要置 Recovered=false")
		}
	case ResumeNone:
		if strings.TrimSpace(m.Resume.NativeIDSource) != "" {
			return fail("resume.native_id_source", "mode=none 时不应声明 native_id_source")
		}
	default:
		return fail("resume.mode", fmt.Sprintf("resume.mode 只能是 %q 或 %q，实际 %q",
			ResumeNativeID, ResumeNone, m.Resume.Mode))
	}

	// 能力声明之间的一致性检查。这些组合本身不合法，不是风格问题。
	if m.Capabilities.CancelVerified && !m.Capabilities.Cancel {
		return fail("capabilities.cancel_verified",
			"声明了 cancel_verified 却没声明 cancel：无法核实的取消不能声明为可核实")
	}
	if m.Capabilities.ApprovalBridge && !m.Approval.Bridged {
		return fail("capabilities.approval_bridge",
			"声明了 approval_bridge 但 approval.bridged=false，两者矛盾")
	}
	if m.Approval.Bridged && !m.Capabilities.ApprovalBridge {
		return fail("approval.bridged",
			"approval.bridged=true 但 capabilities.approval_bridge=false，两者矛盾")
	}
	if m.Capabilities.NativeSessionID != (m.Resume.Mode == ResumeNativeID) {
		if m.Capabilities.NativeSessionID {
			return fail("capabilities.native_session_id",
				"声明了 native_session_id 但 resume.mode 不是 native_id")
		}
		return fail("resume.mode",
			"resume.mode=native_id 但 capabilities.native_session_id=false，两者矛盾")
	}

	return nil
}

// validID 限制 id 字符集。
//
// 限制的理由不是洁癖：id 会进文件名、事件里的 BotID、以及未来可能的
// URL 路径。允许任意字符会让这些地方各写一套转义，迟早出现不一致。
func validID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// decodeError 把 encoding/json 的错误换算成带行号的 ManifestError。
func decodeError(path string, data []byte, lines map[string]int, err error) error {
	me := &ManifestError{Path: path, Err: err}

	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		line, col := offsetToLineCol(data, syn.Offset)
		me.Line, me.Column = line, col
		me.Msg = "JSON 语法错误"
		if isYAMLFile(path) {
			// 最常见的真实情况：作者按方案写了 YAML。
			me.Msg = "JSON 语法错误（本实现不解析 YAML，.yaml 文件内容必须写成 JSON）"
		}
		return me
	}

	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		line, col := offsetToLineCol(data, typ.Offset)
		me.Line, me.Column = line, col
		me.Field = typ.Field
		me.Msg = fmt.Sprintf("字段类型不对：期望 %s，实际 %s", typ.Type, typ.Value)
		return me
	}

	// 未知字段：encoding/json 只说字段名，不给偏移。用字段位置索引兜底。
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		me.Field = name
		// 索引里键名是完整路径，这里退化为"找以该键结尾的第一条"。
		for k, v := range lines {
			if k == name || strings.HasSuffix(k, "."+name) {
				me.Line = v
				break
			}
		}
		me.Msg = "未知字段（字段名拼错，或该字段不属于 manifest）"
		return me
	}

	me.Msg = "解析 manifest 失败"
	return me
}

// ensureEOF 确认解码器后面没有第二个 JSON 值。
func ensureEOF(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("多余内容")
	}
	return err
}

// isYAMLFile 判断扩展名是否是 YAML，用于把语法错误翻译成人话。
func isYAMLFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// offsetToLineCol 把字节偏移换算成 1-based 的行号与列号。
func offsetToLineCol(data []byte, offset int64) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line, col = 1, 1
	for i := int64(0); i < offset; i++ {
		if data[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// indexFieldLines 建立"字段路径 → 行号"的索引。
//
// 为什么需要它：encoding/json 的语义错误（缺字段、取值非法）不带偏移，
// 但 manifest 是给人手写的，报错必须指到行，否则作者只能逐行猜。
//
// 实现方式是用 json.Decoder 的 Token 流走一遍，记录每个对象键出现时的
// 输入偏移。深度最多展开到三层（如 capabilities.send），再深的字段
// 不值得为报错付复杂度。
func indexFieldLines(data []byte) map[string]int {
	lines := make(map[string]int)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	type frame struct {
		isObject  bool
		expectKey bool
	}
	var stack []*frame
	var path []string

	pushKey := func(key string) {
		if len(path) < 3 {
			path = append(path, key)
		}
	}
	popKey := func() {
		if len(path) > 0 {
			path = path[:len(path)-1]
		}
	}
	joinPath := func() string {
		if len(path) == 0 {
			return ""
		}
		return strings.Join(path, ".")
	}

	for {
		// InputOffset 在 Token 之前取，得到的是上一个 token 结束的位置，
		// 对定位"这个键所在行"足够精确。
		off := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return lines
		}

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				// 键已经在读到它时入栈，这里只压容器帧。
				stack = append(stack, &frame{isObject: true, expectKey: true})
			case '[':
				stack = append(stack, &frame{isObject: false})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// 容器结束，对父对象来说"一个值结束了"。
				if n := len(stack); n > 0 && stack[n-1].isObject {
					stack[n-1].expectKey = true
				}
			}
			continue
		}

		n := len(stack)
		topIsObject := n > 0 && stack[n-1].isObject

		if topIsObject && stack[n-1].expectKey {
			if key, ok := tok.(string); ok {
				pushKey(key)
				if _, seen := lines[joinPath()]; !seen {
					line, _ := offsetToLineCol(data, off)
					lines[joinPath()] = line
				}
				stack[n-1].expectKey = false
				continue
			}
		}

		// 这里是一个标量值（字符串/数字/布尔/null）。
		// 如果它属于某个键，就把键路径出栈。
		if topIsObject {
			stack[n-1].expectKey = true
			popKey()
		}
	}
}
