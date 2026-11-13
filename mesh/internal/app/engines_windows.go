//go:build windows

package app

// Windows 下把 bot 的 adapter 映射到真实适配器引擎。
//
// 构造失败（能力声明非法等）的 bot 不进 map：派发层对没有引擎的 bot 走
// unavailableEngine 诚实占位并在 UI 标"适配器未接入"，绝不静默跳过。

import (
	"fmt"
	"os"
	"path/filepath"

	"mesh/internal/adapters"
	"mesh/internal/adapters/codex"
	"mesh/internal/adapters/doubao"
	"mesh/internal/adapters/dsh"
	"mesh/internal/adapters/trae"
	"mesh/internal/contract"
	"mesh/internal/registry"
)

// botWorkspaceDir 引用 bots.go 的 botIDRe；此处仅文档化依赖关系。

func buildEngines(bots []contract.BotSpec, reg *registry.Registry, dataDir string) (map[string]contract.Engine, []string) {
	engines := make(map[string]contract.Engine)
	var notes []string
	// 同一 adapter 的引擎只构造一次：适配器内部按 bot 会话簿记，
	// 每个客户端进程一个引擎实例与 §4.2.2 的绑定语义一致。
	byAdapter := make(map[string]contract.Engine)
	for _, b := range bots {
		adapter := b.Engine.Adapter
		if adapter == "" {
			notes = append(notes, fmt.Sprintf("bot %q 未声明 engine.adapter，将显示为适配器未接入", b.ID))
			continue
		}
		if eng, ok := byAdapter[adapter]; ok {
			engines[b.ID] = eng
			continue
		}
		eng, err := newAdapterEngine(adapter, reg, dataDir)
		if err != nil {
			notes = append(notes, fmt.Sprintf("bot %q 的适配器 %s 构造失败：%v（该 bot 将显示为适配器未接入）", b.ID, adapter, err))
			continue
		}
		byAdapter[adapter] = eng
		engines[b.ID] = eng
	}
	return engines, notes
}

func newAdapterEngine(adapter string, reg *registry.Registry, dataDir string) (contract.Engine, error) {
	switch adapter {
	case codex.AdapterID:
		// Codex CLI 通道（2026-11-14 实测打通）：exec 走 ~/.codex/config.toml
		// 的 magpie 网关。工作区指向 bot 私有目录（exec 的沙箱与 workdir 语义），
		// 目录必须先建好——CLI 子进程的 chdir 不接受不存在的目录。
		ws := botWorkspaceDir(dataDir, codex.AdapterID)
		if ws != "" {
			if err := os.MkdirAll(ws, 0o755); err != nil {
				return nil, fmt.Errorf("app: 创建 codex 工作区 %s 失败: %w", ws, err)
			}
		}
		return codex.New(adapters.CLISpec{
			WorkDir: ws,
		}, reg)
	case doubao.AdapterID:
		return doubao.New(reg)
	case trae.AdapterID:
		return trae.New(reg)
	case dsh.AdapterID:
		// DSH CLI 通道（P0a 实测：rc.3 无 GUI 窗口形态，桌面注入不适用）。
		return dsh.New(dsh.DefaultDSHHome(), dsh.DefaultPatchYAML(),
			filepath.Join(dataDir, "dsh-headless-patch.yml"), reg)
	default:
		return nil, fmt.Errorf("adapters: 未知适配器 %q（内置支持 codex/dsh/trae/doubao）", adapter)
	}
}

// botWorkspaceDir bot 私有工作区（与 config.BotWorkspace 同一约定；
// CLI 子进程的 workdir 在这里，目录不存在时 exec 自己会失败并如实上报）。
func botWorkspaceDir(dataDir, botID string) string {
	if !botIDRe.MatchString(botID) {
		return ""
	}
	return filepath.Join(dataDir, "workspaces", botID)
}

// detectorFor 非空 Windows 探测器：registry 第 3 层在 Windows 上可用。
func detectorFor() registry.Detector { return registry.NewWindowsDetector() }

var _ = adapters.HonestVerifier{} // 保留引用，说明引擎骨架与适配器同源
