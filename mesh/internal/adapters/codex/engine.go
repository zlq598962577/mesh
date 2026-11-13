//go:build windows

// Package codex 是 OpenAI Codex 的 CLI 通道适配器。
//
// # 通道演进（全部实测驱动）
//
// 1. 桌面注入通道（P0a→P0b）：MSIX 客户端 Chrome_WidgetWin_1 无 UIA 控件，
//    注入后无法核实；2026-11-14 真机验证"文字能进输入框但发送触发与回复
//    读取不可用"，且会把字打进用户正在用的会话（专用会话隔离不可实现）。
// 2. CLI 通道（2026-11-14 起默认）：用户安装官方 codex-cli 0.169.0 后实测
//    `codex exec` 走 ~/.codex/config.toml 的 magpie 网关成功（21.8s，
//    provider/model 自动识别，回复直达 stdout）。
//
// # codex exec 的 stdout 结构（实测）
//
//	OpenAI Codex v0.169.0        ← banner 开始
//	--------
//	workdir: / model: / provider: / approval: / sandbox: / ...
//	--------
//	user                          ← 用户输入回显
//	<任务文本>
//	<思考段>
//	codex                         ← 回答段开始
//	<回答>
//	tokens used                   ← 汇总区开始
//	12,703
//	<最终消息（与回答同文）>
//
// StreamFilter 跳过 banner 与 tokens 汇总区；ExtractAnswer 取 "tokens used"
// 之后的内容（exec 的最终消息），保证聊天流里只有回答。
package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mesh/internal/adapters"
	"mesh/internal/registry"
)

// AdapterID 与 bot 配置的 engine.adapter 取值及内置 manifest 的 id 一致。
const AdapterID = "codex"

var caps = adapters.Capabilities{
	Send:            true,
	StreamOutput:    true,
	Cancel:          true,
	CancelVerified:  true, // 终止平台自有子进程是可验证的停止
	ApprovalBridge:  false, // exec 模式 approval=never，客户端自行拒绝交互审批
	FileTransfer:    false,
	NativeSessionID: false,
	ReadOnlyReview:  false,
	HealthCheck:     true,
}

// resolveCmdPath 定位 codex CLI：
//  1. exec.LookPath("codex")——与用户终端同一套 PATH/PATHEXT 机制，最可靠
//    （实测用户的 npm prefix 是自定义目录 D:\...\node-global，固定猜
//    %APPDATA%\npm 会落空——2026-11-14 用户实测事故）；
//  2. 回退 %APPDATA%\npm\codex.cmd（npm 默认 prefix）。
// 全部落空返回带安装指引的错误。
func resolveCmdPath() (string, error) {
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	fallback := filepath.Join(os.Getenv("APPDATA"), "npm", "codex.cmd")
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}
	return "", &adapters.AdapterError{Adapter: AdapterID, Op: "Open", Kind: adapters.ErrManifest,
		Detail:     "codex CLI 不在 PATH 里，%APPDATA%\\npm\\codex.cmd 也不存在",
		UserAction: "npm install -g @openai/codex 安装；或确认 npm 全局目录在 PATH 里"}
}

// New 创建 Codex CLI 引擎。spec 由组装层注入。
func New(spec adapters.CLISpec, _ *registry.Registry) (*adapters.CLIEngine, error) {
	if spec.Name == "" {
		spec.Name = AdapterID
	}
	// 命令路径动态解析（npm prefix 可能是自定义目录），解析失败在
	// CheckEnv 里以可读文案暴露给用户。
	cmdPath, cmdErr := resolveCmdPath()
	if spec.CmdPath == "" {
		spec.CmdPath = cmdPath // 可能为空：CheckEnv 会报出安装指引
	}
	if len(spec.ArgPrefix) == 0 {
		spec.ArgPrefix = []string{"exec", "--skip-git-repo-check"}
	}
	spec.CheckEnv = func() error {
		if cmdErr != nil {
			return cmdErr
		}
		if _, err := os.Stat(spec.CmdPath); err != nil {
			return errCodexMissing(spec.CmdPath, err)
		}
		// config.toml 只查存在性，绝不读内容（§9.4：不读凭据正文）。
		cfg := filepath.Join(os.Getenv("USERPROFILE"), ".codex", "config.toml")
		if _, err := os.Stat(cfg); err != nil {
			return errCodexNoConfig(cfg, err)
		}
		return nil
	}
	if spec.StreamFilter == nil {
		spec.StreamFilter = streamFilter
	}
	if spec.ExtractAnswer == nil {
		spec.ExtractAnswer = extractAnswer
	}
	return adapters.NewCLIEngine(spec, caps)
}

// streamFilter 跳过 exec 的 banner 区与 tokens 汇总区，正文逐行放行。
func streamFilter(line string) bool {
	t := strings.TrimSpace(line)
	// banner 边界与元信息行。
	if t == "--------" || strings.HasPrefix(t, "OpenAI Codex v") {
		return false
	}
	for _, pre := range []string{"workdir:", "model:", "provider:", "approval:", "sandbox:", "reasoning effort:", "reasoning summaries:", "session id:"} {
		if strings.HasPrefix(t, pre) {
			return false
		}
	}
	// user 回显段与 tokens 汇总区不放行（用户消息自己刚发过，汇总是统计）。
	if t == "user" || t == "tokens used" {
		return false
	}
	return true
}

// extractAnswer 取 "tokens used" 之后的最终消息；锚点不存在时回退全文
//（exec 输出结构随版本可能变化，回退保证不丢回复）。
func extractAnswer(stdout string) string {
	idx := strings.LastIndex(stdout, "tokens used")
	if idx < 0 {
		return stdout
	}
	tail := strings.TrimSpace(stdout[idx+len("tokens used"):])
	if tail == "" {
		return stdout
	}
	return tail
}

// errCodexMissing / errCodexNoConfig：两类启动期阻塞的可读文案。
func errCodexMissing(path string, err error) error {
	return &adapters.AdapterError{Adapter: AdapterID, Op: "Open", Kind: adapters.ErrManifest,
		Detail:     "codex CLI 不存在 " + path + "（npm install -g @openai/codex 可安装）",
		Err:        err,
		UserAction: "安装官方 CLI，或在 bots/codex.json 里改回桌面通道"}
}

func errCodexNoConfig(path string, err error) error {
	return &adapters.AdapterError{Adapter: AdapterID, Op: "Open", Kind: adapters.ErrInternal,
		Detail:     path + " 不存在（codex CLI 未登录过）",
		Err:        err,
		UserAction: "先在终端跑一次 codex 登录/配置模型（~/.codex/config.toml）"}
}
