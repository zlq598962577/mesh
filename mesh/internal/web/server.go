// Package web 实现本地 Web UI 的 HTTP 服务端（§6）。
//
// 设计边界（务必先读，改代码前也要读）：
//
//  1. 只监听回环地址。Addr 只接受 127.0.0.1:0 / [::1]:0 / localhost:0 这类形式，
//     端口由内核分配，真实地址用 Server.Addr() 取。绝不监听 0.0.0.0。
//  2. 凭证由调用方（app）用 crypto/rand 生成并写入 <data_dir>/web-token（0600），
//     本包只做校验，绝不生成、绝不打印、绝不放进任何响应体或 SSE 帧。
//  3. 本包不 import orchestrator / store 的具体实现。所有数据都从 Options 里的窄接口拿，
//     具体拼装由 app 负责。这样 web 可以独立测试（见 server_test.go 的假实现）。
//  4. 前端契约以 assets/app.js 为准，不是以本包为准。改字段名先看 app.js 怎么读。
//
// 浏览器怎么拿到凭证：浏览器不能自己带自定义头（EventSource 更不行），所以
// app 打印的 URL 形如 http://127.0.0.1:PORT/?t=<凭证>；GET / 校验 t 之后下发
// HttpOnly 的会话 cookie 并 302 到去掉 t 的同一路径。之后所有请求（含 SSE）
// 靠 cookie 认证。非浏览器调用方（自检脚本、验收测试）可以用 X-Mesh-Token 头
// 或 ?t= 查询参数，三选一即可。
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mesh/internal/contract"
)

// 认证相关的名字。前端只在 postJSON 里硬编码了 X-Mesh-CSRF，其余是本包内部约定。
const (
	// CookieName 是会话 cookie 名，值是凭证的 SHA-256，不是凭证本身。
	CookieName = "mesh_token"
	// HeaderToken 给非浏览器调用方用（curl / 验收测试）。
	HeaderToken = "X-Mesh-Token"
	// HeaderCSRF 是前端 postJSON 里写的那个头（assets/app.js:168）。
	HeaderCSRF = "X-Mesh-CSRF"

	// cookieMaxAge 会话 cookie 的有效期。本机单用户，给一个工作日足够。
	cookieMaxAge = 12 * time.Hour
	// maxJSONBody 限制 POST 请求体，避免一条超长消息把内存吃光。
	maxJSONBody = 1 << 20
	// replayLimit 单次补读上限。超出部分靠游标继续推进，不一次性拉全量。
	replayLimit = 2000
)

// 本包的错误值。调用方用 errors.Is 判断，不要比对字符串。
var (
	// ErrNoCredential 表示没给凭证。
	ErrNoCredential = errors.New("web: 必须提供访问凭证")
	// ErrBadAddr 表示监听地址不是回环地址或格式不对。
	ErrBadAddr = errors.New("web: 监听地址必须是回环地址")
	// ErrMissingDependency 表示某个必需的数据源没注入。
	ErrMissingDependency = errors.New("web: 缺少必需的数据源")
	// ErrUnauthorized 表示凭证不对。
	ErrUnauthorized = errors.New("web: 凭证无效")
	// ErrShutdown 表示服务已经关闭。
	ErrShutdown = errors.New("web: 服务已关闭")
)

// ------------------------------------------------------------------ *
// 与前端对齐的传输结构（字段名以 assets/app.js 的读取代码为准）
// ------------------------------------------------------------------ *

// SessionView 是快照里的 session 段。csrf 是前端每个 POST 都要回带的令牌
// （app.js:217 → postJSON 的 X-Mesh-CSRF 头），缺了它所有写操作都会 403。
type SessionView struct {
	CSRF   string `json:"csrf"`
	RoomID string `json:"roomId"`
}

// RoleView 是"某一版分工里的某个角色"。前端会显示成 v{n}（app.js:280）。
type RoleView struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// WorkView 是当前进行中的委托（app.js:245 renderWork）。
type WorkView struct {
	WorkID string `json:"workId"`
	Goal   string `json:"goal"`
	State  string `json:"state"`
	// Executor / Reviewer 的 Name 为空时前端显示"未指定"。
	Executor RoleView `json:"executor"`
	Reviewer RoleView `json:"reviewer"`
	Rounds   int      `json:"rounds"`
	MaxRounds int     `json:"maxRounds"`
	// StopUnconfirmed 是三类"未知"之二：没有确定的终止确认就不能显示"已停止"
	// （app.js:293 renderStopFlag）。
	StopUnconfirmed bool   `json:"stopUnconfirmed"`
	StopDetail      string `json:"stopDetail"`
}

// CapView 是一条能力受限项（app.js:466）。
type CapView struct {
	Scope  string `json:"scope"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// BotView 是 bot 状态行（app.js:429 buildBotRow）。
type BotView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Adapter      string    `json:"adapter"`
	Status       string    `json:"status"`
	StatusDetail string    `json:"statusDetail"`
	WorkGoal     string    `json:"workGoal"`
	Caps         []CapView `json:"caps"`
}

// PendingView 是待用户决定项（app.js:335 buildDecision）。
// Kind 为 "approval" 时前端按 Options 生成按钮并走 approval 动作；
// 其余 kind 只给一个"知道了"按钮，走 dismiss_pending。
type PendingView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Detail     string   `json:"detail"`
	Options    []string `json:"options"`
	WorkID     string   `json:"workId"`
	ApprovalID string   `json:"approvalId"`
}

// NoticeView 是一条提示（app.js:513）。
type NoticeView struct {
	Text  string `json:"text"`
	Level string `json:"level"`
}

// MessageView 是一条消息（app.js:582 appendMessage）。
//
// OperationState 与 UnknownCost 是"未知"语义的落点：外部副作用结果不确定时
// 必须传 "unknown"，金额不确定时必须让 UnknownCost 为 true——两者都不许用
// 0 / 空字符串冒充"已知"。
type MessageView struct {
	ID             string `json:"id"`
	Author         string `json:"author"`
	AuthorKind     string `json:"authorKind"`
	Kind           string `json:"kind"`
	Text           string `json:"text"`
	CreatedAtMs    int64  `json:"createdAtMs"`
	RoleVersion    int    `json:"roleVersion"`
	Version        int    `json:"version"`
	Streaming      bool   `json:"streaming"`
	Unverified     bool   `json:"unverified"`
	OperationState string `json:"operationState"`
	UnknownCost    bool   `json:"unknownCost"`
}

// Snapshot 是 GET /api/state 的响应，也是 SSE 的 state 帧内容（app.js:214）。
type Snapshot struct {
	Session      SessionView    `json:"session"`
	Current      *WorkView      `json:"current"`
	Bots         []BotView      `json:"bots"`
	Pending      []PendingView  `json:"pending"`
	Capabilities []CapView      `json:"capabilities"`
	Notices      []NoticeView   `json:"notices"`
	Messages     []MessageView  `json:"messages"`
}

// Money 是一个币种的金额。micros 是 1e-6 货币单位（app.js:53 fmtAmount）。
// 跨币种绝不相加——前端 fmtMoneyList 也只是并列展示。
type Money struct {
	Currency     string `json:"currency"`
	AmountMicros int64  `json:"amountMicros"`
}

// CostRow 是成本明细表的一行（app.js:767 buildCostRow）。
type CostRow struct {
	Label              string  `json:"label"`
	Key                string  `json:"key"`
	Calls              int64   `json:"calls"`
	UnknownAmountCalls int64   `json:"unknownAmountCalls"`
	UnknownReason      string  `json:"unknownReason"`
	Note               string  `json:"note"`
	CallsWithTokens    int64   `json:"callsWithTokens"`
	TokensIn           int64   `json:"tokensIn"`
	TokensOut          int64   `json:"tokensOut"`
	ByCurrency         []Money `json:"byCurrency"`
}

// CostLimit 是一条限额说明（app.js:745）。
// Limit 必须是字符串：客户端读不到硬上限时传空，前端会显示"未知"，
// 绝不允许传一个编出来的数字假装有预算。
type CostLimit struct {
	Label string `json:"label"`
	Limit string `json:"limit"`
	Note  string `json:"note"`
}

// CostReport 是 GET /api/cost 的响应（app.js:675 renderCost）。
//
// UnknownAmountCalls 与 ByCurrency 必须分开：金额未知的调用永远单独成一条横幅，
// 绝不并进已知总额（§9.6 + COORDINATION.md §8④）。
type CostReport struct {
	UnknownAmountCalls int64       `json:"unknownAmountCalls"`
	UnknownReason      string      `json:"unknownReason"`
	ByCurrency         []Money     `json:"byCurrency"`
	Calls              int64       `json:"calls"`
	CallsWithTokens    int64       `json:"callsWithTokens"`
	TokensIn           int64       `json:"tokensIn"`
	TokensOut          int64       `json:"tokensOut"`
	CacheReadTokens    int64       `json:"cacheReadTokens"`
	ReservedCalls      int64       `json:"reservedCalls"`
	Rows               []CostRow   `json:"rows"`
	Limits             []CostLimit `json:"limits"`
}

// SendRequest 是 POST /api/message 的请求体（app.js:844 sendMessage）。
// Executor / Reviewer 为空字符串表示"由 agent 建议"。
type SendRequest struct {
	Text            string `json:"text"`
	ClientMessageID string `json:"clientMessageId"`
	Executor        string `json:"executor"`
	Reviewer        string `json:"reviewer"`
}

// ControlRequest 是 POST /api/work/{id}/control 的请求体。
//
// Action 取值来自前端：pause / resume / stop / replace / approval / dismiss_pending。
// approval 用 ApprovalID + Option；dismiss_pending 用 PendingID；replace 用 Executor。
type ControlRequest struct {
	Action     string `json:"action"`
	Executor   string `json:"executor"`
	ApprovalID string `json:"approvalId"`
	Option     string `json:"option"`
	PendingID  string `json:"pendingId"`
}

// streamEvent 是 SSE event 帧的负载（app.js:1028 onStreamEvent）。
//
// seq 用持久化的事件 seq（与 events(run_id, seq) 对齐），前端拿它推进游标。
// BotName 允许为空：为空时前端退回显示 botId（app.js:1067）。
type streamEvent struct {
	EventID  string                `json:"eventId"`
	RunID    string                `json:"runId"`
	WorkID   string                `json:"workId"`
	BotID    string                `json:"botId"`
	BotName  string                `json:"botName"`
	Seq      int64                 `json:"seq"`
	Kind     string                `json:"kind"`
	Text     string                `json:"text"`
	TS       int64                 `json:"ts"`
	Session  *contract.SessionInfo  `json:"session,omitempty"`
	Final    *contract.FinalInfo    `json:"final,omitempty"`
	Approval *contract.ApprovalInfo `json:"approval,omitempty"`
}

// ------------------------------------------------------------------ *
// 数据源接口：由 app 实现，web 只消费
// ------------------------------------------------------------------ *

// StateSource 提供整页快照。
type StateSource interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// EventStream 提供事件补读与实时订阅。
//
// 语义约定（实现方必须遵守，否则断线补齐会丢事件）：
//   - Replay 返回客户端还没见过的事件，按时间升序，最多 limit 条；
//     调用方用 cursor 表示"我已经处理到这个位置了"。
//   - Subscribe 返回的通道在 ctx 取消时必须关闭；实现方负责对多个订阅者做扇出，
//     并且允许丢弃慢订阅者，绝不阻塞事件生产方。
type EventStream interface {
	Replay(ctx context.Context, cursor int64, limit int) ([]contract.Event, error)
	Subscribe(ctx context.Context) (<-chan contract.Event, error)
}

// CostSource 提供成本汇总。
type CostSource interface {
	Cost(ctx context.Context, dim string) (CostReport, error)
}

// MessageSink 接收用户消息，返回落库后的消息视图。
type MessageSink interface {
	SendMessage(ctx context.Context, req SendRequest) (MessageView, error)
}

// ControlSink 处理控制动作。
//
// 返回值 note 会被前端原样显示（app.js:916 sendControl 读 res.note）。
// 动作没被立即确认时（例如停止请求已下发但客户端没回应），note 必须说清楚
// 是"待确认"而不是"已停止"——这是三类"未知"之二的服务端侧义务。
type ControlSink interface {
	Control(ctx context.Context, workID string, req ControlRequest) (note string, err error)
}

// Options 是 New 的全部输入。七个字段都要给，缺一个就启动失败。
type Options struct {
	// Credential 由 app 生成（crypto/rand 32 字节 hex），本包只校验。
	Credential string
	// Addr 必须是回环地址，端口建议 0（内核分配）。
	Addr string
	// State / Events / Cost / Control / Message 是五个窄接口，由 app 实现。
	State   StateSource
	Events  EventStream
	Cost    CostSource
	Control ControlSink
	Message MessageSink
}

// Server 是本地 Web UI 的 HTTP 服务端。
//
// 生命周期：New（校验 + 绑定监听 + 启动实时事件泵）→ Serve（阻塞）→ Shutdown（收尾）。
// New 就绑定监听，是为了让 app 能立刻打印真实端口给用户。
type Server struct {
	opts Options

	ln         net.Listener
	httpServer *http.Server
	hub        *sseHub

	// csrf 是前端每个 POST 都要回带的令牌，由凭证派生，进程内稳定。
	csrf string
	// cookieValue 是会话 cookie 的值，由凭证派生，不把凭证本身交给浏览器。
	cookieValue string

	baseCtx    context.Context
	baseCancel context.CancelFunc

	closeOnce sync.Once
}

// New 校验参数、绑定回环监听、启动实时事件泵。
//
// 返回的 *Server 已经可以调用 Addr()；Serve() 才开始接受请求。
func New(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.Credential) == "" {
		return nil, ErrNoCredential
	}
	if opts.State == nil {
		return nil, fmt.Errorf("%w: State 未注入", ErrMissingDependency)
	}
	if opts.Events == nil {
		return nil, fmt.Errorf("%w: Events 未注入", ErrMissingDependency)
	}
	if opts.Cost == nil {
		return nil, fmt.Errorf("%w: Cost 未注入", ErrMissingDependency)
	}
	if opts.Control == nil {
		return nil, fmt.Errorf("%w: Control 未注入", ErrMissingDependency)
	}
	if opts.Message == nil {
		return nil, fmt.Errorf("%w: Message 未注入", ErrMissingDependency)
	}

	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := validateLoopbackAddr(addr); err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("web: 绑定 %s 失败: %w", addr, err)
	}
	// 双保险：监听成功后重新确认落在回环地址上（防止被 addr 之外的解析绕过）。
	if err := validateLoopbackAddr(ln.Addr().String()); err != nil {
		_ = ln.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:        opts,
		ln:          ln,
		hub:         newSSEHub(),
		csrf:        deriveToken(opts.Credential, "csrf"),
		cookieValue: deriveToken(opts.Credential, "cookie"),
		baseCtx:     ctx,
		baseCancel:  cancel,
	}

	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout 必须为 0：SSE 是长连接，任何写超时都会把它掐断。
		WriteTimeout: 0,
		ErrorLog:     nil,
	}

	go s.pumpEvents()

	return s, nil
}

// Addr 返回真实监听地址（形如 127.0.0.1:52341）。app 打印给用户的就是它。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Handler 暴露完整中间件链，便于测试直接 http.Handler 驱动，不用真起监听。
func (s *Server) Handler() http.Handler { return s.httpServer.Handler }

// Serve 开始接受请求，阻塞直到 Shutdown 或监听出错。
func (s *Server) Serve() error {
	err := s.httpServer.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭：先掐掉所有 SSE 连接（否则 http.Server.Shutdown 会一直等到超时），
// 再关 HTTP。
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	s.closeOnce.Do(func() {
		s.hub.closeAll()
		if s.baseCancel != nil {
			s.baseCancel()
		}
		if err := s.httpServer.Shutdown(ctx); err != nil {
			firstErr = fmt.Errorf("web: 关闭 HTTP 服务失败: %w", err)
		}
	})
	return firstErr
}

// ------------------------------------------------------------------ *
// 路由
// ------------------------------------------------------------------ *

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/assets/", s.handleAsset)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/cost", s.handleCost)
	mux.HandleFunc("/api/message", s.handleMessage)
	mux.HandleFunc("/api/work/", s.handleWorkControl)
	return s.chain(mux)
}

// chain 按固定顺序串中间件：Host → 凭证 → Origin → CSRF。
//
// 顺序不能改：先确认请求打的是本机监听（防 DNS rebinding），再确认调用方有凭证，
// 再确认来源是同源页面，最后才校验写操作令牌。任何一步失败都不进业务逻辑。
func (s *Server) chain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if !hostAllowed(r) {
			writeJSONError(w, http.StatusForbidden, "Host 不在允许列表内")
			return
		}

		authed, needsRedirect := s.checkCredential(w, r)
		if !authed {
			writeJSONError(w, http.StatusUnauthorized, "缺少或错误的访问凭证")
			return
		}
		if needsRedirect {
			// 带 ?t= 的首屏请求：下发 cookie 后跳到干净 URL，凭证不留在地址栏/历史里。
			http.Redirect(w, r, stripTokenQuery(r.URL), http.StatusFound)
			return
		}

		if !originAllowed(r) {
			writeJSONError(w, http.StatusForbidden, "跨源请求被拒绝")
			return
		}

		if isWriteMethod(r.Method) && !s.checkCSRF(r) {
			writeJSONError(w, http.StatusForbidden, "缺少或错误的 "+HeaderCSRF+" 令牌")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// checkCredential 校验凭证。第二个返回值表示"凭证是对的，但要下发 cookie 并跳转"。
//
// 三种带法（任一命中即可）：会话 cookie、X-Mesh-Token 头、?t= 查询参数。
// 全部用常数时间比较，避免把凭证长度泄漏成时序信道。
func (s *Server) checkCredential(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if c, err := r.Cookie(CookieName); err == nil && constantTimeEqual(c.Value, s.cookieValue) {
		return true, false
	}
	if constantTimeEqual(r.Header.Get(HeaderToken), s.opts.Credential) {
		return true, false
	}
	if constantTimeEqual(r.URL.Query().Get("t"), s.opts.Credential) {
		// 查询参数命中：只对首屏下发 cookie，避免每次 API 调用都跳转。
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			http.SetCookie(w, &http.Cookie{
				Name:     CookieName,
				Value:    s.cookieValue,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   int(cookieMaxAge / time.Second),
			})
			return true, true
		}
		return true, false
	}
	return false, false
}

// checkCSRF 校验写操作令牌。前端从 GET /api/state 的 session.csrf 拿到它，
// 之后每个 POST 都放进 X-Mesh-CSRF 头（app.js:168）。
func (s *Server) checkCSRF(r *http.Request) bool {
	return constantTimeEqual(r.Header.Get(HeaderCSRF), s.csrf)
}

// ------------------------------------------------------------------ *
// 静态资源
// ------------------------------------------------------------------ *

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusNotFound, "没有这个页面")
		return
	}
	s.writeAsset(w, r, "index.html")
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	// 白名单在 embed.go 里，这里不重复判断；非白名单名字会拿到 errBadAssetName。
	if name == "" {
		writeJSONError(w, http.StatusNotFound, "没有这个静态资源")
		return
	}
	if _, _, err := asset(name); err != nil {
		if errors.Is(err, errBadAssetName) {
			writeJSONError(w, http.StatusNotFound, "没有这个静态资源")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "读取静态资源失败")
		return
	}
	s.writeAsset(w, r, name)
}

func (s *Server) writeAsset(w http.ResponseWriter, r *http.Request, name string) {
	body, ctype, err := asset(name)
	if err != nil {
		// 错误细节只写服务端日志，不回给浏览器。
		fmt.Fprintf(os.Stderr, "web: 读取静态资源 %q 失败: %v\n", name, err)
		writeJSONError(w, http.StatusInternalServerError, "读取静态资源失败")
		return
	}
	w.Header().Set("Content-Type", ctype)
	// 静态资源不打强缓存：单文件分发时用户会就地替换 exe，强缓存会让改动看不到。
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		fmt.Fprintf(os.Stderr, "web: 写静态资源 %q 失败: %v\n", name, err)
	}
}

// ------------------------------------------------------------------ *
// API
// ------------------------------------------------------------------ *

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	snap, err := s.opts.State.Snapshot(r.Context())
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: 生成快照失败: %v\n", err)
		writeJSONError(w, http.StatusInternalServerError, "读取状态失败")
		return
	}
	// csrf 永远由服务端盖章，不信任任何上游实现填的值。
	snap.Session.CSRF = s.csrf
	if snap.Session.RoomID == "" {
		snap.Session.RoomID = ""
	}
	// 空切片要序列化成 []，不能是 null：前端多处直接 .length / for 循环。
	snap.Bots = nonNilBots(snap.Bots)
	snap.Pending = nonNilPending(snap.Pending)
	snap.Capabilities = nonNilCaps(snap.Capabilities)
	snap.Notices = nonNilNotices(snap.Notices)
	snap.Messages = nonNilMessages(snap.Messages)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	dim := r.URL.Query().Get("dim")
	switch dim {
	case "":
		dim = "bot"
	case "bot", "room", "day":
	default:
		writeJSONError(w, http.StatusBadRequest, "未知的成本维度: "+dim)
		return
	}
	report, err := s.opts.Cost.Cost(r.Context(), dim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: 读取成本失败(dim=%s): %v\n", dim, err)
		writeJSONError(w, http.StatusInternalServerError, "读取成本失败")
		return
	}
	// 同样保证切片非 nil，前端会对它们做 .length。
	if report.ByCurrency == nil {
		report.ByCurrency = []Money{}
	}
	if report.Rows == nil {
		report.Rows = []CostRow{}
	}
	if report.Limits == nil {
		report.Limits = []CostLimit{}
	}
	// 有未知金额调用时必须给出原因，前端会原样显示（app.js:689）。
	if report.UnknownAmountCalls > 0 && report.UnknownReason == "" {
		report.UnknownReason = "该通道不提供计费信息"
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var req SendRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeJSONError(w, http.StatusBadRequest, "消息内容为空")
		return
	}
	msg, err := s.opts.Message.SendMessage(r.Context(), req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: 发送消息失败: %v\n", err)
		writeJSONError(w, http.StatusInternalServerError, publicError(err, "发送消息失败"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

func (s *Server) handleWorkControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	// 用 EscapedPath 而不是 Path：Path 里 %2F 已被 ServeMux 解成 /，id 会被切断。
	workID, ok := workIDFromControlPath(r.URL.EscapedPath())
	if !ok {
		writeJSONError(w, http.StatusNotFound, "没有这个接口")
		return
	}
	var req ControlRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	if !validControlAction(req.Action) {
		writeJSONError(w, http.StatusBadRequest, "未知的控制动作: "+req.Action)
		return
	}
	note, err := s.opts.Control.Control(r.Context(), workID, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: 控制动作失败(work=%s, action=%s): %v\n", workID, req.Action, err)
		writeJSONError(w, http.StatusInternalServerError, publicError(err, "控制动作失败"))
		return
	}
	// note 由前端原样显示（app.js:931）。为空时前端自己退回"已提交：<动作名>"。
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

// workIDFromControlPath 从 /api/work/{id}/control 里取 work id。
//
// 入参必须是 r.URL.EscapedPath()，不能是 r.URL.Path：ServeMux 已经把 %2F 解成了 /，
// 用解码后的路径取 id 会让含斜杠的 id 被误判成多段路径。
// 这里要求 id 恰好一段，"/api/work/control" 这种少了 id 的路径必须拒绝，
// 否则会被当成 id 叫 "control" 的合法请求。
func workIDFromControlPath(p string) (string, bool) {
	const prefix = "/api/work/"
	const suffix = "/control"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == suffix || !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(rest, suffix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

func validControlAction(a string) bool {
	switch a {
	case "pause", "resume", "stop", "replace", "approval", "dismiss_pending":
		return true
	}
	return false
}

// ------------------------------------------------------------------ *
// 中间件细节
// ------------------------------------------------------------------ *

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// assets/ 里没有内联 <style> / style= / 内联 <script>（已核对 index.html），
	// 所以这里不需要 unsafe-inline。
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// hostAllowed 防 DNS rebinding：只接受指向本机回环的 Host 头。
func hostAllowed(r *http.Request) bool {
	host := r.Host
	if host == "" {
		// HTTP/1.0 客户端可能不带 Host；回环连接本身已经说明问题，放行。
		return true
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return isLoopbackHost(h)
}

// originAllowed 校验 Origin。没有 Origin 头（同源导航、curl、EventSource 首连）
// 一律放行；有就必须是回环源。
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func isLoopbackHost(h string) bool {
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// validateLoopbackAddr 只接受回环监听地址。
//
// 端口允许 0（内核分配）；host 为空按 127.0.0.1 处理，因为 net.Listen 的空 host
// 表示"所有接口"，那正是这里必须拒绝的情况。
func validateLoopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %q 不是 host:port 形式: %v", ErrBadAddr, addr, err)
	}
	if host == "" {
		return fmt.Errorf("%w: %q 缺少主机名（空主机名等于监听所有接口）", ErrBadAddr, addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("%w: %q 的 %q 不是回环地址", ErrBadAddr, addr, host)
	}
	if port == "" {
		return fmt.Errorf("%w: %q 缺少端口", ErrBadAddr, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%w: %q 的端口非法", ErrBadAddr, addr)
	}
	return nil
}

// stripTokenQuery 返回去掉 t 参数后的 URL（保留其余查询参数）。
func stripTokenQuery(u *url.URL) string {
	if u == nil {
		return "/"
	}
	q := u.Query()
	q.Del("t")
	out := *u
	out.RawQuery = q.Encode()
	if out.Path == "" {
		out.Path = "/"
	}
	return out.RequestURI()
}

func deriveToken(credential, purpose string) string {
	sum := sha256.Sum256([]byte("mesh-web:" + purpose + ":" + credential))
	return hex.EncodeToString(sum[:])
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ------------------------------------------------------------------ *
// 编解码辅助
// ------------------------------------------------------------------ *

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web: 序列化响应失败: %v\n", err)
		writeJSONError(w, http.StatusInternalServerError, "序列化响应失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		fmt.Fprintf(os.Stderr, "web: 写响应失败: %v\n", err)
	}
}

// writeJSONError 写错误响应。键必须是 error：前端 postJSON 读 data.error（app.js:170）。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	body := http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体为空")
		}
		return errors.New("请求体不是合法 JSON")
	}
	// 允许多余的空白，但不允许多余的 JSON 值（防止拼接注入式请求体）。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("请求体包含多余内容")
	}
	return nil
}

// publicError 决定给浏览器看什么。带 ErrPublic 标记的错误原样透出，
// 其余一律用兜底文案——内部错误（SQL 语句、路径）不进响应体。
func publicError(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	var pe *PublicError
	if errors.As(err, &pe) && pe.Message != "" {
		return pe.Message
	}
	return fallback
}

// PublicError 是"可以给用户看"的错误。业务层（app）用它把
// "当前没有进行中的委托"这类提示送到界面上，而不是让用户看到 500。
type PublicError struct {
	Message string
}

func (e *PublicError) Error() string { return e.Message }

// NewPublicError 构造一个可以透出到界面的错误。
func NewPublicError(format string, args ...any) error {
	return &PublicError{Message: fmt.Sprintf(format, args...)}
}

// ------------------------------------------------------------------ *
// 切片非 nil 化：前端会直接对它们做 .length / for 循环
// ------------------------------------------------------------------ *

func nonNilBots(v []BotView) []BotView {
	if v == nil {
		return []BotView{}
	}
	return v
}

func nonNilPending(v []PendingView) []PendingView {
	if v == nil {
		return []PendingView{}
	}
	return v
}

func nonNilCaps(v []CapView) []CapView {
	if v == nil {
		return []CapView{}
	}
	return v
}

func nonNilNotices(v []NoticeView) []NoticeView {
	if v == nil {
		return []NoticeView{}
	}
	return v
}

func nonNilMessages(v []MessageView) []MessageView {
	if v == nil {
		return []MessageView{}
	}
	return v
}
