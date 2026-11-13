// 本文件刻意不加 //go:build windows。
//
// 锁是纯标准库实现，不碰任何 Win32 API，所以它在任何平台都能编译、能测试。
// 这一点很重要：本机没有 Go 工具链，锁的正确性是本包里唯一能在有 Go 的环境
// 下立刻用 `go test ./internal/desktop/` 验证的部分（其余文件依赖 user32.dll，
// 只能在 Windows 上编译运行）。把锁的测试也锁死在 windows tag 后面，等于
// 放弃了唯一可验证的抓手。

package desktop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 全局桌面操作锁
// ---------------------------------------------------------------------------
//
// 方案 §4.2.2 / §12 要求：同一时刻全进程只允许 1 个需要焦点的桌面操作。
// 聚焦、键盘、鼠标、剪贴板动作必须串行，因为它们在操作系统层面共享同一份
// 全局状态——前台窗口只有一个，剪贴板只有一个。两个 bot 同时打字，结果
// 一定是两边的字混进同一个输入框，且无法事后区分谁是谁的。
//
// 本锁是包级单例：desktopLock 类型不导出，构造它的唯一途径就是包内那个
// 全局变量，外部包无法 new 出第二把锁，也就无法绕过串行化。
//
// ---------------------------------------------------------------------------
// 持有粒度：段落式，不是会话式
// ---------------------------------------------------------------------------
//
// 这是本锁最容易被写错的地方，必须讲清楚。
//
// 错误做法（会话式持有）：抢到锁 → 聚焦 → 注入 → 提交 → 等客户端生成完
// → 读回结果 → 才释放锁。客户端生成一轮可能要几十秒到几分钟，这段时间里
// 锁被一个 bot 独占，其他 bot 全部排队，用户自己也没法用那个窗口——因为
// 一旦用户点了别的窗口，我们的焦点预检就会失败。这等于把用户的电脑锁死了。
//
// 正确做法（段落式持有）：只在真正需要独占屏幕的瞬间持锁。
//
//	段落 A：聚焦 + 注入文本 + 提交          ← 持锁，通常几十到几百毫秒
//	（释放锁）
//	段落 B：等待客户端生成内容              ← 不持锁，可能几分钟
//	段落 C：聚焦 + 核实结果 + 释放焦点      ← 重新持锁
//
// 关键点：段落 B 必须完全在锁外。等待期间其他 bot 可以自由使用桌面，
// 用户可以自由切窗口、复制粘贴、干自己的活。这也是为什么本包不提供
// "持有到发送完成"这种便利 API——那种 API 会诱使调用方写错。
//
// 每个段落都必须独立 Acquire，且 Acquire 失败（超时）时调用方应当放弃本次
// 操作并如实上报，而不是无限重试——无限重试会在锁竞争激烈时形成活锁。
//
// ---------------------------------------------------------------------------
// 使用约定
// ---------------------------------------------------------------------------
//
//	release, err := desktop.AcquireDesktop(ctx)
//	if err != nil { return err }
//	defer release()   // 兜底，异常路径也一定释放
//
//	// 只在这里做需要焦点的动作：聚焦、注入、提交
//	release()         // 段落 A 结束，立刻释放
//
//	// 等客户端生成 —— 不持锁，可能几分钟
//
//	release2, err := desktop.AcquireDesktop(ctx)   // 段落 C 重新取锁
//	if err != nil { return err }
//	defer release2()
//	// 聚焦 + 核实结果
//
// release 是幂等的（内部 sync.Once），重复调用安全，因此 defer release()
// 与显式提前 release 可以共存——这正是段落式持有需要的：段落 A 结束后要
// 立刻释放，但异常路径上还得靠 defer 兜底。
//
// 本锁不可重入。同一个 goroutine 连续 AcquireDesktop 两次会一直阻塞到 ctx
// 超时。这是刻意不做重入支持的：重入会让"哪里真正持有锁"变得不可推理，而
// 段落式持有的正确性完全依赖持有时长可测量。需要嵌套时请把逻辑合并到一个
// 段落里。
// ---------------------------------------------------------------------------

// ErrLockTimeout 表示在 context 期限内没能拿到桌面锁。
//
// 调用方应当把它当作"本次桌面操作没做，需要稍后重试或降级"，绝不能当作
// "操作失败但可能已经生效"——拿到这个错误时我们一个字节都没往目标窗口写。
var ErrLockTimeout = errors.New("desktop: 等待全局桌面锁超时")

// ErrLockNilContext 表示调用方传了 nil context。
// 不接受 nil 是刻意的：没有期限的等待等于可能永久挂死，而本包的硬性要求是
// 所有等待都必须有超时。
var ErrLockNilContext = errors.New("desktop: Acquire 需要非 nil 的 context")

// desktopLock 是包级单例锁的实现。
//
// 用带缓冲 channel 而不是 sync.Mutex，是因为 sync.Mutex 的 Lock() 无法被
// context 打断——一个 bot 拿着锁不放，其他 bot 就只能干等到它释放，没有任何
// 超时手段。channel 可以 select，这是唯一能同时满足"互斥"和"context 超时"
// 的标准库原语。
type desktopLock struct {
	// sem 容量为 1：放进去代表锁被持有，取出来代表释放。
	sem chan struct{}

	// 诊断信息，仅在拿不到锁时用于生成可读的错误/日志。
	// 用独立 mutex 保护，避免与 sem 的获取路径互相干扰。
	mu     sync.Mutex
	holder string    // 当前持有者描述
	since  time.Time // 当前持有者何时拿到的锁
	waited int       // 累计等待次数，仅诊断用
}

// globalLock 是全进程唯一的桌面操作锁。
//
// 只有这一个实例。desktopLock 类型不导出，所以外部包拿不到、也造不出第二把。
var globalLock = &desktopLock{
	sem: make(chan struct{}, 1),
}

// AcquireDesktop 获取全局桌面操作锁。
//
// 成功时返回幂等的 release 函数。失败时返回 ErrLockTimeout（或 ctx 自身的
// 错误），且保证没有对桌面做任何改动。
//
// ctx 必须带期限。传 nil 会被拒绝（ErrLockNilContext）。
//
// 名字里带 Desktop 是为了让调用点一眼看出这把锁保护的是整个桌面（前台焦点 +
// 键盘 + 剪贴板），而不是某个结构体的内部状态。它不保护任何数据，只保护
// "同一时刻只有一个 bot 在动屏幕"这一物理事实。
func AcquireDesktop(ctx context.Context) (release func(), err error) {
	return acquire(ctx, "")
}

// Acquire 是 AcquireDesktop 的旧名别名。
//
// 保留它是因为 task-2 的验收描述与 README 早期草稿都写作 Acquire；两个名字
// 指向同一个函数，不存在第二把锁。新代码请用 AcquireDesktop。
func Acquire(ctx context.Context) (release func(), err error) {
	return AcquireDesktop(ctx)
}

// acquire 是 Acquire 的内部实现，purpose 只用于诊断日志，不参与任何判断。
func acquire(ctx context.Context, purpose string) (func(), error) {
	if ctx == nil {
		return nil, ErrLockNilContext
	}

	// 已经取消的 ctx 不要白试一次：如果锁恰好空闲，select 会随机选中两个
	// 就绪分支中的一个，可能让我们拿到锁却立刻又发现 ctx 已死。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case globalLock.sem <- struct{}{}:
		// 拿到锁。
	default:
		// 锁被占用，进入可打断的等待。
		globalLock.mu.Lock()
		globalLock.waited++
		globalLock.mu.Unlock()

		select {
		case globalLock.sem <- struct{}{}:
			// 等到了。
		case <-ctx.Done():
			// 同时包装 ErrLockTimeout 和 ctx 的原因：调用方既可能想统一按
			// "锁超时"处理，也可能想区分"期限到了"和"被主动取消"。
			// 两条错误链都保留，两种判断都能用 errors.Is 命中。
			return nil, fmt.Errorf("%w: %s（底层原因: %w）",
				ErrLockTimeout, globalLock.describeHolder(), ctx.Err())
		}
	}

	globalLock.mu.Lock()
	globalLock.holder = purpose
	globalLock.since = time.Now()
	globalLock.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			globalLock.mu.Lock()
			globalLock.holder = ""
			globalLock.since = time.Time{}
			globalLock.mu.Unlock()
			// 取出信号量，唤醒一个等待者。
			<-globalLock.sem
		})
	}, nil
}

// describeHolder 生成"谁占着锁、占了多久"的可读描述，供超时错误使用。
//
// 拿不到锁时最想知道的就是这个：是别的 bot 在打字，还是我们自己某处忘了
// 释放。后者会表现为持有时间异常长。
func (l *desktopLock) describeHolder() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == "" {
		return "锁被占用（持有者未标记用途）"
	}
	return fmt.Sprintf("锁被 %q 持有已 %s", l.holder, time.Since(l.since).Round(time.Millisecond))
}

// Held 报告桌面锁当前是否被持有。仅用于诊断与测试断言，业务逻辑不应依赖
// 它的返回值做决策——检查与使用之间存在竞态，这不是原子的。
func Held() bool {
	select {
	case globalLock.sem <- struct{}{}:
		<-globalLock.sem
		return false
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// 可打断的等待
// ---------------------------------------------------------------------------

// sleepCtx 睡 d 但响应 ctx 取消。
//
// 本包所有重试退避都必须走这个函数，不许直接 time.Sleep / Sleep：裸 Sleep
// 不响应取消，会导致"用户已经取消了这个 bot 的任务，它还在后台傻等两分钟"。
// 返回 ctx.Err() 表示没睡完就被取消，调用方应当立刻退出重试循环。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResetForTest 清空锁的诊断计数，仅测试使用。
//
// 不重置 sem 本身：测试如果在持锁状态下调用它，语义上没有意义，而且会掩盖
// 真正的泄漏 bug。它只清统计字段。
func resetForTest() {
	globalLock.mu.Lock()
	defer globalLock.mu.Unlock()
	globalLock.waited = 0
}
