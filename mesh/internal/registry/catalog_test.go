//go:build windows

package registry

import (
	"strings"
	"testing"
)

// 内置 manifest 必须全部能解析并通过 Validate。
//
// 这条测试的价值在于：manifest 是 JSON 文本，写错字段名或漏必填项
// 编译器完全看不见，只有真解析一次才会暴露。四份内置定义是"开箱即用"
// 的全部依据，其中任何一份坏掉都意味着那个客户端直接不可用。
func TestLoadCatalogAllBuiltinValid(t *testing.T) {
	valid, invalid, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	if len(invalid) != 0 {
		for _, im := range invalid {
			t.Errorf("内置 manifest 无效 %s: %v", im.Path, im.Err)
		}
		t.Fatalf("有 %d 份内置 manifest 无效", len(invalid))
	}

	want := []string{"codex", "doubao", "dsh", "trae"}
	for _, id := range want {
		m, ok := valid[id]
		if !ok {
			t.Errorf("缺少内置 manifest: %s", id)
			continue
		}
		if m.Origin != OriginCatalog {
			t.Errorf("%s: Origin 应为 catalog，实际 %v", id, m.Origin)
		}
		if m.Tier != TierDeclarative {
			t.Errorf("%s: Tier 应为 %d", id, TierDeclarative)
		}
	}
	if len(valid) != len(want) {
		t.Errorf("内置 manifest 数量为 %d，期望 %d", len(valid), len(want))
	}
}

// 四个客户端都不暴露原生会话 ID 查询接口，resume 必须一律是 none。
//
// 这条不是形式检查：如果哪份 manifest 声明了 native_id，Registry 就会
// 让调用方去取一个取不到的值，而 §5.6 要求取不到时必须 Recovered=false。
// 与其让上层每次运行都落进"声明了却取不到"的分支，不如在定义处就写死。
func TestBuiltinManifestsNoNativeSessionID(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	for id, m := range valid {
		if m.Resume.Mode != ResumeNone {
			t.Errorf("%s: 实测四个客户端都不暴露会话 ID 查询接口，resume.mode 必须是 %q，实际 %q",
				id, ResumeNone, m.Resume.Mode)
		}
		if m.Resume.NativeIDSource != "" {
			t.Errorf("%s: resume.mode=none 时不应有 native_id_source", id)
		}
		if m.Capabilities.NativeSessionID {
			t.Errorf("%s: 不得声明 native_session_id", id)
		}
	}
}

// 桌面通道的取消一律"已请求未证实"：能发 Esc，但读不到客户端生成状态。
// codex/dsh 已翻转为 CLI 通道（终止自有子进程是可验证的停止），
// 由 TestBuiltinManifestsCLICHannel 覆盖；本测试只剩桌面客户端。
// （原 TestBuiltinManifestsCancelNotVerified 已并入 DesktopChannel 断言。）

// CLI 通道的客户端（codex/dsh，均实测打通）：stdout 流 + 进程退出即完成信号
// + 可验证的取消。桌面通道的客户端（trae/doubao）另有断言，见下。
func TestBuiltinManifestsCLICHannel(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	for _, id := range []string{"codex", "dsh"} {
		m := valid[id]
		if m == nil {
			t.Fatalf("缺少 %s manifest", id)
		}
		if m.Output.Mode != OutputModeStdout {
			t.Errorf("%s: 实测为 CLI 通道，output.mode 应为 %q，实际 %q", id, OutputModeStdout, m.Output.Mode)
		}
		if m.Output.Completion != CompletionSuccessSignal || m.Output.SuccessSignal == "" {
			t.Errorf("%s: CLI 的进程退出是可靠完成信号，completion 应为 %q 且 success_signal 非空", id, m.Output.Completion)
		}
		if !m.Capabilities.StreamOutput {
			t.Errorf("%s: CLI stdout 是流式输出，应声明 stream_output", id)
		}
		if !m.Capabilities.CancelVerified {
			t.Errorf("%s: 终止自有子进程是可验证的停止，应声明 cancel_verified", id)
		}
		if !m.Capabilities.Send {
			t.Errorf("%s: CLI 通道已实测打通，应声明 send", id)
		}
	}
}

// 桌面通道的客户端（trae/doubao）：本机 UIA 控件枚举全空，输出走剪贴板，
// 没有任何可核实的完成信号，取消只能是"已请求未证实"。
func TestBuiltinManifestsDesktopChannel(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	for _, id := range []string{"trae", "doubao"} {
		m := valid[id]
		if m == nil {
			t.Fatalf("缺少 %s manifest", id)
		}
		if m.Output.Mode != OutputModeClipboard {
			t.Errorf("%s: UIA 控件不可达，output.mode 应为 %q，实际 %q", id, OutputModeClipboard, m.Output.Mode)
		}
		if m.Output.Completion != CompletionUnknownOnly {
			t.Errorf("%s: 没有可核实的完成标志，completion 应为 %q，实际 %q", id, CompletionUnknownOnly, m.Output.Completion)
		}
		if m.Capabilities.StreamOutput {
			t.Errorf("%s: 剪贴板整段取回，不能声明 stream_output", id)
		}
		if !m.Capabilities.Cancel {
			t.Errorf("%s: 应声明 cancel=true（可以发 Esc）", id)
		}
		if m.Capabilities.CancelVerified {
			t.Errorf("%s: UIA 实测不可达，读不到生成状态，不得声明 cancel_verified", id)
		}
	}
}

// CodexChinese 会注册管理员计划任务，必须被排除目录挡住。
//
// 这条单独测，因为它是防事故的：一旦 exclude_paths 被误删，探测会把
// CodexChinese 当成 Codex，后续所有桌面操作都会打到那个安装上。
func TestCodexExcludesCodexChinese(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	m := valid["codex"]
	if m == nil {
		t.Fatal("缺少 codex manifest")
	}
	if !isExcludedPath(m, `F:\AIWORKSPACE\CodexChinese\ChatGPT.exe`) {
		t.Error("CodexChinese 目录必须被 exclude_paths 排除")
	}
	if isExcludedPath(m, `C:\Program Files\WindowsApps\OpenAI.Codex_26.1106.6628.0_x64__2p2nqsd0c76g0\app\ChatGPT.exe`) {
		t.Error("正常的 MSIX 安装路径不能被误排除")
	}
}

// 豆包的版本来源必须是启动器，不是主程序。
//
// 主程序 app\Doubao.exe 的 ProductVersion 是 Chromium 版本 147.0.7727.149，
// 真正的应用版本在根目录启动器上。写错这一条会把 Chromium 版本当应用版本
// 报给用户。
func TestDoubaoVersionPathIsLauncher(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	m := valid["doubao"]
	if m == nil {
		t.Fatal("缺少 doubao manifest")
	}
	if m.Detect.VersionPath == "" {
		t.Fatal("豆包必须显式指定 version_path：主程序带的是 Chromium 版本")
	}
	if got, want := m.Detect.VersionPath, `E:\Doubao\Doubao.exe`; got != want {
		t.Errorf("version_path 应指向启动器 %q，实际 %q", want, got)
	}
	// 主程序仍然是探测与启动的首选：装了没装以主程序为准。
	if len(m.Detect.ExePaths) == 0 || m.Detect.ExePaths[0] != `E:\Doubao\app\Doubao.exe` {
		t.Errorf("exe_paths 首项应是主程序，实际 %v", m.Detect.ExePaths)
	}
	if m.Launch.ExePath != `E:\Doubao\app\Doubao.exe` {
		t.Errorf("launch 应指向主程序，实际 %q", m.Launch.ExePath)
	}
}

// Codex 通道演进的三个锁定断言（2026-11-14 实测翻转：桌面 → CLI）：
//   - CLI 启动器（npm codex.cmd）是首选探测目标；
//   - CodexChinese 必须被排除（防事故：它会注册管理员计划任务）；
//   - version_from=exe_product：CLI 的 .cmd 没有版本资源，探测层如实报
//     "版本未知"，真实版本以 codex --version 为准（实测 0.169.0）。
//     旧的 msix_package 口径随桌面通道一并降级——MSIX ChatGPT.exe 只作
//     桌面客户端存在性记录，不再是通道目标。
func TestCodexCLIChannelDetect(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	m := valid["codex"]
	if m == nil {
		t.Fatal("缺少 codex manifest")
	}
	if len(m.Detect.ExePaths) == 0 || !strings.Contains(m.Detect.ExePaths[0], "npm") {
		t.Errorf("CLI 时代 exe_paths 首项应是 npm 的 codex.cmd，实际 %v", m.Detect.ExePaths)
	}
	if m.Detect.VersionFrom != VersionFromExeProduct {
		t.Errorf("CLI 启动器无版本资源，version_from 应为 %q（如实报版本未知），实际 %q",
			VersionFromExeProduct, m.Detect.VersionFrom)
	}
}

// Trae 同目录下的 unins000.exe 是 Inno Setup 卸载器。探测或 launch 一旦
// 选中它就会直接卸载客户端，属于不可逆事故，所以必须逐项确认排除。
func TestTraeNeverMatchesUninstaller(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	m := valid["trae"]
	if m == nil {
		t.Fatal("缺少 trae manifest")
	}
	for _, p := range m.Detect.ExePaths {
		if containsUninstallerName(p) {
			t.Errorf("exe_paths 不得包含卸载器: %s", p)
		}
	}
	for _, n := range m.Detect.ProcessNames {
		if containsUninstallerName(n) {
			t.Errorf("process_names 不得包含卸载器: %s", n)
		}
	}
	if containsUninstallerName(m.Launch.ExePath) {
		t.Errorf("launch.exe_path 不得是卸载器: %s", m.Launch.ExePath)
	}
	// 同时确认真正的主程序在里面，别把整份 manifest 写成空的也算通过。
	found := false
	for _, p := range m.Detect.ExePaths {
		if p == `F:\Trae CN\Trae CN.exe` {
			found = true
		}
	}
	if !found {
		t.Error("exe_paths 必须包含 F:\\Trae CN\\Trae CN.exe")
	}
}

// containsUninstallerName 判断路径或进程名是否指向卸载器。
func containsUninstallerName(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "unins") || strings.Contains(low, "uninstall")
}

// DSH 的 ProductVersion 与 FileVersion 不一致是实测事实，必须显式选一个。
func TestDSHVersionFromProduct(t *testing.T) {
	valid, _, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog 失败: %v", err)
	}
	m := valid["dsh"]
	if m == nil {
		t.Fatal("缺少 dsh manifest")
	}
	if m.Detect.VersionFrom != VersionFromExeProduct {
		t.Errorf("DSH 的 FileVersion 是 rc 构建号，version_from 应为 %q，实际 %q",
			VersionFromExeProduct, m.Detect.VersionFrom)
	}
}
