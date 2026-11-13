package store

// orchestrator.Store 接口实现的行为测试（P0b-5 组装阶段补齐）。
//
// 覆盖点：Work/Message/Review/Assignment 往返、MarkInputSeen 幂等、
// PendingFor/RevokePending 状态机、AppendEvent 幂等语义。
// 全部走真实 SQLite（store.Open 完整初始化路径：迁移+写队列+实例锁）。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"mesh/internal/contract"
	"mesh/internal/orchestrator"
)

// openTestStore 建一个走完整初始化路径的临时 Store（与 openTestDB 的裸库不同：
// 这里的库带 V2 schema 与串行写队列，行为与生产一致）。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open(%s) 失败: %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRoom 建 rooms 行（works/messages 有外键）。
func seedRoom(t *testing.T, s *Store, roomID string) {
	t.Helper()
	ctx := context.Background()
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO rooms (id, title, created_at) VALUES (?, ?, ?)`,
			roomID, "测试房间", time.Now().UnixMilli())
		return err
	})
	if err != nil {
		t.Fatalf("seedRoom(%s) 失败: %v", roomID, err)
	}
}

func TestOrchestratorStoreWorkRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedRoom(t, s, "room-1")

	w := contract.Work{
		ID:     "work-1",
		RoomID: "room-1",
		Goal:   "把 README 翻译成英文",
		State:  contract.WorkExecuting,
		Assign: contract.RoleAssignment{
			Version: 1, Executor: "codex", Reviewer: "dsh", Source: contract.SourceUserChat,
			At: time.Now(),
		},
		Artifacts: []contract.ArtifactRef{{Version: 1, Ref: "readme.en.md", Author: "codex"}},
		Budget:    &contract.Budget{DailyTokens: 100000, MaxStepsPerTurn: 20, MaxTurnSeconds: 600},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.SaveWork(ctx, w); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}

	got, err := s.GetWork(ctx, "work-1")
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if got.Goal != w.Goal || got.State != w.State {
		t.Fatalf("往返不一致: goal=%q state=%q", got.Goal, got.State)
	}
	if got.Assign.Executor != "codex" || got.Assign.Reviewer != "dsh" {
		t.Fatalf("分工丢失: %+v", got.Assign)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Ref != "readme.en.md" {
		t.Fatalf("产物丢失: %+v", got.Artifacts)
	}
	if got.Budget == nil || got.Budget.DailyTokens != 100000 {
		t.Fatalf("预算丢失: %+v", got.Budget)
	}

	// 更新（换状态）：upsert 不产生第二行。
	w.State = contract.WorkCompleted
	w.Final = &contract.ArtifactRef{Version: 1, Ref: "readme.en.md", Author: "codex"}
	if err := s.SaveWork(ctx, w); err != nil {
		t.Fatalf("SaveWork(upsert): %v", err)
	}
	got2, err := s.GetWork(ctx, "work-1")
	if err != nil {
		t.Fatalf("GetWork(2): %v", err)
	}
	if got2.State != contract.WorkCompleted {
		t.Fatalf("更新后状态 = %q，期望 completed", got2.State)
	}

	// 查不到 → orchestrator.ErrNotFound。
	if _, err := s.GetWork(ctx, "work-missing"); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("GetWork(missing) 错误 = %v，期望 ErrNotFound", err)
	}

	// ListWorks 按 room + states 过滤。
	list, err := s.ListWorks(ctx, "room-1", []contract.WorkState{contract.WorkCompleted})
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	if len(list) != 1 || list[0].ID != "work-1" {
		t.Fatalf("ListWorks 结果 = %+v", list)
	}
	empty, err := s.ListWorks(ctx, "room-1", []contract.WorkState{contract.WorkQueued})
	if err != nil {
		t.Fatalf("ListWorks(queued): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListWorks(queued) 应为空，得到 %d 条", len(empty))
	}
}

func TestOrchestratorStoreMessageRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedRoom(t, s, "room-1")

	m := contract.Message{
		ID:       "msg-1",
		RoomID:   "room-1",
		WorkID:   "work-1",
		FromID:   "user",
		FromKind: contract.FromUser,
		ToID:     "codex",
		Kind:     contract.MsgTask,
		Text:     "@codex 帮我改一下 README，@dsh 之后审查",
		Mentions: []string{"codex", "dsh"},
		ReplyTo:  "msg-0",
		Files:    []string{"readme.md"},
		// ClientMessageID 故意留空：UNIQUE 允许多个 NULL，去重是 CreateMessage 的职责。
		CreatedAt: time.Now(),
	}
	if err := s.SaveMessage(ctx, m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	got, err := s.GetMessage(ctx, "msg-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Text != m.Text || got.Kind != m.Kind || got.FromID != m.FromID {
		t.Fatalf("往返不一致: %+v", got)
	}
	if len(got.Mentions) != 2 || got.ReplyTo != "msg-0" || len(got.Files) != 1 {
		t.Fatalf("meta 丢失: mentions=%v replyTo=%q files=%v", got.Mentions, got.ReplyTo, got.Files)
	}

	if _, err := s.GetMessage(ctx, "msg-missing"); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("GetMessage(missing) 错误 = %v，期望 ErrNotFound", err)
	}
}

func TestOrchestratorStoreReviewAndAssignment(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedRoom(t, s, "room-1")

	w := contract.Work{
		ID: "work-1", RoomID: "room-1", Goal: "g", State: contract.WorkReviewing,
		Assign: contract.RoleAssignment{Version: 1, Executor: "codex", Reviewer: "dsh"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.SaveWork(ctx, w); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}

	// 两版分工：v1 → v2 换审查者。
	a1 := contract.RoleAssignment{Version: 1, Executor: "codex", Reviewer: "dsh", Source: contract.SourceUserUI, At: time.Now()}
	a2 := contract.RoleAssignment{Version: 2, Executor: "codex", Reviewer: "trae", Source: contract.SourceUserChat, At: time.Now()}
	if err := s.SaveAssignment(ctx, "work-1", a1); err != nil {
		t.Fatalf("SaveAssignment(v1): %v", err)
	}
	if err := s.SaveAssignment(ctx, "work-1", a2); err != nil {
		t.Fatalf("SaveAssignment(v2): %v", err)
	}
	got, err := s.Assignments(ctx, "work-1")
	if err != nil {
		t.Fatalf("Assignments: %v", err)
	}
	if len(got) != 2 || got[0].Version != 1 || got[1].Version != 2 {
		t.Fatalf("分工历史 = %+v，期望按版本升序两版", got)
	}
	if got[1].Reviewer != "trae" {
		t.Fatalf("v2 审查者 = %q，期望 trae", got[1].Reviewer)
	}
	// SaveAssignment 同步 works 行。
	w2, err := s.GetWork(ctx, "work-1")
	if err != nil {
		t.Fatalf("GetWork(2): %v", err)
	}
	if w2.Assign.Version != 2 {
		t.Fatalf("SaveAssignment 后 works.assignment_version = %d，期望 2", w2.Assign.Version)
	}

	// SaveReview 绑定当前分工版本（此刻 works.assignment_version 已是 2）。
	rv := contract.Review{
		ID: "rev-1", WorkID: "work-1", ArtifactVersion: 1, Reviewer: "trae",
		Verdict: contract.VerdictChangesRequested, Findings: "第 3 段没翻译", CreatedAt: time.Now(),
	}
	if err := s.SaveReview(ctx, rv); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}
	revs, err := s.Reviews(ctx, "work-1")
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}
	if len(revs) != 1 || revs[0].Verdict != contract.VerdictChangesRequested {
		t.Fatalf("Reviews = %+v", revs)
	}
	if revs[0].Findings != "第 3 段没翻译" {
		t.Fatalf("findings 往返丢失: %q", revs[0].Findings)
	}

	// 挂到不存在的 Work → ErrNotFound。
	bad := rv
	bad.ID = "rev-bad"
	bad.WorkID = "work-missing"
	if err := s.SaveReview(ctx, bad); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("SaveReview(missing work) 错误 = %v，期望 ErrNotFound", err)
	}
}

func TestMarkInputSeenIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	seen, err := s.MarkInputSeen(ctx, "msg-1", "codex")
	if err != nil {
		t.Fatalf("MarkInputSeen(1): %v", err)
	}
	if seen {
		t.Fatal("首次登记不应返回 seen=true")
	}

	seen, err = s.MarkInputSeen(ctx, "msg-1", "codex")
	if err != nil {
		t.Fatalf("MarkInputSeen(2): %v", err)
	}
	if !seen {
		t.Fatal("第二次登记应返回 seen=true")
	}

	// 不同 bot 是不同的登记项。
	seen, err = s.MarkInputSeen(ctx, "msg-1", "dsh")
	if err != nil {
		t.Fatalf("MarkInputSeen(3): %v", err)
	}
	if seen {
		t.Fatal("不同 bot 首次登记不应返回 seen=true")
	}
}

func TestPendingForRevoke(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedRoom(t, s, "room-1")

	// 建一个 Work + 一条带投递目标的消息（CreateMessage 建 deliveries 行）。
	w := contract.Work{
		ID: "work-1", RoomID: "room-1", Goal: "g", State: contract.WorkExecuting,
		Assign: contract.RoleAssignment{Version: 1, Executor: "codex"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.SaveWork(ctx, w); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	msg := &contract.Message{
		ID: "msg-1", RoomID: "room-1", WorkID: "work-1",
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "开工", CreatedAt: time.Now(),
	}
	if err := s.CreateMessage(ctx, msg, []string{"codex", "dsh"}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	pendings, err := s.PendingFor(ctx, "work-1")
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	if len(pendings) != 2 {
		t.Fatalf("待投递 = %d 条，期望 2（codex + dsh）", len(pendings))
	}

	// 撤销其中一条：撤销后 PendingFor 只剩一条；重复撤销 → ErrNotFound。
	target := pendings[0].ID
	if err := s.RevokePending(ctx, target, "换人"); err != nil {
		t.Fatalf("RevokePending: %v", err)
	}
	if err := s.RevokePending(ctx, target, "换人"); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("重复撤销错误 = %v，期望 ErrNotFound", err)
	}
	left, err := s.PendingFor(ctx, "work-1")
	if err != nil {
		t.Fatalf("PendingFor(2): %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("撤销后剩余 = %d 条，期望 1", len(left))
	}
	// 已撤销的 bot 之后重新登记 input_seen 不受影响（各自独立）。
	if _, err := s.MarkInputSeen(ctx, "msg-1", pendings[0].BotID); err != nil {
		t.Fatalf("MarkInputSeen: %v", err)
	}
}

func TestAppendEventOrchestratorSignature(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedRoom(t, s, "room-1")

	// 事件挂在 Run 下（FK）。ClaimDelivery 会建 Run；这里用 CreateMessage+ClaimDelivery 走正路。
	w := contract.Work{
		ID: "work-1", RoomID: "room-1", Goal: "g", State: contract.WorkExecuting,
		Assign: contract.RoleAssignment{Version: 1, Executor: "codex"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.SaveWork(ctx, w); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	msg := &contract.Message{
		ID: "msg-1", RoomID: "room-1", WorkID: "work-1",
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "开工", CreatedAt: time.Now(),
	}
	if err := s.CreateMessage(ctx, msg, []string{"codex"}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	d, err := s.ClaimDelivery(ctx, time.Now(), "run-1")
	if err != nil {
		t.Fatalf("ClaimDelivery: %v", err)
	}
	_ = d

	ev := contract.Event{
		EventID: "evt-1", WorkID: "work-1", RunID: "run-1", BotID: "codex",
		Seq: 1, Kind: contract.EventProgress, Text: "开始处理",
	}
	if err := s.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// 同 seq 重复写入：幂等静默成功。
	if err := s.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent(重复) 应幂等成功，得到: %v", err)
	}
	events, err := s.EventsByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("EventsByRun: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1（重复被去重）", len(events))
	}
}
