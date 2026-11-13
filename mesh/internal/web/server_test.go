package web

// 本文件是 web 包的自检测试，只用假数据源，不碰数据库、不碰真引擎。
//
// 覆盖的是"前端契约"和"安全边界"，因为这两类错误在真机上表现为
// "界面莫名其妙不工作"，而不是编译失败：
//   - 没有凭证 / 凭证错 → 401
//   - 跨源 Origin → 403
//   - 写操作没有 X-Mesh-CSRF → 403
//   - GET /api/state 必须带 session.csrf（否则前端所有 POST 都会 403）
//   - 错误响应体必须用 error 键（前端 postJSON 读 data.error）
//   - 控制动作的 note 必须原样透出（"停止待确认"不能变成"已停止"）
//   - 金额未知的调用必须单独计数，不能并进已知总额
//   - SSE 帧名必须是 state / event / ping，event 帧必须带 seq
//   - 慢消费者不能阻塞广播

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mesh/internal/contract"
)

const testCredential = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// ---------------------------------------------------------------- 假数据源

type fakeState struct {
	mu   sync.Mutex
	snap Snapshot
	err  error
}

func (f *fakeState) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.err
}

type fakeEvents struct {
	mu       sync.Mutex
	history  []contract.Event
	live     chan contract.Event
	replayEr error
}

func (f *fakeEvents) Replay(_ context.Context, cursor int64, limit int) ([]contract.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replayEr != nil {
		return nil, f.replayEr
	}
	var out []contract.Event
	for _, ev := range f.history {
		if ev.Seq > cursor && len(out) < limit {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeEvents) Subscribe(ctx context.Context) (<-chan contract.Event, error) {
	f.mu.Lock()
	if f.live == nil {
		f.live = make(chan contract.Event, 32)
	}
	live := f.live
	f.mu.Unlock()

	out := make(chan contract.Event, 32)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-live:
				if !ok {
					return
				}
				select {
				case out <- ev:
				default:
					// 模拟"丢慢消费者"，不阻塞。
				}
			}
		}
	}()
	return out, nil
}

func (f *fakeEvents) push(ev contract.Event) {
	f.mu.Lock()
	live := f.live
	f.mu.Unlock()
	if live == nil {
		return
	}
	select {
	case live <- ev:
	default:
	}
}

type fakeCost struct {
	gotDim string
	report CostReport
	err    error
}

func (f *fakeCost) Cost(_ context.Context, dim string) (CostReport, error) {
	f.gotDim = dim
	return f.report, f.err
}

type fakeMessage struct {
	got SendRequest
	err error
}

func (f *fakeMessage) SendMessage(_ context.Context, req SendRequest) (MessageView, error) {
	f.got = req
	if f.err != nil {
		return MessageView{}, f.err
	}
	return MessageView{
		ID:          "msg-1",
		Author:      "我",
		AuthorKind:  "user",
		Kind:        "chat",
		Text:        req.Text,
		CreatedAtMs: 1700000000000,
	}, nil
}

type fakeControl struct {
	gotWorkID string
	gotReq    ControlRequest
	note      string
	err       error
}

func (f *fakeControl) Control(_ context.Context, workID string, req ControlRequest) (string, error) {
	f.gotWorkID = workID
	f.gotReq = req
	return f.note, f.err
}

// newTestServer 起一个真实的 httptest 服务器（走完整中间件链）。
func newTestServer(t *testing.T) (*Server, *httptest.Server, *fakeState, *fakeEvents, *fakeCost, *fakeMessage, *fakeControl) {
	t.Helper()

	st := &fakeState{snap: Snapshot{
		Session: SessionView{RoomID: "room-main"},
		Current: &WorkView{
			WorkID: "work-1", Goal: "把报告写完", State: "executing",
			Executor: RoleView{Name: "A", Version: 2},
			Reviewer: RoleView{Name: "B", Version: 3},
			Rounds:   1, MaxRounds: 2,
		},
		Bots: []BotView{{
			ID: "bot-a", Name: "A", Adapter: "desktop", Status: "idle",
			Caps: []CapView{{Scope: "browser", Label: "浏览器", Detail: "未安装"}},
		}},
		Pending: []PendingView{{
			ID: "p-1", Kind: "approval", Title: "要授权吗", Options: []string{"允许一次", "拒绝"},
			WorkID: "work-1", ApprovalID: "ap-1",
		}},
		Capabilities: []CapView{{Scope: "browser", Label: "浏览器", Detail: "未安装"}},
		Notices:      []NoticeView{{Text: "注意", Level: "info"}},
		Messages: []MessageView{{
			ID: "msg-0", Author: "A", AuthorKind: "bot", Kind: "chat",
			Text: "好的", CreatedAtMs: 1700000000000, OperationState: "unknown", UnknownCost: true,
		}},
	}}
	ev := &fakeEvents{}
	cost := &fakeCost{report: CostReport{
		UnknownAmountCalls: 3,
		UnknownReason:      "该通道不提供计费信息",
		ByCurrency:         []Money{{Currency: "CNY", AmountMicros: 1250000}},
		Calls:              10, CallsWithTokens: 7, TokensIn: 1000, TokensOut: 200,
		Rows:   []CostRow{{Label: "A", Key: "bot-a", Calls: 10, UnknownAmountCalls: 3}},
		Limits: []CostLimit{{Label: "每日", Limit: "", Note: "客户端不提供"}},
	}}
	msg := &fakeMessage{}
	ctl := &fakeControl{note: "停止待确认：客户端尚未回应"}

	srv, err := New(Options{
		Credential: testCredential,
		Addr:       "127.0.0.1:0",
		State:      st,
		Events:     ev,
		Cost:       cost,
		Control:    ctl,
		Message:    msg,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Shutdown(context.Background())
	})
	return srv, ts, st, ev, cost, msg, ctl
}

// req 发一个带凭证头的请求。
func req(t *testing.T, ts *httptest.Server, method, path string, body string, extra map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	httpReq, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	httpReq.Header.Set(HeaderToken, testCredential)
	if body != "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		httpReq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------- 构造与参数

func TestNewRejectsMissingInputs(t *testing.T) {
	ok := func() Options {
		return Options{
			Credential: testCredential, Addr: "127.0.0.1:0",
			State: &fakeState{}, Events: &fakeEvents{}, Cost: &fakeCost{},
			Control: &fakeControl{}, Message: &fakeMessage{},
		}
	}

	if _, err := New(Options{}); err == nil {
		t.Error("没有凭证时必须报错")
	}

	cases := map[string]func(*Options){
		"State":   func(o *Options) { o.State = nil },
		"Events":  func(o *Options) { o.Events = nil },
		"Cost":    func(o *Options) { o.Cost = nil },
		"Control": func(o *Options) { o.Control = nil },
		"Message": func(o *Options) { o.Message = nil },
	}
	for name, mutate := range cases {
		o := ok()
		mutate(&o)
		if _, err := New(o); err == nil {
			t.Errorf("缺少 %s 时必须报错", name)
		}
	}
}

func TestNewRejectsNonLoopbackAddr(t *testing.T) {
	bad := []string{
		"0.0.0.0:0",
		":8080",
		"192.168.1.10:8080",
		"example.com:80",
		"127.0.0.1",
		"127.0.0.1:99999",
	}
	for _, addr := range bad {
		o := Options{
			Credential: testCredential, Addr: addr,
			State: &fakeState{}, Events: &fakeEvents{}, Cost: &fakeCost{},
			Control: &fakeControl{}, Message: &fakeMessage{},
		}
		if _, err := New(o); err == nil {
			t.Errorf("监听地址 %q 必须被拒绝", addr)
		}
	}

	good := []string{"127.0.0.1:0", "localhost:0", "[::1]:0"}
	for _, addr := range good {
		o := Options{
			Credential: testCredential, Addr: addr,
			State: &fakeState{}, Events: &fakeEvents{}, Cost: &fakeCost{},
			Control: &fakeControl{}, Message: &fakeMessage{},
		}
		srv, err := New(o)
		if err != nil {
			t.Errorf("监听地址 %q 应该被接受: %v", addr, err)
			continue
		}
		if got := srv.Addr(); got == "" || strings.HasSuffix(got, ":0") {
			t.Errorf("Addr() 必须返回真实端口，实际 %q", got)
		}
		_ = srv.Shutdown(context.Background())
	}
}

// ---------------------------------------------------------------- 安全边界

func TestAuthRequired(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	// 完全不带凭证。
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭证应 401，实际 %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("错误响应不是 JSON: %v", err)
	}
	if _, ok := payload["error"]; !ok {
		t.Errorf("错误响应必须用 error 键（前端读 data.error），实际 %s", body)
	}

	// 凭证错。
	resp2, err := http.Get(ts.URL + "/api/state?t=wrong-token")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("错误凭证应 401，实际 %d", resp2.StatusCode)
	}
	_ = readBody(t, resp2)

	// 凭证正确：头。
	resp3 := req(t, ts, http.MethodGet, "/api/state", "", nil)
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("正确凭证头应 200，实际 %d", resp3.StatusCode)
	}
	_ = readBody(t, resp3)

	// 凭证正确：查询参数。
	resp4, err := http.Get(ts.URL + "/api/state?t=" + testCredential)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp4.StatusCode != http.StatusOK {
		t.Errorf("正确凭证参数应 200，实际 %d", resp4.StatusCode)
	}
	_ = readBody(t, resp4)
}

func TestRootWithTokenSetsCookieAndRedirects(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(ts.URL + "/?t=" + testCredential)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("带 t 的首屏应 302，实际 %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "t=") || strings.Contains(loc, testCredential) {
		t.Errorf("跳转地址不许带凭证，实际 %q", loc)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			found = true
			if !c.HttpOnly {
				t.Error("会话 cookie 必须 HttpOnly")
			}
			if c.Value == testCredential {
				t.Error("会话 cookie 不许直接存凭证，必须存派生值")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("会话 cookie 必须 SameSite=Strict")
			}
		}
	}
	if !found {
		t.Error("带 t 的首屏必须下发会话 cookie")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	resp := req(t, ts, http.MethodGet, "/api/state", "", map[string]string{
		"Origin": "http://evil.example.com",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("跨源请求应 403，实际 %d", resp.StatusCode)
	}
	_ = readBody(t, resp)

	// 同源回环 Origin 必须放行。
	resp2 := req(t, ts, http.MethodGet, "/api/state", "", map[string]string{
		"Origin": "http://127.0.0.1:12345",
	})
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("回环 Origin 应 200，实际 %d", resp2.StatusCode)
	}
	_ = readBody(t, resp2)
}

func TestHostRebindingRejected(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	httpReq, err := http.NewRequest(http.MethodGet, ts.URL+"/api/state", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	httpReq.Header.Set(HeaderToken, testCredential)
	httpReq.Host = "attacker.example.com"
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("非回环 Host 应 403（防 DNS rebinding），实际 %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

func TestCSRFRequiredForWrites(t *testing.T) {
	_, ts, _, _, _, msg, _ := newTestServer(t)

	body := `{"text":"你好","clientMessageId":"c1","executor":"","reviewer":""}`

	// 没有 CSRF 头。
	resp := req(t, ts, http.MethodPost, "/api/message", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("无 CSRF 的 POST 应 403，实际 %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	if msg.got.Text != "" {
		t.Error("被 CSRF 拦下的请求不许进业务逻辑")
	}

	// 带上正确 CSRF。
	snapResp := req(t, ts, http.MethodGet, "/api/state", "", nil)
	var snap Snapshot
	if err := json.Unmarshal([]byte(readBody(t, snapResp)), &snap); err != nil {
		t.Fatalf("快照解析失败: %v", err)
	}
	if snap.Session.CSRF == "" {
		t.Fatal("GET /api/state 必须返回 session.csrf，否则前端所有 POST 都会 403")
	}

	resp2 := req(t, ts, http.MethodPost, "/api/message", body, map[string]string{
		HeaderCSRF: snap.Session.CSRF,
	})
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("带正确 CSRF 的 POST 应 200，实际 %d，体: %s", resp2.StatusCode, readBody(t, resp2))
	} else {
		_ = readBody(t, resp2)
	}
	if msg.got.Text != "你好" {
		t.Errorf("消息正文没有传到业务层，实际 %q", msg.got.Text)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	resp := req(t, ts, http.MethodGet, "/api/state", "", nil)
	_ = readBody(t, resp)

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options 应为 nosniff，实际 %q", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options 应为 DENY，实际 %q", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("必须设置 CSP")
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP 不许含 unsafe-*（assets/ 无内联样式脚本），实际 %q", csp)
	}
}

// ---------------------------------------------------------------- 接口行为

func TestStateSnapshotShape(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	resp := req(t, ts, http.MethodGet, "/api/state", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type 应为 JSON，实际 %q", ct)
	}
	raw := readBody(t, resp)

	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("快照不是 JSON: %v", err)
	}
	// app.js:214 applySnapshot 需要的顶层键，缺一个前端就静默不渲染。
	for _, key := range []string{"session", "current", "bots", "pending", "capabilities", "notices", "messages"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("快照缺少顶层键 %q", key)
		}
	}

	session, _ := payload["session"].(map[string]any)
	if session == nil {
		t.Fatal("session 段缺失")
	}
	if session["csrf"] == nil || session["csrf"] == "" {
		t.Error("session.csrf 不能为空")
	}
	if session["roomId"] != "room-main" {
		t.Errorf("session.roomId 应为 room-main，实际 %v", session["roomId"])
	}
	if strings.Contains(raw, testCredential) {
		t.Error("快照里不许出现访问凭证")
	}

	current, _ := payload["current"].(map[string]any)
	if current == nil {
		t.Fatal("current 段缺失")
	}
	for _, key := range []string{"workId", "goal", "state", "executor", "reviewer", "rounds", "maxRounds", "stopUnconfirmed"} {
		if _, ok := current[key]; !ok {
			t.Errorf("current 缺少字段 %q", key)
		}
	}

	// 未知语义必须在快照里如实体现，不能被压成零值。
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 数量应为 1，实际 %d", len(msgs))
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["operationState"] != "unknown" {
		t.Errorf("操作结果未知必须传 unknown，实际 %v", m0["operationState"])
	}
	if m0["unknownCost"] != true {
		t.Errorf("金额未知必须传 unknownCost=true，实际 %v", m0["unknownCost"])
	}
}

func TestStateEmptySlicesNotNull(t *testing.T) {
	srv, ts, st, _, _, _, _ := newTestServer(t)
	_ = srv
	st.mu.Lock()
	st.snap = Snapshot{Session: SessionView{RoomID: "room-main"}}
	st.mu.Unlock()

	resp := req(t, ts, http.MethodGet, "/api/state", "", nil)
	raw := readBody(t, resp)

	// 前端会对这些字段直接 .length / for 循环，null 会抛 TypeError。
	for _, key := range []string{"bots", "pending", "capabilities", "notices", "messages"} {
		if strings.Contains(raw, `"`+key+`":null`) {
			t.Errorf("%q 必须是 [] 而不是 null", key)
		}
	}
}

func TestCostUnknownIsSeparate(t *testing.T) {
	_, ts, _, _, cost, _, _ := newTestServer(t)

	resp := req(t, ts, http.MethodGet, "/api/cost?dim=day", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	if cost.gotDim != "day" {
		t.Errorf("dim 没有透传到数据源，实际 %q", cost.gotDim)
	}
	raw := readBody(t, resp)

	var report map[string]any
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("成本响应不是 JSON: %v", err)
	}
	for _, key := range []string{
		"unknownAmountCalls", "unknownReason", "byCurrency", "calls",
		"callsWithTokens", "tokensIn", "tokensOut", "cacheReadTokens",
		"reservedCalls", "rows", "limits",
	} {
		if _, ok := report[key]; !ok {
			t.Errorf("成本响应缺少字段 %q（前端 renderCost 会读到 undefined）", key)
		}
	}
	// 未知金额调用必须单独计数，不能并进已知总额（COORDINATION.md §8④）。
	if n, _ := report["unknownAmountCalls"].(float64); n != 3 {
		t.Errorf("unknownAmountCalls 应为 3，实际 %v", report["unknownAmountCalls"])
	}
	if report["unknownReason"] == "" {
		t.Error("有未知金额调用时必须给出原因")
	}
	// 跨币种不许合并成一个数字。
	byCurrency, _ := report["byCurrency"].([]any)
	if len(byCurrency) != 1 {
		t.Errorf("byCurrency 应按币种分列，实际 %v", byCurrency)
	}

	// 非法 dim 必须 400。
	resp2 := req(t, ts, http.MethodGet, "/api/cost?dim=week", "", nil)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 dim 应 400，实际 %d", resp2.StatusCode)
	}
	_ = readBody(t, resp2)
}

func TestCostDefaultDim(t *testing.T) {
	_, ts, _, _, cost, _, _ := newTestServer(t)
	resp := req(t, ts, http.MethodGet, "/api/cost", "", nil)
	_ = readBody(t, resp)
	if cost.gotDim != "bot" {
		t.Errorf("默认 dim 应为 bot（前端默认标签页），实际 %q", cost.gotDim)
	}
}

func TestMessageEndpoint(t *testing.T) {
	_, ts, _, _, _, msg, _ := newTestServer(t)
	csrf := csrfOf(t, ts)

	// 正常发送。
	resp := req(t, ts, http.MethodPost, "/api/message",
		`{"text":"  写个报告  ","clientMessageId":"c-9","executor":"bot-a","reviewer":"__none__"}`,
		map[string]string{HeaderCSRF: csrf})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	var out struct {
		Message MessageView `json:"message"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if out.Message.ID == "" {
		t.Error("响应必须带 message.id（前端用 id 去重）")
	}
	if msg.got.Text != "写个报告" {
		t.Errorf("正文应去掉首尾空白，实际 %q", msg.got.Text)
	}
	if msg.got.ClientMessageID != "c-9" {
		t.Errorf("clientMessageId 必须透传（幂等键），实际 %q", msg.got.ClientMessageID)
	}
	if msg.got.Executor != "bot-a" || msg.got.Reviewer != "__none__" {
		t.Errorf("执行/审查选择没有透传，实际 %q / %q", msg.got.Executor, msg.got.Reviewer)
	}

	// 空正文。
	resp2 := req(t, ts, http.MethodPost, "/api/message", `{"text":"   "}`,
		map[string]string{HeaderCSRF: csrf})
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("空正文应 400，实际 %d", resp2.StatusCode)
	}
	_ = readBody(t, resp2)

	// 坏 JSON。
	resp3 := req(t, ts, http.MethodPost, "/api/message", `{"text":`,
		map[string]string{HeaderCSRF: csrf})
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("坏 JSON 应 400，实际 %d", resp3.StatusCode)
	}
	_ = readBody(t, resp3)

	// 方法不对。
	resp4 := req(t, ts, http.MethodGet, "/api/message", "", nil)
	if resp4.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/message 应 405，实际 %d", resp4.StatusCode)
	}
	_ = readBody(t, resp4)
}

func TestMessageInternalErrorHidesDetail(t *testing.T) {
	_, ts, _, _, _, msg, _ := newTestServer(t)
	msg.err = fmt.Errorf("sql: SELECT * FROM messages WHERE secret_column = 'x'")
	csrf := csrfOf(t, ts)

	resp := req(t, ts, http.MethodPost, "/api/message", `{"text":"hi"}`,
		map[string]string{HeaderCSRF: csrf})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	raw := readBody(t, resp)
	if strings.Contains(raw, "secret_column") || strings.Contains(raw, "SELECT") {
		t.Errorf("内部错误细节不许进响应体，实际 %s", raw)
	}
}

func TestMessagePublicErrorPassesThrough(t *testing.T) {
	_, ts, _, _, _, msg, _ := newTestServer(t)
	msg.err = NewPublicError("当前没有进行中的委托。")
	csrf := csrfOf(t, ts)

	resp := req(t, ts, http.MethodPost, "/api/message", `{"text":"hi"}`,
		map[string]string{HeaderCSRF: csrf})
	raw := readBody(t, resp)
	var payload map[string]any
	_ = json.Unmarshal([]byte(raw), &payload)
	if payload["error"] != "当前没有进行中的委托。" {
		t.Errorf("PublicError 应原样透出，实际 %v", payload["error"])
	}
}

func TestControlEndpoint(t *testing.T) {
	_, ts, _, _, _, _, ctl := newTestServer(t)
	csrf := csrfOf(t, ts)

	resp := req(t, ts, http.MethodPost, "/api/work/work-1/control", `{"action":"stop"}`,
		map[string]string{HeaderCSRF: csrf})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	var out struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	// 前端把 note 原样显示（app.js:931）。停止没确认时必须说"待确认"。
	if out.Note != "停止待确认：客户端尚未回应" {
		t.Errorf("note 必须原样透出，实际 %q", out.Note)
	}
	if ctl.gotWorkID != "work-1" || ctl.gotReq.Action != "stop" {
		t.Errorf("控制请求没有透传，实际 %q / %q", ctl.gotWorkID, ctl.gotReq.Action)
	}

	// 审批动作。
	resp2 := req(t, ts, http.MethodPost, "/api/work/work-1/control",
		`{"action":"approval","approvalId":"ap-1","option":"允许一次"}`,
		map[string]string{HeaderCSRF: csrf})
	_ = readBody(t, resp2)
	if ctl.gotReq.ApprovalID != "ap-1" || ctl.gotReq.Option != "允许一次" {
		t.Errorf("审批字段没有透传，实际 %q / %q", ctl.gotReq.ApprovalID, ctl.gotReq.Option)
	}

	// 忽略待决定项。
	resp3 := req(t, ts, http.MethodPost, "/api/work/work-1/control",
		`{"action":"dismiss_pending","pendingId":"p-1"}`,
		map[string]string{HeaderCSRF: csrf})
	_ = readBody(t, resp3)
	if ctl.gotReq.PendingID != "p-1" {
		t.Errorf("pendingId 没有透传，实际 %q", ctl.gotReq.PendingID)
	}

	// 换人必须带 executor。
	resp4 := req(t, ts, http.MethodPost, "/api/work/work-1/control",
		`{"action":"replace","executor":"bot-b"}`,
		map[string]string{HeaderCSRF: csrf})
	_ = readBody(t, resp4)
	if ctl.gotReq.Executor != "bot-b" {
		t.Errorf("executor 没有透传，实际 %q", ctl.gotReq.Executor)
	}

	// 未知动作。
	resp5 := req(t, ts, http.MethodPost, "/api/work/work-1/control", `{"action":"explode"}`,
		map[string]string{HeaderCSRF: csrf})
	if resp5.StatusCode != http.StatusBadRequest {
		t.Errorf("未知动作应 400，实际 %d", resp5.StatusCode)
	}
	_ = readBody(t, resp5)

	// 路径不合法。
	resp6 := req(t, ts, http.MethodPost, "/api/work//control", `{"action":"pause"}`,
		map[string]string{HeaderCSRF: csrf})
	if resp6.StatusCode != http.StatusNotFound {
		t.Errorf("空 work id 应 404，实际 %d", resp6.StatusCode)
	}
	_ = readBody(t, resp6)
}

func TestControlWorkIDUnescaped(t *testing.T) {
	_, ts, _, _, _, _, ctl := newTestServer(t)
	csrf := csrfOf(t, ts)

	resp := req(t, ts, http.MethodPost, "/api/work/work%2F1/control", `{"action":"pause"}`,
		map[string]string{HeaderCSRF: csrf})
	_ = readBody(t, resp)
	// 含 / 的 id 被转义后是合法的，必须解回来再交给业务层。
	if ctl.gotWorkID != "work/1" {
		t.Errorf("work id 必须做 PathUnescape，实际 %q", ctl.gotWorkID)
	}
}

// ---------------------------------------------------------------- 静态资源

func TestStaticAssets(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	index := req(t, ts, http.MethodGet, "/", "", nil)
	if index.StatusCode != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", index.StatusCode)
	}
	if ct := index.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index.html 的 Content-Type 应为 text/html，实际 %q", ct)
	}
	body := readBody(t, index)
	if !strings.Contains(body, "/assets/app.js") {
		t.Error("index.html 应引用 /assets/app.js")
	}

	cases := map[string]string{
		"/assets/app.js":    "text/javascript",
		"/assets/style.css": "text/css",
	}
	for path, wantCT := range cases {
		resp := req(t, ts, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s 应 200，实际 %d", path, resp.StatusCode)
			_ = readBody(t, resp)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantCT) {
			t.Errorf("%s 的 Content-Type 应为 %s，实际 %q", path, wantCT, ct)
		}
		_ = readBody(t, resp)
	}

	// 白名单外的名字必须 404，不能读到别的文件。
	for _, path := range []string{"/assets/embed.go", "/assets/../embed.go", "/assets/nope.txt"} {
		resp := req(t, ts, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("GET %s 应 404，实际 %d", path, resp.StatusCode)
		}
		_ = readBody(t, resp)
	}

	// 静态资源也要凭证。
	resp, err := http.Get(ts.URL + "/assets/app.js")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭证访问静态资源应 401，实际 %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// ---------------------------------------------------------------- SSE

func TestSSEInitialStateFrame(t *testing.T) {
	_, ts, _, ev, _, _, _ := newTestServer(t)
	ev.mu.Lock()
	ev.history = []contract.Event{
		{EventID: "e1", RunID: "run-1", WorkID: "work-1", BotID: "bot-a", Seq: 1,
			Kind: contract.EventOutput, Text: "第一段", TS: time.UnixMilli(1700000000000)},
		{EventID: "e2", RunID: "run-1", WorkID: "work-1", BotID: "bot-a", Seq: 2,
			Kind: contract.EventOutput, Text: "第二段", TS: time.UnixMilli(1700000001000)},
	}
	ev.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?cursor=0", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	httpReq.Header.Set(HeaderToken, testCredential)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}

	// 读到 state 帧为止：补读的 event 帧在它之前。
	got, err := readFrames(resp.Body, 3)
	if err != nil {
		t.Fatalf("读 SSE 帧失败: %v", err)
	}

	var kinds []string
	for _, f := range got {
		kinds = append(kinds, f.name)
	}
	if len(got) < 3 {
		t.Fatalf("至少应有 2 个 event + 1 个 state 帧，实际 %v", kinds)
	}
	if got[0].name != "event" || got[1].name != "event" {
		t.Errorf("补读的历史事件应先是 event 帧，实际 %v", kinds)
	}
	if got[2].name != "state" {
		t.Errorf("补读之后必须推一帧完整 state 让前端对齐，实际 %v", kinds)
	}

	var e1 map[string]any
	if err := json.Unmarshal([]byte(got[0].data), &e1); err != nil {
		t.Fatalf("event 帧不是 JSON: %v", err)
	}
	// 前端靠这些字段渲染并推进游标（app.js:1028）。
	for _, key := range []string{"seq", "kind", "runId", "botId", "text"} {
		if _, ok := e1[key]; !ok {
			t.Errorf("event 帧缺少字段 %q", key)
		}
	}
	if e1["kind"] != "output" {
		t.Errorf("kind 应为 output，实际 %v", e1["kind"])
	}
	if seq, _ := e1["seq"].(float64); seq != 1 {
		t.Errorf("第一个补读事件的 seq 应为 1，实际 %v", e1["seq"])
	}

	var snap map[string]any
	if err := json.Unmarshal([]byte(got[2].data), &snap); err != nil {
		t.Fatalf("state 帧不是 JSON: %v", err)
	}
	session, _ := snap["session"].(map[string]any)
	if session == nil || session["csrf"] == nil || session["csrf"] == "" {
		t.Error("SSE 的 state 帧也必须带 session.csrf")
	}
	if strings.Contains(got[2].data, testCredential) {
		t.Error("SSE 流里不许出现访问凭证")
	}
}

func TestSSECursorReplay(t *testing.T) {
	_, ts, _, ev, _, _, _ := newTestServer(t)
	ev.mu.Lock()
	for i := int64(1); i <= 5; i++ {
		ev.history = append(ev.history, contract.Event{
			EventID: fmt.Sprintf("e%d", i), RunID: "run-1", Seq: i,
			Kind: contract.EventOutput, Text: fmt.Sprintf("第%d段", i),
			TS: time.UnixMilli(1700000000000 + i*1000),
		})
	}
	ev.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?cursor=3", nil)
	httpReq.Header.Set(HeaderToken, testCredential)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	got, err := readFrames(resp.Body, 3)
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("cursor=3 应补出 seq 4、5 两帧，实际 %d 帧", len(got))
	}
	for i, wantSeq := range []float64{4, 5} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(got[i].data), &payload); err != nil {
			t.Fatalf("帧解析失败: %v", err)
		}
		if seq, _ := payload["seq"].(float64); seq != wantSeq {
			t.Errorf("第 %d 帧的 seq 应为 %.0f，实际 %v", i, wantSeq, payload["seq"])
		}
	}
}

func TestSSEBadCursor(t *testing.T) {
	_, ts, _, _, _, _, _ := newTestServer(t)

	for _, cur := range []string{"abc", "-1"} {
		resp := req(t, ts, http.MethodGet, "/api/events?cursor="+cur, "", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cursor=%q 应 400，实际 %d", cur, resp.StatusCode)
		}
		_ = readBody(t, resp)
	}
}

func TestSSELiveEventDelivered(t *testing.T) {
	_, ts, _, ev, _, _, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?cursor=0", nil)
	httpReq.Header.Set(HeaderToken, testCredential)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 先吃掉初始 state 帧，确保订阅已建立。
	if _, err := readFrames(resp.Body, 1); err != nil {
		t.Fatalf("读初始帧失败: %v", err)
	}

	ev.push(contract.Event{
		EventID: "live-1", RunID: "run-9", BotID: "bot-a", Seq: 1,
		Kind: contract.EventOutput, Text: "实时输出",
		TS: time.UnixMilli(1700000009000),
	})

	got, err := readFrames(resp.Body, 1)
	if err != nil {
		t.Fatalf("读实时帧失败: %v", err)
	}
	if got[0].name != "event" {
		t.Fatalf("实时帧名应为 event，实际 %q", got[0].name)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got[0].data), &payload); err != nil {
		t.Fatalf("帧解析失败: %v", err)
	}
	if payload["text"] != "实时输出" {
		t.Errorf("实时事件正文不对，实际 %v", payload["text"])
	}
}

func TestSSEShutdownClosesConnections(t *testing.T) {
	srv, ts, _, _, _, _, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events?cursor=0", nil)
	httpReq.Header.Set(HeaderToken, testCredential)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if _, err := readFrames(resp.Body, 1); err != nil {
		t.Fatalf("读初始帧失败: %v", err)
	}

	// Shutdown 必须主动掐断 SSE，否则 http.Server.Shutdown 会一直等到超时。
	done := make(chan error, 1)
	go func() {
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		done <- srv.Shutdown(sctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown 失败: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Shutdown 被 SSE 长连接卡住")
	}
}

func TestSSESlowConsumerDropped(t *testing.T) {
	h := newSSEHub()
	sub := h.subscribe()

	// 灌满缓冲再灌一轮：慢消费者必须被摘掉，broadcast 绝不能阻塞。
	for i := 0; i < sseBuffer*4; i++ {
		h.broadcast(contract.Event{EventID: fmt.Sprint(i), RunID: "r", Seq: int64(i + 1)})
	}

	if n := h.count(); n != 0 {
		t.Errorf("慢消费者应被摘掉，剩余 %d 个", n)
	}
	// 通道必须已关闭，让 handler 的 select 醒来。
	if _, ok := <-sub.ch; ok {
		// 缓冲区里还有残留事件是正常的；排空后必须收到关闭。
		for range sub.ch {
		}
	}

	// 摘掉之后继续广播不许 panic。
	h.broadcast(contract.Event{EventID: "x", RunID: "r", Seq: 9999})
	h.closeAll()
	h.closeAll() // 幂等
}

func TestAcceptEventDedupes(t *testing.T) {
	seen := make(map[string]int64)

	if !acceptEvent(seen, contract.Event{RunID: "r1", Seq: 5}) {
		t.Error("首个事件应被接受")
	}
	if acceptEvent(seen, contract.Event{RunID: "r1", Seq: 5}) {
		t.Error("同 (run, seq) 必须去重")
	}
	if acceptEvent(seen, contract.Event{RunID: "r1", Seq: 4}) {
		t.Error("seq 回退必须丢弃")
	}
	if !acceptEvent(seen, contract.Event{RunID: "r1", Seq: 6}) {
		t.Error("seq 前进应被接受")
	}
	// 不同 Run 的 seq 各自独立（契约里 seq 只在单 Run 内单调）。
	if !acceptEvent(seen, contract.Event{RunID: "r2", Seq: 1}) {
		t.Error("不同 run 的 seq 应各自独立")
	}
	// 没有 seq 的事件无法去重也无法推进游标。
	if acceptEvent(seen, contract.Event{RunID: "r3", Seq: 0}) {
		t.Error("seq<=0 必须丢弃")
	}
}

// ---------------------------------------------------------------- 辅助

type sseFrame struct {
	name string
	data string
}

// readFrames 从 SSE 流里读最多 n 个具名帧（跳过注释行）。
func readFrames(r io.Reader, n int) ([]sseFrame, error) {
	var frames []sseFrame
	var name, data string
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1)

	for len(frames) < n {
		b, err := r.Read(tmp)
		if err != nil {
			return frames, err
		}
		if b == 0 {
			continue
		}
		if tmp[0] != '\n' {
			buf = append(buf, tmp[0])
			continue
		}
		line := strings.TrimSuffix(string(buf), "\r")
		buf = buf[:0]
		switch {
		case line == "":
			if name != "" {
				frames = append(frames, sseFrame{name: name, data: data})
			}
			name, data = "", ""
		case strings.HasPrefix(line, ":"):
			// 注释行（keepalive），忽略。
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return frames, nil
}

// csrfOf 从 /api/state 取 csrf 令牌，模拟前端启动流程。
func csrfOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := req(t, ts, http.MethodGet, "/api/state", "", nil)
	var snap Snapshot
	if err := json.Unmarshal([]byte(readBody(t, resp)), &snap); err != nil {
		t.Fatalf("取 csrf 失败: %v", err)
	}
	if snap.Session.CSRF == "" {
		t.Fatal("session.csrf 为空")
	}
	return snap.Session.CSRF
}
