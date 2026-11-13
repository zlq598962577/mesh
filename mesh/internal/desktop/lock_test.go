// 本文件刻意不加 //go:build windows。
//
// lock.go 是纯标准库实现，所以它的测试在任何平台都能编译运行。这是本包里
// 唯一可以在有 Go 工具链的环境下立刻验证的部分。
//
// 【重要】本机没有 Go 工具链，这些测试**写出来但从未运行过**。它们不是
// "通过的测试"，只是"准备好待运行的测试"。任何人在 CI 里第一次跑它们时，
// 应当预期可能需要小幅修正（尤其是时序相关的断言）。

package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testTimeout 是测试里等待锁的默认期限。
//
// 故意取得比较宽松（2 秒）：这些测试断言的是"能不能拿到锁"，不是"多快能
// 拿到"。用太紧的期限会让测试在负载高的 CI 机器上随机失败。
const testTimeout = 2 * time.Second

// shortTimeout 用于断言"这次一定拿不到锁"。
//
// 100 毫秒足够让一个必然阻塞的 Acquire 完成 select 并超时，又不至于让整个
// 测试包变慢。
const shortTimeout = 100 * time.Millisecond

// TestAcquireReleaseBasic 验证最基本的取锁/放锁。
func TestAcquireReleaseBasic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	release, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("第一次 AcquireDesktop 应当成功，却返回错误: %v", err)
	}
	if !Held() {
		t.Error("拿到锁之后 Held() 应当返回 true")
	}

	release()

	if Held() {
		t.Error("释放之后 Held() 应当返回 false")
	}
}

// TestReleaseIsIdempotent 验证 release 可以被重复调用。
//
// 这不是吹毛求疵：段落式持有的标准写法是 `defer release()` 加显式提前
// `release()`，两者必然同时发生。如果 release 不幂等，每次正常路径都会
// 因为重复释放而 panic 或把信号量搞坏。
func TestReleaseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	release, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("AcquireDesktop 失败: %v", err)
	}

	// 连调三次，不能 panic，也不能把信号量多取出。
	release()
	release()
	release()

	if Held() {
		t.Fatal("多次 release 之后锁不应处于持有状态")
	}

	// 关键断言：重复 release 没有破坏信号量，锁仍然可正常获取。
	release2, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("重复 release 破坏了锁状态，无法再次获取: %v", err)
	}
	release2()
}

// TestMutualExclusion 验证同一时刻只有一个持有者。
//
// 这是本锁存在的全部理由：如果这个测试失败，两个 bot 会同时往屏幕上打字。
func TestMutualExclusion(t *testing.T) {
	resetForTest()

	const goroutines = 8
	const holdFor = 20 * time.Millisecond

	var (
		concurrent int32 // 当前同时持锁的 goroutine 数
		maxSeen    int32 // 观察到的最大并发持有数
		acquired   int32 // 成功拿到锁的总次数
		wg         sync.WaitGroup
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()

			release, err := AcquireDesktop(ctx)
			if err != nil {
				// 超时在高竞争下是允许的（锁确实可能被占满 2 秒），
				// 但此时绝不能已经持锁。
				t.Logf("goroutine 获取锁超时（可接受）: %v", err)
				return
			}

			n := atomic.AddInt32(&concurrent, 1)
			// 记录历史最大值。
			for {
				old := atomic.LoadInt32(&maxSeen)
				if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
					break
				}
			}

			atomic.AddInt32(&acquired, 1)

			// 持锁一小会儿，制造真实的竞争窗口。
			time.Sleep(holdFor)

			atomic.AddInt32(&concurrent, -1)
			release()
		}()
	}

	wg.Wait()

	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Fatalf("互斥被破坏：同时有 %d 个持有者（期望 1）", got)
	}
	if got := atomic.LoadInt32(&acquired); got == 0 {
		t.Fatal("没有任何 goroutine 拿到锁，说明锁被永久占住了")
	}
	if Held() {
		t.Fatal("全部 goroutine 结束后锁不应仍被持有（有泄漏）")
	}
}

// TestAcquireTimeout 验证拿不到锁时按 context 期限返回。
//
// 场景：一个持有者长期不放，另一个调用方必须能在期限内失败退出，
// 而不是无限挂起。
func TestAcquireTimeout(t *testing.T) {
	ctx := context.Background()

	// 先占住锁，不释放。
	holderRelease, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("占锁失败: %v", err)
	}
	defer holderRelease() // 兜底，避免污染后续测试

	// 第二个调用方应当超时。
	waitCtx, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()

	start := time.Now()
	release, err := AcquireDesktop(waitCtx)
	elapsed := time.Since(start)

	if err == nil {
		release()
		t.Fatal("锁被占用时第二次 AcquireDesktop 不应成功")
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("错误应当匹配 ErrLockTimeout，实际为: %v", err)
	}
	// acquire 用两个 %w 同时包装了 ErrLockTimeout 和 ctx 的原因，
	// 所以两种判断都能命中。这里验证第二条链确实存在。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误链中应当包含 context.DeadlineExceeded，实际为: %v", err)
	}
	if elapsed > testTimeout {
		t.Fatalf("超时耗时 %v 远超期限 %v，说明等待没有被 context 打断", elapsed, shortTimeout)
	}
}

// TestAcquireNilContext 验证 nil context 被拒绝。
//
// 没有期限的等待等于可能永久挂死，所以必须在入口拦住。
func TestAcquireNilContext(t *testing.T) {
	//nolint:staticcheck // 这里就是要传 nil 来验证防护
	release, err := AcquireDesktop(nil)
	if err == nil {
		if release != nil {
			release()
		}
		t.Fatal("nil context 应当被拒绝")
	}
	if !errors.Is(err, ErrLockNilContext) {
		t.Fatalf("错误应当是 ErrLockNilContext，实际为: %v", err)
	}
}

// TestAcquireCancelledContext 验证已取消的 ctx 立刻失败且不拿到锁。
func TestAcquireCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	release, err := AcquireDesktop(ctx)
	if err == nil {
		release()
		t.Fatal("已取消的 context 不应拿到锁")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应当是 context.Canceled，实际为: %v", err)
	}
	if Held() {
		t.Fatal("已取消的 context 却把锁拿走了（泄漏）")
	}
}

// TestReleaseWakesWaiter 验证释放能唤醒等待者。
//
// 这是段落式持有的核心机制：段落 A 结束 release()，段落 C 才能开始。
// 如果释放不能唤醒等待者，整个适配器层会永久卡死。
func TestReleaseWakesWaiter(t *testing.T) {
	ctx := context.Background()

	holderRelease, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("占锁失败: %v", err)
	}

	// 等待者：在另一个 goroutine 里等锁。
	got := make(chan error, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
		defer cancel()

		release, err := AcquireDesktop(waitCtx)
		if err != nil {
			got <- err
			return
		}
		release()
		got <- nil
	}()

	// 给等待者一点时间真正进入等待状态。
	time.Sleep(50 * time.Millisecond)

	// 释放，等待者应当被唤醒。
	holderRelease()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("释放后等待者应当拿到锁，却失败: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("释放后等待者没有被唤醒（死锁）")
	}
}

// TestNonReentrant 验证同一 goroutine 不能重入。
//
// 这是刻意的设计选择，用测试固定住它，避免后来者"顺手"加上重入支持而
// 破坏段落式持有的可推理性和持有时长可测量性。
func TestNonReentrant(t *testing.T) {
	ctx := context.Background()

	release, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("占锁失败: %v", err)
	}
	defer release()

	// 第二次获取应当阻塞直到超时。
	inner, cancel := context.WithTimeout(ctx, shortTimeout)
	defer cancel()

	release2, err := AcquireDesktop(inner)
	if err == nil {
		release2()
		t.Fatal("本锁不可重入，第二次 AcquireDesktop 应当超时失败")
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("错误应当是 ErrLockTimeout，实际为: %v", err)
	}
}

// TestSegmentStyleHolding 演示并验证段落式持有的正确用法。
//
// 这个测试的用途是文档：它把 README 里那张时序图变成可执行的代码，说明
// "生成期间必须释放锁"到底长什么样。将来若有人把锁改成会话式持有，这个
// 测试会暴露问题——因为段落 B 期间另一个 goroutine 必须能拿到锁。
func TestSegmentStyleHolding(t *testing.T) {
	ctx := context.Background()

	// 段落 A：注入 + 提交。
	releaseA, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("段落 A 取锁失败: %v", err)
	}
	// ... 真实代码在这里聚焦、注入、提交 ...
	releaseA() // 立刻释放，进入段落 B

	// 段落 B：等待客户端生成。此时**不持锁**。
	if Held() {
		t.Fatal("段落 B（等待生成）期间不应持有锁")
	}

	// 关键断言：生成期间别的 bot 必须能拿到锁。
	otherCtx, otherCancel := context.WithTimeout(ctx, shortTimeout)
	defer otherCancel()
	otherRelease, err := AcquireDesktop(otherCtx)
	if err != nil {
		t.Fatalf("生成期间其他 bot 应当能拿到锁，实际失败: %v", err)
	}
	otherRelease()

	// 段落 C：生成完了，重新取锁去核实结果。
	releaseC, err := AcquireDesktop(ctx)
	if err != nil {
		t.Fatalf("段落 C 取锁失败: %v", err)
	}
	// ... 真实代码在这里聚焦、读回结果 ...
	releaseC()
}

// TestSleepCtxCancel 验证可打断等待确实响应取消。
func TestSleepCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		// 睡很久，但应当被立刻取消。
		done <- sleepCtx(ctx, 10*time.Second)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应当返回 context.Canceled，实际为: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("sleepCtx 没有响应取消（裸 Sleep 式的实现）")
	}
}

// TestSleepCtxZero 验证 d<=0 时立刻返回。
func TestSleepCtxZero(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("d=0 应当立刻返回 nil，实际: %v", err)
	}
}
