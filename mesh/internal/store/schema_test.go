package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// schema 一致性测试。
//
// 注意：本机没有 Go 工具链（COORDINATION.md §5），本文件"已编写、未运行"。
// 有工具链后执行：CGO_ENABLED=0 go test ./internal/store/
//
// 测试策略说明：
//   - 唯一约束用"插重复行必须失败"来验证，而不是去解析 sqlite_master / PRAGMA
//     index_list。理由：唯一约束才是真正要保证的行为，SQLite 为 UNIQUE 生成的
//     自动索引名（sqlite_autoindex_xxx_n）不是稳定接口，按名字断言等于在测实现细节。
//   - 唯一约束测试期间关闭外键：目的是只测唯一性，不想为每条断言造一整棵
//     rooms/works/runs 外键数据。外键是否真的生效由 TestForeignKeysEnforced 单独覆盖。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mesh-test.db")
	db, err := openDB(context.Background(), path)
	if err != nil {
		t.Fatalf("openDB(%s) 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateCreatesExpectedSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	version, err := Migrate(ctx, db, "")
	if err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// V2：编排层对接（works.work_json / reviews.skipped / assignments / input_seen）。
	if version != 2 {
		t.Fatalf("迁移后版本 = %d，期望 2", version)
	}

	for _, name := range expectedTables {
		ok, err := tableExists(ctx, db, name)
		if err != nil {
			t.Fatalf("tableExists(%s) 失败: %v", name, err)
		}
		if !ok {
			t.Errorf("缺少表 %s", name)
		}
	}

	// Routine 相关表属 P3（§9.1 末段），本版本不得提前创建。
	for _, name := range []string{"routines", "routine_runs", "routine_triggers"} {
		ok, err := tableExists(ctx, db, name)
		if err != nil {
			t.Fatalf("tableExists(%s) 失败: %v", name, err)
		}
		if ok {
			t.Errorf("表 %s 属 P3，不应在 P0b-1 创建", name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("首次 Migrate 失败: %v", err)
	}
	version, err := Migrate(ctx, db, "")
	if err != nil {
		t.Fatalf("二次 Migrate 失败: %v", err)
	}
	if version != 2 {
		t.Fatalf("二次迁移后版本 = %d，期望仍为 2（V2：编排层对接）", version)
	}

	// schema_version 每个版本恰好一行（V2 后共 2 行）；重复迁移不得给同一版本造出重复行。
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("统计 schema_version 失败: %v", err)
	}
	if rows != 2 {
		t.Fatalf("schema_version 行数 = %d，期望 2（V1+V2 各一行）", rows)
	}
	var v2Rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version WHERE version = 2`).Scan(&v2Rows); err != nil {
		t.Fatalf("统计 version=2 失败: %v", err)
	}
	if v2Rows != 1 {
		t.Fatalf("version=2 出现 %d 行，期望恰好 1（重复迁移不得重复登记）", v2Rows)
	}
}

func TestSchemaTooNewRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// 伪造一个未来版本，模拟"用新版程序跑过、又退回旧版"。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`,
		99, time.Now().UnixMilli()); err != nil {
		t.Fatalf("写入未来版本失败: %v", err)
	}

	_, err := Migrate(ctx, db, "")
	if err == nil {
		t.Fatal("数据库版本高于程序支持版本时必须拒绝启动，但 Migrate 返回了 nil")
	}
	if !strings.Contains(err.Error(), "schema 版本高于") {
		t.Fatalf("错误信息未说明版本过高: %v", err)
	}
}

// TestUniqueConstraints 逐个验证方案要求的所有唯一约束（§9.1）。
// 任何一条缺失都会让重复投递、重复计费、事件乱序等 bug 静默发生。
func TestUniqueConstraints(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// 只测唯一性，不造外键数据。
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("关闭外键失败: %v", err)
	}

	now := time.Now().UnixMilli()

	t.Run("messages.client_message_id", func(t *testing.T) {
		const insert = `INSERT INTO messages
			(id, client_message_id, room_id, thread_id, from_id, from_kind, kind, text, created_at)
			VALUES (?, ?, 'r1', 't1', 'u1', 'user', 'chat', 'hi', ?)`
		mustExec(t, db, insert, "m1", "cm-1", now)
		// 不同 id、相同 client_message_id：必须被拒（§5.6 重投去重）。
		mustReject(t, db, "UNIQUE", insert, "m2", "cm-1", now)
		// client_message_id 为 NULL 时不受唯一约束限制（SQLite 允许多个 NULL）。
		mustExec(t, db, insert, "m3", nil, now)
		mustExec(t, db, insert, "m4", nil, now)
	})

	t.Run("deliveries(message_id, bot_id)", func(t *testing.T) {
		const insert = `INSERT INTO deliveries
			(id, message_id, bot_id, status, attempt, created_at, updated_at)
			VALUES (?, 'm1', ?, 'pending', 0, ?, ?)`
		mustExec(t, db, insert, "d1", "bot-a", now, now)
		// 同一消息、同一 bot：必须被拒（否则同一个 bot 会被唤醒两次）。
		mustReject(t, db, "UNIQUE", insert, "d2", "bot-a", now, now)
		// 同一消息、不同 bot：允许。
		mustExec(t, db, insert, "d3", "bot-b", now, now)
	})

	t.Run("events(run_id, seq)", func(t *testing.T) {
		const insert = `INSERT INTO events (id, run_id, seq, type, payload_json, ts)
			VALUES (?, 'run-1', ?, 'output', '{}', ?)`
		mustExec(t, db, insert, "e1", 1, now)
		// 同一 Run 内 seq 重复：必须被拒（§4.3 seq 单调、补读靠游标）。
		mustReject(t, db, "UNIQUE", insert, "e2", 1, now)
		mustExec(t, db, insert, "e3", 2, now)
	})

	t.Run("artifacts(work_id, version)", func(t *testing.T) {
		const insert = `INSERT INTO artifacts
			(id, work_id, version, author_id, path_or_text_ref, content_hash, created_at)
			VALUES (?, 'w1', ?, 'bot-a', 'ref', 'hash', ?)`
		mustExec(t, db, insert, "a1", 1, now)
		// 同一 Work 的同一版本号：必须被拒（§9.1 产物版本唯一）。
		mustReject(t, db, "UNIQUE", insert, "a2", 1, now)
		mustExec(t, db, insert, "a3", 2, now)
	})

	t.Run("usage.request_id", func(t *testing.T) {
		const insert = `INSERT INTO usage
			(id, request_id, provider, model, reserved_amount, status, created_at)
			VALUES (?, ?, 'p', 'm', 0, 'settled', ?)`
		mustExec(t, db, insert, "u1", "req-1", now)
		// 同一 request_id：必须被拒（§9.1 幂等键，防止重复计费）。
		mustReject(t, db, "UNIQUE", insert, "u2", "req-1", now)
	})

	t.Run("primary keys", func(t *testing.T) {
		// 复合主键也是唯一约束，容易被忽略，单独覆盖。
		const insert = `INSERT INTO room_members (room_id, bot_id, joined_at)
			VALUES ('r1', ?, ?)`
		mustExec(t, db, insert, "bot-a", now)
		mustReject(t, db, "UNIQUE", insert, "bot-a", now)

		const insertWM = `INSERT INTO work_members (work_id, bot_id, role)
			VALUES ('w1', 'bot-a', ?)`
		mustExec(t, db, insertWM, "executor")
		mustReject(t, db, "UNIQUE", insertWM, "executor")
		// 同一个 bot 在同一 Work 里可以有多个角色，这是复合主键包含 role 的原因。
		mustExec(t, db, insertWM, "reviewer")
	})
}

// TestForeignKeysEnforced 验证外键真的被打开。
// SQLite 默认关闭外键；如果 DSN 里的 foreign_keys(1) 丢了，ON DELETE CASCADE
// 与外键约束会全部失效，而这类失效不会有任何报错，只会在数据里慢慢烂掉。
func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	// 引用不存在的 room：必须被拒。
	_, err := db.ExecContext(ctx,
		`INSERT INTO messages
			(id, room_id, thread_id, from_id, from_kind, kind, text, created_at)
			VALUES ('m-x', 'no-such-room', 't1', 'u1', 'user', 'chat', 'hi', ?)`,
		time.Now().UnixMilli())
	if err == nil {
		t.Fatal("外键未生效：可以插入引用不存在 room 的消息")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("期望外键约束错误，实际: %v", err)
	}
}

// TestUnknownUsageIsNull 验证"未知 ≠ 0"（§9.6）。
//
// 这条是硬要求：桌面客户端报不出 token/金额时，列必须是 NULL。
// 一旦写成 0，成本面板会显示"花了 0 元"，用户会误以为金额硬顶在生效。
func TestUnknownUsageIsNull(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO usage (id, request_id, provider, model, reserved_amount, status, created_at)
		 VALUES ('u-unknown', 'req-unknown', 'desktop', 'claude', 0, 'unknown', ?)`,
		time.Now().UnixMilli()); err != nil {
		t.Fatalf("写入未知用量失败: %v", err)
	}

	var (
		tokensIn sql.NullInt64
		amount   sql.NullInt64
		currency sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT tokens_in, amount, currency FROM usage WHERE request_id = 'req-unknown'`,
	).Scan(&tokensIn, &amount, &currency); err != nil {
		t.Fatalf("读取用量失败: %v", err)
	}
	if tokensIn.Valid {
		t.Errorf("未知用量的 tokens_in 必须是 NULL，实际 = %d", tokensIn.Int64)
	}
	if amount.Valid {
		t.Errorf("未知用量的 amount 必须是 NULL（不能写 0），实际 = %d", amount.Int64)
	}
	if currency.Valid {
		t.Errorf("未知用量的 currency 必须是 NULL，实际 = %q", currency.String)
	}
}

// TestBackupCreatesConsistentSnapshot 验证迁移前备份能产出可打开的单文件快照。
func TestBackupCreatesConsistentSnapshot(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}

	dir := t.TempDir()
	path, err := Backup(ctx, db, dir, "test")
	if err != nil {
		t.Fatalf("Backup 失败: %v", err)
	}
	if !strings.HasSuffix(path, ".db") {
		t.Fatalf("备份文件后缀异常: %s", path)
	}

	// 备份必须是一个能独立打开的完整库，而不是半截文件。
	bdb, err := sql.Open(driverName, buildDSN(path))
	if err != nil {
		t.Fatalf("打开备份失败: %v", err)
	}
	defer func() { _ = bdb.Close() }()

	var n int
	if err := bdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&n); err != nil {
		t.Fatalf("查询备份内容失败: %v", err)
	}
	if n != 1 {
		t.Fatal("备份里缺少 events 表，快照不完整")
	}
}

// TestCostAggregationByBotRoomDay 验证成本面板三个维度的聚合口径（§9.1）。
//
// 关注三件事：
//  1. usage 表没有 bot_id / room_id，必须经 runs 关联才拿得到维度；
//  2. run_id 关联不到的用量不能被静默丢掉，要落在空 Key 桶里；
//  3. 未知金额的行只计 unknown 条数，绝不能把金额累加成 0 当成"没花钱"。
func TestCostAggregationByBotRoomDay(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, ""); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// 只测聚合口径，不造完整的外键数据链。
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("关闭外键失败: %v", err)
	}

	// 2026-11-13T00:00:00Z 与次日，用于验证按天切分。
	day1 := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)

	for _, r := range []struct{ id, bot, room string }{
		{"run-a", "bot-a", "room-1"},
		{"run-b", "bot-b", "room-2"},
	} {
		mustExec(t, db,
			`INSERT INTO runs (id, work_id, bot_id, room_id, assignment_version, config_version, status, started_at)
			 VALUES (?, 'w1', ?, ?, 1, 1, 'done', ?)`,
			r.id, r.bot, r.room, day1.UnixMilli())
	}

	insertUsage := `INSERT INTO usage
		(id, work_id, run_id, request_id, provider, model, reserved_amount,
		 tokens_in, tokens_out, cached_tokens, amount, currency, status, created_at)
		VALUES (?, 'w1', ?, ?, 'p', 'm', 0, ?, ?, 0, ?, ?, ?, ?)`
	// bot-a：两次已知金额（CNY），跨两天。
	mustExec(t, db, insertUsage, "u1", "run-a", "req-1", 100, 50, 2000, "CNY", "settled", day1.UnixMilli())
	mustExec(t, db, insertUsage, "u2", "run-a", "req-2", 300, 100, 3000, "CNY", "settled", day2.UnixMilli())
	// bot-b：一次未知金额。
	mustExec(t, db, insertUsage, "u3", "run-b", "req-3", nil, nil, nil, nil, "unknown", day2.UnixMilli())
	// run_id 为 NULL：无法归属到 bot / 房间，必须单独成桶。
	mustExec(t, db, insertUsage, "u4", nil, "req-4", nil, nil, nil, nil, "unknown", day1.UnixMilli())

	s := &Store{db: db}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	bots, err := s.UsageByBot(ctx, since)
	if err != nil {
		t.Fatalf("UsageByBot 失败: %v", err)
	}
	got := bucketMap(bots)
	if len(got) != 3 {
		t.Fatalf("按 bot 分组数 = %d，期望 3（bot-a / bot-b / 空桶）: %+v", len(got), bots)
	}
	if a := got["bot-a"]; a.Calls != 2 || a.UnknownAmountCalls != 0 {
		t.Errorf("bot-a 调用数 = %d 未知 = %d，期望 2 / 0", a.Calls, a.UnknownAmountCalls)
	}
	if a := got["bot-a"]; a.TokensIn != 400 || a.TokensOut != 150 {
		t.Errorf("bot-a tokens = %d/%d，期望 400/150", a.TokensIn, a.TokensOut)
	}
	if a := got["bot-a"]; len(a.ByCurrency) != 1 || a.ByCurrency[0].AmountMicros != 5000 {
		t.Errorf("bot-a 金额 = %+v，期望单币种 CNY 合计 5000", a.ByCurrency)
	}
	if b := got["bot-b"]; b.Calls != 1 || b.UnknownAmountCalls != 1 || b.KnownAmountCalls != 0 {
		t.Errorf("bot-b 期望 1 次调用且金额未知，实际 calls=%d unknown=%d known=%d",
			b.Calls, b.UnknownAmountCalls, b.KnownAmountCalls)
	}
	if b := got["bot-b"]; len(b.ByCurrency) != 0 {
		t.Errorf("金额未知的行不得产生币种分组，实际 %+v", b.ByCurrency)
	}
	if u := got[""]; u.Calls != 1 {
		t.Errorf("关联不到 Run 的用量必须落在空 Key 桶且不丢失，实际 calls=%d", u.Calls)
	}

	rooms, err := s.UsageByRoom(ctx, since)
	if err != nil {
		t.Fatalf("UsageByRoom 失败: %v", err)
	}
	roomMap := bucketMap(rooms)
	if len(roomMap) != 3 {
		t.Fatalf("按房间分组数 = %d，期望 3: %+v", len(roomMap), rooms)
	}
	if r := roomMap["room-1"]; r.Calls != 2 || r.UnknownAmountCalls != 0 {
		t.Errorf("room-1 期望 2 次已知金额调用，实际 calls=%d unknown=%d", r.Calls, r.UnknownAmountCalls)
	}

	days, err := s.UsageByDay(ctx, since)
	if err != nil {
		t.Fatalf("UsageByDay 失败: %v", err)
	}
	dayMap := bucketMap(days)
	if len(dayMap) != 2 {
		t.Fatalf("按天分组数 = %d，期望 2: %+v", len(dayMap), days)
	}
	if d := dayMap["2026-11-13"]; d.Calls != 2 {
		t.Errorf("2026-11-13 调用数 = %d，期望 2", d.Calls)
	}
	if d := dayMap["2026-11-14"]; d.Calls != 2 || d.UnknownAmountCalls != 1 {
		t.Errorf("2026-11-14 期望 2 次调用含 1 次未知，实际 calls=%d unknown=%d", d.Calls, d.UnknownAmountCalls)
	}

	// 分桶之和必须等于总账，否则面板和总额对不上。
	total, err := s.UsageSince(ctx, since)
	if err != nil {
		t.Fatalf("UsageSince 失败: %v", err)
	}
	var sumCalls int
	for _, b := range bots {
		sumCalls += b.Summary.Calls
	}
	if sumCalls != total.Calls {
		t.Fatalf("按 bot 分桶调用数之和 = %d，总账 = %d，口径不一致", sumCalls, total.Calls)
	}
	if total.KnownAmountCalls != 2 || total.UnknownAmountCalls != 2 {
		t.Fatalf("总账期望 2 已知 / 2 未知，实际 %d / %d", total.KnownAmountCalls, total.UnknownAmountCalls)
	}
}

// bucketMap 把分组结果转成 map，便于按 Key 断言。
func bucketMap(buckets []CostBucket) map[string]UsageSummary {
	out := make(map[string]UsageSummary, len(buckets))
	for _, b := range buckets {
		out[b.Key] = b.Summary
	}
	return out
}

// ---------- 测试工具 ----------

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("期望执行成功，实际失败: %v\nSQL: %s", err, firstLine(query))
	}
}

// mustReject 断言语句被约束拒绝。
// want 是要在错误信息里出现的关键字（如 "UNIQUE"）：只断言"报错了"不够，
// 否则 NOT NULL / 语法错误也会让测试假通过。
func mustReject(t *testing.T, db *sql.DB, want, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), query, args...)
	if err == nil {
		t.Fatalf("期望违反 %s 约束，但语句执行成功\nSQL: %s", want, firstLine(query))
	}
	if !strings.Contains(strings.ToUpper(err.Error()), strings.ToUpper(want)) {
		t.Fatalf("期望 %s 约束错误，实际: %v", want, err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
