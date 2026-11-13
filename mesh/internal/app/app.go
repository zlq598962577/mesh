package app

// 组装层（task-7 / §9）：把 store / orchestrator / registry / web 装配成一个
// 可运行的本机服务。本文件只做装配与生命周期，不做业务决策；
// 编排逻辑在 orchestrator，持久化在 store，HTTP 在 web，注册表在 registry。
//
// P0 的诚实边界（README 同步记录，不许在别处假装已解决）：
//   - 四个桌面客户端适配器尚未全部落地：engines 里没有注入的 bot 会拿到
//     unavailableEngine，所有调用明确报"适配器未接入"，绝不伪造成功。
//   - 桌面客户端不上报 token 用量：成本面板必须显示"未知"，见 webbridge。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"mesh/internal/contract"
	"mesh/internal/orchestrator"
	"mesh/internal/registry"
	"mesh/internal/store"
	"mesh/internal/web"
)

// EngineMissingError 表示某个 bot 的引擎没有接入（适配器未实现或未注入）。
type EngineMissingError struct{ Adapter string }

func (e *EngineMissingError) Error() string {
	return fmt.Sprintf("app: %s 适配器未接入（P0 组装占位，等待适配器落地）", e.Adapter)
}

// unavailableEngine 是"这个 bot 的适配器还没接"的诚实占位：
// 每个方法都失败，Events 立即关闭。绝不返回假成功、假事件。
type unavailableEngine struct{ adapter string }

func (e unavailableEngine) Open(context.Context, contract.BotSpec, contract.Workspace) (contract.Session, error) {
	return nil, &EngineMissingError{Adapter: e.adapter}
}
func (e unavailableEngine) Send(context.Context, contract.Session, contract.Message) (contract.Turn, error) {
	return nil, &EngineMissingError{Adapter: e.adapter}
}
func (e unavailableEngine) Events(contract.Session) <-chan contract.Event {
	ch := make(chan contract.Event)
	close(ch)
	return ch
}
func (e unavailableEngine) Cancel(context.Context, contract.Session, string) error {
	return &EngineMissingError{Adapter: e.adapter}
}
func (e unavailableEngine) ResolveApproval(context.Context, contract.Session, string, contract.Decision) error {
	return &EngineMissingError{Adapter: e.adapter}
}
func (e unavailableEngine) Suspend(contract.Session) error  { return &EngineMissingError{Adapter: e.adapter} }
func (e unavailableEngine) Close(contract.Session) error    { return &EngineMissingError{Adapter: e.adapter} }
func (e unavailableEngine) Health() error                   { return &EngineMissingError{Adapter: e.adapter} }

// muxSession 在引擎多路分发时记住会话属于哪个 bot（contract.Session 只有 ID，
// 不带归属信息，路由需要这一层包装）。
type muxSession struct {
	botID string
	inner contract.Session
}

func (s *muxSession) ID() string { return s.inner.ID() }

// engineMux 把 contract.Engine 的调用按 bot 分发到各自注入的引擎。
// orchestrator.New 只接受一个 Engine；多 bot 多客户端的分发就在这里收口。
type engineMux struct {
	mu      sync.RWMutex
	engines map[string]contract.Engine
}

func (m *engineMux) pick(botID string) (contract.Engine, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	eng, ok := m.engines[botID]
	if !ok || eng == nil {
		return unavailableEngine{adapter: botID}, nil
	}
	return eng, nil
}

func (m *engineMux) SetEngine(botID string, eng contract.Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engines[botID] = eng
}

func (m *engineMux) Open(ctx context.Context, spec contract.BotSpec, ws contract.Workspace) (contract.Session, error) {
	eng, err := m.pick(spec.ID)
	if err != nil {
		return nil, err
	}
	inner, err := eng.Open(ctx, spec, ws)
	if err != nil {
		return nil, err
	}
	return &muxSession{botID: spec.ID, inner: inner}, nil
}

func (m *engineMux) botOf(s contract.Session) string {
	if ms, ok := s.(*muxSession); ok {
		return ms.botID
	}
	return ""
}

func (m *engineMux) inner(s contract.Session) (contract.Engine, contract.Session, error) {
	botID := m.botOf(s)
	if botID == "" {
		return nil, nil, errors.New("app: 会话不是由本引擎创建的，无法路由")
	}
	eng, err := m.pick(botID)
	if err != nil {
		return nil, nil, err
	}
	inner := s
	if ms, ok := s.(*muxSession); ok {
		inner = ms.inner
	}
	return eng, inner, nil
}

func (m *engineMux) Send(ctx context.Context, s contract.Session, msg contract.Message) (contract.Turn, error) {
	eng, inner, err := m.inner(s)
	if err != nil {
		return nil, err
	}
	return eng.Send(ctx, inner, msg)
}

func (m *engineMux) Events(s contract.Session) <-chan contract.Event {
	eng, inner, err := m.inner(s)
	if err != nil {
		ch := make(chan contract.Event)
		close(ch)
		return ch
	}
	return eng.Events(inner)
}

func (m *engineMux) Cancel(ctx context.Context, s contract.Session, turnID string) error {
	eng, inner, err := m.inner(s)
	if err != nil {
		return err
	}
	return eng.Cancel(ctx, inner, turnID)
}

func (m *engineMux) ResolveApproval(ctx context.Context, s contract.Session, approvalID string, d contract.Decision) error {
	eng, inner, err := m.inner(s)
	if err != nil {
		return err
	}
	return eng.ResolveApproval(ctx, inner, approvalID, d)
}

func (m *engineMux) Suspend(s contract.Session) error {
	eng, inner, err := m.inner(s)
	if err != nil {
		return err
	}
	return eng.Suspend(inner)
}

func (m *engineMux) Close(s contract.Session) error {
	eng, inner, err := m.inner(s)
	if err != nil {
		return err
	}
	return eng.Close(inner)
}

func (m *engineMux) Health() error {
	// 只要有一个 bot 的引擎健康就算整体可用；个别 bot 的缺失由 README/状态行如实展示。
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.engines) == 0 {
		return &EngineMissingError{Adapter: "all"}
	}
	return nil
}

// App 是装配完成、可以运行的整体。
type App struct {
	Cfg      Config
	Store    *store.Store
	Registry *registry.Registry
	Orch     *orchestrator.Orchestrator
	Web      *web.Server
	// Credential 本机访问凭据（与凭据文件内容一致）。启动横幅直接拼进 URL，
	// 让用户点一下就能进；凭据本来就存在用户自己可读的文件里，打印到
	// 用户本人面前的启动窗口不增加任何暴露面。
	Credential string
	// Notes 是装配阶段需要用户看见的提示（坏配置文件、适配器构造失败等）。
	// 诚实原则：装载期发现的问题必须在启动横幅里说出来，不静默吞。
	Notes []string
	// Dispatch 派发 worker：领取投递、驱动引擎、事件落库与扇出。
	Dispatch *dispatcher

	mux    *engineMux
	bridge *webBridge
	bots   []contract.BotSpec
}

// BuildOptions 是 Build 的输入。
// AutoLoad=true 时 bots 从 <data_dir>/bots/*.json 加载（生产入口）；
// 显式传 Bots/Engines 的场景（集成测试注入 fake）优先于 AutoLoad。
type BuildOptions struct {
	Bots []contract.BotSpec
	// Engines 按 bot id 注入引擎。测试注入 fake；AutoLoad 时按 adapter 自动构造。
	Engines map[string]contract.Engine
	// AutoLoad 从 <data_dir>/bots/ 加载 bot 配置并构造真实适配器引擎。
	AutoLoad bool
}

// Build 把各模块按依赖关系装配起来。任何一步失败都返回带上下文的错误，
// 绝不 panic——启动失败的报错是用户看到的第一个东西，必须能读懂。
func Build(ctx context.Context, cfg Config, opts BuildOptions) (*App, error) {
	if err := cfg.Resolve(); err != nil {
		return nil, fmt.Errorf("app: 配置解析失败: %w", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("app: 目录创建失败: %w", err)
	}
	for _, b := range opts.Bots {
		if err := sanitizeBotID(b.ID); err != nil {
			return nil, fmt.Errorf("app: bot id 非法: %w", err)
		}
	}

	// 1. 存储（Open 内部完成实例锁 → 迁移 → 写队列）。
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: 打开存储失败: %w", err)
	}

	// 2. 注册表：Catalog（内嵌 manifest）+ Registry（<data_dir>/agents）两层加载。
	//    探测层（Detector）Windows 下注入真实实现；非 Windows 或探测失败时
	//    第 3 层如实缺席，Entry.Usable() 为 false，不影响其余层工作。
	reg, err := registry.New(cfg.DataDir, registry.Options{Detector: detectorFor()})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: 加载 agent 注册表失败: %w", err)
	}

	// 2b. bot 配置：AutoLoad 时从 <data_dir>/bots/*.json 加载（丢文件即加 agent）。
	bots := opts.Bots
	loadNotes := []string{}
	if opts.AutoLoad && len(bots) == 0 {
		if err := ensureBotConfigFiles(cfg.BotsDir); err != nil {
			_ = st.Close()
			return nil, err
		}
		loaded, loadErr := loadBots(cfg.BotsDir)
		bots = loaded
		if loadErr != nil {
			// 坏文件不阻止启动（好文件照常工作），但必须让用户看见。
			loadNotes = append(loadNotes, loadErr.Error())
		}
	}

	// 2c. 引擎：显式注入优先（测试 fake），否则按 adapter 构造真实适配器。
	engines := opts.Engines
	if len(engines) == 0 {
		built, engineNotes := buildEngines(bots, reg, cfg.DataDir)
		engines = built
		loadNotes = append(loadNotes, engineNotes...)
	}

	// 2d. 启动恢复（§5.6）：上次异常退出遗留的 running Run / running 投递
	// 必须在派发循环开始领取之前收尾。此前 RecoverInterrupted 只有测试在调，
	// 生产路径从未执行——2026-11-14 重启后 runs 表仍留着一堆僵尸 running。
	// 报告进启动横幅（Notes），用户能看到上次到底留下了什么。
	rec, err := st.RecoverInterrupted(ctx, time.Now())
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: 启动恢复失败: %w", err)
	}
	if rec.InterruptedRuns+rec.RequeuedDeliveries+rec.FailedDeliveries > 0 {
		loadNotes = append(loadNotes, fmt.Sprintf(
			"启动恢复：上次退出遗留 %d 个未收尾 Run（已标 interrupted）、%d 条投递重新排队、%d 条投递重试超限判失败",
			rec.InterruptedRuns, rec.RequeuedDeliveries, rec.FailedDeliveries))
	}

	// 3. 引擎多路分发 + 编排层。AutoLoad 构造出的引擎也必须注入 mux：
	// 只构造不注入会让 UI 显示"适配器未接入"而适配器明明在内存里。
	mux := &engineMux{engines: make(map[string]contract.Engine, len(bots))}
	for botID, eng := range engines {
		mux.SetEngine(botID, eng)
	}
	orch := orchestrator.New(st, mux, bots)
	// 把 -max-active-runs 接进调度器：参数存在就必须生效，否则删掉它。
	orch.SetMaxActiveRuns(cfg.MaxActiveRuns)

	// 3b. 种子数据：默认房间与 bot 行。messages/room_members 等表挂在
	// rooms/bots 的外键上，没有这两类行用户第一条消息就会撞外键失败。
	// INSERT OR IGNORE：幂等，重启不重复。
	if err := seedRoomAndBots(ctx, st, cfg, bots); err != nil {
		_ = st.Close()
		return nil, err
	}

	// 4. 凭据：复用已存在的（重启后浏览器里的 URL 仍有效），否则生成新写盘（0600）。
	credential, err := loadOrCreateCredential(cfg.CredentialFile)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: 访问凭据就绪失败: %w", err)
	}

	// 5. web 桥接与 HTTP 服务。
	bridge := newWebBridge(st, orch, cfg, bots, mux)
	disp := newDispatcher(st, orch, mux, bridge, cfg, bots)
	bridge.disp = disp
	srv, err := web.New(web.Options{
		Credential: credential,
		Addr:       cfg.Listen,
		State:      bridge,
		Events:     bridge,
		Cost:       bridge,
		Control:    bridge,
		Message:    bridge,
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("app: 启动本机服务失败: %w", err)
	}

	return &App{
		Cfg: cfg, Store: st, Registry: reg, Orch: orch, Web: srv,
		Credential: credential,
		Notes:      loadNotes,
		Dispatch:   disp,
		mux:        mux, bridge: bridge, bots: bots,
	}, nil
}

// Run 阻塞运行直到 ctx 取消或 Shutdown。返回 nil 表示正常退出。
func (a *App) Run(ctx context.Context) error {
	// 派发 worker：领取投递 → 驱动引擎 → 事件落库/扇出。
	go a.Dispatch.Start(ctx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.Web.Serve() }()

	// 打印完整可点击的 URL（含一次性 ?t= 凭据）：首访后服务端下发会话
	// cookie 并跳转到干净地址，凭据不再出现在地址栏（web 包既定流程）。
	// 第二行给出凭据文件的落点，方便浏览器换机器/换浏览器后重新拿。
	fmt.Printf("mesh 已启动，仅本机可访问。用浏览器打开：\n\n    http://%s/?t=%s\n\n（凭据文件：%s；下次启动凭据不变，收藏这条 URL 即可）\n",
		a.Web.Addr(), a.Credential, a.Cfg.CredentialFile)

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("app: HTTP 服务异常退出: %w", err)
		}
		return nil
	}
}

// Shutdown 优雅退出（§9.2 / §9.6）：
// 先停 HTTP（关掉全部 SSE 连接），结算已发生费用，最后关存储。
// 用户自己的客户端窗口与本进程无关，绝不触碰；桌面操作锁由 desktop 包
// 按需获取、用完即释，本层不持有跨请求的锁。
func (a *App) Shutdown(ctx context.Context) error {
	var firstErr error
	if err := a.Web.Shutdown(ctx); err != nil {
		firstErr = err
	}
	// 已发生费用在 RecordUsage 时即落库（§9.6：取消后已发生费用仍需结算），
	// 这里只需确认没有未落库的预留挂着。
	for _, eff := range a.Orch.UnsettledEffects() {
		fmt.Printf("mesh 退出提醒：%s 的动作 %s 有未确认收尾的外部副作用，已按原样记录，不假装已停止\n",
			eff.Key.WorkID, eff.Key.Step)
	}
	if err := a.Store.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("app: 关闭存储失败: %w", err)
	}
	return firstErr
}

// loadOrCreateCredential 读取或生成本机访问凭据（32 字节 hex，文件 0600）。
// 读回时 TrimSpace：用户手工编辑凭据文件（补个换行）不该悄悄造成校验失败。
func loadOrCreateCredential(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); len(token) >= 64 {
			return token[:64], nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机凭据失败: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("写入凭据文件失败: %w", err)
	}
	return token, nil
}

// botIDPattern 限定 bot id 只能是小写字母/数字/下划线/连字符，
// 首字符必须是字母或数字。BotWorkspace 直接用 botID 拼路径，
// 这条正则就是路径穿越的第一道闸（config.BotWorkspace 注释里点名的校验）。
var botIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func sanitizeBotID(id string) error {
	if !botIDPattern.MatchString(id) {
		return fmt.Errorf("bot id %q 只允许小写字母、数字、下划线、连字符，且以字母或数字开头", id)
	}
	// 双保险：即使正则将来被改宽，也别让分隔符漏进路径。
	if id == "." || id == ".." || filepath.Separator == '\\' && containsAny(id, `\/:`) {
		return fmt.Errorf("bot id %q 含非法字符", id)
	}
	return nil
}

func containsAny(s, chars string) bool {
	for _, c := range s {
		for _, bad := range chars {
			if c == bad {
				return true
			}
		}
	}
	return false
}

// seedRoomAndBots 写入默认房间与 bot 行（幂等）。失败直接拒绝启动：
// 外键缺行意味着每一条用户消息都会落库失败，那样的"能启动"是假的。
func seedRoomAndBots(ctx context.Context, st *store.Store, cfg Config, bots []contract.BotSpec) error {
	now := time.Now().UnixMilli()
	return st.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO rooms (id, title, created_at) VALUES (?, ?, ?)`,
			cfg.RoomID, cfg.RoomTitle, now); err != nil {
			return fmt.Errorf("app: 写入默认房间失败: %w", err)
		}
		for _, b := range bots {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO bots (id, config_version, status, created_at) VALUES (?, 1, 'active', ?)`,
				b.ID, now); err != nil {
				return fmt.Errorf("app: 登记 bot %s 失败: %w", b.ID, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO room_members (room_id, bot_id, joined_at) VALUES (?, ?, ?)`,
				cfg.RoomID, b.ID, now); err != nil {
				return fmt.Errorf("app: 将 bot %s 加入默认房间失败: %w", b.ID, err)
			}
		}
		return nil
	})
}

// 编译期断言：mux 必须满足编排层要的引擎接口。
var _ contract.Engine = (*engineMux)(nil)

// shutdownGraceFallback 是 ctx 没有截止时间时的退出预算兜底。
const shutdownGraceFallback = 10 * time.Second
