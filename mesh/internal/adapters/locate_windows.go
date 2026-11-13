//go:build windows

package adapters

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mesh/internal/probe"
	"mesh/internal/registry"
)

// 本文件是四个适配器共用的**客户端定位**层。
//
// 为什么不放在四个子包里各写一份：定位要做的事完全一样——读 manifest、
// 枚举进程、按 exe 路径与排除目录过滤、挑一个可见的顶层窗口。唯一随客户端
// 变化的是 manifest 内容，而 manifest 已经是数据了，不需要再复制四份代码。
//
// 更关键的一点：探测逻辑已经在 registry 的 WindowsDetector 里实现过一次，
// 包括通配符展开、MSIX 包目录、exclude_paths 拦截。适配器如果自己再枚举一遍
// 进程，就会出现两套判定，迟早一套认了另一套不认。所以这里直接调用探测器。

// ClientWindow 一个已定位的客户端窗口。
type ClientWindow struct {
	// HWND 顶层窗口句柄，交给 desktop 包做聚焦与注入。
	HWND uintptr
	// PID 拥有该窗口的进程。
	PID uint32
	// Title 窗口标题。Trae 的标题是项目名，不能拿来判客户端身份。
	Title string
	// ExePath 命中进程的镜像路径，用于日志与错误提示。
	ExePath string
	// Version / VersionSource 版本号及它的来源（exe 资源 / MSIX 包目录名）。
	Version       string
	VersionSource string
}

// ManifestFor 取内置 manifest。
//
// 每次调用都重新读一遍内嵌数据，看起来有点浪费，但内置 manifest 是编译期
// 嵌入的常量文本，解析开销是微秒级；反过来缓存它就要处理"缓存与代码不一致"
// 的失效问题，不值得。
func ManifestFor(id string) (*registry.Manifest, error) {
	valid, _, err := registry.LoadCatalog()
	if err != nil {
		return nil, &AdapterError{
			Adapter: id, Op: "ManifestFor", Kind: ErrManifest,
			Detail: "读取内置 manifest 目录失败",
			Err:    err,
		}
	}
	m, ok := valid[id]
	if !ok {
		return nil, &AdapterError{
			Adapter: id, Op: "ManifestFor", Kind: ErrManifest,
			Detail: fmt.Sprintf("没有 id 为 %q 的内置 manifest", id),
		}
	}
	return m, nil
}

// DetectClient 用注册表的 Windows 探测器定位客户端。
//
// 返回的 Detection 里带着 PIDs 与窗口标题，已经过 exclude_paths 过滤，
// 因此 CodexChinese 那种会注册管理员计划任务的安装不会被误认成 Codex。
func DetectClient(ctx context.Context, id string) (*registry.Detection, *registry.Manifest, error) {
	m, err := ManifestFor(id)
	if err != nil {
		return nil, nil, err
	}
	d := registry.NewWindowsDetector().Detect(ctx, m)
	if d == nil {
		return nil, m, &AdapterError{
			Adapter: id, Op: "DetectClient", Kind: ErrInternal,
			Detail: "探测器返回空结果",
		}
	}
	if d.Err != "" {
		return d, m, &AdapterError{
			Adapter: id, Op: "DetectClient", Kind: ErrManifest,
			Detail: d.Err,
		}
	}
	return d, m, nil
}

// LocateClientWindow 找到客户端的主窗口。
//
// 挑窗口的规则，按优先级：
//
//  1. 进程 PID 必须属于探测命中的客户端进程（这一条是安全边界：
//     绝不能凭标题相似就去操作一个别的程序的窗口）。
//  2. 可见、非最小化、标题非空。
//  3. 若 manifest 声明了 window_title，优先匹配它的窗口；但 Trae 实测
//     没有任何标题含 "Trae"（主窗口标题是项目名），所以它没有声明，
//     此时退回"按进程挑"。
//
// 找不到就返回 ErrClientNotRunning 而不是猜一个窗口。
func LocateClientWindow(ctx context.Context, id string) (*ClientWindow, error) {
	d, m, err := DetectClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if !d.Available {
		return nil, &AdapterError{
			Adapter: id, Op: "LocateClientWindow", Kind: ErrClientNotRunning,
			Detail:     "没有探测到正在运行的客户端：" + m.Name,
			UserAction: "先启动客户端并登录，再重试",
		}
	}
	if len(d.PIDs) == 0 {
		return nil, &AdapterError{
			Adapter: id, Op: "LocateClientWindow", Kind: ErrClientNotRunning,
			Detail:     "客户端进程存在但没有可用 PID（探测结果不确定）",
			UserAction: "确认客户端已完全启动后重试",
		}
	}

	pidSet := make(map[uint32]bool, len(d.PIDs))
	for _, p := range d.PIDs {
		pidSet[p] = true
	}

	wins := probe.EnumTopWindows()
	cands := make([]probe.WindowInfo, 0, len(wins))
	for _, w := range wins {
		if !pidSet[w.PID] {
			continue
		}
		if !w.Visible || w.Minimized || strings.TrimSpace(w.Title) == "" {
			continue
		}
		cands = append(cands, w)
	}
	if len(cands) == 0 {
		return nil, &AdapterError{
			Adapter: id, Op: "LocateClientWindow", Kind: ErrClientNotRunning,
			Detail:     "客户端进程在运行，但没有可见且带标题的顶层窗口（可能被最小化到托盘）",
			UserAction: "把客户端窗口打开并保持在桌面上，然后重试",
		}
	}

	// manifest 声明了标题特征时优先匹配。
	titlePatterns := m.Detect.WindowTitle
	if len(titlePatterns) > 0 {
		matched := make([]probe.WindowInfo, 0, len(cands))
		for _, w := range cands {
			for _, pat := range titlePatterns {
				if strings.Contains(strings.ToLower(w.Title), strings.ToLower(pat)) {
					matched = append(matched, w)
					break
				}
			}
		}
		if len(matched) > 0 {
			cands = matched
		}
	}

	// 稳定排序：同一客户端可能有多个窗口（Chromium 的辅助窗口也带标题），
	// 按 HWND 排序保证同一台机器上多次调用选到同一个窗口，避免行为漂移。
	sort.Slice(cands, func(i, j int) bool { return cands[i].HWND < cands[j].HWND })
	best := cands[0]

	return &ClientWindow{
		HWND:          uintptr(best.HWND),
		PID:           best.PID,
		Title:         best.Title,
		ExePath:       d.ExePath,
		Version:       d.Version,
		VersionSource: d.VersionSource,
	}, nil
}

// LocateClientWindowByHWND 校验一个已知 HWND 是否仍属于该客户端。
//
// 会话打开后用户可能关掉客户端再重开，此时旧 HWND 会被系统回收给别的进程。
// 直接往旧句柄上打字是"打到错误窗口"的经典事故，所以每次操作前都要确认
// 这个 HWND 现在仍属于客户端进程。
func LocateClientWindowByHWND(ctx context.Context, id string, hwnd uintptr) (*ClientWindow, error) {
	if hwnd == 0 {
		return nil, &AdapterError{
			Adapter: id, Op: "LocateClientWindowByHWND", Kind: ErrInternal,
			Detail: "HWND 为 0",
		}
	}
	d, m, err := DetectClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if !d.Available || len(d.PIDs) == 0 {
		return nil, &AdapterError{
			Adapter: id, Op: "LocateClientWindowByHWND", Kind: ErrClientNotRunning,
			Detail:     "客户端已不在运行：" + m.Name,
			UserAction: "重新启动客户端后重开会话",
		}
	}
	pidSet := make(map[uint32]bool, len(d.PIDs))
	for _, p := range d.PIDs {
		pidSet[p] = true
	}
	for _, w := range probe.EnumTopWindows() {
		if uintptr(w.HWND) != hwnd {
			continue
		}
		if !pidSet[w.PID] {
			return nil, &AdapterError{
				Adapter: id, Op: "LocateClientWindowByHWND", Kind: ErrClientNotForeground,
				Detail:     fmt.Sprintf("HWND %#x 已不属于客户端进程（窗口被回收给了 PID %d）", hwnd, w.PID),
				UserAction: "重新打开会话",
			}
		}
		return &ClientWindow{
			HWND:          hwnd,
			PID:           w.PID,
			Title:         w.Title,
			ExePath:       d.ExePath,
			Version:       d.Version,
			VersionSource: d.VersionSource,
		}, nil
	}
	return nil, &AdapterError{
		Adapter: id, Op: "LocateClientWindowByHWND", Kind: ErrClientNotRunning,
		Detail:     fmt.Sprintf("HWND %#x 对应的窗口已不存在", hwnd),
		UserAction: "重新打开会话",
	}
}

// ClientRunning 供 Health 使用：只判断客户端在不在跑，不做任何窗口操作。
func ClientRunning(ctx context.Context, id string) error {
	d, m, err := DetectClient(ctx, id)
	if err != nil {
		return err
	}
	if !d.Available {
		return &AdapterError{
			Adapter: id, Op: "Health", Kind: ErrClientNotRunning,
			Detail:     "没有探测到正在运行的客户端：" + m.Name,
			UserAction: "先启动客户端并登录",
		}
	}
	return nil
}

// clientSummary 把定位结果压成一行人可读描述，用于 progress 事件与错误详情。
func clientSummary(w *ClientWindow) string {
	if w == nil {
		return "（未定位到窗口）"
	}
	parts := []string{fmt.Sprintf("PID %d", w.PID), "窗口《" + w.Title + "》"}
	if w.Version != "" {
		v := "版本 " + w.Version
		if w.VersionSource != "" {
			v += "（" + w.VersionSource + "）"
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, "，")
}
