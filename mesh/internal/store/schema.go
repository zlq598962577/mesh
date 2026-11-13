package store

// 存储层 schema（P0b-1）。
//
// 权威来源：多Agent协作平台技术方案_v4.md §9.1（第 493-528 行）。
// 本文件逐条落地方案给出的建表草图与唯一约束，不做"改进"。
//
// 显式假设（README 同步记录，方案未写死的地方由存储层定，上层必须按此写）：
//  1. 所有 id 列为 TEXT，由上层生成（存储层不生成业务 id）。
//  2. 所有时间列为 INTEGER，Unix 毫秒（UTC）；按天聚合时按 UTC 切日。
//  3. 所有 *_json 列为 TEXT，内容是 contract 包定义的 JSON，存储层不解析。
//  4. 所有 token / 金额列可空：NULL 表示"未知"，绝不用 0 冒充未知（§9.6）。
//  5. Routine 相关表属 P3，本版本不建（§9.1 末段）。
//  6. status/kind/role 等枚举列不加 CHECK 约束，由 Go 层 contract 常量校验：
//     新增枚举值不应逼着做一次迁移。
var schemaV1 = []string{
	// ---------- bots：bot 配置版本与状态 ----------
	// bot.yaml 是配置权威，DB 只存版本号与快照状态（§9.1：validate-then-replace）。
	`CREATE TABLE IF NOT EXISTS bots (
		id             TEXT    PRIMARY KEY,
		config_version INTEGER NOT NULL,
		status         TEXT    NOT NULL,
		created_at     INTEGER NOT NULL
	)`,

	// ---------- rooms ----------
	`CREATE TABLE IF NOT EXISTS rooms (
		id                  TEXT    PRIMARY KEY,
		title               TEXT    NOT NULL,
		default_executor_id TEXT,
		default_reviewer_id TEXT,
		created_at          INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS room_members (
		room_id   TEXT    NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
		bot_id    TEXT    NOT NULL REFERENCES bots(id)  ON DELETE CASCADE,
		joined_at INTEGER NOT NULL,
		PRIMARY KEY (room_id, bot_id)
	)`,

	// ---------- works：一个 Work = 一次任务闭环 ----------
	// resume_phase 保存暂停/等待用户前的阶段（§5.7）；assignment_version 随角色变更自增（§5.8）。
	`CREATE TABLE IF NOT EXISTS works (
		id                 TEXT    PRIMARY KEY,
		room_id            TEXT    NOT NULL REFERENCES rooms(id),
		thread_id          TEXT    NOT NULL,
		root_message_id    TEXT,
		goal               TEXT    NOT NULL,
		executor_id        TEXT,
		reviewer_id        TEXT,
		assignment_version INTEGER NOT NULL DEFAULT 1,
		status             TEXT    NOT NULL,
		resume_phase       TEXT,
		artifact_version   INTEGER NOT NULL DEFAULT 0,
		revision_count     INTEGER NOT NULL DEFAULT 0,
		budget_json        TEXT,
		result_ref         TEXT,
		created_at         INTEGER NOT NULL,
		ended_at           INTEGER
	)`,

	`CREATE TABLE IF NOT EXISTS work_members (
		work_id TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
		bot_id  TEXT NOT NULL,
		role    TEXT NOT NULL,
		PRIMARY KEY (work_id, bot_id, role)
	)`,

	// ---------- messages ----------
	// client_message_id 是去重键：客户端重发同一条消息不得产生第二个 Work（§5.6）。
	// SQLite 的唯一索引允许多个 NULL，因此本列可空且不影响唯一性。
	`CREATE TABLE IF NOT EXISTS messages (
		id                TEXT    PRIMARY KEY,
		client_message_id TEXT    UNIQUE,
		room_id           TEXT    NOT NULL REFERENCES rooms(id),
		thread_id         TEXT    NOT NULL,
		work_id           TEXT,
		cause_id          TEXT,
		from_id           TEXT    NOT NULL,
		from_kind         TEXT    NOT NULL,
		to_id             TEXT,
		kind              TEXT    NOT NULL,
		text              TEXT    NOT NULL,
		meta_json         TEXT,
		created_at        INTEGER NOT NULL
	)`,

	// ---------- deliveries：投递状态机 ----------
	// 唯一约束 (message_id, bot_id)：同一条消息对同一个 bot 只允许一条投递记录（§9.1）。
	`CREATE TABLE IF NOT EXISTS deliveries (
		id              TEXT    PRIMARY KEY,
		message_id      TEXT    NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		bot_id          TEXT    NOT NULL,
		status          TEXT    NOT NULL,
		attempt         INTEGER NOT NULL DEFAULT 0,
		run_id          TEXT,
		next_attempt_at INTEGER,
		last_error      TEXT,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE (message_id, bot_id)
	)`,

	// ---------- runs ----------
	// 一个 Run = 一次 bot 唤醒。重启时未完成的 Run 会被标成 interrupted（§5.6）。
	`CREATE TABLE IF NOT EXISTS runs (
		id                 TEXT    PRIMARY KEY,
		work_id            TEXT    NOT NULL REFERENCES works(id),
		bot_id             TEXT    NOT NULL,
		room_id            TEXT    NOT NULL,
		message_id         TEXT,
		assignment_version INTEGER NOT NULL,
		config_version     INTEGER NOT NULL,
		session_ref        TEXT,
		status             TEXT    NOT NULL,
		started_at         INTEGER NOT NULL,
		ended_at           INTEGER
	)`,

	// ---------- events ----------
	// 唯一约束 (run_id, seq)：seq 在单个 Run 内单调递增，重复的终态事件按幂等丢弃（§4.3）。
	// payload_json 存完整 contract.Event，读取时原样还原，避免列与结构体二次映射。
	`CREATE TABLE IF NOT EXISTS events (
		id           TEXT    PRIMARY KEY,
		run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		seq          INTEGER NOT NULL,
		type         TEXT    NOT NULL,
		payload_json TEXT    NOT NULL,
		ts           INTEGER NOT NULL,
		UNIQUE (run_id, seq)
	)`,

	// ---------- artifacts ----------
	// 唯一约束 (work_id, version)：产物版本唯一（§9.1）。
	`CREATE TABLE IF NOT EXISTS artifacts (
		id               TEXT    PRIMARY KEY,
		work_id          TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
		version          INTEGER NOT NULL,
		author_id        TEXT    NOT NULL,
		path_or_text_ref TEXT    NOT NULL,
		content_hash     TEXT    NOT NULL,
		created_at       INTEGER NOT NULL,
		UNIQUE (work_id, version)
	)`,

	`CREATE TABLE IF NOT EXISTS reviews (
		id                 TEXT    PRIMARY KEY,
		work_id            TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
		reviewer_id        TEXT    NOT NULL,
		assignment_version INTEGER NOT NULL,
		artifact_version   INTEGER NOT NULL,
		verdict            TEXT    NOT NULL,
		findings_json      TEXT,
		ts                 INTEGER NOT NULL
	)`,

	// ---------- approvals：审批绑定 Work/Run/角色版本/参数哈希/有效期（§9.3） ----------
	`CREATE TABLE IF NOT EXISTS approvals (
		id                 TEXT    PRIMARY KEY,
		work_id            TEXT    NOT NULL,
		run_id             TEXT,
		operation_id       TEXT,
		assignment_version INTEGER NOT NULL,
		args_hash          TEXT    NOT NULL,
		status             TEXT    NOT NULL,
		expires_at         INTEGER,
		created_at         INTEGER NOT NULL
	)`,

	// ---------- operations：外部副作用的执行记录（prepared/executing/succeeded/failed/unknown） ----------
	`CREATE TABLE IF NOT EXISTS operations (
		id         TEXT    PRIMARY KEY,
		work_id    TEXT,
		run_id     TEXT,
		kind       TEXT    NOT NULL,
		args_hash  TEXT    NOT NULL,
		status     TEXT    NOT NULL,
		result_ref TEXT,
		ts         INTEGER NOT NULL
	)`,

	// ---------- usage ----------
	// 唯一约束 request_id：一次模型/接口调用的幂等键，重复写入不得重复计费（§9.1）。
	// tokens_in / tokens_out / cached_tokens / amount 全部可空：
	//   桌面客户端首版不提供逐 Run token，NULL 表示未知，禁止写 0（§9.6）。
	`CREATE TABLE IF NOT EXISTS usage (
		id              TEXT    PRIMARY KEY,
		work_id         TEXT,
		run_id          TEXT,
		request_id      TEXT    NOT NULL UNIQUE,
		provider        TEXT    NOT NULL,
		model           TEXT    NOT NULL,
		reserved_amount INTEGER NOT NULL DEFAULT 0,
		tokens_in       INTEGER,
		tokens_out      INTEGER,
		cached_tokens   INTEGER,
		amount          INTEGER,
		currency        TEXT,
		price_version   TEXT,
		status          TEXT    NOT NULL,
		created_at      INTEGER NOT NULL,
		settled_at      INTEGER
	)`,

	`CREATE TABLE IF NOT EXISTS memory (
		id              TEXT    PRIMARY KEY,
		bot_id          TEXT    NOT NULL,
		scope           TEXT    NOT NULL,
		kind            TEXT    NOT NULL,
		text            TEXT    NOT NULL,
		source_ref      TEXT,
		supersedes_id   TEXT,
		embedding       BLOB,
		embedding_model TEXT,
		dimensions      INTEGER,
		updated_at      INTEGER NOT NULL
	)`,

	// ---------- 索引（§9.1：覆盖房间时间线、待投递队列、Work 查询） ----------
	`CREATE INDEX IF NOT EXISTS ix_messages_room_timeline ON messages(room_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_messages_thread        ON messages(thread_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_messages_work          ON messages(work_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_deliveries_pending     ON deliveries(status, next_attempt_at)`,
	`CREATE INDEX IF NOT EXISTS ix_deliveries_bot         ON deliveries(bot_id, status)`,
	`CREATE INDEX IF NOT EXISTS ix_works_room_status      ON works(room_id, status)`,
	`CREATE INDEX IF NOT EXISTS ix_works_thread           ON works(thread_id)`,
	`CREATE INDEX IF NOT EXISTS ix_runs_work              ON runs(work_id)`,
	`CREATE INDEX IF NOT EXISTS ix_runs_status            ON runs(status)`,
	`CREATE INDEX IF NOT EXISTS ix_runs_bot               ON runs(bot_id, started_at)`,
	`CREATE INDEX IF NOT EXISTS ix_events_ts              ON events(ts)`,
	`CREATE INDEX IF NOT EXISTS ix_reviews_work           ON reviews(work_id, ts)`,
	`CREATE INDEX IF NOT EXISTS ix_approvals_status       ON approvals(status, expires_at)`,
	`CREATE INDEX IF NOT EXISTS ix_operations_run         ON operations(run_id)`,
	`CREATE INDEX IF NOT EXISTS ix_usage_run              ON usage(run_id)`,
	`CREATE INDEX IF NOT EXISTS ix_usage_work             ON usage(work_id)`,
	`CREATE INDEX IF NOT EXISTS ix_usage_created          ON usage(created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_memory_bot_scope       ON memory(bot_id, scope, updated_at)`,
}

// expectedTables 是 P0b-1 必须存在的表清单，供 schema 一致性测试核对。
// 注意：Routine 相关表属 P3，故意不在此列，测试同时校验它们没有被提前创建。
var expectedTables = []string{
	"bots", "rooms", "room_members", "works", "work_members",
	"messages", "deliveries", "runs", "events", "artifacts",
	"reviews", "approvals", "operations", "usage", "memory",
	// V2 追加（编排层对接，见 schemaV2 注释）：
	"assignments", "input_seen",
}

// schemaV2 是 P0b-5 组装阶段的追加迁移：让 *Store 能直接满足编排层的
// orchestrator.Store 接口（13 个方法），避免在 app 层拼 SQL 漏出 schema 知识。
//
// 设计原则与 events.payload_json 一致：列用于索引与查询，JSON 原样还原结构体，
// 避免列与 contract 结构体二次映射出错。
var schemaV2 = []string{
	// works.work_json：完整 contract.Work。列字段（status/assignment_version/...）
	// 继续承担索引职责；Artifacts/Assign/Budget 等复杂字段只在 JSON 里。
	`ALTER TABLE works ADD COLUMN work_json TEXT`,

	// reviews.skipped：contract.Review.Skipped（用户明确跳过审查，不得伪造通过）。
	`ALTER TABLE reviews ADD COLUMN skipped INTEGER NOT NULL DEFAULT 0`,

	// assignments：分工变更历史（§5.8 换人时递增版本，UI 按版本排序展示）。
	`CREATE TABLE IF NOT EXISTS assignments (
		work_id         TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
		version         INTEGER NOT NULL,
		assignment_json TEXT    NOT NULL,
		created_at      INTEGER NOT NULL,
		PRIMARY KEY (work_id, version)
	)`,

	// input_seen：编排层护栏一（§5.3 (message_id, bot_id) 唯一投递去重）的幂等登记。
	// 不复用 deliveries：广播类消息没有投递行，且往 deliveries 里插非投递状态
	// 会污染 pending/running/done/failed/cancelled 状态机。
	`CREATE TABLE IF NOT EXISTS input_seen (
		message_id TEXT    NOT NULL,
		bot_id     TEXT    NOT NULL,
		seen_at    INTEGER NOT NULL,
		PRIMARY KEY (message_id, bot_id)
	)`,

	`CREATE INDEX IF NOT EXISTS ix_assignments_work ON assignments(work_id, version)`,
	`CREATE INDEX IF NOT EXISTS ix_input_seen_bot   ON input_seen(bot_id, message_id)`,
}
