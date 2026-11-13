//go:build windows

package adapters

// CLI 通道公共引擎（adapters 包级，供多个 CLI 型客户端子包共用——
// 硬性要求：公共逻辑一份，子包不抄）。
//
// 与 DesktopEngine（桌面注入通道）的本质差异，见 dsh 包头实测记录：
//   - 进程生命周期 = 任务生命周期：headless 子进程退出本身是可靠的完成信号；
//   - Cancel 可验证：终止平台自有子进程后不可能继续产出（§9.2 允许；
//     用户的客户端/服务进程绝不触碰）；
//   - 回复走 stdout，流式直达聊天流；每次 Send 一个全新会话。
//
// 已实测打通的通道（2026-11-14）：
//   - dsh  headless（9.4s 真机调用）
//   - codex exec（21.8s 真机调用，复用 ~/.codex/config.toml 的 magpie 网关）

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"mesh/internal/contract"
)

// CLISpec CLI 通道的运行参数。全部由组装层/子包按实测事实注入。
type CLISpec struct {
	// Name 适配器名（codex | dsh），错误与日志用。
	Name string
	// CmdPath CLI 可执行文件绝对路径（.cmd shim 或 .exe）。
	CmdPath string
	// ArgPrefix 固定参数前缀（如 ["headless"] 或 ["exec","--skip-git-repo-check"]），
	// 任务文本追加在后面。
	ArgPrefix []string
	// EnvExtra 注入子进程的环境变量（KEY=VALUE 形式，如 DSH_HOME=...）。
	EnvExtra []string
	// WorkDir 子进程工作目录（空 = 继承平台进程的）。
	// codex exec 语义上在"项目目录"里干活，应指向 bot 的私有工作区。
	WorkDir string
	// CheckEnv 子包特有环境自检（启动器存在、凭据文件存在等）。
	// 只允许存在性检查，绝不读凭据正文（§9.4）。nil = 不检查。
	CheckEnv func() error
	// StreamFilter 决定一行 stdout 是否作为流式 output 事件发给前端。
	// 用于跳过 banner/汇总区（codex exec 的头部元信息与尾部 tokens 统计）。
	// nil = 全部行都发。
	StreamFilter func(line string) bool
	// ExtractAnswer 从完整 stdout 提取最终回复正文（final 事件用）。
	// nil = 全文。思考段/汇总区在这里剔除，保证聊天流里只有回答。
	ExtractAnswer func(stdout string) string
	// StderrTailBytes 失败时附带的 stderr 末尾字节数上限。
	StderrTailBytes int
}

// 编译期断言：CLI 引擎满足引擎契约。
var _ contract.Engine = (*CLIEngine)(nil)

// CLIEngine CLI 通道公共引擎。簿记复用 BaseEngine。
type CLIEngine struct {
	*BaseEngine

	spec CLISpec

	procMu  sync.Mutex
	current *exec.Cmd // 当前子进程（Cancel 的目标；同一时刻最多一个）
	turnID  string
}

// NewCLIEngine 创建 CLI 通道引擎。
func NewCLIEngine(spec CLISpec, caps Capabilities) (*CLIEngine, error) {
	base, err := NewBaseEngine(spec.Name, caps)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.CmdPath) == "" {
		return nil, fmt.Errorf("%s: CLI 引擎缺少 CmdPath", spec.Name)
	}
	if spec.StderrTailBytes <= 0 {
		spec.StderrTailBytes = 4000
	}
	return &CLIEngine{BaseEngine: base, spec: spec}, nil
}

// Open 实现 contract.Engine：环境自检 → 建会话 → session 事件。
// CLI 通道没有客户端进程/窗口可绑定：会话代表本引擎到 CLI 的连接状态。
func (e *CLIEngine) Open(ctx context.Context, spec contract.BotSpec, ws contract.Workspace) (contract.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Engine.Adapter != e.spec.Name {
		return nil, &AdapterError{
			Adapter: e.spec.Name, Op: "Open", Kind: ErrManifest,
			Detail: fmt.Sprintf("bot %q 声明的 engine.adapter 是 %q，与引擎 %q 不符", spec.ID, spec.Engine.Adapter, e.spec.Name),
		}
	}
	if e.spec.CheckEnv != nil {
		if err := e.spec.CheckEnv(); err != nil {
			return nil, &AdapterError{Adapter: e.spec.Name, Op: "Open", Kind: ErrInternal, Detail: err.Error(), Err: err}
		}
	}

	info := contract.SessionInfo{
		Adapter:   e.spec.Name,
		NativeID:  "", // 每次执行都是全新会话，无原生 ID（manifest resume.mode=none）
		Recovered: false,
	}
	h, err := e.NewSession(NewID("clisess"), info, EmitterOptions{BotID: spec.ID})
	if err != nil {
		return nil, err
	}
	h.Emitter().Session(info)
	h.Emitter().Progress("CLI 通道就绪：每次委托拉起一个子进程，stdout 即输出流。")
	return h, nil
}

// Send 实现 contract.Engine：同步拉起 CLI 子进程执行本轮委托。
//
// 结论判定（cli-completed）：
//   exit=0 且 stdout 非空 → NativeDone（Success=true）
//   exit≠0                → ClientError（stderr 末尾附给用户）
//   exit=0 但 stdout 空   → UnknownOnly（完成状态未确认）
// 退出码仍不单独信任（失败也可能 exit 0），必须与 stdout 非空同时满足。
func (e *CLIEngine) Send(ctx context.Context, s contract.Session, msg contract.Message) (contract.Turn, error) {
	h, ok := e.Lookup(s.ID())
	if !ok {
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrInternal, Detail: "会话不存在：" + s.ID()}
	}
	if h.Suspended() {
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrUnsupported, Detail: "会话已挂起，不接受发送"}
	}
	if strings.TrimSpace(msg.Text) == "" {
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrInject, Detail: "消息文本为空，拒绝执行"}
	}
	if len(msg.Text) > 30000 {
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrUnsupported,
			Detail: "任务文本超过 30000 字符（Windows 命令行长度上限），请拆分任务", UserAction: "把委托拆成多条消息"}
	}
	if e.spec.CheckEnv != nil {
		if err := e.spec.CheckEnv(); err != nil {
			return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrInternal, Detail: err.Error(), Err: err}
		}
	}

	turn := e.NewTurn(h, "")
	// 本轮 final 会关闭 emitter（final=会话终结的既定语义），会话随之终结，
	// 派发层据此在下一轮重建。放在这里：早退路径没有发出事件，会话仍可复用。
	defer e.Close(s)

	// CommandContext：进程退出（ctx 取消）时终止子进程，Send 不悬挂；
	// 用户主动停止走 Cancel（taskkill），两条路径互不影响。
	cmd := exec.CommandContext(ctx, e.spec.CmdPath, append(append([]string{}, e.spec.ArgPrefix...), msg.Text)...)
	if len(e.spec.EnvExtra) > 0 {
		cmd.Env = append(os.Environ(), e.spec.EnvExtra...)
	}
	if e.spec.WorkDir != "" {
		cmd.Dir = e.spec.WorkDir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.FinishTurn(turn)
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrInternal, Detail: "创建 stdout 管道失败: " + err.Error(), Err: err}
	}
	var stderrTail strings.Builder
	cmd.Stderr = &tailWriter{b: &stderrTail, limit: e.spec.StderrTailBytes}

	e.procMu.Lock()
	e.current = cmd
	e.turnID = turn.ID()
	e.procMu.Unlock()

	if err := cmd.Start(); err != nil {
		e.procMu.Lock()
		e.current = nil
		e.procMu.Unlock()
		e.FinishTurn(turn)
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrInternal, Detail: "启动 CLI 子进程失败: " + err.Error(), Err: err}
	}
	h.Emitter().Progress("子进程已启动，任务执行中（stdout 逐行回显）。")

	var output strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			output.WriteString(line)
			output.WriteString("\n")
			if e.spec.StreamFilter != nil && !e.spec.StreamFilter(line) {
				continue
			}
			h.Emitter().Output(line)
		}
	}()
	waitErr := cmd.Wait()
	<-done

	e.procMu.Lock()
	e.current = nil
	e.procMu.Unlock()
	e.FinishTurn(turn)

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	trimmed := strings.TrimSpace(output.String())

	switch {
	case waitErr == nil && exitCode == 0 && trimmed != "":
		answer := trimmed
		if e.spec.ExtractAnswer != nil {
			if a := strings.TrimSpace(e.spec.ExtractAnswer(trimmed)); a != "" {
				answer = a
			}
		}
		h.Emitter().Final(DecideFinal(Outcome{
			Signal:  SignalNativeDone,
			Text:    answer,
			Summary: "CLI 进程正常退出且 stdout 非空（cli-completed）",
		}))
		return turn, nil
	case exitCode != 0 || waitErr != nil:
		detail := fmt.Sprintf("CLI 退出码 %d", exitCode)
		if tail := strings.TrimSpace(stderrTail.String()); tail != "" {
			detail += "；stderr 末尾：" + tail
		}
		h.Emitter().Final(DecideFinal(Outcome{
			Signal:      SignalClientError,
			ClientError: detail,
			Summary:     "CLI 任务失败",
		}))
		return nil, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrVerify, Detail: detail, UserAction: "查看事件流里的 stderr，修正后重发"}
	default:
		h.Emitter().Final(DecideFinal(Outcome{
			Signal:  SignalUnknownOnly,
			Summary: "CLI 正常退出但 stdout 为空，完成状态未确认",
		}))
		return turn, &AdapterError{Adapter: e.spec.Name, Op: "Send", Kind: ErrVerify,
			Detail: "正常退出但没有输出，完成状态未确认", UserAction: "到客户端侧确认任务是否真的完成"}
	}
}

// Cancel 实现 contract.Engine：终止当前 CLI 子进程。
// 进程终止是可验证的停止（进程已死即不可能继续产出），明确返回成功；
// 目标只是平台自有子进程，用户的客户端/服务进程绝不触碰（§9.2）。
func (e *CLIEngine) Cancel(ctx context.Context, s contract.Session, turnID string) error {
	e.procMu.Lock()
	cmd := e.current
	runningTurn := e.turnID
	e.procMu.Unlock()

	h, _ := e.Lookup(s.ID())
	if cmd == nil || cmd.Process == nil {
		if h != nil {
			h.Emitter().Progress("当前没有运行中的任务，无需停止。")
		}
		return nil
	}

	killErr := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	_ = ctx

	if killErr != nil {
		detail := fmt.Sprintf("终止子进程失败（pid=%d）：%v；任务可能仍在进行，请手动确认", cmd.Process.Pid, killErr)
		if h != nil {
			h.Emitter().Error(detail)
		}
		return &AdapterError{Adapter: e.spec.Name, Op: "Cancel", Kind: ErrInternal, Detail: detail, Err: killErr, UserAction: "手动确认任务是否已停"}
	}

	detail := fmt.Sprintf("已终止子进程（pid=%d，turn=%s）：进程已死，任务确认停止。", cmd.Process.Pid, runningTurn)
	if h != nil {
		h.Emitter().Progress(detail)
	}
	return nil
}

// Health 实现 contract.Engine：环境自检。
func (e *CLIEngine) Health() error {
	if e.spec.CheckEnv != nil {
		return e.spec.CheckEnv()
	}
	return nil
}

// tailWriter 收集 stderr 的末尾若干字节（失败时给用户看原因）。
type tailWriter struct {
	mu    sync.Mutex
	b     *strings.Builder
	limit int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.b.String()
	if len(s) > w.limit {
		s = s[len(s)-w.limit:]
	}
	w.b.Reset()
	w.b.WriteString(s)
	w.b.Write(p)
	return len(p), nil
}
