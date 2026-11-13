package app

// P0 集成验收测试（技术方案 §10.1「必须通过的验收场景」的可自动化子集）。
//
// 覆盖方式：真实 store（store.Open 完整初始化：单实例锁 + 迁移 + 串行写队列）+
// 真实编排层 + 真实 web 中间件链；引擎注入可编程 fake（实现 contract.Engine 全部
// 八方法），绝不 import adapters 的真实实现。断言一律读落库状态与 fake 的调用记录，
// 不读编排层内部私有字段；app 包内测试访问 App 的私有装配入口（mux / bridge）仅用于
// 把消息送进引擎与驱动桥接层。
//
// 真实客户端部分（锁屏、焦点、DPI、真实客户端会话恢复、真实审批桥接）无法在本层
// 自动化，对照表见本目录 README.md，一律标「需人工」。
//
// 运行：go test ./internal/app/ -v
// 真实运行结果与已知缺陷（含 TestAccControlPauseMovesWorkToPaused 暴露的
// Control(pause) 接线缺陷）逐行记录在本目录 README.md 的验收对照表里。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mesh/internal/contract"
	"mesh/internal/orchestrator"
	"mesh/internal/store"
	"mesh/internal/web"
)

// ---------------------------------------------------------------------------
// 可编程 fake 引擎：contract.Engine 全部八方法 + 可查询调用记录
// ---------------------------------------------------------------------------

type accFakeSession struct{ id string }

func (s accFakeSession) ID() string { return s.id }

type accFakeTurn struct{ id string }

func (t accFakeTurn) ID() string { return t.id }

// accFakeEngine 是测试专用的桌面客户端替身。
//
// 事件模型：测试用 queueEvent 预置事件，Send 时投递到 Events 通道（与真实客户端
// "提交任务后事件逐步回流"同形）。成功语义遵守 §4.3：fake 客户端进程退出码恒为 0
// （不可信），成功与否只由 Final.Success + SuccessSignal 决定。
type accFakeEngine struct {
	mu        sync.Mutex
	bot       string
	opened    int
	sends     []contract.Message
	cancels   []string
	approvals []string
	suspends  int
	closes    int
	healths   int
	ch        chan contract.Event
	emit      []contract.Event
	errOpen   error
	errSend   error
	// errSendKeepTurn 为 true 时 Send 返回 (turn, errSend)——对应桌面适配器
	// "注入已执行但无法核实是否送达"的 Unknown 结论；false 返回 (nil, errSend)，
	// 对应"确认没写入客户端"。两者的投递处理路径不同（见 dispatch.handle）。
	errSendKeepTurn bool
}

func (e *accFakeEngine) Open(_ context.Context, spec contract.BotSpec, _ contract.Workspace) (contract.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.errOpen != nil {
		return nil, e.errOpen
	}
	e.bot = spec.ID
	e.opened++
	return accFakeSession{id: spec.ID + "-sess-" + strconv.Itoa(e.opened)}, nil
}

func (e *accFakeEngine) Send(_ context.Context, _ contract.Session, msg contract.Message) (contract.Turn, error) {
	e.mu.Lock()
	if e.errSend != nil {
		keep := e.errSendKeepTurn
		e.mu.Unlock()
		if keep {
			return accFakeTurn{id: e.bot + "-turn-unknown"}, e.errSend
		}
		return nil, e.errSend
	}
	e.sends = append(e.sends, msg)
	turnID := fmt.Sprintf("%s-turn-%d", e.bot, len(e.sends))
	pending := e.emit
	e.emit = nil
	if e.ch == nil {
		e.ch = make(chan contract.Event, 32)
	}
	ch := e.ch
	e.mu.Unlock()
	for _, ev := range pending {
		ev.BotID = e.bot
		ch <- ev // 缓冲 32，单次测试预置的事件数远小于此
	}
	return accFakeTurn{id: turnID}, nil
}

func (e *accFakeEngine) Events(contract.Session) <-chan contract.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ch == nil {
		e.ch = make(chan contract.Event, 32)
	}
	return e.ch
}

func (e *accFakeEngine) Cancel(_ context.Context, _ contract.Session, turnID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels = append(e.cancels, turnID)
	return nil
}

func (e *accFakeEngine) ResolveApproval(_ context.Context, _ contract.Session, approvalID string, d contract.Decision) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approvals = append(e.approvals, approvalID+":"+string(d))
	return nil
}

func (e *accFakeEngine) Suspend(contract.Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.suspends++
	return nil
}

func (e *accFakeEngine) Close(contract.Session) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closes++
	return nil
}

func (e *accFakeEngine) Health() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healths++
	return nil
}

// queueEvent 预置下一次 Send 时投递的事件（可编程事件源）。
func (e *accFakeEngine) queueEvent(ev contract.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emit = append(e.emit, ev)
}

func (e *accFakeEngine) sendCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sends)
}

func (e *accFakeEngine) sendTexts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.sends))
	for _, m := range e.sends {
		out = append(out, m.Text)
	}
	return out
}

func (e *accFakeEngine) cancelCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cancels)
}

// ---------------------------------------------------------------------------
// 验收夹具：真实 store + 真实编排层 + 真实 web，引擎注入 fake
// ---------------------------------------------------------------------------

type accFixture struct {
	app   *App
	cred  string
	codex *accFakeEngine
	dsh   *accFakeEngine
}

// accDataRoot 返回当前可写的一个 F 盘根目录，供夹具与参数用例派生路径。
// 项目红线：临时文件不进 C 盘（config.Resolve 的 IsSystemDrive 检查会拒绝 C 盘
// temp_dir），所以候选全部在 F 盘、绝不退回系统盘。首选项目约定的
// F:\AIWORKSPACE\temp\mesh；该路径不可写时（如沙箱环境只开放会话工作区）
// 退回工作区内的 .go\acc-tmp。
//
// 注意探测必须实际写文件：MkdirAll 对已存在目录幂等成功，而"目录存在"
// 与"可写"是两回事——只查存在会让夹具选到无写权限的目录。
func accDataRoot(t *testing.T) string {
	t.Helper()
	for _, root := range []string{`F:\AIWORKSPACE\temp\mesh`, `F:\AIWORKSPACE\agent_harness\.go\acc-tmp`} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			continue
		}
		probe := filepath.Join(root, "write-probe.tmp")
		if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
			continue
		}
		_ = os.Remove(probe)
		return root
	}
	t.Fatal("F 盘临时根目录都不可写（红线：不退回 C 盘）")
	return ""
}

// newAccFixture 组装一个走完整初始化路径的 App。数据与临时目录互为兄弟目录
// （config.Resolve 拒绝二者互相包含），listen 用回环随机端口。
func newAccFixture(t *testing.T) *accFixture {
	t.Helper()
	base, err := os.MkdirTemp(accDataRoot(t), "acc-")
	if err != nil {
		t.Fatalf("创建验收临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(base, "data")
	cfg.TempDir = filepath.Join(base, "tmp")
	cfg.Listen = "127.0.0.1:0"

	codex := &accFakeEngine{}
	dsh := &accFakeEngine{}
	a, err := Build(context.Background(), cfg, BuildOptions{
		Bots: []contract.BotSpec{
			{ID: "codex", Name: "Codex", Engine: contract.EngineSpec{Type: contract.EngineDesktop, Adapter: "codex"}},
			{ID: "dsh", Name: "DSH", Engine: contract.EngineSpec{Type: contract.EngineDesktop, Adapter: "dsh"}},
			// trae 不注入引擎：验证 unavailableEngine 的诚实占位。
			{ID: "trae", Name: "Trae", Engine: contract.EngineSpec{Type: contract.EngineDesktop, Adapter: "trae"}},
		},
		Engines: map[string]contract.Engine{"codex": codex, "dsh": dsh},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 收尾与 Shutdown 同序（先 web 掐 SSE，再关库），但不走 app.Shutdown——
	// 它在存在未确认副作用时会向 stdout 打提醒，测试保持安静。
	t.Cleanup(func() {
		_ = a.Web.Shutdown(context.Background())
		_ = a.Store.Close()
	})
	credRaw, err := os.ReadFile(a.Cfg.CredentialFile)
	if err != nil {
		t.Fatalf("读取凭据文件失败: %v", err)
	}
	accSeedRoom(t, a.Store, a.Cfg.RoomID)
	return &accFixture{
		app:   a,
		cred:  strings.TrimSpace(string(credRaw)),
		codex: codex,
		dsh:   dsh,
	}
}

// accSeedRoom works/messages/runs 都有 room 外键（DSN foreign_keys(1)），
// 与 store 层测试同款种子。
func accSeedRoom(t *testing.T, s *store.Store, roomID string) {
	t.Helper()
	err := s.Write(context.Background(), func(_ context.Context, tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO rooms (id, title, created_at) VALUES (?, ?, ?)`,
			roomID, "验收房间", time.Now().UnixMilli())
		return err
	})
	if err != nil {
		t.Fatalf("种子房间 %s 失败: %v", roomID, err)
	}
}

// accSeedWork 直建一个 Work。P0 组装层还没有「聊天消息建委托」的派发器
// （见 README 已知限制），按 §5.6 规定的顺序由测试先建 Work 再投消息。
func accSeedWork(t *testing.T, f *accFixture, id, exec, rev string, state contract.WorkState, source contract.RoleSource) contract.Work {
	t.Helper()
	now := time.Now()
	w := contract.Work{
		ID:     id,
		RoomID: f.app.Cfg.RoomID,
		Goal:   "把验收清单跑完",
		State:  state,
		Assign: contract.RoleAssignment{
			Version: 1, Executor: exec, Reviewer: rev, Source: source, At: now,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.app.Store.SaveWork(context.Background(), w); err != nil {
		t.Fatalf("种子 Work %s 失败: %v", id, err)
	}
	return w
}

// accSubmit 以执行者 codex 提交第 ver 版产物（work.submit 的薄包装）。
func accSubmit(t *testing.T, f *accFixture, workID string, ver int) orchestrator.ToolResult {
	t.Helper()
	raw := fmt.Sprintf(`{"summary":"第 %d 版","kind":"file","ref":"draft-v%d.md"}`, ver, ver)
	res, err := f.app.Orch.CallTool(context.Background(), "codex", workID, orchestrator.ToolWorkSubmit, raw)
	if err != nil || !res.OK {
		t.Fatalf("work.submit v%d: err=%v res=%+v", ver, err, res)
	}
	return res
}

// accReview 以审查者 dsh 提交结论（review.submit 的薄包装）。
// 注意：审批被拒绝（如旧版本获批）不算调用失败，res.OK=false 原样返回。
func accReview(t *testing.T, f *accFixture, workID string, verdict contract.Verdict, ver int, findings string) orchestrator.ToolResult {
	t.Helper()
	raw := fmt.Sprintf(`{"verdict":%q,"artifact_version":%d,"findings":%q}`, string(verdict), ver, findings)
	res, err := f.app.Orch.CallTool(context.Background(), "dsh", workID, orchestrator.ToolReviewSubmit, raw)
	if err != nil {
		t.Fatalf("review.submit %s: %v", verdict, err)
	}
	return res
}

// ---------------------------------------------------------------------------
// 场景 1：用户委托 → 编排层只唤醒执行者 → 任务消息送达 fake engine
// ---------------------------------------------------------------------------

func TestAccDelegationWakesExecutorAndEngineGetsTask(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w1", "codex", "dsh", contract.WorkQueued, contract.SourceUserUI)

	msg := contract.Message{
		ID: "acc-m1", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: w.Goal, CreatedAt: time.Now(),
	}
	targets, notice, err := f.app.Orch.OnUserMessage(ctx, msg)
	if err != nil {
		t.Fatalf("OnUserMessage: %v", err)
	}
	if notice != "" {
		t.Fatalf("正常委托不应有 notice，得到 %q", notice)
	}
	if len(targets) != 1 {
		t.Fatalf("新委托应只唤醒 1 人，得到 %+v", targets)
	}
	tg := targets[0]
	if tg.BotID != "codex" || tg.Reason != orchestrator.WakeNewWork || tg.Depth != 0 {
		t.Fatalf("唤醒目标不对: %+v", tg)
	}
	if tg.BotID == "dsh" {
		t.Fatal("审查者在新委托时不应被唤醒（它等产物提交）")
	}

	// 落库 + 建投递 + 领取（§5.6 顺序）。
	if err := f.app.Store.CreateMessage(ctx, &msg, targetsToIDs(targets)); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	d, err := f.app.Store.ClaimDelivery(ctx, time.Now(), "acc-run-1")
	if err != nil || d == nil {
		t.Fatalf("ClaimDelivery: %v (delivery=%+v)", err, d)
	}
	if d.BotID != "codex" {
		t.Fatalf("投递应属于执行者 codex，得到 %s", d.BotID)
	}
	stored, err := f.app.Store.GetMessage(ctx, d.MessageID)
	if err != nil || stored.Text != w.Goal {
		t.Fatalf("领取到的消息内容不对: err=%v msg=%+v", err, stored)
	}

	// 任务消息经 engineMux 按 bot 分发：codex 的 fake 收到原文，dsh 收不到。
	spec := contract.BotSpec{ID: "codex", Engine: contract.EngineSpec{Type: contract.EngineDesktop, Adapter: "codex"}}
	session, err := f.app.mux.Open(ctx, spec, contract.Workspace{Mode: "private", Dir: f.app.Cfg.BotWorkspace("codex")})
	if err != nil {
		t.Fatalf("mux.Open: %v", err)
	}
	// 成功判定绑定 SuccessSignal：fake 客户端退出码恒为 0（不可信）。
	// 事件必须先预置：fake 引擎在 Send 时把 emit 一次性投进事件通道。
	f.codex.queueEvent(contract.Event{
		EventID: "acc-fin-1", RunID: d.RunID, WorkID: w.ID, BotID: "codex", Seq: 1,
		Kind: contract.EventFinal,
		Final: &contract.FinalInfo{Success: true, SuccessSignal: "client showed done", Summary: "客户端界面显示已完成"},
	})
	turn, err := f.app.mux.Send(ctx, session, stored)
	if err != nil {
		t.Fatalf("mux.Send: %v", err)
	}
	gotTexts := f.codex.sendTexts()
	if len(gotTexts) != 1 || gotTexts[0] != w.Goal {
		t.Fatalf("fake engine 收到的任务消息 = %v", gotTexts)
	}
	if f.dsh.sendCount() != 0 {
		t.Fatalf("dsh 不应收到任何消息，收到 %d 条", f.dsh.sendCount())
	}

	// 成功判定来自 SuccessSignal 而非退出码：final 事件已在 Send 前预置，
	// 这里从事件通道读回并核对。
	select {
	case ev := <-f.app.mux.Events(session):
		if ev.Kind != contract.EventFinal || ev.Final == nil {
			t.Fatalf("应收到 final 事件，得到 %+v", ev)
		}
		if !ev.Final.Success || ev.Final.SuccessSignal != "client showed done" || ev.Final.Unverified {
			t.Fatalf("成功判定必须来自 SuccessSignal: %+v", ev.Final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2 秒内没等到 final 事件")
	}

	// Cancel 调用有记录。
	if err := f.app.mux.Cancel(ctx, session, turn.ID()); err != nil {
		t.Fatalf("mux.Cancel: %v", err)
	}
	if got := f.codex.cancelCount(); got != 1 {
		t.Fatalf("Cancel 应被调用 1 次，实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 场景 2：跨对象角色 —— 执行者 A 交产物，只唤醒审查者 B
// ---------------------------------------------------------------------------

func TestAccExecutorSubmitWakesOnlyReviewer(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w2", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	accSubmit(t, f, w.ID, 1)

	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkReviewing {
		t.Fatalf("交产物后应 reviewing，得到 %s", got.State)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Version != 1 || got.Artifacts[0].Ref != "draft-v1.md" {
		t.Fatalf("产物记录不对: %+v", got.Artifacts)
	}

	// A 的结果消息只唤醒 B；发送者自己不醒（避免自激）。
	resMsg := contract.Message{
		ID: "acc-m2", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "codex", FromKind: contract.FromBot, ToID: "dsh",
		Kind: contract.MsgResult, Text: "初稿在此", CreatedAt: time.Now(),
	}
	targets, err := f.app.Orch.Route(ctx, resMsg)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].BotID != "dsh" || targets[0].Reason != orchestrator.WakeReviewReady {
		t.Fatalf("产物提交应只唤醒审查者 dsh(review_ready)，得到 %+v", targets)
	}
}

// ---------------------------------------------------------------------------
// 场景 3：修改循环 —— 自动修改 2 轮封顶，第 3 次 changes_requested 转 waiting_user
// ---------------------------------------------------------------------------

func TestAccAutoReviewCapTurnsWaitingUser(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w3", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	for v := 1; v <= contract.MaxAutoReviewRounds; v++ {
		accSubmit(t, f, w.ID, v)
		res := accReview(t, f, w.ID, contract.VerdictChangesRequested, v, fmt.Sprintf("第 %d 版要改", v))
		if res.Notice != "" {
			t.Fatalf("第 %d 轮还在自动循环内，不应报超限提示: %q", v, res.Notice)
		}
		got, err := f.app.Store.GetWork(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != contract.WorkExecuting || got.ReviewRounds != v {
			t.Fatalf("第 %d 轮后应 executing(rounds=%d)，得到 %s(%d)", v, v, got.State, got.ReviewRounds)
		}
	}

	// 第三次要求修改：自动修改已到上限（2 轮），转 waiting_user 等用户裁决。
	accSubmit(t, f, w.ID, 3)
	res := accReview(t, f, w.ID, contract.VerdictChangesRequested, 3, "第三版还不行")
	if res.Notice == "" {
		t.Fatal("超限转 waiting_user 必须给用户一句说明")
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkWaitingUser || got.ReviewRounds != 3 {
		t.Fatalf("期望 waiting_user(rounds=3)，得到 %s(%d)", got.State, got.ReviewRounds)
	}
	revs, err := f.app.Store.Reviews(ctx, w.ID)
	if err != nil || len(revs) != 3 {
		t.Fatalf("应有 3 条审查结论，得到 %d 条 (err=%v)", len(revs), err)
	}
	if len(got.Artifacts) != 3 {
		t.Fatalf("应有 3 个产物版本，得到 %d", len(got.Artifacts))
	}
}

// ---------------------------------------------------------------------------
// 场景 4：旧版本 approved 不能放行新版本
// ---------------------------------------------------------------------------

func TestAccStaleApprovalCannotPassNewVersion(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w4", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	accSubmit(t, f, w.ID, 1)
	// v1 先被要求修改退回 executing，执行者才允许再交 v2（提交只在执行中合法）。
	accReview(t, f, w.ID, contract.VerdictChangesRequested, 1, "改一版")
	accSubmit(t, f, w.ID, 2) // 期间执行者又提交了 v2

	// 迟到的旧审批：v1 已不是最新版本。
	res := accReview(t, f, w.ID, contract.VerdictApproved, 1, "v1 我验过没问题")
	if res.OK || res.Notice == "" {
		t.Fatalf("旧版本 approved 必须被拒绝并说明原因，得到 OK=%v notice=%q", res.OK, res.Notice)
	}
	if !strings.Contains(res.Notice, "旧审批") {
		t.Fatalf("拒绝原因应点名旧审批: %q", res.Notice)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkReviewing || got.Final != nil {
		t.Fatalf("旧审批不得推进状态：state=%s final=%+v", got.State, got.Final)
	}
	revs, err := f.app.Store.Reviews(ctx, w.ID)
	if err != nil || len(revs) != 2 {
		t.Fatalf("此时审查记录应为 2 条（1 次返修 + 旧审批），得到 %d (err=%v)", len(revs), err)
	}
	stale := revs[len(revs)-1]
	if stale.ArtifactVersion != 1 || stale.Verdict != contract.VerdictApproved {
		t.Fatalf("旧审批应如实入账: %+v", stale)
	}

	// 对当前最新版本的 approved 才放行。
	res = accReview(t, f, w.ID, contract.VerdictApproved, 2, "v2 验过")
	if !res.OK {
		t.Fatalf("最新版本 approved 应通过: %+v", res)
	}
	revs, err = f.app.Store.Reviews(ctx, w.ID)
	if err != nil || len(revs) != 3 {
		t.Fatalf("最终审查记录应为 3 条，得到 %d (err=%v)", len(revs), err)
	}
	got, err = f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkCompleted {
		t.Fatalf("期望 completed，得到 %s", got.State)
	}
	if got.Final == nil || got.Final.Version != 2 {
		t.Fatalf("Final 必须绑定被批准的 v2，得到 %+v", got.Final)
	}
}

// ---------------------------------------------------------------------------
// 场景 5：@B 咨询不改审查者
// ---------------------------------------------------------------------------

func TestAccMentionConsultDoesNotTouchAssignment(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w5", "codex", "dsh", contract.WorkQueued, contract.SourceUserChat)

	msg := contract.Message{
		ID: "acc-m5", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
		Text: "@dsh 这个思路行吗", Mentions: []string{"dsh"}, CreatedAt: time.Now(),
	}
	targets, notice, err := f.app.Orch.OnUserMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if notice != "" {
		t.Fatalf("咨询不应产生分工变更回执，得到 %q", notice)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assign.Version != 1 || got.Assign.Executor != "codex" || got.Assign.Reviewer != "dsh" {
		t.Fatalf("@ 咨询不得改分工: %+v", got.Assign)
	}
	history, err := f.app.Store.Assignments(ctx, w.ID)
	if err != nil || len(history) != 0 {
		// 种子 Work 的 v1 只存在 work_json 里（SaveWork 不写 assignments 历史，
		// 历史从第一次分工变更开始记）。咨询不改分工 → 不产生任何新版本行。
		t.Fatalf("咨询不得新增分工版本行，得到 %d 行 (err=%v)", len(history), err)
	}
	var wokeExecutor, wokeMention bool
	for _, tg := range targets {
		if tg.BotID == "codex" && tg.Reason == orchestrator.WakeNewWork {
			wokeExecutor = true
		}
		if tg.BotID == "dsh" && tg.Reason == orchestrator.WakeMention {
			wokeMention = true
		}
	}
	if !wokeExecutor || !wokeMention {
		t.Fatalf("应唤醒 codex(委托) + dsh(mention)，得到 %+v", targets)
	}
}

// ---------------------------------------------------------------------------
// 场景 6：界面指定与聊天指定产生同一份分工记录
// ---------------------------------------------------------------------------

func TestAccUIAndChatProduceSameAssignmentRecord(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w6", "codex", "dsh", contract.WorkQueued, contract.SourceUserUI)

	// 界面动作：replace 按钮换成 trae。来源与聊天一致记为 user_chat。
	note, err := f.app.bridge.Control(ctx, w.ID, web.ControlRequest{Action: "replace", Executor: "trae"})
	if err != nil {
		t.Fatalf("Control(replace): %v", err)
	}
	if note == "" {
		t.Fatal("换人必须给回执")
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assign.Version != 2 || got.Assign.Executor != "trae" || got.Assign.Reviewer != "dsh" {
		t.Fatalf("界面换人后分工 = %+v", got.Assign)
	}
	if got.Assign.Source != contract.SourceUserChat {
		t.Fatalf("界面指定的来源应与聊天一致为 user_chat，得到 %s", got.Assign.Source)
	}

	// 聊天指令：与 webBridge.SendMessage 为界面选择合并出的指令文本完全同形
	// （"改由 trae 执行"），在同一份历史里继续递增版本。
	if _, _, err := f.app.Orch.OnUserMessage(ctx, contract.Message{
		ID: "acc-m6", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
		Text: "改由 trae 执行", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	history, err := f.app.Store.Assignments(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 种子 v1 只在 work_json 里（无历史行），历史从第一次变更开始：
	// Control(replace) 记 v2，聊天指令记 v3 —— 两条路径进同一份历史。
	if len(history) != 2 || history[0].Version != 2 || history[1].Version != 3 {
		t.Fatalf("界面+聊天两次指定应记 v2、v3 两版，得到 %+v", history)
	}
	last := history[len(history)-1]
	if last.Version != 3 || last.Executor != "trae" || last.Reviewer != "dsh" || last.Source != contract.SourceUserChat {
		t.Fatalf("聊天指定的分工记录 = %+v", last)
	}

	// 界面的下拉选择合并出的指令文本与聊天写法同形（§12 硬要求）。
	view, err := f.app.bridge.SendMessage(ctx, web.SendRequest{Text: "请继续推进", Executor: "trae", Reviewer: "dsh"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !strings.Contains(view.Text, "改由 trae 执行") || !strings.Contains(view.Text, "改由 dsh 审查") {
		t.Fatalf("界面选择应合并成聊天指令文本，得到 %q", view.Text)
	}
}

// ---------------------------------------------------------------------------
// 场景 7：重复消息幂等
// ---------------------------------------------------------------------------

func TestAccDuplicateDeliveryIsIdempotent(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w7", "codex", "dsh", contract.WorkQueued, contract.SourceUserChat)

	// (a) client_message_id 去重：同一条用户输入重投不重复落库（§5.6）。
	first := &contract.Message{
		ID: "acc-dup-1", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "第一次投递", ClientMessageID: "acc-client-1", CreatedAt: time.Now(),
	}
	if err := f.app.Store.CreateMessage(ctx, first, []string{"codex"}); err != nil {
		t.Fatalf("首次落库: %v", err)
	}
	second := *first
	second.ID = "acc-dup-2"
	second.Text = "同一输入的重复投递"
	if err := f.app.Store.CreateMessage(ctx, &second, []string{"codex"}); !errors.Is(err, store.ErrDuplicateClientMessage) {
		t.Fatalf("同 ClientMessageID 二次落库应返回 ErrDuplicateClientMessage，得到 %v", err)
	}

	// (b) input_seen 登记幂等（护栏一，§5.3）。
	seen, err := f.app.Store.MarkInputSeen(ctx, "acc-seen-msg", "codex")
	if err != nil || seen {
		t.Fatalf("首次登记应 seen=false，得到 %v (err=%v)", seen, err)
	}
	if seen, _ = f.app.Store.MarkInputSeen(ctx, "acc-seen-msg", "codex"); !seen {
		t.Fatal("第二次登记应返回 seen=true")
	}

	// (c) 同一条消息第二次进编排层：被护栏去重，不再唤醒任何人。
	msg := contract.Message{
		ID: "acc-dup-wake", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: w.Goal, CreatedAt: time.Now(),
	}
	targets, _, err := f.app.Orch.OnUserMessage(ctx, msg)
	if err != nil || len(targets) != 1 {
		t.Fatalf("首次应唤醒执行者: targets=%+v err=%v", targets, err)
	}
	again, _, err := f.app.Orch.OnUserMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("重复投递必须被去重，得到 %+v", again)
	}
}

// ---------------------------------------------------------------------------
// 场景 8：断线游标补齐去重 —— EventsAfterRowid 不重不漏 + webBridge.Replay
// ---------------------------------------------------------------------------

func TestAccEventCursorReplayNoGapNoDup(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w8", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	msg := &contract.Message{
		ID: "acc-m8", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "跑事件流", CreatedAt: time.Now(),
	}
	if err := f.app.Store.CreateMessage(ctx, msg, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	d, err := f.app.Store.ClaimDelivery(ctx, time.Now(), "acc-run-8")
	if err != nil || d == nil {
		t.Fatalf("ClaimDelivery: %v (delivery=%+v)", err, d)
	}

	ev := func(seq int64, id, text string) contract.Event {
		return contract.Event{
			EventID: id, RunID: d.RunID, WorkID: w.ID, BotID: "codex", Seq: seq,
			Kind: contract.EventOutput, Text: text, TS: time.Now(),
		}
	}
	for _, s := range []struct {
		seq  int64
		id   string
		text string
	}{{1, "acc-e1", "第一步"}, {2, "acc-e2", "第二步"}, {3, "acc-e3", "第三步"}} {
		if err := f.app.Store.AppendEvent(ctx, ev(s.seq, s.id, s.text)); err != nil {
			t.Fatalf("AppendEvent(%s): %v", s.id, err)
		}
	}
	// 同 (run_id, seq) 重复写入（终态重发很常见）：幂等静默跳过，不产生新行。
	if err := f.app.Store.AppendEvent(ctx, ev(2, "acc-e2-dup-id", "第二步（重发）")); err != nil {
		t.Fatalf("重复 AppendEvent 应幂等成功: %v", err)
	}

	// 游标 0 → 全量 3 条；游标推进后 → 空且 next 不动。
	got, next, err := f.app.Store.EventsAfterRowid(ctx, 0, 100)
	if err != nil || len(got) != 3 {
		t.Fatalf("游标补齐应得 3 条: n=%d err=%v", len(got), err)
	}
	if next <= 0 {
		t.Fatalf("游标应推进到正数，得到 %d", next)
	}
	got2, next2, err := f.app.Store.EventsAfterRowid(ctx, next, 100)
	if err != nil || len(got2) != 0 || next2 != next {
		t.Fatalf("游标之后应无事件且游标不动: n=%d next2=%d next=%d err=%v", len(got2), next2, next, err)
	}

	// 走 webBridge.Replay（web 层 EventStream 的真实实现）。
	r1, err := f.app.bridge.Replay(ctx, 0, 100)
	if err != nil || len(r1) != 3 {
		t.Fatalf("Replay(0) 应 3 条: n=%d err=%v", len(r1), err)
	}
	if r0, _ := f.app.bridge.Replay(ctx, next, 100); len(r0) != 0 {
		t.Fatalf("Replay(next) 应空，得到 %d 条", len(r0))
	}
	if err := f.app.Store.AppendEvent(ctx, ev(4, "acc-e4", "断线期间的新事件")); err != nil {
		t.Fatal(err)
	}
	r3, err := f.app.bridge.Replay(ctx, next, 100)
	if err != nil || len(r3) != 1 || r3[0].EventID != "acc-e4" {
		t.Fatalf("断线后补齐应只有新事件: %+v (err=%v)", r3, err)
	}
	// 不重不漏：两次补齐的并集恰好是 4 个唯一事件。
	all := append(append([]contract.Event{}, r1...), r3...)
	ids := map[string]bool{}
	for _, e := range all {
		ids[e.EventID] = true
	}
	if len(all) != 4 || len(ids) != 4 {
		t.Fatalf("补齐去重失败: 共 %d 条、%d 个唯一 id", len(all), len(ids))
	}
}

// ---------------------------------------------------------------------------
// 场景 9a：Control(stop) —— 客户端未回应时停在「停止待确认」
// ---------------------------------------------------------------------------

func TestAccControlStopKeepsUnconfirmedUntilClientConfirms(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w9", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	// 一次已发出、还没拿到回执的外部动作。
	f.app.Orch.BeginEffect(w.ID, "client-run")
	f.app.Orch.EffectExecuting(w.ID, "client-run")

	note, err := f.app.bridge.Control(ctx, w.ID, web.ControlRequest{Action: "stop"})
	if err != nil {
		t.Fatalf("Control(stop): %v", err)
	}
	if note != orchestrator.StopPendingNotice {
		t.Fatalf("停止未确认时必须返回停止待确认文案，得到 %q", note)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkCancelled {
		t.Fatalf("期望 cancelled，得到 %s", got.State)
	}
	unsettled := f.app.Orch.UnsettledEffects()
	if len(unsettled) != 1 || unsettled[0].Key.WorkID != w.ID || unsettled[0].Key.Step != "client-run" {
		t.Fatalf("未确认副作用应留在待交代列表: %+v", unsettled)
	}
	if unsettled[0].State != contract.OpUnknown {
		t.Fatalf("失去观测的动作必须是 unknown，得到 %s", unsettled[0].State)
	}
}

// ---------------------------------------------------------------------------
// 场景 9b：聊天「暂停」→ Control「继续」→ 回到可派发状态
// ---------------------------------------------------------------------------

func TestAccPauseThenResumeReturnsToDispatchable(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w10", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	targets, notice, err := f.app.Orch.OnUserMessage(ctx, contract.Message{
		ID: "acc-m10a", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
		Text: "暂停", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkPaused {
		t.Fatalf("「暂停」应 paused，得到 %s", got.State)
	}
	if notice == "" || len(targets) != 0 {
		t.Fatalf("暂停要给回执且不唤醒任何人: notice=%q targets=%+v", notice, targets)
	}

	note, err := f.app.bridge.Control(ctx, w.ID, web.ControlRequest{Action: "resume"})
	if err != nil {
		t.Fatalf("Control(resume): %v", err)
	}
	if note == "" {
		t.Fatal("「继续」要给回执")
	}
	got, err = f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkQueued {
		t.Fatalf("恢复后应回 queued（可重新派发），得到 %s", got.State)
	}
}

// ---------------------------------------------------------------------------
// 场景 9c：Control(pause) 必须真的暂停（§5.8 控制动词）
//
// 这条编码的是规格行为。当前已知缺陷：webBridge.Control 把 pause 翻译成
// 「暂停这件事」，而 roles.go 的 matchControlCommand 只认整句命令表（「暂停」），
// 解析结果为 InstrNone —— Work 不会被暂停。若本用例失败即该缺陷仍在，
// 修 webbridge.go 的动作译文或控制命令表，不要放宽本断言。
// ---------------------------------------------------------------------------

func TestAccControlPauseMovesWorkToPaused(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w11", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	if _, err := f.app.bridge.Control(ctx, w.ID, web.ControlRequest{Action: "pause"}); err != nil {
		t.Fatalf("Control(pause): %v", err)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != contract.WorkPaused {
		t.Fatalf("界面暂停必须把 Work 置为 paused，实际 %s（见用例头注释：Control(pause) 接线缺陷）", got.State)
	}
}

// ---------------------------------------------------------------------------
// 场景 9d：Control(replace) 换人 + 角色互换 + 旧审查者结论失效
// ---------------------------------------------------------------------------

func TestAccControlReplaceReassignsExecutor(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w12", "codex", "dsh", contract.WorkQueued, contract.SourceUserUI)

	// 界面换人：执行者换成原审查者 dsh —— 自审必须被显式取消（§5.8）。
	note, err := f.app.bridge.Control(ctx, w.ID, web.ControlRequest{Action: "replace", Executor: "dsh"})
	if err != nil {
		t.Fatalf("Control(replace): %v", err)
	}
	if !strings.Contains(note, "dsh") || !strings.Contains(note, "审查已取消") {
		t.Fatalf("换人回执应说明新分工并交代审查取消，得到 %q", note)
	}
	got, err := f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assign.Version != 2 || got.Assign.Executor != "dsh" || got.Assign.Reviewer != "" {
		t.Fatalf("换人后分工 = %+v", got.Assign)
	}

	// 角色互换：聊天指令「改由 codex 审查」→ dsh 做、codex 审。
	if _, _, err := f.app.Orch.OnUserMessage(ctx, contract.Message{
		ID: "acc-m12b", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
		Text: "改由 codex 审查", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err = f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assign.Version != 3 || got.Assign.Executor != "dsh" || got.Assign.Reviewer != "codex" {
		t.Fatalf("角色互换后分工 = %+v", got.Assign)
	}

	// 旧审查者的迟到结论不得推进状态（换审查者后 dsh 已无权提交结论）。
	if _, err := f.app.Orch.CallTool(ctx, "dsh", w.ID, orchestrator.ToolReviewSubmit,
		`{"verdict":"approved","artifact_version":1,"findings":"我早批过了"}`); err == nil {
		t.Fatal("换审查者后旧 agent 的结论不得被接受")
	}
	got, err = f.app.Store.GetWork(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assign.Version != 3 {
		t.Fatalf("被拒绝的迟到结论不得改分工，版本应仍为 3，得到 %d", got.Assign.Version)
	}
}

// ---------------------------------------------------------------------------
// 场景 10：交接深度护栏 —— replyTo 链超限被 GuardDepth 拦截并产生 notice
// ---------------------------------------------------------------------------

func TestAccHandoffDepthGuardBlocksAndNotifies(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	room := f.app.Cfg.RoomID

	// 8 环因果链：acc-d0 ← acc-d1 ← ... ← acc-d7。
	for i := 0; i < 8; i++ {
		m := contract.Message{
			ID: fmt.Sprintf("acc-d%d", i), RoomID: room,
			FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgChat,
			Text: fmt.Sprintf("因果链第 %d 环", i), CreatedAt: time.Now(),
		}
		if i > 0 {
			m.ReplyTo = fmt.Sprintf("acc-d%d", i-1)
		}
		if err := f.app.Store.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%s): %v", m.ID, err)
		}
	}
	handoff := func(id, replyTo string) []orchestrator.WakeTarget {
		t.Helper()
		// 先落库再路由：depthOf 经 GetMessage 沿 ReplyTo 链回溯，消息不在库里深度就是 0。
		m := contract.Message{
			ID: id, RoomID: room, FromID: "codex", FromKind: contract.FromBot,
			ToID: "trae", Kind: contract.MsgHandoff, Text: "这一段交给你",
			ReplyTo: replyTo, CreatedAt: time.Now(),
		}
		if err := f.app.Store.SaveMessage(ctx, m); err != nil {
			t.Fatalf("SaveMessage(%s): %v", id, err)
		}
		targets, err := f.app.Orch.Route(ctx, m)
		if err != nil {
			t.Fatalf("Route(%s): %v", id, err)
		}
		return targets
	}

	// 深度恰为 6（d5 之下 5 环 + 本次交接 1 环）：放行，唤醒 trae。
	if got := handoff("acc-h-ok", "acc-d5"); len(got) != 1 || got[0].BotID != "trae" || got[0].Depth != 6 {
		t.Fatalf("深度=6 的交接应放行给 trae: %+v", got)
	}
	// 深度 7（d6 之下 6 环 + 本次交接 1 环）：MaxHandoffDepth=6 之上，拦截。
	if got := handoff("acc-h-blocked", "acc-d6"); len(got) != 0 {
		t.Fatalf("深度=7 的交接必须被 GuardDepth 拦截: %+v", got)
	}
	// 拦截必须留下用户可见 notice（§5.3 不许静默丢弃）。
	var text string
	err := f.app.Store.DB().QueryRowContext(ctx,
		`SELECT text FROM messages WHERE cause_id = ? AND kind = ?`,
		"acc-h-blocked", string(contract.MsgNotice)).Scan(&text)
	if err != nil {
		t.Fatalf("深度拦截应产生 notice 消息: %v", err)
	}
	if !strings.Contains(text, "层") || !strings.Contains(text, "6") {
		t.Fatalf("notice 应说明深度上限: %q", text)
	}
}

// ---------------------------------------------------------------------------
// 场景 11：本机入口 —— 回环绑定、无凭证 401、跨源 403、无 CSRF 写 403
// ---------------------------------------------------------------------------

func TestAccLocalEntrySecurity(t *testing.T) {
	f := newAccFixture(t)
	// 真实 web 中间件链（Host → 凭证 → Origin → CSRF）直接驱动，不叠假实现。
	ts := httptest.NewServer(f.app.Web.Handler())
	t.Cleanup(ts.Close)

	// 回环绑定：真实监听地址必须是 127.0.0.1。
	if addr := f.app.Web.Addr(); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("本机服务必须绑定回环，得到 %q", addr)
	}
	if err := ValidateListen("0.0.0.0:8080"); err == nil {
		t.Fatal("非回环监听地址必须被配置校验拒绝")
	}
	if err := ValidateListen("127.0.0.1:0"); err != nil {
		t.Fatalf("回环监听地址应被接受: %v", err)
	}

	accReq := func(method, path, body string, headers map[string]string) *http.Response {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		httpReq, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		httpReq.Header.Set(web.HeaderToken, f.cred)
		if body != "" {
			httpReq.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("请求 %s %s 失败: %v", method, path, err)
		}
		return resp
	}
	accBody := func(resp *http.Response) string {
		t.Helper()
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读响应体失败: %v", err)
		}
		return string(b)
	}

	// 无凭证 → 401。
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	if code := resp.StatusCode; code != http.StatusUnauthorized {
		t.Errorf("无凭证应 401，实际 %d", code)
	}
	_ = resp.Body.Close()

	// 错凭证 → 401。
	respWrong := accReq(http.MethodGet, "/api/state", "", map[string]string{web.HeaderToken: "wrong-token"})
	if respWrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("错凭证应 401，实际 %d", respWrong.StatusCode)
	}
	_ = accBody(respWrong)

	// 跨源 → 403。
	respOrigin := accReq(http.MethodGet, "/api/state", "", map[string]string{"Origin": "http://evil.example.com"})
	if respOrigin.StatusCode != http.StatusForbidden {
		t.Errorf("跨源请求应 403，实际 %d", respOrigin.StatusCode)
	}
	_ = accBody(respOrigin)

	// 无 CSRF 的写操作 → 403。
	respNoCSRF := accReq(http.MethodPost, "/api/message", `{"text":"没有令牌的写入"}`, nil)
	if respNoCSRF.StatusCode != http.StatusForbidden {
		t.Errorf("无 CSRF 的 POST 应 403，实际 %d", respNoCSRF.StatusCode)
	}
	_ = accBody(respNoCSRF)

	// 取 CSRF → 带令牌的写操作放行并真实落库。
	respState := accReq(http.MethodGet, "/api/state", "", nil)
	var snap web.Snapshot
	if err := json.Unmarshal([]byte(accBody(respState)), &snap); err != nil {
		t.Fatalf("快照解析失败: %v", err)
	}
	if snap.Session.CSRF == "" {
		t.Fatal("session.csrf 不能为空（前端所有 POST 依赖它）")
	}
	respSend := accReq(http.MethodPost, "/api/message", `{"text":"验收：带令牌的写入"}`,
		map[string]string{web.HeaderCSRF: snap.Session.CSRF})
	raw := accBody(respSend)
	if respSend.StatusCode != http.StatusOK {
		t.Fatalf("带 CSRF 的写入应 200，实际 %d，体: %s", respSend.StatusCode, raw)
	}
	var out struct {
		Message web.MessageView `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if out.Message.ID == "" || !strings.Contains(out.Message.Text, "带令牌的写入") {
		t.Fatalf("写入回执不对: %+v", out.Message)
	}
}

// ---------------------------------------------------------------------------
// 场景 12：外部副作用 unknown → 待核实列表 + 快照 StopUnconfirmed
// ---------------------------------------------------------------------------

func TestAccUnknownEffectSurfacesAsStopUnconfirmed(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w15", "codex", "dsh", contract.WorkExecuting, contract.SourceUserChat)

	// 操作已执行但响应丢失 → unknown，绝不许当成成功。
	f.app.Orch.EffectUnknown(w.ID, "browser-submit", "客户端窗口被关，无法确认结果")

	unsettled := f.app.Orch.UnsettledEffects()
	if len(unsettled) != 1 || unsettled[0].Key.WorkID != w.ID || unsettled[0].Key.Step != "browser-submit" {
		t.Fatalf("unknown 必须列入待核实列表: %+v", unsettled)
	}
	if unsettled[0].State != contract.OpUnknown {
		t.Fatalf("必须是 unknown，得到 %s", unsettled[0].State)
	}

	snap, err := f.app.bridge.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Current == nil || snap.Current.WorkID != w.ID {
		t.Fatalf("当前委托应对准 %s: %+v", w.ID, snap.Current)
	}
	if !snap.Current.StopUnconfirmed {
		t.Fatal("有未确认副作用时 StopUnconfirmed 必须为 true")
	}
	if !strings.Contains(snap.Current.StopDetail, "停止待确认") {
		t.Fatalf("停止详情必须如实说明: %q", snap.Current.StopDetail)
	}

	// 适配器未接入的 bot（trae）如实列入能力受限。
	traeLimited := false
	for _, c := range snap.Capabilities {
		if c.Scope == "trae" && strings.Contains(c.Label, "适配器未接入") {
			traeLimited = true
		}
	}
	if !traeLimited {
		t.Fatalf("trae 应出现在能力受限列表: %+v", snap.Capabilities)
	}

	// bot 状态行 P0 只有空闲/思考中两态：当前执行者思考中，其余空闲。
	status := map[string]string{}
	for _, b := range snap.Bots {
		status[b.ID] = b.Status
	}
	if status["codex"] != "思考中" || status["dsh"] != "空闲" || status["trae"] != "空闲" {
		t.Fatalf("bot 状态行 = %v", status)
	}
}

// ---------------------------------------------------------------------------
// P0 诚实边界：适配器未接入时 unavailableEngine 明确报错
// ---------------------------------------------------------------------------

func TestAccUnavailableEngineFailsHonestly(t *testing.T) {
	f := newAccFixture(t)
	_ = f
	m := &engineMux{engines: map[string]contract.Engine{}}

	_, err := m.Open(context.Background(), contract.BotSpec{ID: "trae"}, contract.Workspace{})
	var missing *EngineMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("未接入的适配器必须返回 *EngineMissingError，得到 %v", err)
	}
	if missing.Adapter != "trae" || !strings.Contains(err.Error(), "适配器未接入") {
		t.Fatalf("报错要指明适配器: %v", err)
	}
	if err := m.Health(); !errors.As(err, &missing) {
		t.Fatalf("空引擎表的 Health 也必须报缺失，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 启动参数与两条硬红线（README 记载的行为）
// ---------------------------------------------------------------------------

func TestAccStartupArgsAndPathRedLines(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := ParseArgs([]string{
		"-data-dir", filepath.Join(accDataRoot(t), "args-data"),
		"-listen", "127.0.0.1:0",
		"-room", "room-acc",
		"-room-title", "验收房间",
		"-max-active-runs", "3",
		"-shutdown-grace", "5s",
	}, &stderr)
	if err != nil {
		t.Fatalf("合法参数被拒: %v", err)
	}
	if cfg.RoomID != "room-acc" || cfg.MaxActiveRuns != 3 || cfg.ShutdownGrace != 5*time.Second {
		t.Fatalf("参数没生效: %+v", cfg)
	}
	if _, err := ParseArgs([]string{"--nope"}, &stderr); err == nil {
		t.Fatal("未知参数必须报错")
	}

	// 红线一：listen 只接受回环。
	bad := cfg
	bad.Listen = "0.0.0.0:8080"
	if err := bad.Resolve(); err == nil {
		t.Fatal("非回环 listen 必须在 Resolve 被拒")
	}

	// 红线二：临时目录不许落系统盘。
	sysDrive := os.Getenv("SystemDrive")
	if sysDrive == "" {
		sysDrive = "C:"
	}
	sys := cfg
	sys.TempDir = filepath.Join(sysDrive, `mesh-acc-redline-tmp`)
	if err := sys.Resolve(); err == nil || !strings.Contains(err.Error(), "系统盘") {
		t.Fatalf("临时目录在系统盘必须拒绝启动，得到 %v", err)
	}

	// temp 与 data 互相包含也拒绝。
	nested := cfg
	nested.TempDir = filepath.Join(cfg.DataDir, "tmp")
	if err := nested.Resolve(); err == nil {
		t.Fatal("temp_dir 在 data_dir 内必须拒绝启动")
	}
}

// ---------------------------------------------------------------------------
// 场景（§10.1「退出与断线」落库半边）：消息已落库未执行时退出，重启后可恢复投递
// ---------------------------------------------------------------------------

func TestAccRestartRecoversPendingDelivery(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()
	w := accSeedWork(t, f, "acc-w18", "codex", "dsh", contract.WorkQueued, contract.SourceUserChat)

	msg := &contract.Message{
		ID: "acc-m18", RoomID: f.app.Cfg.RoomID, WorkID: w.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: w.Goal, CreatedAt: time.Now(),
	}
	if err := f.app.Store.CreateMessage(ctx, msg, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Store.ClaimDelivery(ctx, time.Now(), "acc-run-18"); err != nil {
		t.Fatalf("首次领取: %v", err)
	}

	// 模拟进程重启：关掉 store，再对同一个 data_dir 重新 store.Open。
	dir := f.app.Store.Dir()
	_ = f.app.Store.Close()
	st2, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("重开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	rep, err := st2.RecoverInterrupted(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.InterruptedRuns != 1 {
		t.Fatalf("在跑的 Run 应标记为 interrupted（不得当成功）: %+v", rep)
	}
	if rep.RequeuedDeliveries != 1 {
		t.Fatalf("未完成的投递应重新排队: %+v", rep)
	}
	d2, err := st2.ClaimDelivery(ctx, time.Now(), "acc-run-18b")
	if err != nil || d2 == nil {
		t.Fatalf("重启后应能重新领取: %v (delivery=%+v)", err, d2)
	}
	if d2.MessageID != msg.ID {
		t.Fatalf("恢复的投递应对准原消息: %+v", d2)
	}
}

// 发送失败必须按"是否可能已送达"分流（2026-11-14 @codex 无反应事故的回归测试）：
//   - turn=nil（确认没写入客户端）→ 投递重新排队，run 行收尾为 failed；
//   - turn≠nil（注入已执行但结果未知）→ 投递完成、绝不自动重发，并推系统通知。
func TestAccSendFailureSplitsByDeliveryEvidence(t *testing.T) {
	f := newAccFixture(t)
	ctx := context.Background()

	wA := accSeedWork(t, f, "acc-w19a", "codex", "", contract.WorkQueued, contract.SourceUserChat)
	msgA := &contract.Message{
		ID: "acc-m19a", RoomID: f.app.Cfg.RoomID, WorkID: wA.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "case-a", CreatedAt: time.Now(),
	}
	if err := f.app.Store.CreateMessage(ctx, msgA, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	dlA, err := f.app.Store.ClaimDelivery(ctx, time.Now(), "acc-run-19a")
	if err != nil || dlA == nil {
		t.Fatalf("领取案例一投递: %v (dl=%+v)", err, dlA)
	}
	f.codex.mu.Lock()
	f.codex.errSend = errors.New("desktop: focus: 目标窗口已不存在")
	f.codex.mu.Unlock()
	f.app.Dispatch.handle(ctx, dlA, "acc-run-19a")

	statusA, attemptA := accDeliveryRow(t, f, dlA.ID)
	if statusA != store.DeliveryPending {
		t.Fatalf("确认未送达的投递应重新排队，得到 %s（attempt=%d）", statusA, attemptA)
	}
	if st := accRunStatus(t, f, "acc-run-19a"); st != store.RunFailed {
		t.Fatalf("早退 Run 行应收尾为 failed，得到 %s（僵尸 running 会让重启恢复误判）", st)
	}

	wB := accSeedWork(t, f, "acc-w19b", "codex", "", contract.WorkQueued, contract.SourceUserChat)
	msgB := &contract.Message{
		ID: "acc-m19b", RoomID: f.app.Cfg.RoomID, WorkID: wB.ID,
		FromID: "user", FromKind: contract.FromUser, Kind: contract.MsgTask,
		Text: "case-b", CreatedAt: time.Now(),
	}
	if err := f.app.Store.CreateMessage(ctx, msgB, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	dlB, err := f.app.Store.ClaimDelivery(ctx, time.Now(), "acc-run-19b")
	if err != nil || dlB == nil {
		t.Fatalf("领取案例二投递: %v (dl=%+v)", err, dlB)
	}
	f.codex.mu.Lock()
	f.codex.errSend = errors.New("desktop: verify: 注入已执行但无法核实")
	f.codex.errSendKeepTurn = true
	f.codex.mu.Unlock()
	f.app.Dispatch.handle(ctx, dlB, "acc-run-19b")

	statusB, _ := accDeliveryRow(t, f, dlB.ID)
	if statusB != store.DeliveryDone {
		t.Fatalf("结果未知的投递应标 done（绝不自动重发），得到 %s", statusB)
	}
	if st := accRunStatus(t, f, "acc-run-19b"); st != store.RunFailed {
		t.Fatalf("发送失败的 Run 行应收尾为 failed，得到 %s", st)
	}
	found := false
	f.app.bridge.mu.Lock()
	for _, v := range f.app.bridge.ring {
		if v.AuthorKind == "system" && strings.Contains(v.Text, "无法核实") {
			found = true
			break
		}
	}
	f.app.bridge.mu.Unlock()
	if !found {
		t.Fatal("结果未知的发送必须推系统通知（用户需要到客户端确认）")
	}
}

// accDeliveryRow 读投递的 status/attempt（store 没有投递查询接口，测试内直查）。
func accDeliveryRow(t *testing.T, f *accFixture, deliveryID string) (string, int) {
	t.Helper()
	var (
		status  string
		attempt int
	)
	err := f.app.Store.Write(context.Background(), func(_ context.Context, tx *sql.Tx) error {
		return tx.QueryRow(`SELECT status, attempt FROM deliveries WHERE id = ?`, deliveryID).
			Scan(&status, &attempt)
	})
	if err != nil {
		t.Fatalf("读取投递 %s: %v", deliveryID, err)
	}
	return status, attempt
}

// accRunStatus 读 run 行状态，用于断言"不留僵尸 running"。
func accRunStatus(t *testing.T, f *accFixture, runID string) string {
	t.Helper()
	var status string
	err := f.app.Store.Write(context.Background(), func(_ context.Context, tx *sql.Tx) error {
		return tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("读取 run %s: %v", runID, err)
	}
	return status
}
