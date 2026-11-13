package registry

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 本文件实现用户层 manifest 的热加载：新增/删除/修改 <data_dir>/agents/*
// 之后，不需要重启平台就能生效。
//
// # 为什么用 mtime 轮询，而不是 fsnotify
//
// 方案里提到过文件监听，本实现改用定时轮询，理由有三条，按重要性排序：
//
//  1. 依赖。fsnotify 是第三方库。本机没有网络，go.mod 里加一条依赖就
//     意味着这个包永远编译不过。整个 mesh 的约束是纯标准库，注册表
//     作为最底层的包更不能开这个口子。
//
//  2. 正确性。ReadDirectoryChangesW 在网络盘、被挂起的进程、以及某些
//     杀毒软件介入时会丢事件或干脆不投递。一旦丢事件，表现是"用户改了
//     manifest 但没生效"，且没有任何办法自查——因为它本来就不该漏。
//     轮询最坏情况只是慢一个周期，不会漏。
//
//  3. 成本可控。监听目录里是"个位数到几十"个几 KB 的 JSON 文件。
//     os.ReadDir 加 os.Stat 的量级是几十微秒。默认 2 秒一次，对一台
//     桌面机来说是噪声级别，换来的是"不会漏事件"这个确定性。
//
// 代价也要说清楚：轮询有延迟（最多一个 PollInterval），并且空闲时也
// 会周期性唤醒。前者对本场景无影响（人改完配置文件不会掐秒表），
// 后者已经通过默认 2 秒的间隔压到可忽略。
//
// # 为什么不用 inotify 式的"事件驱动增量更新"
//
// 因为增量更新要自己维护"哪个文件对应哪个 id"，而 id 是文件内容里的
// 字段，不是文件名。文件改名、内容里 id 变化、两个文件交换 id，这些
// 情况下的增量状态机比整表重建复杂一个数量级，而整表重建的开销在
// 这个规模下根本不需要优化。

// DefaultPollInterval 是默认轮询间隔。
//
// 2 秒的选择：比人手动改文件的节奏快得多，又不至于让笔记本的风扇
// 因为一个配置文件监听而转起来。
const DefaultPollInterval = 2 * time.Second

// WatchOptions 是热加载的配置。
type WatchOptions struct {
	// PollInterval 为 0 时用 DefaultPollInterval。
	PollInterval time.Duration
	// OnReload 在每次成功重新加载后调用，可为空。
	// 回调在 Watcher 自己的 goroutine 里执行，不要在里面做阻塞很久的事。
	OnReload func(*ReloadReport)
	// OnError 在重新加载失败时调用，可为空。
	// 失败不会让 Watcher 停止：下一个周期还会再试，因为用户可能只是
	// 存盘到一半，下一秒文件就完整了。
	OnError func(error)
}

// fileStamp 是一个文件的"身份指纹"。
//
// 只用 mtime 不够：某些编辑器保存时会保留 mtime（显式 utime），
// 某些文件系统 mtime 精度只有 2 秒。加上大小能挡住大部分漏判。
// 再加个 inode 无关的 modtime 纳秒值，够了——真正的兜底是 Reload
// 本身是幂等的，多触发一次没有副作用。
type fileStamp struct {
	Size    int64
	ModTime time.Time
}

// Watcher 轮询监听用户层目录的变化。
type Watcher struct {
	reg  *Registry
	dir  string
	opts WatchOptions

	mu      sync.Mutex
	stamps  map[string]fileStamp
	dirSeen time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	started  bool
}

// Watch 启动热加载。
//
// 返回的 Watcher 已开始工作；调用 Stop 或取消 ctx 结束。
// 重复调用 Watch 会创建多个 Watcher，通常不需要——注册表持有一个即可，
// 由调用方管理。
func (r *Registry) Watch(ctx context.Context, opts WatchOptions) *Watcher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	w := &Watcher{
		reg:    r,
		dir:    r.AgentsDir(),
		opts:   opts,
		stamps: make(map[string]fileStamp),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	// 以当前状态作为基线，避免启动瞬间因为"第一次看到的文件"而误报一次变化。
	w.stamps, w.dirSeen = scanStamps(w.dir)
	w.started = true

	go w.loop(ctx)
	return w
}

// Stop 停止监听并等待 goroutine 退出。可重复调用。
func (w *Watcher) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stop) })
	if w.started {
		<-w.done
	}
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)

	t := time.NewTicker(w.opts.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-t.C:
			w.tick()
		}
	}
}

// tick 做一次比对，有变化就重新加载。
func (w *Watcher) tick() {
	cur, curDir := scanStamps(w.dir)

	w.mu.Lock()
	changed := !sameStamps(w.stamps, cur) || !w.dirSeen.Equal(curDir)
	if !changed {
		w.mu.Unlock()
		return
	}
	// 先记下新指纹再加载：如果加载失败，我们也不该在每个周期重复报
	// 同一个错。用户修好文件后指纹会再变，自然重试。
	w.stamps, w.dirSeen = cur, curDir
	w.mu.Unlock()

	report, err := w.reg.Reload()
	if err != nil {
		if w.opts.OnError != nil {
			w.opts.OnError(err)
		}
		return
	}
	if w.opts.OnReload != nil {
		w.opts.OnReload(report)
	}
}

// scanStamps 读取目录下所有 manifest 文件的指纹，以及目录自身的 mtime。
//
// 目录 mtime 用于发现"新增/删除文件"——新增一个文件时它的 mtime 是
// 新的，但如果新文件的内容与某个旧文件时间戳恰好一致（复制粘贴会这样），
// 单看文件列表会漏。目录 mtime 在增删条目时必然变化，用它兜底。
func scanStamps(dir string) (map[string]fileStamp, time.Time) {
	out := make(map[string]fileStamp)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// 目录不存在（全新安装）或读不了：返回空表。空表与空表相同，
		// 不会触发重载，这是对的——没有变化就没有可加载的东西。
		return out, time.Time{}
	}

	var dirTime time.Time
	if info, err := os.Stat(dir); err == nil {
		dirTime = info.ModTime()
	}

	for _, e := range entries {
		if e.IsDir() || !hasManifestExt(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// 文件在这一瞬间被删掉了。用零值指纹占位，下一轮它会消失，
			// 从而触发一次重载——这正是我们想要的。
			out[e.Name()] = fileStamp{}
			continue
		}
		out[e.Name()] = fileStamp{Size: info.Size(), ModTime: info.ModTime()}
	}
	return out, dirTime
}

// sameStamps 比较两份指纹表。
func sameStamps(a, b map[string]fileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if va.Size != vb.Size || !va.ModTime.Equal(vb.ModTime) {
			return false
		}
	}
	return true
}

// ChangedFiles 返回相对上次快照有变化的文件名，仅供诊断与测试。
func (w *Watcher) ChangedFiles() []string {
	cur, _ := scanStamps(w.dir)

	w.mu.Lock()
	defer w.mu.Unlock()

	var out []string
	for name, st := range cur {
		if prev, ok := w.stamps[name]; !ok || prev != st {
			out = append(out, name)
		}
	}
	for name := range w.stamps {
		if _, ok := cur[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// PollOnce 立即做一次比对，不等待下一个周期。
//
// 给"用户点了刷新按钮"这类场景用：比等 2 秒更符合直觉。
func (w *Watcher) PollOnce() {
	if w == nil {
		return
	}
	w.tick()
}

// ManifestPaths 返回当前用户层目录下所有 manifest 的绝对路径，按名排序。
// 供诊断页面展示"注册表实际读了哪些文件"。
func (r *Registry) ManifestPaths() []string {
	dir := r.AgentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !hasManifestExt(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}
