//go:build windows

// Package probe 实现 mesh 的 P0a 客户端探测：对四个实际桌面客户端
// （Codex / DSH / Trae / 豆包）逐一测量安装版本、控件可达性、专用会话定位、
// 收发与完成检测、焦点与锁屏行为、依赖清单。
//
// 设计约束（来自《多Agent协作平台技术方案 v4》，实现时不可放宽）：
//
//   - 只用标准库；不用 cgo；不用 golang.org/x/sys；CGO_ENABLED=0 必须能编译。
//   - 所有外部探测都必须带超时。
//   - 不以管理员身份启动平台（§9.5）；不递归扫盘，只探测配置/清单里的已知路径（§9.5）。
//   - 输出不得包含任何凭据正文，登录态只允许「存在 / 不存在 / 过期 / unknown」（§9.4）。
//   - 完成判定不得使用「N 秒无新字符」（§4.2.2 第 218 行）；成功判定不得使用退出码
//     （§4.3 第 242 行：successSignal 必填，DSH 模型调用失败仍 exit 0）。
//   - 四个指定对象必须逐一实测，不能用同名 CLI / 网页 / 模型 API 代测（§10、§12）。
//
// 本包的核心约定是「三态」：任何一项能力只允许给出 Verified / Unverified /
// Unavailable 三种诚实结论之一，并且 Unavailable 必须带 Reason。
// 不允许把「没测到」写成 verified，也不允许把「测不了」写成一句模糊的「不支持」。
package probe

import "time"

// SchemaVersion 是探测结果 JSON 的版本号。字段语义变化时递增。
const SchemaVersion = 1

// Tri 是探测结论的三态。
type Tri string

const (
	// Verified 实测通过：有可复现的测量证据，证据写在该项的 Evidence 字段里。
	Verified Tri = "verified"

	// Unverified 测过但证据不足，或本机条件不满足导致无法判定。
	// 例如：进程能起、窗口能枚举，但 UIA 后代树只返回 1 个节点，
	// 此时不能写 unavailable（客户端未必没有控件），只能写 unverified 并说明原因。
	Unverified Tri = "unverified"

	// Unavailable 该能力在当前客户端/系统上确认不存在或确认不可达。
	// 必须同时给出 Reason，且 Reason 要写清楚是「测出来的不可达」还是「环境限制无法测」。
	Unavailable Tri = "unavailable"
)

// Valid 用于自检：防止拼错字符串常量导致下游静默当成未知值。
func (t Tri) Valid() bool {
	switch t {
	case Verified, Unverified, Unavailable:
		return true
	}
	return false
}

// LoginState 是登录态。§9.4 规定：只允许输出「存在 / 不存在 / 过期」，
// 绝不能输出凭据正文、token、cookie 或账号名。
//
// 注意：本探测只能给出「存在 / 不存在 / unknown」。
// 「过期」需要解析凭据内容才能判定，而读取凭据正文被 §9.4 禁止，
// 因此 probe 永不输出 LoginExpired，只会输出 unknown 并在 Reason 里说明原因。
type LoginState string

const (
	LoginPresent LoginState = "存在"
	LoginAbsent  LoginState = "不存在"
	LoginExpired LoginState = "过期"
	LoginUnknown LoginState = "unknown"
)

// ProbeResult 是一次完整探测的根结果，同时用于 JSON 与人类可读表格两种输出。
type ProbeResult struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Host          HostInfo      `json:"host"`
	Clients       []ClientProbe `json:"clients"`
	// Blockers 汇总所有阻碍接入的问题（跨客户端）。P0a 的完成判据要求
	// 「可靠入口缺失则明确记录阻碍」，这里是唯一的汇总出口。
	Blockers []string `json:"blockers,omitempty"`
	// Notes 是全局说明，包含诚实性声明（例如「本机无 Go 工具链，未经编译验证」）。
	Notes []string `json:"notes,omitempty"`
}

// HostInfo 描述探测所在的机器与桌面环境。§4.2.2 第 221 行要求
// 「纯 UI 控制需要 Windows 已登录、桌面未锁、目标窗口可访问」，
// 这些条件逐项记录，条件不满足时上层必须停止派发并显示原因。
type HostInfo struct {
	OSBuild        string   `json:"os_build"`
	User           string   `json:"user"`
	Elevated       bool     `json:"elevated"`
	IntegrityLevel string   `json:"integrity_level"`
	SessionID      uint32   `json:"session_id"`
	WindowStation  string   `json:"window_station"`
	Desktop        string   `json:"desktop"`
	InputDesktopOK bool     `json:"input_desktop_openable"`
	MonitorCount   int      `json:"monitor_count"`
	PrimaryDPI     uint32   `json:"primary_dpi"`
	DPIAware       bool     `json:"process_dpi_aware"`
	DotnetRuntimes []string `json:"dotnet_runtimes,omitempty"`
	TempDir        string   `json:"temp_dir"`
	GoVersion      string   `json:"go_version"`
}

// ClientProbe 是单个客户端的探测结果。
type ClientProbe struct {
	Adapter     string `json:"adapter"`      // codex | dsh | trae | doubao
	DisplayName string `json:"display_name"` // 人类可读名

	Install   InstallProbe   `json:"install"`
	Controls  ControlProbe   `json:"controls"`
	Session   SessionProbe   `json:"session"`
	SendCheck SendCheckProbe `json:"send_check"`
	Focus     FocusProbe     `json:"focus"`
	Deps      DepsProbe      `json:"deps"`

	// Blockers 是该客户端自己的阻碍项，供 P0a 完成判据逐条对照。
	Blockers []string `json:"blockers,omitempty"`
}

// InstallProbe 覆盖 §10 的「安装版本」要求：可执行路径、版本、登录态。
type InstallProbe struct {
	State Tri `json:"state"`

	ExePath string `json:"exe_path,omitempty"`
	// LauncherPath 是客户端自带的启动脚本（.cmd/.bat）。只用来确认安装根目录，
	// 绝不代替桌面客户端做验收：§10 明确禁止用同名 CLI 代测。
	LauncherPath string `json:"launcher_path,omitempty"`
	// Version 来自可执行文件的版本资源；MSIX 客户端另记 PackageVersion。
	Version       string `json:"version,omitempty"`
	VersionSource string `json:"version_source,omitempty"`
	// VersionFile 是版本实际取自哪个文件，可能不是 ExePath。
	// 豆包就是这种情况：主程序 app\Doubao.exe 只带 Chromium 版本，
	// 应用版本在根目录启动器里。不写出这个字段，报告里的版本号就无法复核。
	VersionFile string `json:"version_file,omitempty"`
	// PackageFamily 仅 MSIX/AppX 客户端有值，用于说明「路径带版本号、升级后会变」。
	PackageFamily string `json:"package_family,omitempty"`

	// ProcessIDs / WindowTitles 是实测到的进程与窗口，用于 §4.2.2 第 213 行的绑定。
	ProcessIDs   []uint32 `json:"process_ids,omitempty"`
	WindowTitles []string `json:"window_titles,omitempty"`

	// Login 只允许存在/不存在/unknown，绝不包含凭据正文。
	Login  LoginProbe `json:"login"`
	Reason string     `json:"reason,omitempty"`
}

// LoginProbe 是登录态的诚实表示。
type LoginProbe struct {
	State LoginState `json:"state"`
	// Evidence 说明判定依据，例如「凭据文件存在」。
	// 只允许写文件名与存在性，绝不允许写文件内容或账号标识。
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ControlHit 是一个关键控件的命中情况。
type ControlHit struct {
	// Role 取值：main_window | chat_input | send_button | message_list
	Role         string `json:"role"`
	State        Tri    `json:"state"`
	Name         string `json:"name,omitempty"`
	ControlType  string `json:"control_type,omitempty"`
	AutomationID string `json:"automation_id,omitempty"`
	ClassName    string `json:"class_name,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// ControlProbe 覆盖 §10 的「接口/UI Automation 可行性」。
//
// 诚实性要求：如果 UIA 只返回空树，必须区分两种情形：
//  1. 本进程能正常枚举其它已知含控件的窗口（如资源管理器）→ 客户端确实没暴露控件；
//  2. 本进程对所有窗口都只返回空树 → 是本探测环境受限，不能归罪于客户端。
//
// BaselineOK / BaselineDescendants 就是为此存在的：没有基线就不许下结论。
type ControlProbe struct {
	State Tri `json:"state"`

	WindowFound  bool   `json:"window_found"`
	WindowClass  string `json:"window_class,omitempty"`
	WindowTitle  string `json:"window_title,omitempty"`
	ClientPID    uint32 `json:"client_pid,omitempty"`
	HWND         uint64 `json:"hwnd,omitempty"`
	ControlType  string `json:"control_type,omitempty"` // UIA ControlType，如 Window
	TreeDepth    int    `json:"tree_depth"`
	ControlView  int    `json:"control_view_descendants"`
	RawView      int    `json:"raw_view_descendants"`
	UIAvailable  bool   `json:"uia_available"`
	UIAErrorText string `json:"uia_error,omitempty"`

	// Baseline 是同一进程对「已知含控件的窗口」的测量结果，用于区分
	// 「客户端没控件」和「本进程拿不到控件」。
	BaselineWindow     string `json:"baseline_window,omitempty"`
	BaselineOK         bool   `json:"baseline_ok"`
	BaselineDescendant int    `json:"baseline_descendants"`

	Hits []ControlHit `json:"hits,omitempty"`
	// Reason 在 State != Verified 时必填，写清是哪种不可达。
	Reason string `json:"reason,omitempty"`
}

// SessionProbe 覆盖 §10 的「专用会话」与 §4.2.2 第 219 行的会话定位/恢复要求。
type SessionProbe struct {
	State Tri `json:"state"`

	// Locate 描述能否定位到「平台专用会话」。
	Locate LocateProbe `json:"locate"`
	// Reassociate 描述客户端重启后能否重新关联到同一会话。
	Reassociate ReassociateProbe `json:"reassociate"`
	Reason      string           `json:"reason,omitempty"`
}

// LocateProbe 是专用会话定位能力。
type LocateProbe struct {
	State Tri `json:"state"`
	// Method 是实际可用的定位方式：window_title | process_pid | native_session_id | none
	Method   string   `json:"method"`
	Signals  []string `json:"signals,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// ReassociateProbe 是重启后重关联能力。
type ReassociateProbe struct {
	State Tri `json:"state"`
	// Signals 是实测可读的重关联信号（窗口标题 / 进程 PID / 客户端原生会话 id）。
	Signals []string `json:"signals,omitempty"`
	// Stable 说明信号是否在重启后仍然稳定可读。
	Stable bool   `json:"stable"`
	Reason string `json:"reason,omitempty"`
}

// SendCheckProbe 覆盖 §10 的「收发/完成/停止检测」与 §4.2.2 第 217、218 行。
//
// 这里只探测「客户端是否提供了可靠的信号」，不实现发送本身。
// 结论会直接决定 P0b 能不能自动接入：Completion.State 不是 Verified 时，
// 该客户端只能进「待核实」通道，不允许自动宣告成功。
type SendCheckProbe struct {
	State Tri `json:"state"`

	TextInjection SignalProbe     `json:"text_injection"`
	SendTrigger   SignalProbe     `json:"send_trigger"`
	Completion    CompletionProbe `json:"completion"`
	Failure       FailureProbe    `json:"failure"`
	Stop          SignalProbe     `json:"stop"`

	Reason string `json:"reason,omitempty"`
}

// SignalProbe 是一项「有没有可靠信号」的探测结果。
type SignalProbe struct {
	State Tri `json:"state"`
	// Method 是实测可用的手段；不可用时为 none。
	Method   string `json:"method"`
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// CompletionProbe 是「一轮是否结束」的检测能力。
//
// Forbidden 字段不是装饰：它把 §4.2.2 第 218 行禁止的判定方式显式写进输出，
// 让任何后续实现都无法假装自己不知道这条约束。
type CompletionProbe struct {
	State Tri `json:"state"`
	// Signals 是实测可用的完成信号（客户端可核验的生成状态 / 显式结束标记等）。
	Signals []string `json:"signals,omitempty"`
	// Forbidden 固定列出被方案禁止的判定方式，输出里始终存在。
	Forbidden []string `json:"forbidden"`
	Reason    string   `json:"reason,omitempty"`
}

// FailureProbe 是失败检测能力。§4.2.2 第 218 行：错误、限额、重新登录、
// 弹窗和工具审批都是独立状态，不能被当成「还在生成」或「已完成」。
type FailureProbe struct {
	State   Tri      `json:"state"`
	Signals []string `json:"signals,omitempty"`
	// DistinctStates 说明这些异常是否被当作独立状态区分开。
	DistinctStates bool   `json:"distinct_states"`
	Reason         string `json:"reason,omitempty"`
}

// FocusProbe 覆盖 §10 的「焦点与锁屏测试」与 §4.2.2 第 221 行。
// 四种条件分别测试、分别报告，不合并成一个「支持/不支持」。
type FocusProbe struct {
	State Tri `json:"state"`

	Minimized    ConditionProbe `json:"minimized"`
	LockedScreen ConditionProbe `json:"locked_screen"`
	MultiMonitor ConditionProbe `json:"multi_monitor"`
	DPIChange    ConditionProbe `json:"dpi_change"`
	// FocusSteal 说明平台操作是否会抢用户焦点（§4.2.2 第 215 行禁止抢焦点）。
	FocusSteal ConditionProbe `json:"focus_steal"`

	Reason string `json:"reason,omitempty"`
}

// ConditionProbe 是单个环境条件的行为结论。
type ConditionProbe struct {
	State Tri `json:"state"`
	// Observed 是本机实测到的客观事实（例如「本机只有 1 个显示器，无法测多屏」）。
	Observed string `json:"observed,omitempty"`
	// Behavior 是该条件下的预期行为结论：是否停止派发、是否保留草稿。
	Behavior string `json:"behavior,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// DepsProbe 覆盖 §10 的「依赖清单」。
type DepsProbe struct {
	State Tri `json:"state"`
	// AdminRequired 表示该客户端本身是否需要管理员权限才能正常操作。
	// 平台自己永不以管理员身份启动（§9.5），所以这里为 true 就是阻碍。
	AdminRequired bool `json:"admin_required"`
	// HelperProcesses 是实测到的辅助进程（Electron 多进程布局等）。
	HelperProcesses []string `json:"helper_processes,omitempty"`
	// RuntimeDeps 是运行时依赖（.NET / VC++ / Chromium 版本等）。
	RuntimeDeps []string `json:"runtime_deps,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// WindowInfo 是一个顶层窗口的客观事实，供 detect / session / focus 共用。
type WindowInfo struct {
	HWND      uint64 `json:"hwnd"`
	PID       uint32 `json:"pid"`
	Title     string `json:"title"`
	Class     string `json:"class"`
	Visible   bool   `json:"visible"`
	Minimized bool   `json:"minimized"`
}

// FileVersionInfo 是 PE 版本资源里的客观字段。
type FileVersionInfo struct {
	FileVersion    string `json:"file_version,omitempty"`
	ProductVersion string `json:"product_version,omitempty"`
	CompanyName    string `json:"company_name,omitempty"`
	ProductName    string `json:"product_name,omitempty"`
	FixedFileVer   string `json:"fixed_file_version,omitempty"`
}
