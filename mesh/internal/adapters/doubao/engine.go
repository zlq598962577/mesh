//go:build windows

// Package doubao 是豆包桌面客户端（Chromium）的适配器。
//
// 所有与客户端无关的编排都在公共骨架 mesh/internal/adapters.DesktopEngine
// 里，本包只承载豆包的身份、能力声明与实测差异。结构说明与 codex 包一致，
// 参见该包注释。
//
// # 豆包的实测差异（P0a，写入注释与 README，不写进逻辑）
//
//   - "启动器 + 主程序"两份 exe 的结构，与"一个客户端一个 exe"的直觉不同：
//     启动器 E:\Doubao\Doubao.exe 带真应用版本 2.36.8（ProductName 是
//     Doubao Launcher），主程序 E:\Doubao\app\Doubao.exe 带的是 Chromium
//     版本 147.x。因此 manifest 用 version_path 显式指定版本读启动器、
//     exe_paths 把主程序排前面（"装没装"以主程序为准），窗口定位走的
//     是主程序命中的进程。适配器代码不需要也不允许自己再做一遍这套
//     区分——探测结果里的版本号已由探测层按 manifest 取对。
//   - 运行时通常有 8 个全部同名的 Doubao.exe 进程，不能用进程名区分
//     主程序与启动器，只能靠镜像路径。定位窗口的 PID 过滤基于 manifest
//     探测给出的进程集合，不会把启动器的窗口（如果有）误当主程序窗口。
//   - UIA 控件枚举不可达：主窗口类 Chrome_WidgetWin_1，深度 6 走查只有
//     Window/Pane/TitleBar，渲染子树为空，SPI_GETSCREENREADER 为 false。
//     输出只能走剪贴板抓取（待真机验证），completion=unknown_only；
//     客户端常驻不退出，退出码天然不可用。
//
// 这些差异全部由内置 manifest internal/registry/manifests/doubao.json 驱动，
// 本包代码里不出现任何豆包特有的路径、窗口类名或版本逻辑。
package doubao

import (
	"mesh/internal/adapters"
	"mesh/internal/contract"
	"mesh/internal/registry"
)

// AdapterID 与 bot.yaml 的 engine.adapter 取值及内置 manifest 的 id 一致。
const AdapterID = "doubao"

// caps 是豆包在本机实测后的能力声明，与 manifests/doubao.json 的
// capabilities 段同源，语义与其它三个客户端一致（能发、不能流式读、
// 取消不可核实、审批不代答、会话恢复恒为未恢复）。豆包专属的风险点
// 不在能力布尔值上，而在"版本读启动器、窗口定位用主程序"这条探测
// 规则里，由 manifest 表达。
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

// New 创建豆包引擎。reg 可为 nil（语义见 codex 包同名函数）。
func New(reg *registry.Registry) (*adapters.DesktopEngine, error) {
	return adapters.NewDesktopEngine(AdapterID, caps, reg)
}
