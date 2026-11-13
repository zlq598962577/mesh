//go:build windows

// Package trae 是 Trae CN 编辑器内置 AI 智能体面板的适配器。
//
// 所有与客户端无关的编排都在公共骨架 mesh/internal/adapters.DesktopEngine
// 里，本包只承载 Trae 的身份、能力声明与实测差异。结构说明与 codex 包一致，
// 参见该包注释。
//
// # Trae 的实测差异（P0a，写入注释与 README，不写进逻辑）
//
//   - 要驱动的是它内置的 AI 智能体面板，不是编辑器本身。主程序
//     F:\Trae CN\Trae CN.exe（ProductVersion 3.4.18；FileVersion 2.3.102387
//     是内嵌 Electron 的版本号，manifest 用 version_from=exe_product 避免
//     把 Electron 版本当 Trae 版本报出去）。
//   - 窗口定位只能靠进程 + exe 路径：实测枚举全部有标题的顶层窗口，没有
//     任何一个标题含 "Trae"——主窗口标题是当前项目名，智能体面板窗口标题
//     是 "Agent"。所以 manifest 不声明 window_title，写了必然漏判；
//     adapters.LocateClientWindow 在没有标题特征时退回"按进程挑窗口"，
//     并用按 HWND 稳定排序保证多次调用选到同一个窗口。
//   - 卸载器绝不能选中：同目录的 unins000.exe 是 Inno Setup 卸载器，
//     它是独立可执行文件、有版本资源、也能被启动。一旦被探测或 launch
//     选中就会直接卸载客户端。manifest 的 exe_paths 与 process_names
//     因此只写 Trae CN.exe 这个完整文件名，不带任何通配——这里任何
//     "图方便"的通配都是事故。
//   - UIA 控件枚举按同族 Chromium 推断不可达（DSH/豆包/ChatGPT 的渲染
//     子树实测全空，Trae 是待实测假设而非实测结论，README 里单列标注）。
//     输出走剪贴板（待真机验证），completion=unknown_only；编辑器常驻
//     不退出，退出码天然不可用。
//
// 这些差异全部由内置 manifest internal/registry/manifests/trae.json 驱动，
// 本包代码里不出现任何 Trae 特有的路径、窗口类名或版本逻辑。
package trae

import (
	"mesh/internal/adapters"
	"mesh/internal/contract"
	"mesh/internal/registry"
)

// AdapterID 与 bot.yaml 的 engine.adapter 取值及内置 manifest 的 id 一致。
const AdapterID = "trae"

// caps 是 Trae 在本机实测后的能力声明，与 manifests/trae.json 的
// capabilities 段同源。send=true 依赖剪贴板注入路径（与其它三个客户端
// 同一条公共实现）；输出读取整条标"待真机验证"——Trae 的 UIA 不可达是
// 同族推断，四个客户端里唯一未经本机实测确认的一条，所以它是 README
// 对照表里最需要真机复验的一行。
var caps = adapters.Capabilities{
	Send:            true,
	StreamOutput:    false,
	Cancel:          true,
	CancelVerified:  false,
	ApprovalBridge:  false,
	FileTransfer:    false,
	NativeSessionID: false,
	ReadOnlyReview:  false,
	HealthCheck:     true,
}

// 编译期断言：公共骨架必须满足引擎契约。
var _ contract.Engine = (*adapters.DesktopEngine)(nil)

// New 创建 Trae 引擎。reg 可为 nil（语义见 codex 包同名函数）。
func New(reg *registry.Registry) (*adapters.DesktopEngine, error) {
	return adapters.NewDesktopEngine(AdapterID, caps, reg)
}
