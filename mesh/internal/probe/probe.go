//go:build windows

package probe

import (
	"time"
)

// 本文件是探测的编排层：把六个检查项按依赖顺序串起来，产出 ProbeResult。
//
// 执行顺序是有讲究的，不是随便排的：
//   1. 主机环境   —— 后续所有结论都要挂在它下面（会话 ID/完整性级别/DPI…）
//   2. 进程表     —— 只枚举一次，安装探测和依赖探测共用
//   3. 安装与版本 —— 决定后续有没有对象可测
//   4. 控件树     —— 基线对照只测一次，供四个客户端共用
//   5. 会话定位   —— 依赖安装探测给出的进程集合
//   6. 发送链路   —— 依赖控件树结论
//   7. 焦点条件   —— 依赖窗口信息
//   8. 依赖清单   —— 依赖进程表
//
// 超时策略：每个可能卡住的 COM/文件操作都有各自的短超时（见 version.go 的
// probeTimeout），编排层再套一层总超时。总超时到点后，已拿到的结果照常返回，
// 没做完的项标 unknown 并写明「超时未完成」，绝不挂死，也绝不假装完成。

// Options 控制一次探测。
type Options struct {
	// Clients 指定要探测的客户端适配器名；空表示全部。
	Clients []string
	// Timeout 是整次探测的总超时。
	Timeout time.Duration
	// Now 可注入时钟，便于测试；为零值时用 time.Now。
	Now func() time.Time
}

// DefaultTimeout 是默认总超时。
const DefaultTimeout = 60 * time.Second

// Run 执行一次完整探测。
func Run(opt Options) ProbeResult {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	res := ProbeResult{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now(),
	}

	deadline := time.Now().Add(timeout)

	// ---- 1. 主机环境 ----
	host := collectHost()
	res.Host = host
	res.Blockers = append(res.Blockers, hostBlockers(host)...)

	// ---- 2. 进程表（只枚举一次）----
	procs := EnumProcesses()

	// ---- 3. 基线对照（只测一次）----
	baseDesc, baseOK := baselineDescendantCount()

	// ---- 逐个客户端 ----
	specs := Specs()
	wanted := specSet(opt.Clients)
	windows := EnumTopWindows()

	for _, spec := range specs {
		if len(wanted) > 0 && !wanted[spec.Adapter] {
			continue
		}
		if time.Now().After(deadline) {
			// 总超时到点：不再开始新的客户端，但把未做的客户端也列出来，
			// 标 unavailable 并说明超时，避免「结果里没有 = 不存在」的误读。
			res.Clients = append(res.Clients, timeoutStub(spec))
			continue
		}
		res.Clients = append(res.Clients, probeOneClient(spec, procs, windows, host, baseDesc, baseOK))
	}

	if len(res.Clients) == 0 {
		res.Blockers = append(res.Blockers, "未匹配到任何要探测的客户端；请检查 -client 参数")
	}

	res.Notes = append(res.Notes, notesForRun(host)...)
	return res
}

// probeOneClient 探测单个客户端。
func probeOneClient(spec clientSpec, procs []ProcInfo, windows []WindowInfo,
	host HostInfo, baseDesc int, baseOK bool) ClientProbe {

	cp := ClientProbe{
		Adapter:     spec.Adapter,
		DisplayName: spec.DisplayName,
	}

	// ---- 安装与版本 ----
	cp.Install = DetectInstall(spec, procs)
	// 同一个客户端可能有多个顶层窗口且标题重复（例如设置窗口与主窗口同名），
	// 去重后便于人工核对「进程 → 窗口」的绑定关系。
	cp.Install.WindowTitles = sortedUnique(cp.Install.WindowTitles)

	// ---- 窗口 ----
	w, _, hasWindow := findMainWindow(cp.Install.ProcessIDs, windows)

	// ---- 控件树 ----
	cp.Controls = probeControls(w, hasWindow, "")
	applyBaseline(&cp.Controls, baseDesc, baseOK)
	if hasWindow {
		fillControlsLive(&cp.Controls, w)
	}

	// ---- 会话定位 ----
	cp.Session = probeSession(cp.Install, windows)

	// ---- 发送链路 ----
	cp.SendCheck = probeSendCheck(cp.Install, cp.Controls)

	// ---- 焦点与锁屏 ----
	cp.Focus = probeFocus(host, w, hasWindow)

	// ---- 依赖 ----
	cp.Deps = probeDeps(cp.Install, procs, host)

	// ---- 汇总该客户端的阻塞项 ----
	cp.Blockers = collectClientBlockers(cp)
	return cp
}

// fillControlsLive 打开 UIA 会话读取目标窗口控件树。
//
// 单独拆出来是为了让 COM 会话的开启/释放范围尽可能小，
// 并且 UIA 初始化失败时只影响控件树一项，不影响其他检查。
func fillControlsLive(cp *ControlProbe, w WindowInfo) {
	s, err := openUIA()
	if err != nil {
		cp.UIAvailable = false
		cp.UIAErrorText = err.Error()
		if cp.Reason == "" {
			cp.Reason = "UI Automation 初始化失败：" + err.Error()
		}
		return
	}
	defer s.close()
	cp.UIAvailable = true
	fillControlTree(s, cp, w)
}

// timeoutStub 为超时未探测的客户端生成占位结果。
//
// 关键是「说明为什么没测」，而不是给一个空结构让读者以为该客户端不存在。
func timeoutStub(spec clientSpec) ClientProbe {
	reason := "总超时到达，该客户端未开始探测；结果缺失不代表客户端不可用"
	cp := ClientProbe{
		Adapter:     spec.Adapter,
		DisplayName: spec.DisplayName,
		Install:     InstallProbe{State: Unavailable, Reason: reason},
		Controls:    ControlProbe{State: Unavailable, Reason: reason},
		Session: SessionProbe{
			State:       Unavailable,
			Locate:      LocateProbe{State: Unavailable, Reason: reason},
			Reassociate: ReassociateProbe{State: Unavailable, Reason: reason},
			Reason:      reason,
		},
		SendCheck: SendCheckProbe{
			State:         Unavailable,
			TextInjection: SignalProbe{State: Unavailable, Reason: reason},
			SendTrigger:   SignalProbe{State: Unavailable, Reason: reason},
			Completion:    CompletionProbe{State: Unavailable, Reason: reason},
			Failure:       FailureProbe{State: Unavailable, Reason: reason},
			Stop:          SignalProbe{State: Unavailable, Reason: reason},
			Reason:        reason,
		},
		Focus: FocusProbe{
			State:        Unavailable,
			Minimized:    ConditionProbe{State: Unavailable, Reason: reason},
			LockedScreen: ConditionProbe{State: Unavailable, Reason: reason},
			MultiMonitor: ConditionProbe{State: Unavailable, Reason: reason},
			DPIChange:    ConditionProbe{State: Unavailable, Reason: reason},
			FocusSteal:   ConditionProbe{State: Unavailable, Reason: reason},
			Reason:       reason,
		},
		Deps:     DepsProbe{State: Unavailable, Reason: reason},
		Blockers: []string{reason},
	}
	return cp
}

// collectClientBlockers 汇总单个客户端的阻塞项。
//
// 只有真正阻碍「把这个客户端接进来」的才算阻塞项；
// 「某项未验证」是待办，不是阻塞。两者混在一起会让报告失去信息量。
func collectClientBlockers(cp ClientProbe) []string {
	var out []string

	if cp.Install.State == Unavailable {
		out = append(out, "安装/运行："+cp.Install.Reason)
	}
	if cp.Controls.State == Unavailable {
		out = append(out, "控件可达性："+cp.Controls.Reason)
	}
	if cp.SendCheck.TextInjection.State == Unavailable {
		out = append(out, "文本注入不可用："+cp.SendCheck.TextInjection.Reason)
	}
	if cp.SendCheck.Completion.State == Unavailable {
		out = append(out, "完成检测不可用："+cp.SendCheck.Completion.Reason)
	}
	return out
}

// specSet 把适配器名列表转成集合。
func specSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// notesForRun 生成整次探测的说明。
func notesForRun(host HostInfo) []string {
	notes := []string{
		"本结果由 mesh-probe 生成；未编译环境下的探针代码未经验证，结论以字段中的 state 为准",
		"探测主机：会话 " + u32str(host.SessionID) +
			"，窗口站 " + host.WindowStation + `\` + host.Desktop +
			"，完整性级别 " + host.IntegrityLevel,
		"探针不读取客户端凭据正文（§9.4），登录状态仅为「存在/不存在/过期」三态",
		"探针不做递归磁盘扫描，只在已知安装路径与数据目录内查找（§9.5）",
		"探针不重启用户客户端、不点击发送、不注入测试文本；需要真实交互的项一律标 unverified",
	}
	return notes
}
