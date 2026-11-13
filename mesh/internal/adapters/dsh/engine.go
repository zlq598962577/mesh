//go:build windows

// Package dsh 是 DeepSeek Harness 的 CLI 通道适配器。
//
// # 为什么是 CLI 而不是桌面注入（P0a 真机实测，2026-11-14）
//
// mesh-probe 实测：DSH 0.2.2-rc.3 的 3 个进程全部没有可见顶层窗口（启动 GUI
// 后同样无窗口）——rc.3 的产品形态是"无窗口本机服务 + 浏览器 Web GUI"，桌面
// 注入通道从根上不适用。官方程序化入口是 CLI：`dsh headless "<任务>"`，实测
// 一次真实调用（magpie overlay + 用户凭据）：stdout「测试成功」，exit=0，9.4s。
//
// 公共编排（进程管理/流式/结论判定/Cancel）在 adapters.CLIEngine，本包只承载
// DSH 的路径、环境与输出结构差异。
package dsh

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mesh/internal/adapters"
	"mesh/internal/registry"
)

// AdapterID 与 bot 配置的 engine.adapter 取值及内置 manifest 的 id 一致。
const AdapterID = "dsh"

var caps = adapters.Capabilities{
	Send:            true,
	StreamOutput:    true, // stdout 逐行流式
	Cancel:          true,
	CancelVerified:  true, // 终止平台自有子进程是可验证的停止
	ApprovalBridge:  false,
	FileTransfer:    false,
	NativeSessionID: false,
	ReadOnlyReview:  false,
	HealthCheck:     true,
}

// DefaultCmdPath 本机实测的 CLI 启动器路径（与内置 manifest launch.exe_path 同源）。
func DefaultCmdPath() string { return `F:\deepseek\resources\runtime\cli\bin\dsh.cmd` }

// DefaultDSHHome 用户级 DSH 数据目录（凭据所在，headless 复用用户已登录的模型）。
func DefaultDSHHome() string {
	if v := os.Getenv("DSH_HOME"); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("USERPROFILE"), ".dsh")
}

// DefaultPatchYAML headless 的 LLM provider overlay 模板（本机实测成功的那份）。
// headless 默认 profile 走 deepseek-official 路由，而用户凭据在 magpie 网关。
// 落点在 data_dir 下，用户可手工改模型后重启生效。
func DefaultPatchYAML() string {
	return `- id: agent-default-model
  config:
    provider: magpie
    model: workbuddy/glm-5.3-flash
- id: llm-pi-ai
  name: "@deepseek-ai/dsh-llm-pi-ai"
  config:
    providers:
      magpie:
        displayName: Magpie
        apiKeyEnv: MAGPIE_GATEWAY_KEY
        api: openai-completions
        baseURL: http://127.0.0.1:3425/v1
        models:
          - id: workbuddy/glm-5.3-flash
            name: GLM-5.3-Flash
            contextWindow: 1000000
            maxTokens: 131072
            input: [text]
`
}

// New 创建 DSH CLI 引擎。参数由组装层注入（实测值见 Default* 函数）。
func New(home, patchYAML, patchPath string, _ *registry.Registry) (*adapters.CLIEngine, error) {
	if home == "" {
		home = DefaultDSHHome()
	}
	spec := adapters.CLISpec{
		Name:      AdapterID,
		CmdPath:   DefaultCmdPath(),
		EnvExtra:  []string{"DSH_HOME=" + home},
		CheckEnv: func() error {
			if _, err := os.Stat(home); err != nil {
				return fmt.Errorf("dsh: DSH_HOME 不存在 %s: %w", home, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".credentials.yaml")); err != nil {
				return fmt.Errorf("dsh: %s 下没有 .credentials.yaml（DSH 未保存过模型凭据），headless 无法取到模型: %w", home, err)
			}
			return nil
		},
	}
	// overlay 落点：EnsurePatch 在 Send 前幂等写入（内容一致不覆盖用户手改）。
	if patchYAML != "" && patchPath != "" {
		spec.CheckEnv = chainCheck(spec.CheckEnv, func() error {
			if data, err := os.ReadFile(patchPath); err == nil && strings.TrimSpace(string(data)) == strings.TrimSpace(patchYAML) {
				return nil
			}
			return os.WriteFile(patchPath, []byte(patchYAML), 0o644)
		})
	}
	// 实测（2026-11-14）：reasoning 段走 stderr，stdout 只有干净正文——
	// 无需 StreamFilter 与 ExtractAnswer，全文即回复。
	return adapters.NewCLIEngine(spec, caps)
}

// chainCheck 串联两个自检（全部通过才算就绪）。
func chainCheck(a, b func() error) func() error {
	return func() error {
		if a != nil {
			if err := a(); err != nil {
				return err
			}
		}
		return b()
	}
}

