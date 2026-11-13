package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 本文件实现四层注册表的装配与查询（方案 §8.2）。
//
//	第 1 层 Catalog   —— embed.FS 内置 manifest，只读，随二进制走
//	第 2 层 Registry  —— <data_dir>/agents/*，用户可增删改，热加载
//	第 3 层 Detection —— 运行时探测结果的缓存，有 TTL
//	第 4 层 Instance  —— 实际连接实例（会话 ID / 客户端 PID）
//
// 四层是"逐层补充"的关系，不是"逐层覆盖"：
//   - Catalog 与 Registry 共同决定"有哪些 agent、怎么探测、怎么发"。
//   - Detection 只回答"这台机器上它现在在不在、在哪"。
//   - Instance 只回答"我们当前连着哪个会话"。
//
// 为什么不把四层合成一个大结构：探测会失败、实例会断开，而 manifest
// 是静态的。混在一起会让"manifest 没错但探测失败"看起来像配置错误，
// 排查时会往错的方向找。
//
// # 重复 id 的处理
//
// 分两种情况，规则不同：
//
//   - 同一层内重复 id：硬拒绝。两个文件声明同一个 id 说明有人写错了，
//     无论保留哪个都是在猜。两个都标 invalid，报错里互相指出对方路径。
//   - 跨层重复 id（用户层与内置层同 id）：用户层覆盖内置层。这是分层
//     的本意——允许用户修正内置定义而不必等发版。被覆盖的内置定义仍
//     保留在 Entry.Overrides 里，UI 可以显示"已由用户覆盖"。
//
// # 坏 manifest 不影响其他
//
// 任何一个 manifest 解析失败，只把那一项标 invalid，其余照常加载。
// 理由：一个用户手写的文件写错，不应该让整个平台的 agent 列表消失。

// EntryState 是一个条目在四层里的状态。
type EntryState string

const (
	// EntryValid 定义有效，可用于派发。
	EntryValid EntryState = "valid"
	// EntryInvalid manifest 解析或校验失败，不可用，但会出现在列表里，
	// 让用户看得见"我这里写错了"，而不是悄悄消失。
	EntryInvalid EntryState = "invalid"
	// EntryOrphaned 定义已被删除，但仍有 Run 在引用它。
	// 这类 Run 必须被如实标记为失败/中断，不许当作完成。
	EntryOrphaned EntryState = "orphaned"
)

// InvalidManifest 一条无法加载的 manifest 记录。
type InvalidManifest struct {
	Path   string
	Origin Origin
	Err    error
}

func (v *InvalidManifest) Error() string {
	if v == nil {
		return "<nil>"
	}
	if v.Err == nil {
		return v.Path + ": 未知错误"
	}
	return v.Err.Error()
}

func sortInvalid(list []*InvalidManifest) {
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
}

// Detection 是第 3 层的探测结果缓存。
//
// 注意 Available 与 Err 的区别，这是本层最容易被误读的地方：
//   - Available=false 且 Err==""：探测成功，结论是"本机没有/不可用"。
//   - Available=false 且 Err!=""：探测本身没做成（权限、超时、系统调用失败）。
//     这不等于"没装"，UI 必须显示"未确认"，不能显示"未安装"。
type Detection struct {
	Adapter       string    `json:"adapter"`
	Available     bool      `json:"available"`
	ExePath       string    `json:"exe_path,omitempty"`
	Version       string    `json:"version,omitempty"`
	VersionSource string    `json:"version_source,omitempty"`
	PIDs          []uint32  `json:"pids,omitempty"`
	WindowTitles  []string  `json:"window_titles,omitempty"`
	ProbedAt      time.Time `json:"probed_at"`
	Err           string    `json:"err,omitempty"`
	Notes         []string  `json:"notes,omitempty"`
}

// Running 判断客户端进程当前是否在运行。
func (d *Detection) Running() bool {
	return d != nil && len(d.PIDs) > 0
}

// Uncertain 判断这次探测是否"没做成"（而不是"做成了，结论是没有"）。
func (d *Detection) Uncertain() bool {
	return d != nil && d.Err != ""
}

// Instance 是第 4 层：我们实际持有的连接实例。
//
// Recovered 的语义由 contract.SessionInfo 定死：取不到原生会话 ID 时
// 必须为 false，不许假装恢复成功。这里不重复判断，只是如实透传。
type Instance struct {
	Adapter   string    `json:"adapter"`
	SessionID string    `json:"session_id"`
	ClientPID int       `json:"client_pid,omitempty"`
	NativeID  string    `json:"native_id,omitempty"`
	Recovered bool      `json:"recovered"`
	StartedAt time.Time `json:"started_at"`
	// Detached 表示实例已释放，仅作诊断保留，不再可用。
	Detached bool `json:"detached,omitempty"`
}

// Entry 四层合并之后的一个 agent 视图。
type Entry struct {
	Manifest *Manifest
	// Source 说明当前生效的 manifest 来自哪一层。
	Source Origin
	// Overrides 非 nil 表示用户层覆盖了一个内置定义。
	Overrides *Manifest
	// State 见 EntryState。
	State EntryState
	// Invalid 非 nil 时说明为什么不可用（State=EntryInvalid）。
	Invalid *InvalidManifest
	// Detection 是第 3 层缓存，可能为 nil（还没探测过）。
	Detection *Detection
	// Instance 是第 4 层，可能为 nil（没连着）。
	Instance *Instance
	// OrphanRuns 在 State=EntryOrphaned 时列出仍引用它的 Run。
	OrphanRuns []string
}

// ID 返回条目 id（invalid 条目也尽量返回）。
func (e *Entry) ID() string {
	if e == nil || e.Manifest == nil {
		return ""
	}
	return e.Manifest.ID
}

// Usable 判断该条目当前能否用于派发。
//
// 三条都要满足：定义有效、探测确认可用、且不是孤儿。
// 注意这里不看 Instance——没连着不算不可用，只是还没连。
func (e *Entry) Usable() bool {
	if e == nil || e.State != EntryValid || e.Manifest == nil {
		return false
	}
	return e.Detection != nil && e.Detection.Available
}

// Detector 是第 3 层的探测器接口。
//
// 做成接口而不是直接调 probe 包，有两个实在的理由：
//   - registry 包本身要能跨平台编译与测试，而 probe 是 //go:build windows。
//   - 测试时可以用假探测器，不必真的去枚举进程。
//
// 实现见 detect_windows.go，它复用 probe 包的进程/窗口/版本原语，
// 不另造一套 Win32 调用。
type Detector interface {
	// Detect 探测单个 manifest。必须返回非 nil，失败时把原因写进 Err。
	Detect(ctx context.Context, m *Manifest) *Detection
}

// DetectorFunc 让普通函数满足 Detector。
type DetectorFunc func(ctx context.Context, m *Manifest) *Detection

func (f DetectorFunc) Detect(ctx context.Context, m *Manifest) *Detection {
	return f(ctx, m)
}

// Options 是 Registry 的可选配置。
type Options struct {
	// Detector 为空时第 3 层不可用，Detect 会返回"未配置探测器"。
	Detector Detector
	// DetectionTTL 探测缓存的有效期。0 表示用 DefaultDetectionTTL。
	// 负数表示永不缓存（每次都重新探测）。
	DetectionTTL time.Duration
	// Now 便于测试注入时钟，为空时用 time.Now。
	Now func() time.Time
}

// DefaultDetectionTTL 是探测缓存的默认有效期。
//
// 为什么需要缓存：枚举进程 + 读版本资源是几十毫秒级的操作，UI 上
// 刷新 agent 列表时不该每次都全量探测。为什么又必须过期：用户可能
// 刚刚启动了客户端，缓存太久会让"明明开着却显示未运行"。
const DefaultDetectionTTL = 30 * time.Second

// Registry 是四层注册表的持有者。
//
// 并发安全：所有导出的读写方法都可并发调用。内部用 RWMutex 保护，
// 探测在锁外执行（探测可能慢，不能在锁内做 IO）。
type Registry struct {
	dataDir string
	opts    Options

	mu       sync.RWMutex
	catalog  map[string]*Manifest
	catalogV []*InvalidManifest
	user     map[string]*Manifest
	userV    []*InvalidManifest
	entries  map[string]*Entry
	orphans  map[string]*Orphan

	// detectMu 串行化探测，避免 UI 刷新与后台巡检同时打满进程枚举。
	detectMu sync.Mutex
	// inflight 记录正在探测的 adapter，用于合并并发请求。
	inflight map[string]*detectCall

	loadedAt time.Time
}

// detectCall 是一次进行中的探测，多个调用者共享同一个结果。
type detectCall struct {
	done chan struct{}
	res  *Detection
}

// New 创建一个注册表。dataDir 下的 agents/ 目录会被扫描（不存在则跳过）。
func New(dataDir string, opts Options) (*Registry, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("registry: dataDir 不能为空")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DetectionTTL == 0 {
		opts.DetectionTTL = DefaultDetectionTTL
	}
	r := &Registry{
		dataDir:  dataDir,
		opts:     opts,
		entries:  make(map[string]*Entry),
		orphans:  make(map[string]*Orphan),
		inflight: make(map[string]*detectCall),
	}
	if _, err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// AgentsDir 返回用户层目录。
func (r *Registry) AgentsDir() string { return filepath.Join(r.dataDir, "agents") }

// ReloadReport 是一次重新加载的结果。
type ReloadReport struct {
	At       time.Time
	Loaded   int
	Invalid  int
	Orphans  int
	Override int
	Duration time.Duration
}

// Reload 重新扫描两层 manifest 并重建条目表。
//
// 这个方法是热加载的入口：watch.go 检测到变化后调它。它不做增量更新，
// 而是整表重建——manifest 数量是"个位数到几十"，重建成本远低于维护
// 增量合并的正确性成本，而且不会留下"删掉的文件还在表里"这类残留。
//
// 已存在的 Detection 缓存与 Instance 会按 id 保留，避免每次重载都
// 让 UI 闪一下"未探测"。
func (r *Registry) Reload() (*ReloadReport, error) {
	start := time.Now()

	catalog, catalogInvalid, err := LoadCatalog()
	if err != nil {
		return nil, err
	}
	user, userInvalid, err := loadUserDir(r.AgentsDir())
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.entries
	r.catalog, r.catalogV = catalog, catalogInvalid
	r.user, r.userV = user, userInvalid
	// 孤儿表整表重建，和 entries 一起换掉。不重建会留下"定义又加回来了，
	// 孤儿记录却还在"的残留，UI 上会同时显示可用和孤儿两个状态。
	r.orphans = make(map[string]*Orphan)

	entries := make(map[string]*Entry, len(catalog)+len(user))
	report := &ReloadReport{At: r.opts.Now()}

	// 第 1 层：内置。
	for id, m := range catalog {
		entries[id] = &Entry{Manifest: m, Source: OriginCatalog, State: EntryValid}
	}
	// 第 2 层：用户层覆盖内置。
	for id, m := range user {
		if builtin, ok := catalog[id]; ok {
			entries[id] = &Entry{
				Manifest:  m,
				Source:    OriginUser,
				Overrides: builtin,
				State:     EntryValid,
			}
			report.Override++
			continue
		}
		entries[id] = &Entry{Manifest: m, Source: OriginUser, State: EntryValid}
	}
	// 无效项也进列表，让用户看得见自己的错。
	for _, inv := range append(append([]*InvalidManifest{}, catalogInvalid...), userInvalid...) {
		id := idFromInvalidPath(inv.Path)
		if id == "" {
			continue
		}
		// 同一个 id 已经有有效定义时不覆盖它——一个坏文件不应该
		// 把同名好定义打成不可用。这种情况在 invalid 列表里单独可见。
		if _, exists := entries[id]; exists {
			continue
		}
		entries[id] = &Entry{
			Manifest: &Manifest{ID: id, Path: inv.Path, Origin: inv.Origin},
			Source:   inv.Origin,
			State:    EntryInvalid,
			Invalid:  inv,
		}
	}

	// 保留第 3、4 层缓存。
	for id, e := range entries {
		if prev, ok := old[id]; ok {
			e.Detection = prev.Detection
			e.Instance = prev.Instance
		}
	}

	// 孤儿：定义没了，但之前有实例（说明还有 Run 在引用）。
	for id, prev := range old {
		if _, still := entries[id]; still {
			continue
		}
		if prev.Instance != nil && !prev.Instance.Detached {
			e := &Entry{
				Manifest:   prev.Manifest,
				Source:     prev.Source,
				State:      EntryOrphaned,
				Instance:   prev.Instance,
				Detection:  prev.Detection,
				OrphanRuns: []string{prev.Instance.SessionID},
			}
			entries[id] = e
			r.orphans[id] = &Orphan{
				Adapter: id,
				RunIDs:  []string{prev.Instance.SessionID},
				Reason:  "manifest 已被删除，但仍有会话引用它",
			}
		}
	}

	r.entries = entries
	r.loadedAt = r.opts.Now()

	for _, e := range entries {
		switch e.State {
		case EntryValid:
			report.Loaded++
		case EntryInvalid:
			report.Invalid++
		case EntryOrphaned:
			report.Orphans++
		}
	}
	report.Duration = time.Since(start)
	return report, nil
}

// loadUserDir 扫描 <data_dir>/agents/*。
//
// 目录不存在不是错误：全新安装时它就是空的。这一点很关键——如果当成
// 错误返回，用户第一次启动平台就会看到一个失败的初始化。
func loadUserDir(dir string) (map[string]*Manifest, []*InvalidManifest, error) {
	valid := make(map[string]*Manifest)
	var invalid []*InvalidManifest

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return valid, nil, nil
		}
		// 权限不足等：返回错误而不是静默空表，否则用户会以为
		// "我放的 manifest 没生效"却看不到任何提示。
		return nil, nil, fmt.Errorf("registry: 读取用户 agent 目录 %s 失败: %w", dir, err)
	}

	// byID 用于检测同层重复。
	byID := make(map[string]string, len(entries))

	for _, e := range entries {
		if e.IsDir() || !hasManifestExt(e.Name()) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		m, err := LoadManifestFile(full)
		if err != nil {
			invalid = append(invalid, &InvalidManifest{
				Path:   full,
				Origin: OriginUser,
				Err:    err,
			})
			continue
		}
		m.Origin = OriginUser

		if prevPath, dup := byID[m.ID]; dup {
			// 同层重复：两个都判无效，并互相指出对方。
			invalid = append(invalid, &InvalidManifest{
				Path:   full,
				Origin: OriginUser,
				Err: &ManifestError{
					Path: full,
					Msg: fmt.Sprintf("重复 id %q，与 %s 冲突（两者都不加载）",
						m.ID, prevPath),
				},
			})
			if _, ok := valid[m.ID]; ok {
				invalid = append(invalid, &InvalidManifest{
					Path:   prevPath,
					Origin: OriginUser,
					Err: &ManifestError{
						Path: prevPath,
						Msg: fmt.Sprintf("重复 id %q，与 %s 冲突（两者都不加载）",
							m.ID, full),
					},
				})
				delete(valid, m.ID)
			}
			continue
		}
		byID[m.ID] = full
		valid[m.ID] = m
	}

	sortInvalid(invalid)
	return valid, invalid, nil
}

// idFromInvalidPath 从文件路径猜 id，仅用于把无效项塞进列表展示。
// 猜不出来就返回空，宁可不显示也不编一个 id。
func idFromInvalidPath(p string) string {
	base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	base = strings.ToLower(strings.TrimSpace(base))
	if !validID(base) {
		return ""
	}
	return base
}

// List 返回全部条目，按 id 排序。
func (r *Registry) List() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Get 按 id 取条目。
func (r *Registry) Get(id string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	return e, ok
}

// Invalid 返回所有无法加载的 manifest。
func (r *Registry) Invalid() []*InvalidManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*InvalidManifest, 0, len(r.catalogV)+len(r.userV))
	out = append(out, r.catalogV...)
	out = append(out, r.userV...)
	sortInvalid(out)
	return out
}

// Orphans 返回孤儿条目。
func (r *Registry) Orphans() []*Orphan {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Orphan, 0, len(r.orphans))
	for _, o := range r.orphans {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Adapter < out[j].Adapter })
	return out
}

// Orphan 描述一个"定义被删掉但仍有会话引用"的 agent。
type Orphan struct {
	Adapter string
	RunIDs  []string
	Reason  string
}

// ---- 第 3 层：Detection ----

// Detect 探测单个 agent 并缓存结果。
//
// 缓存规则：
//   - 未过期直接返回缓存。
//   - 同一 adapter 的并发探测会被合并成一次，避免 UI 刷新打满进程枚举。
//   - 探测失败（Detector 返回 Err）不缓存，下次重试。
func (r *Registry) Detect(ctx context.Context, id string) (*Detection, error) {
	e, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("registry: 没有 id 为 %q 的 agent", id)
	}
	if e.State == EntryInvalid {
		return nil, fmt.Errorf("registry: agent %q 的 manifest 无效，无法探测: %w", id, e.Invalid)
	}
	if e.Manifest == nil {
		return nil, fmt.Errorf("registry: agent %q 没有 manifest", id)
	}

	if d := r.cachedDetection(id); d != nil {
		return d, nil
	}

	r.detectMu.Lock()
	// 双检：可能在等锁期间别人已经探测完了。
	if d := r.cachedDetection(id); d != nil {
		r.detectMu.Unlock()
		return d, nil
	}
	if call, running := r.inflight[id]; running {
		r.detectMu.Unlock()
		select {
		case <-call.done:
			return call.res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &detectCall{done: make(chan struct{})}
	r.inflight[id] = call
	r.detectMu.Unlock()

	d := r.runDetect(ctx, e.Manifest)

	r.detectMu.Lock()
	delete(r.inflight, id)
	r.detectMu.Unlock()
	call.res = d
	close(call.done)

	return d, nil
}

// runDetect 执行一次真实探测并写回缓存。
func (r *Registry) runDetect(ctx context.Context, m *Manifest) *Detection {
	if r.opts.Detector == nil {
		d := &Detection{
			Adapter:  m.ID,
			ProbedAt: r.opts.Now(),
			Err:      "未配置探测器（registry.Options.Detector 为空）",
		}
		r.storeDetection(m.ID, d, false)
		return d
	}

	d := r.opts.Detector.Detect(ctx, m)
	if d == nil {
		// 探测器违反接口约定返回 nil：当成"没做成"，绝不当成"没有"。
		d = &Detection{
			Adapter:  m.ID,
			ProbedAt: r.opts.Now(),
			Err:      "探测器返回 nil（违反 Detector 约定）",
		}
	}
	if d.Adapter == "" {
		d.Adapter = m.ID
	}
	if d.ProbedAt.IsZero() {
		d.ProbedAt = r.opts.Now()
	}

	// 探测没做成（Err 非空）不缓存，下次重试；做成了才缓存。
	r.storeDetection(m.ID, d, d.Err == "")
	return d
}

func (r *Registry) storeDetection(id string, d *Detection, cache bool) {
	// 探测没做成时不写缓存。这里刻意不改 d 的任何字段：d 是调用方
	// 马上要看的返回值，为了让缓存逻辑好用而去篡改它，会让调用方
	// 看到 ProbedAt 为零的结果，误以为"从来没探测过"。
	if !cache {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.Detection = d
	}
}

// cachedDetection 返回仍然有效的缓存结果。
func (r *Registry) cachedDetection(id string) *Detection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.Detection == nil {
		return nil
	}
	d := e.Detection
	if d.ProbedAt.IsZero() {
		// 标记为"不缓存"的结果。
		return nil
	}
	if r.opts.DetectionTTL < 0 {
		return nil
	}
	if r.opts.Now().Sub(d.ProbedAt) > r.opts.DetectionTTL {
		return nil
	}
	return d
}

// DetectAll 并发探测全部有效条目。
//
// 单个失败不影响其他；失败的项以 Err 非空的 Detection 返回。
func (r *Registry) DetectAll(ctx context.Context) []*Detection {
	entries := r.List()
	out := make([]*Detection, 0, len(entries))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, e := range entries {
		if e.State != EntryValid || e.Manifest == nil {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			d, err := r.Detect(ctx, id)
			if err != nil {
				d = &Detection{
					Adapter:  id,
					ProbedAt: r.opts.Now(),
					Err:      err.Error(),
				}
			}
			mu.Lock()
			out = append(out, d)
			mu.Unlock()
		}(e.Manifest.ID)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Adapter < out[j].Adapter })
	return out
}

// InvalidateDetection 丢弃某个 agent 的探测缓存，下次 Detect 会重新探测。
func (r *Registry) InvalidateDetection(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.Detection = nil
	}
}

// ---- 第 4 层：Instance ----

// Attach 登记一个已建立的连接实例。
//
// 这里只做记账，不做连接——连接是适配器的事。注册表不该知道怎么
// 跟客户端说话，否则每加一个客户端都要改注册表。
func (r *Registry) Attach(inst *Instance) error {
	if inst == nil {
		return errors.New("registry: Attach 收到 nil 实例")
	}
	if strings.TrimSpace(inst.Adapter) == "" {
		return errors.New("registry: Instance.Adapter 不能为空")
	}
	if strings.TrimSpace(inst.SessionID) == "" {
		return errors.New("registry: Instance.SessionID 不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[inst.Adapter]
	if !ok {
		return fmt.Errorf("registry: 没有 id 为 %q 的 agent，无法登记实例", inst.Adapter)
	}
	if e.State == EntryInvalid {
		return fmt.Errorf("registry: agent %q 的 manifest 无效，不能登记实例", inst.Adapter)
	}
	if inst.StartedAt.IsZero() {
		inst.StartedAt = r.opts.Now()
	}
	// 取不到原生会话 ID 就必须置 Recovered=false。
	// 这里做一次兜底，避免调用方忘了——contract 的约定不能只靠自觉。
	if strings.TrimSpace(inst.NativeID) == "" {
		inst.Recovered = false
	}
	e.Instance = inst
	return nil
}

// Detach 释放实例登记。不关闭用户窗口，只清平台这边的记账。
func (r *Registry) Detach(adapter, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[adapter]
	if !ok || e.Instance == nil {
		return
	}
	if sessionID != "" && e.Instance.SessionID != sessionID {
		return
	}
	e.Instance.Detached = true
	e.Instance = nil
}

// Instance 取当前实例。
func (r *Registry) Instance(adapter string) *Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[adapter]; ok {
		return e.Instance
	}
	return nil
}

// LoadedAt 返回上次加载完成的时间，供诊断使用。
func (r *Registry) LoadedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadedAt
}
