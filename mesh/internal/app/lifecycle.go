package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"
)

// 进程生命周期（§9.2）。
//
// 这个文件只管三件事：什么时候该退、退的时候按什么顺序收尾、收尾超时了怎么办。
// 具体每个收尾动作做什么由 app.go 注册进来 —— 这样关机顺序可以被测试单独验证，
// 不需要真的起一个进程。
//
// 顺序约定是 LIFO（后注册先执行，等同 defer 的语义）：
// 谁先建立依赖，谁就最后拆。所以 app.go 里第一个注册的是关库，
// 它一定在最后执行，因为前面的收尾动作还要写库。
//
// 两条不能违反的规则：
//  1. 收尾有宽限时间上限，超时也必须退 —— 卡住的收尾不能把进程吊死在那里。
//  2. 收尾只释放平台自己持有的东西（连接、句柄、辅助进程），
//     绝不关闭用户自己的客户端窗口（§9.2，见 job_windows.go 的进程归属检查）。

// Phase 进程当前所处阶段。
type Phase int

const (
	// PhaseRunning 正常运行。
	PhaseRunning Phase = iota
	// PhaseDraining 已决定退出，正在按顺序收尾。
	PhaseDraining
	// PhaseStopped 收尾结束（或已放弃）。
	PhaseStopped
)

// String 便于日志阅读。
func (p Phase) String() string {
	switch p {
	case PhaseRunning:
		return "running"
	case PhaseDraining:
		return "draining"
	case PhaseStopped:
		return "stopped"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

// shutdownStep 一个收尾动作。
type shutdownStep struct {
	name string
	fn   func(context.Context) error
}

// Lifecycle 进程生命周期与关机编排。
//
// 零值不可用，必须用 NewLifecycle 构造。
type Lifecycle struct {
	grace time.Duration
	logf  func(format string, args ...any)

	baseCtx context.Context
	cancel  context.CancelFunc

	mu     sync.Mutex
	phase  Phase
	steps  []shutdownStep
	reason string
	err    error
	forced bool

	stopOnce sync.Once
	stopped  chan struct{}
	signals  chan os.Signal
}

// NewLifecycle 构造生命周期管理器。
//
// grace <= 0 时取 20 秒：宁可给收尾留够时间，也不要让用户以为进程挂死。
// logf 为 nil 时静默（测试用）。
func NewLifecycle(grace time.Duration, logf func(format string, args ...any)) *Lifecycle {
	if grace <= 0 {
		grace = 20 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Lifecycle{
		grace:   grace,
		logf:    logf,
		baseCtx: ctx,
		cancel:  cancel,
		phase:   PhaseRunning,
		stopped: make(chan struct{}),
		signals: make(chan os.Signal, 2),
	}
}

// Context 进程级 context。进程开始收尾时被取消。
//
// 后台循环（事件泵、SSE 扇出）都从它派生，这样它们能跟着进程一起退。
func (l *Lifecycle) Context() context.Context { return l.baseCtx }

// Grace 关机宽限时间。
func (l *Lifecycle) Grace() time.Duration { return l.grace }

// OnShutdown 注册一个收尾动作。
//
// 执行顺序与注册顺序相反（LIFO）。必须在 Wait 之前注册；
// 重复注册同名步骤不报错，但会被当成两个独立步骤依次执行。
func (l *Lifecycle) OnShutdown(name string, fn func(context.Context) error) {
	if fn == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, shutdownStep{name: name, fn: fn})
}

// WatchSignals 开始监听退出信号（Ctrl+C / SIGTERM）。
//
// 重复调用无副作用。
func (l *Lifecycle) WatchSignals() {
	signal.Notify(l.signals, os.Interrupt, os.Kill)
}

// Wait 阻塞到进程该退为止，然后执行收尾并返回收尾结果。
//
// 返回值：
//   - nil：按顺序收尾完成；
//   - 非 nil：某个收尾动作失败或超时（errors.Join 聚合），或收到第二个信号被强制中断。
//
// 注意：即使返回 error，进程也已经处于可以安全退出的状态 —— 收尾是尽力而为，
// 失败只影响"退得干不干净"，不影响"该不该退"。
func (l *Lifecycle) Wait() error {
	select {
	case sig := <-l.signals:
		l.Stop("收到退出信号 " + signalName(sig))
	case <-l.stopped:
		// 已经被别人 Stop 了，直接等收尾结束。
	}
	<-l.stopped

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Stop 主动触发退出（例如主循环出错、web 服务监听失败）。
//
// 幂等：只有第一次调用真正执行收尾。reason 会写进日志，便于事后判断是谁先要退的。
func (l *Lifecycle) Stop(reason string) {
	l.stopOnce.Do(func() {
		l.runShutdown(reason)
	})
}

// Phase 当前阶段。
func (l *Lifecycle) Phase() Phase {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.phase
}

// Reason 触发退出的原因。
func (l *Lifecycle) Reason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

// Forced 是否因为第二个信号而放弃了宽限时间。
func (l *Lifecycle) Forced() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forced
}

// runShutdown 按 LIFO 执行收尾。只在 Stop 的 once 里被调用一次。
func (l *Lifecycle) runShutdown(reason string) {
	l.mu.Lock()
	l.reason = reason
	l.phase = PhaseDraining
	steps := make([]shutdownStep, len(l.steps))
	copy(steps, l.steps)
	l.mu.Unlock()

	l.logf("开始退出：%s（宽限 %s，%d 个收尾步骤）", reason, l.grace, len(steps))

	ctx, cancel := context.WithTimeout(context.Background(), l.grace)
	defer cancel()

	// 收尾期间再收到信号 = 用户不耐烦了，放弃宽限立刻收尾。
	// 这一步是必要的：某个收尾动作卡住时，用户唯一能做的就是再按一次 Ctrl+C，
	// 如果这里不响应，进程会一直吊到宽限时间结束，看起来就是"关不掉"。
	go l.watchSecondSignal(ctx, cancel)

	var errs []error
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("app: 收尾超时，步骤 %q 未执行", st.name))
			l.logf("收尾超时，跳过步骤 %q", st.name)
			continue
		}
		start := time.Now()
		if err := safeStep(ctx, st); err != nil {
			errs = append(errs, fmt.Errorf("app: 收尾步骤 %q 失败: %w", st.name, err))
			l.logf("收尾步骤 %q 失败：%v", st.name, err)
			continue
		}
		l.logf("收尾步骤 %q 完成（%s）", st.name, time.Since(start).Round(time.Millisecond))
	}

	l.mu.Lock()
	l.phase = PhaseStopped
	l.err = errors.Join(errs...)
	l.mu.Unlock()

	// 最后才取消进程级 context：前面的收尾动作可能还要用它派生的 ctx。
	l.cancel()
	close(l.stopped)
}

// watchSecondSignal 收尾期间监听第二个信号，收到就放弃宽限。
func (l *Lifecycle) watchSecondSignal(ctx context.Context, cancel context.CancelFunc) {
	select {
	case sig := <-l.signals:
		l.mu.Lock()
		l.forced = true
		l.mu.Unlock()
		l.logf("收到第二个信号 %s，放弃宽限时间立即收尾", signalName(sig))
		cancel()
	case <-ctx.Done():
	}
}

// safeStep 执行一个收尾步骤，把 panic 也转成错误。
//
// 收尾阶段的 panic 如果放出去，整个进程会带着半拆的状态崩掉 ——
// 剩下还没执行的步骤全丢，那比"这一步失败"糟得多。
func safeStep(ctx context.Context, st shutdownStep) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return st.fn(ctx)
}

// signalName 信号的可读名字。Windows 上 os.Kill 与 SIGTERM 在信号表里可能重名，
// 这里只用于日志，不做分支判断。
func signalName(sig os.Signal) string {
	if sig == nil {
		return "unknown"
	}
	return sig.String()
}
