//go:build windows

package probe

import (
	"sort"
	"strings"
)

// 本文件回答 P0a 的第 2 个问题：主窗口、聊天输入框、发送按钮、消息列表
// 这些控件到底能不能被程序化访问。
//
// 这一项最容易得出错误结论，所以必须带对照组。
//
// 本机实测的关键现象：对四个客户端窗口调用 FindAll(Descendants) 全部返回空，
// 连资源管理器（CabinetWClass）也只返回 1 个后代（一个 Pane），
// 整个桌面的 RootElement 只有 14 个顶层子元素，ControlType.Button 与 Edit 全为 0。
//
// 如果只看客户端，很容易得出「这四个客户端不暴露控件」的结论——
// 但连系统自带的资源管理器都枚举不出按钮，说明是探测侧的限制，
// 而不是客户端的问题。这两者的后续动作完全不同：
//   - 若是客户端不暴露控件 → 该客户端没有可靠入口，P0a 记为阻碍；
//   - 若是探测侧限制     → 换探测方式（换会话/换权限/换进程）可能就能读到。
//
// 所以本文件固定做一次资源管理器基线对照，把结论钉在「谁的问题」上。

// baselineClass 是用于对照的系统窗口类名。
// 资源管理器（CabinetWClass）必定有丰富的控件，本机实测可枚举到其后代，
// 因此适合作为「探测侧是否具备枚举能力」的基线。
var baselineClass = "CabinetWClass"

// probeControls 探测某个客户端窗口的控件可达性，并附带基线对照。
func probeControls(w WindowInfo, hasWindow bool, uiaErr string) ControlProbe {
	var cp ControlProbe
	cp.State = Unavailable
	cp.UIAvailable = uiaErr == ""

	if !hasWindow {
		cp.Reason = "未找到该客户端的可见主窗口，控件探测无法进行"
		return cp
	}

	cp.WindowFound = true
	cp.HWND = w.HWND
	cp.ClientPID = w.PID
	cp.WindowClass = w.Class
	cp.WindowTitle = w.Title

	if uiaErr != "" {
		cp.UIAErrorText = uiaErr
		cp.Reason = "UI Automation 初始化失败，控件树无法读取：" + uiaErr
		return cp
	}

	// 基线对照由编排层算一次后注入（applyBaseline），避免每个客户端
	// 各开一次 COM 会话重复测同一件事。
	return cp
}

// applyBaseline 写入探测能力基线。
//
// 基线只测一次：资源管理器是否有可枚举的后代。这个值决定了
// 「客户端窗口没有控件」这个结论能不能成立。
func applyBaseline(cp *ControlProbe, desc int, ok bool) {
	cp.BaselineWindow = "资源管理器（" + baselineClass + "）"
	cp.BaselineOK = ok
	cp.BaselineDescendant = desc
}

// baselineDescendantCount 统计资源管理器窗口的后代元素数，作为探测能力基线。
// 返回 (后代数, 是否测到)。
func baselineDescendantCount() (int, bool) {
	wins := EnumTopWindows()
	for _, w := range wins {
		if w.Class != baselineClass || !w.Visible {
			continue
		}
		s, err := openUIA()
		if err != nil {
			return 0, false
		}
		defer s.close()
		el, err := s.elementFromHWND(w.HWND)
		if err != nil {
			return 0, false
		}
		defer comCall(el, 2)
		n, _, err := s.findAll(el, 0)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// fillControlTree 在已有 uiaSession 的情况下读取目标窗口的控件树统计。
func fillControlTree(s *uiaSession, cp *ControlProbe, w WindowInfo) {
	el, err := s.elementFromHWND(w.HWND)
	if err != nil {
		cp.UIAErrorText = err.Error()
		cp.Reason = "ElementFromHandle 失败，该窗口无法作为 UIA 元素访问：" + err.Error()
		return
	}
	defer comCall(el, 2)

	root := readElement(el)
	cp.ControlType = controlTypeName(root.ControlType)

	total, elems, err := s.findAll(el, 400)
	if err != nil {
		cp.UIAErrorText = err.Error()
		cp.Reason = "FindAll(Descendants) 失败：" + err.Error()
		return
	}
	cp.ControlView = total
	cp.RawView = s.rawChildren(el, 400)
	cp.TreeDepth = s.rawDepth(el, 12)

	// 命中判定：按 ControlType + 名称/类名关键字匹配。
	// 这里刻意保守——匹配到只标 unverified（入口疑似存在），
	// 因为没做真实写入验证；匹配不到标 unavailable。
	hits := matchControls(elems)
	cp.Hits = hits

	verified := 0
	for _, h := range hits {
		if h.State == Verified {
			verified++
		}
	}

	if verified > 0 {
		cp.State = Verified
		cp.Reason = "控件树可读且关键控件已定位"
		return
	}

	// 没有 verified 命中。区分「树是空的」和「树有东西但没我们要的」。
	if total == 0 {
		cp.State = Unavailable
		if cp.BaselineDescendant <= 1 {
			cp.Reason = "该窗口 ControlView 后代数为 0；但基线对照（资源管理器）后代数仅 " +
				itoa(cp.BaselineDescendant) + "，" +
				"说明本探测进程无法枚举 UIA 后代树，属探测侧限制，不能据此判定客户端不暴露控件"
		} else {
			cp.Reason = "该窗口 ControlView 后代数为 0，而基线对照可枚举 " +
				itoa(cp.BaselineDescendant) + " 个后代，" +
				"说明是该客户端窗口不向 UIA 暴露子树"
		}
		return
	}

	cp.State = Unverified
	cp.Reason = "控件树可读（ControlView 后代 " + itoa(total) +
		" 个）但未匹配到聊天输入框/发送按钮/消息列表，需人工核对控件命名"
}

// matchControls 在元素列表里按角色匹配关键控件。
func matchControls(elems []element) []ControlHit {
	roles := []struct {
		role string
		want int32
		keys []string
	}{
		{"main_window", ctWindow, nil},
		{"chat_input", ctEdit, []string{"输入", "发送消息", "message", "prompt", "chat", "ask", "输入框"}},
		{"send_button", ctButton, []string{"发送", "send", "submit", "提交"}},
		{"message_list", ctList, []string{"消息", "message", "conversation", "对话", "chat", "list"}},
	}

	var out []ControlHit
	for _, r := range roles {
		hit := ControlHit{Role: r.role, State: Unavailable}

		// main_window 特例：只要树可读，窗口本身就是 Window 类型，
		// 或者根本拿不到类型，都算「主窗口可达」。
		if r.role == "main_window" {
			hit.State = Verified
			hit.Reason = "顶层窗口已作为 UIA 元素取得"
			out = append(out, hit)
			continue
		}

		found := false
		for _, e := range elems {
			if e.ControlType != r.want {
				continue
			}
			if matchesAny(e, r.keys) {
				hit.State = Unverified
				hit.Name = e.Name
				hit.ControlType = controlTypeName(e.ControlType)
				hit.AutomationID = e.AutomationID
				hit.ClassName = e.ClassName
				hit.Reason = "类型与名称疑似匹配，但未做真实交互验证"
				found = true
				break
			}
		}
		if !found {
			hit.Reason = "未在该窗口的控件树中找到匹配的 " + controlTypeName(r.want) + " 类型控件"
		}
		out = append(out, hit)
	}
	return out
}

// matchesAny 判断元素的名称/自动化 ID/类名是否命中任一关键字（不区分大小写）。
func matchesAny(e element, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	hay := strings.ToLower(e.Name + "\x00" + e.AutomationID + "\x00" + e.ClassName)
	for _, k := range keys {
		if strings.Contains(hay, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// sortedUnique 去重并排序。
func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
