package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 版本化迁移（§9.1）。
//
// 规则：
//   - schema_version 表记录已应用的最高版本，单行单列。
//   - 每个版本在一个事务里执行：SQLite 的 DDL 是事务性的，失败即整体回滚，
//     数据库停在上一个版本，不会出现"迁移到一半"。
//   - 迁移前先做一致性备份：用 VACUUM INTO 生成单文件快照。
//     不能只拷 mesh.db 主文件——WAL 里还有未合并的数据，只拷主库会得到不一致的备份。
//   - 失败时返回的错误里带版本号与原始错误，日志与 UI 直接展示，不吞异常。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER NOT NULL,
	applied_at INTEGER NOT NULL
)`

type migration struct {
	version int
	stmts   []string
}

// migrations 按版本升序排列，只能追加，不能修改已发布的条目。
var migrations = []migration{
	{version: 1, stmts: schemaV1},
	{version: 2, stmts: schemaV2},
}

// ErrSchemaTooNew 表示数据库版本高于本程序支持的版本（降级运行）。
var ErrSchemaTooNew = errors.New("store: 数据库 schema 版本高于当前程序支持的版本，拒绝启动以免损坏数据")

// SchemaVersion 返回数据库当前 schema 版本，空库返回 0。
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	exists, err := tableExists(ctx, db, "schema_version")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v int
	// 正常只有一行；取 MAX 兜底历史脏数据，避免多行导致报错。
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: 读取 schema_version 失败: %w", err)
	}
	return v, nil
}

// Migrate 把数据库升级到最新版本，返回升级后的版本号。
// 需要备份时由 dataDir 决定备份落点；dataDir 为空则不做备份（仅用于内存库测试）。
func Migrate(ctx context.Context, db *sql.DB, dataDir string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := ensureSchemaVersionTable(ctx, db); err != nil {
		return 0, err
	}
	current, err := SchemaVersion(ctx, db)
	if err != nil {
		return 0, err
	}

	latest := 0
	for _, m := range migrations {
		if m.version > latest {
			latest = m.version
		}
	}
	if current > latest {
		return current, fmt.Errorf("%w: db=%d program=%d", ErrSchemaTooNew, current, latest)
	}
	if current == latest {
		return current, nil
	}

	// 一致性备份：库非空才备份，空库没什么可备份的。
	if dataDir != "" && current > 0 {
		path, err := Backup(ctx, db, dataDir, fmt.Sprintf("pre-migrate-v%d", current))
		if err != nil {
			return current, fmt.Errorf("store: 迁移前备份失败，已中止迁移: %w", err)
		}
		_ = path // 备份路径由 Backup 内部打日志/返回，调用方可记录
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return current, fmt.Errorf("store: 迁移到 v%d 失败，数据库仍停留在 v%d: %w", m.version, current, err)
		}
		current = m.version
	}
	return current, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("语句执行失败: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("写入 schema_version 失败: %w", err)
	}
	return tx.Commit()
}

func ensureSchemaVersionTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaVersionTable); err != nil {
		return fmt.Errorf("store: 创建 schema_version 表失败: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: 查询表 %s 是否存在失败: %w", name, err)
	}
	return n > 0, nil
}

// Backup 用 VACUUM INTO 生成一致性单文件快照，返回备份文件路径。
//
// 为什么用 VACUUM INTO 而不是拷文件：WAL 模式下主库文件本身不完整，
// 直接拷 mesh.db 会漏掉 WAL 中已提交的事务，属于方案禁止的"只拷主库"。
//
// 方案还要求备份包含配置与产物清单（§9.1）。配置与产物不归存储层管，
// 存储层只负责数据库部分；调用方（应用启动流程）须把 config 目录与产物清单
// 一并归档到同一备份目录下，README 已记录该分工。
func Backup(ctx context.Context, db *sql.DB, dataDir, tag string) (string, error) {
	dir := filepath.Join(dataDir, "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("store: 创建备份目录失败: %w", err)
	}
	// 文件名只由内部生成（tag 由调用方常量传入），不含用户输入。
	name := fmt.Sprintf("mesh-%s-%s.db", tag, time.Now().UTC().Format("20060102T150405.000Z"))
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("store: 备份文件已存在: %s", path)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return "", fmt.Errorf("store: VACUUM INTO %s 失败: %w", path, err)
	}
	return path, nil
}
