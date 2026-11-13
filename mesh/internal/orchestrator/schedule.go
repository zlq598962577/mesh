package orchestrator

import (
	"errors"
	"sync"
)

// 并发与公平（方案 §5.6）。
//
// 三条规则：
//   1. 同一个 bot 同时只能有一个 Run —— 桌面客户端是单实例独占的。
//   2. 全局默认最多 2 个活跃 Run（MaxActiveRuns）。
//   3. 跨 Work 公平轮转：每个 Work 排一条队，按 Work 轮转取，不让一个 Work
//      连续占满两个名额把别人饿死。
//
// 第四条最容易被忽略：等待外部（等用户审批、等客户端回话）时必须把执行名额
// 还回去，否则两个卡住的 Work 会把全局名额占死，第三个 Work 永远启动不了。
// 所以 Release 和"Run 结束"是两件不同的事 —— WaitExternal 只释放名额，不结束 Run。

// MaxActiveRuns 全局活跃 Run 上限（§5.6）。
const MaxActiveRuns = 2

// ErrSchedulerFull 名额已满。
var ErrSchedulerFull = errors.New("orchestrator: 没有空闲执行名额")

// ErrBotBusy 该 bot 已经有一个 Run 在跑。
var ErrBotBusy = errors.New("orchestrator: 该 bot 已有 Run 在执行")

// RunTicket 一个已获准执行的 Run。
type RunTicket struct {
	WorkID string
	BotID  string
	RunID  string
	// released 防止重复归还名额。
	released bool
}

// schedEntry 一个排队中的请求。
type schedEntry struct {
	ticket RunTicket
}

// scheduler 名额与公平轮转。纯内存，进程重启即重置（Run 本来就是易失的）。
type scheduler struct {
	mu sync.Mutex
	// maxActive 全局活跃 Run 上限。默认 MaxActiveRuns=2（§5.6 / §12），
	// 可通过 SetMaxActive 按启动参数覆盖。
	maxActive int
	// active 当前占着名额的 Run，key 是 RunID。
	active map[string]*RunTicket
	// byBot 每个 bot 当前的 RunID，保证每 bot 串行。
	byBot map[string]string
	// queues 每个 Work 的待启动队列。
	queues map[string][]*schedEntry
	// order Work 的轮转顺序。
	order []string
	// cursor 轮转指针。
	cursor int
}

func newScheduler() *scheduler {
	return &scheduler{
		maxActive: MaxActiveRuns,
		active:    map[string]*RunTicket{},
		byBot:     map[string]string{},
		queues:    map[string][]*schedEntry{},
	}
}

// SetMaxActive 覆盖活跃 Run 上限（≤0 视为无效，保持现值）。
func (s *scheduler) SetMaxActive(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxActive = n
}

// TryStart 尝试为某个 Work 的某个 bot 启动一个 Run。
//
// 成功返回 ticket；名额满返回 ErrSchedulerFull（调用方应把 Work 留在 queued，
// 等别人 Release 后再试，而不是报错给用户）；该 bot 忙返回 ErrBotBusy
// （调用方应排队，因为这是串行约束不是错误）。
func (s *scheduler) TryStart(workID, botID, runID string) (*RunTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, busy := s.byBot[botID]; busy {
		return nil, ErrBotBusy
	}
	if len(s.active) >= s.maxActive {
		return nil, ErrSchedulerFull
	}

	t := &RunTicket{WorkID: workID, BotID: botID, RunID: runID}
	s.active[runID] = t
	s.byBot[botID] = runID
	return t, nil
}

// Enqueue 把一个暂时起不来的 Work 放进它自己的队列，并按轮转顺序登记。
func (s *scheduler) Enqueue(workID, botID, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queues[workID]; !ok {
		s.order = append(s.order, workID)
	}
	s.queues[workID] = append(s.queues[workID], &schedEntry{
		ticket: RunTicket{WorkID: workID, BotID: botID, RunID: runID},
	})
}

// Next 按 Work 轮转取下一个能启动的排队请求。
//
// 轮转而不是 FIFO：FIFO 会让先来的 Work 把它自己的十个 Run 全排前面，
// 后到的 Work 等到超时。这里每次从一个 Work 取一个就换下一个 Work。
// 取不到（名额满 / 该 bot 忙）返回 nil，调用方等下一次 Release。
func (s *scheduler) Next() *RunTicket {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.active) >= s.maxActive || len(s.order) == 0 {
		return nil
	}

	for tried := 0; tried < len(s.order); tried++ {
		if s.cursor >= len(s.order) {
			s.cursor = 0
		}
		workID := s.order[s.cursor]
		s.cursor++

		q := s.queues[workID]
		if len(q) == 0 {
			continue
		}
		head := q[0]
		if _, busy := s.byBot[head.ticket.BotID]; busy {
			// 这个 bot 还忙着，跳过它，别让整个队列卡在这一个 Work 上。
			continue
		}

		s.queues[workID] = q[1:]
		t := head.ticket
		tk := &RunTicket{WorkID: t.WorkID, BotID: t.BotID, RunID: t.RunID}
		s.active[tk.RunID] = tk
		s.byBot[tk.BotID] = tk.RunID
		return tk
	}
	return nil
}

// Release 归还执行名额。
//
// 幂等：同一个 ticket 调两次不会把名额还成负数（等待外部时调用方可能
// 在多个路径上都调一次）。
func (s *scheduler) Release(t *RunTicket) {
	if t == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(t)
}

func (s *scheduler) releaseLocked(t *RunTicket) {
	if t.released {
		return
	}
	t.released = true
	if _, ok := s.active[t.RunID]; ok {
		delete(s.active, t.RunID)
	}
	if cur, ok := s.byBot[t.BotID]; ok && cur == t.RunID {
		delete(s.byBot, t.BotID)
	}
}

// Active 当前活跃 Run 数（用于测试和 UI 展示）。
func (s *scheduler) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// Queued 当前排队数。
func (s *scheduler) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.queues {
		n += len(q)
	}
	return n
}

// ForgetWork Work 终结时清掉它的队列，避免内存泄漏和幽灵排队。
func (s *scheduler) ForgetWork(workID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.queues, workID)
	for i, id := range s.order {
		if id == workID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			if s.cursor > i {
				s.cursor--
			}
			break
		}
	}
}

// ---- 对外接口：调用方驱动引擎前先拿名额 ----
//
// 用法（顺序不能乱）：
//
//	t, err := o.StartRun(workID, botID, runID)
//	if errors.Is(err, ErrSchedulerFull) || errors.Is(err, ErrBotBusy) {
//	    // 排队了，等别人 FinishRun/WaitExternal 之后再调 o.NextRun()
//	}
//	defer o.FinishRun(t)
//	// 中途要等用户审批 / 等客户端回话时：
//	o.WaitExternal(t) // 只还名额，Run 没结束
//
// 注意 StartRun 返回 ErrSchedulerFull / ErrBotBusy 时已经自动入队了，
// 调用方不需要再调一次 Enqueue，也不要把它当错误报给用户 —— 这是正常排队。

// StartRun 申请一个执行名额；拿不到会自动排队。
//
// 返回的 error 只用于区分"现在能跑"和"已排队"，两者都不是故障。
func (o *Orchestrator) StartRun(workID, botID, runID string) (*RunTicket, error) {
	t, err := o.sched.TryStart(workID, botID, runID)
	if err != nil {
		o.sched.Enqueue(workID, botID, runID)
		return nil, err
	}
	return t, nil
}

// NextRun 按跨 Work 公平轮转取下一个能启动的 Run；没有则返回 nil。
func (o *Orchestrator) NextRun() *RunTicket { return o.sched.Next() }

// SetMaxActiveRuns 覆盖全局活跃 Run 上限（§5.6 默认 2）。≤0 忽略。
// 供组装层把启动参数 -max-active-runs 接进来；不接通的话参数只是摆设。
func (o *Orchestrator) SetMaxActiveRuns(n int) { o.sched.SetMaxActive(n) }

// WaitExternal 等待外部时归还执行名额，但 Run 本身不结束。
//
// 不调这个就是 §5.6 说的死锁：两个卡在等审批的 Work 把 2 个名额占满，
// 第三个 Work 永远起不来。归还后调用方仍持有 ticket，后续照常 FinishRun。
func (o *Orchestrator) WaitExternal(t *RunTicket) { o.sched.Release(t) }

// FinishRun 结束一个 Run 并归还名额（幂等）。
func (o *Orchestrator) FinishRun(t *RunTicket) { o.sched.Release(t) }

// ActiveRuns 当前活跃 Run 数（UI 展示用）。
func (o *Orchestrator) ActiveRuns() int { return o.sched.Active() }

// QueuedRuns 当前排队 Run 数（UI 展示用）。
func (o *Orchestrator) QueuedRuns() int { return o.sched.Queued() }
