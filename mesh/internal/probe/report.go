//go:build windows

package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// 本文件负责输出：结构化 JSON 与人读表格。
//
// 两种输出必须表达同一批事实，不能各说一套。所以都从同一个 ProbeResult 渲染，
// 表格里的每个字都能在 JSON 里找到对应字段。
//
// 汇总判定的三态定义（这个定义很关键，因为它决定用户看到「能用」还是「不能用」）：
//
//	可用   —— 六个检查项全部为 verified
//	不可用 —— 至少一项为 unavailable（即存在明确阻碍）
//	未知   —— 没有 unavailable，但有 unverified（即没测出来，不能算好也不能算坏）
//
// 之所以把 unverified 单列为「未知」而不是并入「可用」：本机实测控件树
// 根本枚举不到，如果 unverified 算可用，报告就会显示四个客户端全部可用，
// 这显然是假的。宁可显示「未知」，也不能给出乐观的假结论。

// Verdict 是单个客户端的汇总判定。
type Verdict string

const (
	// VerdictUsable 表示六项全部已验证。
	VerdictUsable Verdict = "可用"
	// VerdictUnusable 表示存在明确阻碍。
	VerdictUnusable Verdict = "不可用"
	// VerdictUnknown 表示没有明确阻碍，但也没验出来。
	VerdictUnknown Verdict = "未知"
)

// ClientSummary 是单个客户端的一句话结论。
type ClientSummary struct {
	Adapter  string   `json:"adapter"`
	Verdict  Verdict  `json:"verdict"`
	Blocking []string `json:"blocking,omitempty"`
	Pending  []string `json:"pending,omitempty"`
}

// Summarize 计算每个客户端的汇总判定。
func Summarize(res ProbeResult) []ClientSummary {
	out := make([]ClientSummary, 0, len(res.Clients))
	for _, c := range res.Clients {
		s := ClientSummary{Adapter: c.Adapter}
		var unavailable, unverified []string

		add := func(name string, state Tri, reason string) {
			switch state {
			case Unavailable:
				unavailable = append(unavailable, name+"（"+reason+"）")
			case Unverified:
				unverified = append(unverified, name)
			}
		}

		add("安装与版本", c.Install.State, c.Install.Reason)
		add("控件可达性", c.Controls.State, c.Controls.Reason)
		add("会话定位", c.Session.Locate.State, c.Session.Locate.Reason)
		add("重启重关联", c.Session.Reassociate.State, c.Session.Reassociate.Reason)
		add("文本注入", c.SendCheck.TextInjection.State, c.SendCheck.TextInjection.Reason)
		add("发送触发", c.SendCheck.SendTrigger.State, c.SendCheck.SendTrigger.Reason)
		add("完成检测", c.SendCheck.Completion.State, c.SendCheck.Completion.Reason)
		add("失败检测", c.SendCheck.Failure.State, c.SendCheck.Failure.Reason)
		add("焦点与锁屏", c.Focus.State, c.Focus.Reason)
		add("依赖清单", c.Deps.State, c.Deps.Reason)

		s.Blocking = unavailable
		s.Pending = unverified
		switch {
		case len(unavailable) > 0:
			s.Verdict = VerdictUnusable
		case len(unverified) > 0:
			s.Verdict = VerdictUnknown
		default:
			s.Verdict = VerdictUsable
		}
		out = append(out, s)
	}
	return out
}

// WriteJSON 输出结构化 JSON。
func WriteJSON(w io.Writer, res ProbeResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(res)
}

// WriteTable 输出人读表格。
func WriteTable(w io.Writer, res ProbeResult) {
	sums := Summarize(res)

	fmt.Fprintf(w, "mesh-probe 客户端探测结果\n")
	fmt.Fprintf(w, "生成时间：%s\n", res.GeneratedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "主机：%s  |  用户 %s  |  会话 %d  |  %s\\%s  |  完整性 %s  |  管理员 %s\n",
		res.Host.OSBuild, res.Host.User, res.Host.SessionID,
		res.Host.WindowStation, res.Host.Desktop,
		res.Host.IntegrityLevel, boolStr(res.Host.Elevated))
	fmt.Fprintf(w, "显示：%d 块屏，系统 DPI %d，进程 DPI 感知 %s  |  .NET：%s\n",
		res.Host.MonitorCount, res.Host.PrimaryDPI, boolStr(res.Host.DPIAware),
		joinOr(res.Host.DotnetRuntimes, "未发现"))
	fmt.Fprintf(w, "\n")

	// ---- 汇总表 ----
	fmt.Fprintf(w, "=== 汇总 ===\n")
	fmt.Fprintf(w, "%-8s %-8s %s\n", "客户端", "结论", "阻塞项 / 待验证")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 100))
	for _, s := range sums {
		detail := ""
		if len(s.Blocking) > 0 {
			detail = "阻塞：" + strings.Join(s.Blocking, "；")
		} else if len(s.Pending) > 0 {
			detail = "待验证：" + strings.Join(s.Pending, "、")
		}
		fmt.Fprintf(w, "%-8s %-8s %s\n", s.Adapter, s.Verdict, detail)
	}
	fmt.Fprintf(w, "\n")

	// ---- 每客户端明细 ----
	for i, c := range res.Clients {
		writeClientDetail(w, c, sums[i])
	}

	// ---- 整体阻塞与说明 ----
	if len(res.Blockers) > 0 {
		fmt.Fprintf(w, "=== 整体阻塞 ===\n")
		for _, b := range res.Blockers {
			fmt.Fprintf(w, "  - %s\n", b)
		}
		fmt.Fprintf(w, "\n")
	}
	if len(res.Notes) > 0 {
		fmt.Fprintf(w, "=== 说明 ===\n")
		for _, n := range res.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
}

// writeClientDetail 输出单个客户端的明细。
func writeClientDetail(w io.Writer, c ClientProbe, s ClientSummary) {
	fmt.Fprintf(w, "=== %s（%s）—— %s ===\n", c.DisplayName, c.Adapter, s.Verdict)

	// 安装与版本
	fmt.Fprintf(w, "[安装与版本] %s\n", c.Install.State)
	if c.Install.ExePath != "" {
		fmt.Fprintf(w, "  主程序：%s\n", c.Install.ExePath)
	}
	if c.Install.LauncherPath != "" {
		fmt.Fprintf(w, "  启动器：%s（仅用于确认安装位置，不作验收对象）\n", c.Install.LauncherPath)
	}
	fmt.Fprintf(w, "  版本：%s（来源：%s）\n", c.Install.Version, c.Install.VersionSource)
	if c.Install.PackageFamily != "" {
		fmt.Fprintf(w, "  包族名：%s（MSIX 路径含版本号，升级后会变）\n", c.Install.PackageFamily)
	}
	if len(c.Install.ProcessIDs) > 0 {
		fmt.Fprintf(w, "  进程：%s\n", u32list(c.Install.ProcessIDs))
	} else {
		fmt.Fprintf(w, "  进程：无\n")
	}
	fmt.Fprintf(w, "  登录：%s（%s）\n", c.Install.Login.State, joinOr([]string{c.Install.Login.Evidence, c.Install.Login.Reason}, ""))
	if c.Install.Reason != "" {
		fmt.Fprintf(w, "  备注：%s\n", c.Install.Reason)
	}

	// 控件
	fmt.Fprintf(w, "[控件可达性] %s\n", c.Controls.State)
	if c.Controls.WindowFound {
		fmt.Fprintf(w, "  窗口：%s  class=%s  hwnd=%s\n",
			c.Controls.WindowTitle, c.Controls.WindowClass, u64str(c.Controls.HWND))
	}
	fmt.Fprintf(w, "  ControlView 后代 %d 个；RawView 直接子节点 %d 个；RawView 深度 %d\n",
		c.Controls.ControlView, c.Controls.RawView, c.Controls.TreeDepth)
	if c.Controls.BaselineWindow != "" {
		fmt.Fprintf(w, "  基线对照：%s 后代 %d 个（%s）\n",
			c.Controls.BaselineWindow, c.Controls.BaselineDescendant,
			map[bool]string{true: "探测侧具备枚举能力", false: "探测侧无法枚举"}[c.Controls.BaselineOK])
	}
	for _, h := range c.Controls.Hits {
		fmt.Fprintf(w, "  - %-14s %-12s %s\n", h.Role, h.State, firstNonEmpty(h.Reason, h.Name))
	}
	if c.Controls.Reason != "" {
		fmt.Fprintf(w, "  结论：%s\n", c.Controls.Reason)
	}

	// 会话
	fmt.Fprintf(w, "[会话定位] %s\n", c.Session.Locate.State)
	for _, sig := range c.Session.Locate.Signals {
		fmt.Fprintf(w, "  - %s\n", sig)
	}
	if c.Session.Locate.Reason != "" {
		fmt.Fprintf(w, "  结论：%s\n", c.Session.Locate.Reason)
	}
	fmt.Fprintf(w, "[重启重关联] %s（信号稳定性：%s）\n",
		c.Session.Reassociate.State, boolStr(c.Session.Reassociate.Stable))
	for _, sig := range c.Session.Reassociate.Signals {
		fmt.Fprintf(w, "  - %s\n", sig)
	}
	if c.Session.Reassociate.Reason != "" {
		fmt.Fprintf(w, "  结论：%s\n", c.Session.Reassociate.Reason)
	}

	// 发送链路
	fmt.Fprintf(w, "[发送链路]\n")
	fmt.Fprintf(w, "  文本注入  %-12s %s\n", c.SendCheck.TextInjection.State, c.SendCheck.TextInjection.Reason)
	fmt.Fprintf(w, "  发送触发  %-12s %s\n", c.SendCheck.SendTrigger.State, c.SendCheck.SendTrigger.Reason)
	fmt.Fprintf(w, "  完成检测  %-12s %s\n", c.SendCheck.Completion.State, c.SendCheck.Completion.Reason)
	fmt.Fprintf(w, "  失败检测  %-12s %s（五类状态是否可区分：%s）\n",
		c.SendCheck.Failure.State, c.SendCheck.Failure.Reason, boolStr(c.SendCheck.Failure.DistinctStates))
	fmt.Fprintf(w, "  停止      %-12s %s\n", c.SendCheck.Stop.State, c.SendCheck.Stop.Reason)
	if len(c.SendCheck.Completion.Forbidden) > 0 {
		fmt.Fprintf(w, "  被禁止的替代判据（不得用于判定完成）：\n")
		for _, f := range c.SendCheck.Completion.Forbidden {
			fmt.Fprintf(w, "    × %s\n", f)
		}
	}

	// 焦点
	fmt.Fprintf(w, "[焦点与锁屏] %s\n", c.Focus.State)
	for _, row := range []struct {
		name string
		c    ConditionProbe
	}{
		{"最小化", c.Focus.Minimized},
		{"锁屏", c.Focus.LockedScreen},
		{"多显示器", c.Focus.MultiMonitor},
		{"DPI 变化", c.Focus.DPIChange},
		{"焦点抢占", c.Focus.FocusSteal},
	} {
		fmt.Fprintf(w, "  %-10s %-12s %s\n", row.name, row.c.State, row.c.Observed)
		if row.c.Behavior != "" {
			fmt.Fprintf(w, "             %s\n", row.c.Behavior)
		}
	}

	// 依赖
	fmt.Fprintf(w, "[依赖清单] %s\n", c.Deps.State)
	fmt.Fprintf(w, "  需要管理员：%s\n", boolStr(c.Deps.AdminRequired))
	if len(c.Deps.HelperProcesses) > 0 {
		fmt.Fprintf(w, "  辅助进程：%s\n", strings.Join(c.Deps.HelperProcesses, "、"))
	}
	for _, d := range c.Deps.RuntimeDeps {
		fmt.Fprintf(w, "  运行时：%s\n", d)
	}
	for _, n := range c.Deps.Notes {
		fmt.Fprintf(w, "  备注：%s\n", n)
	}
	fmt.Fprintf(w, "\n")
}

// u32list 把 PID 列表渲染成可读字符串。
func u32list(v []uint32) string {
	parts := make([]string, 0, len(v))
	for _, p := range v {
		parts = append(parts, itoa(int(p)))
	}
	return strings.Join(parts, ", ")
}

// joinOr 用分隔符连接非空项；全空时返回 fallback。
func joinOr(vals []string, fallback string) string {
	var kept []string
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 {
		return fallback
	}
	return strings.Join(kept, "；")
}
