package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"path/filepath"
	"sync"
	"time"
)

// Store 是存储层句柄。所有写操作走串行写入队列，读操作直接用 DB()。
type Store struct {
	db   *sql.DB
	dir  string
	q    *writeQueue
	lock *instanceLock
}

// ErrAlreadyRunning 表示另一个 mesh 进程已经占用了同一个 data_dir（§9.1 单实例锁）。
var ErrAlreadyRunning = errors.New("store: 该 data_dir 已被另一个 mesh 进程占用")

// DBPath 返回数据库文件路径。
func DBPath(dataDir string) string { return filepath.Join(dataDir, "mesh.db") }

// diagnose 在打开存储失败时自动做一次分层探测，把"哪一层被拦"写进错误。
//
// SQLite 的 CANTOPEN(14) 无法区分权限被拒、杀软拦截、路径被占用——这些原因
// 的处置方式完全不同。探测顺序与 acquireInstanceLock 的真实写入动作一致：
//   1. 目录是否存在（acquireInstanceLock 前刚 MkdirAll 过，存在才合理）；
//   2. 直接写一个探测文件（Go 层 CreateFile，与 SQLite 同一条 OS 通道）；
//   3. 探测文件能写而 SQLite 打不开 → 问题在驱动/DSN 层，不是环境。
//
// 只在失败路径上运行，成功路径零开销。
func diagnose(dataDir string, cause error) error {
	var b strings.Builder
	b.WriteString("store 自检：")
	if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
		b.WriteString(fmt.Sprintf("[1/3] 数据目录不可见(%v) ", err))
	} else {
		b.WriteString("[1/3] 数据目录存在 ")
	}
	probe := filepath.Join(dataDir, "mesh-write-probe.tmp")
	if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
		b.WriteString(fmt.Sprintf("[2/3] 直写文件被拒(%v) → 说明是环境在拦截本程序（权限/杀软/勒索防护），不是 SQLite 的问题 ", err))
		_ = os.Remove(probe)
	} else {
		_ = os.Remove(probe)
		b.WriteString("[2/3] 直写文件成功 → 环境可写，问题在 SQLite/驱动层 ")
	}
	return fmt.Errorf("%w（%s）", cause, strings.TrimSpace(b.String()))
}

// Open 打开（必要时创建）数据目录下的 mesh.db，加单实例锁，执行迁移，启动写入队列。
//
// 顺序是有意的：先拿单实例锁再打开主库，避免两个进程同时跑迁移。
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("store: data_dir 为空")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建 data_dir 失败: %w", err)
	}

	lock, err := acquireInstanceLock(ctx, dataDir)
	if err != nil {
		return nil, diagnose(dataDir, err)
	}

	db, err := openDB(ctx, DBPath(dataDir))
	if err != nil {
		lock.release()
		return nil, diagnose(dataDir, err)
	}

	if _, err := Migrate(ctx, db, dataDir); err != nil {
		_ = db.Close()
		lock.release()
		return nil, err
	}

	s := &Store{db: db, dir: dataDir, lock: lock}
	s.q = newWriteQueue(db)
	return s, nil
}

// Close 停止写入队列、关闭数据库、释放单实例锁。可重复调用。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	if s.q != nil {
		if err := s.q.close(); err != nil {
			firstErr = err
		}
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.lock != nil {
		if err := s.lock.release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DB 暴露底层连接供只读查询使用（上层仓储代码用）。
func (s *Store) DB() *sql.DB { return s.db }

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// Write 在串行写入队列里执行一个写事务。
//
// 为什么串行（§9.1 P0 决策）：SQLite 同一时刻只有一个写者，靠 busy 重试抢锁
// 会带来不确定的等待与 SQLITE_BUSY 错误；单进程单写队列把冲突消解在应用层。
// 事务内不得做网络调用或长耗时操作——事务持有期间整个写入队列都被阻塞。
func (s *Store) Write(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	return s.q.submit(ctx, fn)
}

// Backup 生成一致性备份快照，返回文件路径。
func (s *Store) Backup(ctx context.Context, tag string) (string, error) {
	return Backup(ctx, s.db, s.dir, tag)
}

// ---------- 连接与 PRAGMA ----------

// openDB 打开主库并设置 P0 必需参数：WAL、busy timeout、外键约束。
//
// 连接数固定为 1：
//   - 写入本来就串行；
//   - PRAGMA（busy_timeout / foreign_keys）是连接级的，单连接保证不会漏设；
//   - 单用户自用场景下读阻塞写在可接受范围内（README 记录了该取舍）。
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open(driverName, buildDSN(path))
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接数据库失败: %w", err)
	}
	// journal_mode 是持久化属性（写在库文件头里），设一次即可；失败说明文件不可写。
	if _, err := db.ExecContext(pingCtx, `PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 开启 WAL 失败: %w", err)
	}
	// synchronous=NORMAL 在 WAL 下足够安全，且避免每次提交都 fsync。
	if _, err := db.ExecContext(pingCtx, `PRAGMA synchronous = NORMAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 设置 synchronous 失败: %w", err)
	}
	return db, nil
}

// ---------- 单实例锁 ----------

// instanceLock 用独立的锁库文件 + 长期持有的 BEGIN IMMEDIATE 实现单实例互斥。
//
// 为什么不用锁文件：进程崩溃后锁文件会残留，需要 pid 存活检测，而跨平台检测
// 需要平台相关 syscall（本仓库禁止引入额外依赖）。SQLite 的写锁由操作系统在进程
// 退出时自动释放，天然没有残留问题，也不需要判断 pid。
type instanceLock struct {
	db   *sql.DB
	conn *sql.Conn
	path string
	once sync.Once
}

func acquireInstanceLock(ctx context.Context, dataDir string) (*instanceLock, error) {
	path := filepath.Join(dataDir, "mesh.lock.db")
	db, err := sql.Open(driverName, buildDSN(path))
	if err != nil {
		return nil, fmt.Errorf("store: 打开锁文件失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 获取锁连接失败: %w", err)
	}
	// BEGIN IMMEDIATE 立即申请写锁并一直持有到进程结束。
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("%w (%s): %v", ErrAlreadyRunning, path, err)
	}
	return &instanceLock{db: db, conn: conn, path: path}, nil
}

func (l *instanceLock) release() error {
	if l == nil {
		return nil
	}
	var firstErr error
	l.once.Do(func() {
		if l.conn != nil {
			if _, err := l.conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
				firstErr = err
			}
			if err := l.conn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if l.db != nil {
			if err := l.db.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// ---------- 串行写入队列 ----------

type writeReq struct {
	ctx  context.Context
	fn   func(context.Context, *sql.Tx) error
	done chan error
}

type writeQueue struct {
	db     *sql.DB
	ch     chan *writeReq
	quit   chan struct{}
	closed chan struct{}
	once   sync.Once
	mu     sync.RWMutex
	dead   bool
}

func newWriteQueue(db *sql.DB) *writeQueue {
	q := &writeQueue{
		db:     db,
		ch:     make(chan *writeReq, 256),
		quit:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *writeQueue) run() {
	defer close(q.closed)
	for {
		select {
		case req := <-q.ch:
			q.exec(req)
		case <-q.quit:
			// 关闭前把已入队的请求执行完，避免调用方永远等不到结果。
			for {
				select {
				case req := <-q.ch:
					q.exec(req)
				default:
					return
				}
			}
		}
	}
}

func (q *writeQueue) exec(req *writeReq) {
	// 调用方已经放弃（ctx 取消）就不要再写库。
	if err := req.ctx.Err(); err != nil {
		req.done <- err
		return
	}
	req.done <- q.runTx(req.ctx, req.fn)
}

func (q *writeQueue) runTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启写事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交写事务失败: %w", err)
	}
	return nil
}

func (q *writeQueue) submit(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	q.mu.RLock()
	dead := q.dead
	q.mu.RUnlock()
	if dead {
		return errors.New("store: 写入队列已关闭")
	}

	req := &writeReq{ctx: ctx, fn: fn, done: make(chan error, 1)}
	select {
	case q.ch <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return errors.New("store: 写入队列已关闭")
	}

	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *writeQueue) close() error {
	q.once.Do(func() {
		q.mu.Lock()
		q.dead = true
		q.mu.Unlock()
		close(q.quit)
	})
	<-q.closed
	return nil
}
