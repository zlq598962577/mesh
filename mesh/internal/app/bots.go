package app

// bot 配置加载（"新增一种 agent = 丢一个文件"承诺的组装层落地）。
//
// <data_dir>/bots/*.json 每个文件是一个 bot（BotSpec 的 JSON），增删文件即
// 增删 agent，无需重新编译。首装时目录为空会自动写入四个内置客户端的默认
// 配置（Codex/DSH/Trae/豆包）——用户删掉不想要的、照抄格式加新的，都是
// 普通文件操作。
//
// 解析失败的文件：跳过并把文件名带进错误汇总，绝不因为一个坏文件拒绝启动
// （与 registry 对坏 manifest 的策略一致）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mesh/internal/contract"
)

// stripBOM 剥掉 UTF-8 BOM。记事本等编辑器保存 UTF-8 时可能带 BOM，
// 而 encoding/json 拒绝 BOM 开头——"丢文件加 agent"不能被编辑器差异卡住。
func stripBOM(payload []byte) []byte {
	return bytes.TrimPrefix(payload, []byte("\xef\xbb\xbf"))
}

// botIDRe 与 sanitizeBotID 同一条正则；engines_windows.go 判定 bot 工作区目录名用。
var botIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// builtinBotSpecs 四个内置客户端的默认配置，与 registry 的内置 manifest 一一对应。
// codex 与 dsh 实测均为 CLI 通道（2026-11-14），Engine.Type=cli。
func builtinBotSpecs() []contract.BotSpec {
	mk := func(id, name string, engType contract.EngineType) contract.BotSpec {
		return contract.BotSpec{
			ID:   id,
			Name: name,
			Role: "执行者或审查者，由用户在委托时指定",
			Engine: contract.EngineSpec{
				Type:        engType,
				Adapter:     id,
				Connection:  "auto",
				SessionMode: "dedicated",
			},
			Workspace: contract.Workspace{Mode: "private", Dir: "auto"},
		}
	}
	return []contract.BotSpec{
		mk("codex", "Codex", contract.EngineCLI),
		mk("dsh", "DSH", contract.EngineCLI),
		mk("trae", "Trae CN", contract.EngineDesktop),
		mk("doubao", "豆包", contract.EngineDesktop),
	}
}

// ensureBotConfigFiles 保证 bots 目录里至少有内置客户端的默认配置。
// 只在目录里没有任何 *.json 时写入；用户已有配置就一个字节都不动。
func ensureBotConfigFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return fmt.Errorf("app: 创建 bot 配置目录失败: %w", mkErr)
			}
			entries = nil
		} else {
			return fmt.Errorf("app: 读取 bot 配置目录失败: %w", err)
		}
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			return nil // 已有配置，尊重用户现状
		}
	}
	for _, spec := range builtinBotSpecs() {
		payload, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			return fmt.Errorf("app: 序列化默认 bot 配置失败: %w", err)
		}
		path := filepath.Join(dir, spec.ID+".json")
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			return fmt.Errorf("app: 写入默认 bot 配置 %s 失败: %w", path, err)
		}
	}
	return nil
}

// loadBots 读取 bots 目录下的全部 BotSpec JSON。
// 文件名（不含后缀）与 spec.ID 冲突时以文件内容为准；重复 ID 后者报错。
func loadBots(dir string) ([]contract.BotSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("app: 读取 bot 配置目录失败: %w", err)
	}

	// 按文件名排序：目录顺序不稳定，确定性加载便于排错。
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	bots := make([]contract.BotSpec, 0, len(names))
	seen := map[string]string{}
	var bad []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		payload, err := os.ReadFile(path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var spec contract.BotSpec
		if err := json.Unmarshal(stripBOM(payload), &spec); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if spec.ID == "" {
			bad = append(bad, fmt.Sprintf("%s: 缺少 id 字段", name))
			continue
		}
		if err := sanitizeBotID(spec.ID); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if prev, dup := seen[spec.ID]; dup {
			bad = append(bad, fmt.Sprintf("%s: bot id %q 与 %s 重复，拒绝加载", name, spec.ID, prev))
			continue
		}
		seen[spec.ID] = name
		bots = append(bots, spec)
	}
	if len(bad) > 0 {
		// 已解析的照常返回（坏文件不拖累好文件），错误里点名坏文件。
		return bots, fmt.Errorf("app: 以下 bot 配置文件未能加载：%s", strings.Join(bad, "；"))
	}
	return bots, nil
}
