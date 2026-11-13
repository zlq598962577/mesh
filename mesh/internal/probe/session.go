//go:build windows

package probe

import (
	"sort"
	"strings"
)

// 本文件回答 P0a 的第 3 个问题：会话在哪，客户端重启后还能不能重新对上。
//
// 术语先说清（§1.4）：一个 Bot 绑定「一个指定客户端 + 它的专用会话」。
// 所以「会话定位」要能唯一回答：现在这块屏幕上，哪个窗口是这个 Bot 的那个会话。
//
// 可用信号只有三类，而且可靠性差别很大：
//
//	进程 PID：每次重启都变。能用来确认「现在跑的是谁」，不能用来跨重启认人。
//	窗口标题：DSH 的标题会跟着当前对话变；豆包是 "豆包"；Codex 是 "ChatGPT"；
//	          Trae 没有任何标题含 "Trae" 的窗口——主窗口标题是当前打开的项目名，
//	          智能体面板窗口标题是 "Agent"。都不足以单独定位会话。
//	原生会话 ID：四个客户端都没有对外暴露（无公开的「当前会话 ID」查询接口）。
//
// 因此本探针的态度是：
//   - 能测的（窗口 + 进程 + 类名三元组唯一性）如实测；
//   - 定位客户端一律以「进程归属 + exe 安装路径」为准，不依赖窗口标题里出现应用名
//     （Trae 就是反例：按标题找必然漏判）；
//   - 需要重启客户端才能测的（重关联），一律标 unverified 并说明原因，
//     绝不用「看起来稳定」冒充已验证——探针不重启用户的客户端。

// findMainWindow 在该客户端的进程集合里找主窗口。
//
// 判定：进程归属 + 可见 + 有标题。Electron 会创建多个隐藏的辅助窗口
// （离屏渲染、GPU、托盘），所以必须同时满足这三个条件。
// 若有多个候选，取面积/标题最长者不可靠，这里按「标题非空且非空字符串」排序后
// 取第一个，并在 Signals 里如实列出所有候选数量。
func findMainWindow(pids []uint32, windows []WindowInfo) (WindowInfo, int, bool) {
	set := make(map[uint32]bool, len(pids))
	for _, p := range pids {
		set[p] = true
	}
	var cands []WindowInfo
	for _, w := range windows {
		if !set[w.PID] {
			continue
		}
		if !w.Visible || strings.TrimSpace(w.Title) == "" {
			continue
		}
		cands = append(cands, w)
	}
	if len(cands) == 0 {
		return WindowInfo{}, 0, false
	}
	// 稳定排序：标题长的优先（Electron 主窗口标题通常包含应用名/对话名，
	// 隐藏辅助窗口的标题往往是空或单字），同长按 HWND 保证可复现。
	sort.SliceStable(cands, func(i, j int) bool {
		if len(cands[i].Title) != len(cands[j].Title) {
			return len(cands[i].Title) > len(cands[j].Title)
		}
		return cands[i].HWND < cands[j].HWND
	})
	return cands[0], len(cands), true
}

// probeSession 探测会话定位与重启后重关联能力。
func probeSession(ip InstallProbe, windows []WindowInfo) SessionProbe {
	var sp SessionProbe
	sp.State = Unavailable

	if ip.State == Unavailable {
		sp.Reason = "客户端未定位到安装或未运行，无法探测会话"
		sp.Locate.State = Unavailable
		sp.Locate.Reason = sp.Reason
		sp.Reassociate.State = Unavailable
		sp.Reassociate.Reason = sp.Reason
		return sp
	}

	w, ncand, ok := findMainWindow(ip.ProcessIDs, windows)
	if !ok {
		sp.Locate.State = Unavailable
		sp.Locate.Method = "进程归属 + 可见窗口枚举（EnumWindows + GetWindowThreadProcessId）"
		sp.Locate.Reason = "该客户端进程存在但没有可见且有标题的顶层窗口；" +
			"可能是仅托盘运行、窗口被关闭或正在启动"
		sp.Reassociate.State = Unavailable
		sp.Reassociate.Reason = "没有可用的窗口信号，无法谈重关联"
		sp.State = Unavailable
		sp.Reason = sp.Locate.Reason
		return sp
	}

	// ---- 会话定位 ----
	sp.Locate.State = Verified
	sp.Locate.Method = "进程归属 + 窗口类名 + 窗口标题 + 窗口句柄（EnumWindows / GetClassNameW / GetWindowTextW / GetWindowThreadProcessId）"
	sp.Locate.Signals = []string{
		"hwnd=" + u64str(w.HWND),
		"pid=" + itoa(int(w.PID)),
		"class=" + w.Class,
		"title=" + w.Title,
	}
	if ncand > 1 {
		sp.Locate.Signals = append(sp.Locate.Signals,
			"同进程可见有标题窗口数="+itoa(ncand)+"（取标题最长者为主窗口，需人工确认）")
	}
	sp.Locate.Evidence = "唯一性由「进程归属 + 类名 + 标题」三元组确定；" +
		"原生会话 ID 不可得（该客户端未暴露当前会话 ID 的查询接口）"

	// ---- 重启后重关联 ----
	//
	// 这一项无法在不重启客户端的前提下测出来，而探针被禁止动用户的客户端。
	// 所以只能给出「可用信号」和「稳定性判断」，并把结论标成未验证。
	sp.Reassociate.State = Unverified
	sp.Reassociate.Signals = []string{
		"进程 PID：重启后必变，只能用于当次确认，不能跨重启认人",
		"窗口标题：" + describeTitleStability(w.Title),
		"窗口类名：" + w.Class + "（Electron 恒定值，不能区分会话）",
		"原生会话 ID：不可得",
	}
	sp.Reassociate.Stable = titleIsStable(w.Title)
	sp.Reassociate.Reason = "重关联必须在客户端重启后实测，探针不重启用户的客户端（§4.2.2 line 219：能迁移专用会话才迁移，否则暂停）。" +
		"当前仅能给出候选信号与稳定性判断，未做重启验证，故不标 verified"

	sp.State = Unverified
	sp.Reason = "会话定位已验证（窗口三元组）；重启后重关联未验证（需重启客户端）"
	return sp
}

// describeTitleStability 说明窗口标题作为会话信号的可用性。
func describeTitleStability(title string) string {
	if titleIsStable(title) {
		return title + "（固定标题，可判断「是哪个应用」，不能判断「是哪个会话」）"
	}
	return title + "（随对话内容变化，可作为会话指纹，但换对话就失效）"
}

// titleIsStable 判断标题是否是不随对话变化的固定标题。
//
// 这里用「是否等于已知应用名」的白名单判断，而不是猜：
// 已知固定值来自实测——豆包="豆包"、Codex="ChatGPT"。
//
// 特别注意 Trae：它没有任何标题含 "Trae" 的窗口，主窗口标题是当前打开的项目名，
// 智能体面板窗口标题才是 "Agent"。因此：
//   - "Agent" 不是应用标识，而是 Trae 的一个子面板，绝不能当成「固定标题」；
//   - 靠标题找 Trae 必然漏判，本包定位 Trae 只走「进程归属 + exe 安装路径」，
//     与标题无关（见 findMainWindow / DetectInstall）。
func titleIsStable(title string) bool {
	switch strings.TrimSpace(title) {
	case "豆包", "ChatGPT":
		return true
	}
	return false
}

// u64str 把句柄转成十进制字符串。
func u64str(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
