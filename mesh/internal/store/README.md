# store — 存储层（P0b-1）

SQLite 存储层：schema、版本化迁移、投递事务、事件流、用量记录。

权威来源：`多Agent协作平台技术方案_v4.md` §9.1（表结构与唯一约束）、§5.6（投递事务）、
§5.7（Work 状态机）、§5.8（角色分配版本）、§4.3（事件信封）、§9.6（用量与成本）。

---

## 1. 构建与依赖

本目录代码 **已编写、未运行**。开发机上没有 Go 工具链，也没有网络，因此没有执行过
`go build` / `go vet` / `go test`。任何"已通过测试"的说法都是假的，不要写进汇报。

首次构建需要网络，因为要拉唯一一个第三方依赖（纯 Go SQLite 驱动）：

```powershell
cd F:\AIWORKSPACE\agent_harness\mesh
go mod tidy                      # 拉取 modernc.org/sqlite，写入 go.mod / go.sum
$env:CGO_ENABLED = "0"
go build ./...
go test ./internal/store/        # 本目录测试需要工具链才能跑
```

注意：

- `mesh/go.mod` 目前还没有 `modernc.org/sqlite` 依赖行，`go mod tidy` 会补上。
  **该文件由 Lead 统一维护**，存储层不直接改。
- 必须 `CGO_ENABLED=0`：方案要求单二进制、不依赖 gcc/MSVC（§11）。
- `modernc.org/sqlite` 是纯 Go 实现，满足无 cgo 约束；代价是性能低于 cgo 版
  `mattn/go-sqlite3`。单用户单机规模（万级行）下不可感知，接受该取舍。

---

## 2. 文件职责

| 文件 | 职责 |
| --- | --- |
| `driver.go` | **唯一** import SQLite 驱动的文件。驱动名 + DSN 构造（`busy_timeout`、`foreign_keys`）。换驱动只改这里。 |
| `schema.go` | 全部 DDL（15 张表 + 18 个索引），以 Go 常量字符串数组给出。 |
| `migrate.go` | `schema_version` 表、版本化迁移、迁移前一致性备份（`VACUUM INTO`）、版本过高拒绝启动。 |
| `store.go` | `Open` / `Close`、单实例锁、串行写入队列、WAL、连接参数。 |
| `delivery.go` | 投递状态机、消息+投递同事务落库、原子领取、重试退避、重启恢复。 |
| `events.go` | 事件追加（`(run_id, seq)` 幂等）、游标补读、按 Work 的时间线。 |
| `usage.go` | 用量预留/结算/记录、成本聚合（未知 ≠ 0）。 |
| `schema_test.go` | schema 一致性、唯一约束、外键生效、未知用量为 NULL、备份可打开。 |

---

## 3. 表结构

时间列一律 `INTEGER` = Unix 毫秒 UTC。id 列一律 `TEXT`，由上层生成。
`*_json` 列是 `contract` 包定义的 JSON，存储层不解析。

### 3.1 配置与房间

**bots** — bot 配置版本与状态（`bot.yaml` 是配置权威，DB 只存版本号）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | bot id |
| `config_version` | INTEGER NOT NULL | 配置版本，用于 Run 快照 |
| `status` | TEXT NOT NULL | `active` / `disabled` 等 |
| `created_at` | INTEGER NOT NULL | |

**rooms** — 房间

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `title` | TEXT NOT NULL | |
| `default_executor_id` | TEXT | 房间默认执行者 |
| `default_reviewer_id` | TEXT | 房间默认审阅者 |
| `created_at` | INTEGER NOT NULL | |

**room_members** — 房间成员，PK `(room_id, bot_id)`

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `room_id` | TEXT NOT NULL → rooms(id) ON DELETE CASCADE | |
| `bot_id` | TEXT NOT NULL → bots(id) ON DELETE CASCADE | |
| `joined_at` | INTEGER NOT NULL | |

### 3.2 Work

**works** — 一个 Work = 一次任务闭环（§5.7）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `room_id` | TEXT NOT NULL → rooms(id) | |
| `thread_id` | TEXT NOT NULL | 线程，用于把同一话题的消息归并 |
| `root_message_id` | TEXT | 起始消息 |
| `goal` | TEXT NOT NULL | 目标描述 |
| `executor_id` / `reviewer_id` | TEXT | 当前角色（§5.8） |
| `assignment_version` | INTEGER NOT NULL DEFAULT 1 | 角色每次变更 +1，用于让旧 Run 的结果失效 |
| `status` | TEXT NOT NULL | `queued`/`executing`/`reviewing`/`paused`/`waiting_user`/`completed`/`failed`/`cancelled` |
| `resume_phase` | TEXT | 暂停或等待用户前所处的阶段，恢复时据此回到原阶段 |
| `artifact_version` | INTEGER NOT NULL DEFAULT 0 | 当前产物版本 |
| `revision_count` | INTEGER NOT NULL DEFAULT 0 | 返工轮次，与 `contract.MaxAutoReviewRounds` 配合（§5.9） |
| `budget_json` | TEXT | 预算快照 |
| `result_ref` | TEXT | 最终结果引用 |
| `created_at` | INTEGER NOT NULL | |
| `ended_at` | INTEGER | 终态时间 |

**work_members** — Work 内的角色成员，PK `(work_id, bot_id, role)`
复合主键包含 `role`：同一个 bot 可以在同一 Work 里既是执行者又是审阅者。

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `work_id` | TEXT NOT NULL → works(id) ON DELETE CASCADE | |
| `bot_id` | TEXT NOT NULL | |
| `role` | TEXT NOT NULL | `executor` / `reviewer` / `helper` |

### 3.3 消息与投递

**messages**（§5.1）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `client_message_id` | TEXT UNIQUE | 客户端幂等键，可空 |
| `room_id` | TEXT NOT NULL → rooms(id) | |
| `thread_id` | TEXT NOT NULL | |
| `work_id` | TEXT | 归属 Work |
| `cause_id` | TEXT | 因果链（交接深度上限 §5.3） |
| `from_id` | TEXT NOT NULL | |
| `from_kind` | TEXT NOT NULL | `user` / `bot` / `system` |
| `to_id` | TEXT | 语义指向，**不代表私密**；可见性由 `room_id` 决定 |
| `kind` | TEXT NOT NULL | `chat` / `task` / `result` / `handoff` / `notice` |
| `text` | TEXT NOT NULL | |
| `meta_json` | TEXT | `mentions` / `reply_to` / `files` |
| `created_at` | INTEGER NOT NULL | |

**deliveries** — 投递状态机（§5.6）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 派生自 `message_id + ":" + bot_id` |
| `message_id` | TEXT NOT NULL → messages(id) ON DELETE CASCADE | |
| `bot_id` | TEXT NOT NULL | |
| `status` | TEXT NOT NULL | `pending`/`running`/`done`/`failed`/`cancelled` |
| `attempt` | INTEGER NOT NULL DEFAULT 0 | 已尝试次数，上限 3 |
| `run_id` | TEXT | 领取后指向产生的 Run |
| `next_attempt_at` | INTEGER | 退避后的下次可领取时间 |
| `last_error` | TEXT | 最近一次失败原因 |
| `created_at` / `updated_at` | INTEGER NOT NULL | |

### 3.4 Run 与事件

**runs** — 一次 bot 唤醒

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `work_id` | TEXT NOT NULL → works(id) | |
| `bot_id` | TEXT NOT NULL | |
| `room_id` | TEXT NOT NULL | |
| `message_id` | TEXT | 触发消息 |
| `assignment_version` | INTEGER NOT NULL | 领取时的角色版本快照 |
| `config_version` | INTEGER NOT NULL | 领取时的 bot 配置版本快照 |
| `session_ref` | TEXT | 外部会话引用（窗口标题/客户端 pid 等） |
| `status` | TEXT NOT NULL | `running`/`done`/`failed`/`interrupted` |
| `started_at` | INTEGER NOT NULL | |
| `ended_at` | INTEGER | |

**events** — 事件流（§4.3）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 默认 `run_id + ":" + seq` |
| `run_id` | TEXT NOT NULL → runs(id) ON DELETE CASCADE | |
| `seq` | INTEGER NOT NULL | 同一 Run 内单调递增，从 1 开始 |
| `type` | TEXT NOT NULL | `session`/`progress`/`output`/`approval`/`final`/`error` |
| `payload_json` | TEXT NOT NULL | 完整 `contract.Event`，补读时原样还原 |
| `ts` | INTEGER NOT NULL | |

同时存 `(run_id, seq)` 与 `payload_json`：前者给唯一约束、索引与游标用，后者保证
UI 补读到的事件与原始事件逐字段一致（不会被列裁剪掉 `Final`/`Approval` 等结构）。

### 3.5 产物、审阅、审批、操作

**artifacts**（§9.1 产物版本）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `work_id` | TEXT NOT NULL → works(id) ON DELETE CASCADE | |
| `version` | INTEGER NOT NULL | 同一 Work 内自增 |
| `author_id` | TEXT NOT NULL | |
| `path_or_text_ref` | TEXT NOT NULL | 文件路径或正文引用 |
| `content_hash` | TEXT NOT NULL | 内容指纹 |
| `created_at` | INTEGER NOT NULL | |

**reviews** — 审阅结论

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `work_id` | TEXT NOT NULL → works(id) ON DELETE CASCADE | |
| `reviewer_id` | TEXT NOT NULL | |
| `assignment_version` | INTEGER NOT NULL | 审阅针对的角色版本 |
| `artifact_version` | INTEGER NOT NULL | 审阅针对的产物版本 |
| `verdict` | TEXT NOT NULL | `approved`/`changes_requested`/`blocked` |
| `findings_json` | TEXT | |
| `ts` | INTEGER NOT NULL | |

**approvals**（§9.3 审批）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `work_id` | TEXT NOT NULL | |
| `run_id` | TEXT | |
| `operation_id` | TEXT | |
| `assignment_version` | INTEGER NOT NULL | 审批只在同版本内有效 |
| `args_hash` | TEXT NOT NULL | 审批绑定具体参数，参数变了审批失效 |
| `status` | TEXT NOT NULL | |
| `expires_at` | INTEGER | |
| `created_at` | INTEGER NOT NULL | |

**operations** — 外部副作用操作（§3 恰好一次语义）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `work_id` / `run_id` | TEXT | |
| `kind` | TEXT NOT NULL | |
| `args_hash` | TEXT NOT NULL | |
| `status` | TEXT NOT NULL | `prepared`/`executing`/`succeeded`/`failed`/`unknown` |
| `result_ref` | TEXT | |
| `ts` | INTEGER NOT NULL | |

### 3.6 用量与记忆

**usage**（§9.6 用量与成本）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 等于 `request_id` |
| `work_id` / `run_id` | TEXT | |
| `request_id` | TEXT NOT NULL UNIQUE | 幂等键，防重复计费 |
| `provider` / `model` | TEXT NOT NULL | |
| `reserved_amount` | INTEGER NOT NULL DEFAULT 0 | 调用前预留额（微单位） |
| `tokens_in` | INTEGER **可空** | NULL = 未知 |
| `tokens_out` | INTEGER **可空** | NULL = 未知 |
| `cached_tokens` | INTEGER **可空** | NULL = 未知 |
| `amount` | INTEGER **可空** | NULL = 未知；微单位 |
| `currency` | TEXT 可空 | |
| `price_version` | TEXT 可空 | 价目表版本，价格调整后可回溯 |
| `status` | TEXT NOT NULL | `reserved`/`settled`/`unknown` |
| `created_at` | INTEGER NOT NULL | |
| `settled_at` | INTEGER | |

**memory**（§9.4 记忆，P1 起用）

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | |
| `bot_id` | TEXT NOT NULL | |
| `scope` | TEXT NOT NULL | |
| `kind` | TEXT NOT NULL | |
| `text` | TEXT NOT NULL | |
| `source_ref` | TEXT | |
| `supersedes_id` | TEXT | 被本行取代的旧记忆 |
| `embedding` | BLOB | 向量；P0 留空 |
| `embedding_model` | TEXT | |
| `dimensions` | INTEGER | |
| `updated_at` | INTEGER NOT NULL | |

### 3.7 schema_version

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `version` | INTEGER NOT NULL | 已应用版本 |
| `applied_at` | INTEGER NOT NULL | |

### 3.8 索引

`ix_messages_room_timeline`、`ix_messages_thread`、`ix_messages_work`、
`ix_deliveries_pending(status, next_attempt_at)`、`ix_deliveries_bot`、
`ix_works_room_status`、`ix_works_thread`、`ix_runs_work`、`ix_runs_status`、
`ix_runs_bot(bot_id, started_at)`、`ix_events_ts`、`ix_reviews_work`、
`ix_approvals_status`、`ix_operations_run`、`ix_usage_run`、`ix_usage_work`、
`ix_usage_created`、`ix_memory_bot_scope`。

`ix_deliveries_pending` 的列顺序是 `(status, next_attempt_at)`：领取查询按
`status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)` 过滤，
这个顺序能让扫描直接命中。

---

## 4. 唯一约束 → 方案条款对照

一条都不能少。缺任何一条都会让对应 bug 静默发生（不报错，只是数据慢慢错）。

| # | 唯一约束 | 方案条款 | 缺失后果 |
| --- | --- | --- | --- |
| 1 | `messages.client_message_id` | §5.6 幂等（同一条用户输入重投不得重复建 Work） | 客户端重发 → 重复建 Work、重复执行、重复计费 |
| 2 | `deliveries(message_id, bot_id)` | §9.1 表结构 / §5.6 投递 | 同一条消息对同一个 bot 产生两条投递 → bot 被唤醒两次 |
| 3 | `events(run_id, seq)` | §4.3 事件信封（seq 单调、UI 按游标补读） | 同一 seq 两条事件 → 补读错乱、事件重复展示或丢失 |
| 4 | `artifacts(work_id, version)` | §9.1 产物版本 | 同一版本号两个产物 → 审阅针对的产物不确定 |
| 5 | `usage.request_id` | §9.6 用量与成本（幂等键） | 重试/重放 → 同一请求被计费两次，成本虚高 |

另外两个复合主键也承担唯一性职责：

| # | 主键 | 方案条款 | 缺失后果 |
| --- | --- | --- | --- |
| 6 | `room_members(room_id, bot_id)` | §9.1 | 同一 bot 重复入房 |
| 7 | `work_members(work_id, bot_id, role)` | §5.8 角色分配 | 同一 Work 内同角色重复成员 |

`schema_test.go` 的 `TestUniqueConstraints` 逐条以"插重复行必须失败"的方式验证上述 7 项。

---

## 5. 关键设计取舍

### 5.1 串行写入队列（P0 决策）

SQLite 同一时刻只允许一个写者。靠 `busy_timeout` 抢锁会带来不确定等待和
`SQLITE_BUSY` 错误，因此所有写操作进一个 Go channel，由单一 goroutine 串行执行。

**约束**：事务内不得做网络调用或长耗时操作——事务持有期间整个写入队列都被阻塞。
读操作不走队列，直接用 `Store.DB()`。

### 5.2 连接数固定为 1

`SetMaxOpenConns(1)` 的理由：

- 写入本来就串行；
- `busy_timeout` / `foreign_keys` 是**连接级** PRAGMA，单连接保证不会漏设；
- 单用户自用下"读阻塞写"可接受。

代价：长查询会挡住写。单用户规模下不构成问题；若未来出现慢查询，再拆读写连接池。

### 5.3 单实例锁用 SQLite 写锁，不用 pid 文件

锁文件是 `mesh.lock.db`，进程存活期间持有一个长期 `BEGIN IMMEDIATE`。

选它而不选 pid 文件的原因：pid 文件需要做 pid 存活检测，Windows 上要走平台系统调用
（本仓库禁止引入额外系统依赖）；SQLite 的写锁由操作系统在进程退出时自动释放，
进程被强杀也不会留下死锁。

失败时返回 `ErrAlreadyRunning`，错误信息带锁文件路径。

### 5.4 投递与消息同事务

`CreateMessage` 在一个事务里写 `messages` 与所有 `deliveries`，提交之后调用方才可以
唤醒 worker（§5.6）。理由：消息落库而投递没落库，消息永远不会被处理；投递落库而
消息没落库，worker 领到投递却读不到内容。

领取是原子 UPDATE：`UPDATE deliveries ... WHERE id = ? AND status = 'pending'`，
`RowsAffected != 1` 即判定被别的 worker 抢走，直接返回错误而不是静默继续。
这保证两个 worker 不可能领到同一条投递。

### 5.5 重启恢复：Run 标 interrupted，不猜结局

进程被杀时正在跑的 Run 无法得知真实结局，只能如实标成 `interrupted`，
**不得当作成功或失败**。同时：

- `attempt >= 3` 的 running 投递标 `failed`；
- 其余 running 投递回到 `pending`，`last_error` 写明"进程重启时该投递仍在运行，已重新排队"。

### 5.6 事件幂等靠 `ON CONFLICT DO NOTHING`，不靠驱动错误码

重复的 `(run_id, seq)` 被静默丢弃并返回 `inserted = false`。用 `ON CONFLICT` 而不是
解析驱动的唯一约束错误，是为了不把驱动错误码泄漏到上层。

终态的 `final` / `error` 事件被重发是常见情况（重连、重试），必须幂等。

存储层不对事件做语义判断：`output` 流式文本不得触发唤醒（§5.6）是上层调度器的职责，
存储层只负责原样落库。

### 5.7 未知 ≠ 0（§9.6 硬要求）

桌面客户端通常报不出每次 Run 的 token 与金额。这类通道必须写 **NULL**，
绝不写 0：

- 写 0 会让成本面板显示"花了 0 元"，用户会误以为金额硬顶在生效；
- `contract.Usage.Known = false` 时，`tokens_in`/`tokens_out`/`cached_tokens`/
  `amount`/`currency`/`price_version` 全部写 NULL，`status = 'unknown'`；
- `Known = true` 时才写数字，此时 0 是合法值。

调用前先 `ReserveUsage` 写预留额，调用后 `SettleUsage` 结算。这样即使进程崩溃，
也能看到"这笔钱可能已经花出去了"，而不是一笔消失的账。

聚合结果 `UsageSummary` 同时给出 `KnownAmountCalls` 与 `UnknownAmountCalls`：
UI 必须显示"另有 N 次调用金额未知"，不能把已知小计当成总额展示。

### 5.8 备份只覆盖数据库

迁移前用 `VACUUM INTO` 生成单文件快照（不能用拷文件：WAL 里还有未合并的事务，
只拷 `mesh.db` 会得到不一致的备份）。

**分工**：方案 §9.1 要求备份包含配置与产物清单。存储层只负责数据库部分；
配置目录与产物清单由应用启动流程一并归档到同一备份目录下。这条分工写在
`migrate.go` 的 `Backup` 注释里，调用方不得漏做。

### 5.9 迁移失败必须显式报错

- 每个版本在一个事务里执行，SQLite 的 DDL 是事务性的，失败整体回滚，
  数据库停在上一个版本，不会出现"迁移到一半"；
- 错误信息带版本号：`store: 迁移到 v%d 失败，数据库仍停留在 v%d: ...`；
- 数据库版本高于程序支持的版本 → 返回 `ErrSchemaTooNew` 拒绝启动，
  绝不在降级场景下继续跑（会按旧代码写新结构的数据，直接损坏）。

---

## 6. 显式假设（方案未写死、由存储层定，上层必须按此写）

1. **id**：所有 id 是 TEXT，由上层生成，存储层不生成业务 id。
2. **时间**：所有时间列是 INTEGER，Unix 毫秒 UTC。按天聚合时按 UTC 切日。
3. **JSON 列**：`*_json` 是 TEXT，内容是 `contract` 包定义的 JSON，存储层不解析
   （唯一例外：`EventsByWork` 会尝试解析 `payload_json` 取 `text` 给时间线用，
   解析失败则留空，不影响查询）。
4. **可空 token / 金额列**：NULL 表示未知，绝不写 0（见 5.7）。
5. **枚举列不加 CHECK 约束**：`status`/`kind`/`role` 由 Go 层 `contract` 常量校验。
   理由：新增枚举值不应逼着做一次 schema 迁移。代价：数据库层面拦不住写错的枚举值。
6. **Routine 相关表属 P3**，本版本不建（§9.1 末段），测试同时校验它们没有被提前创建。
7. **外键**：每个连接都通过 DSN 打开 `foreign_keys(1)`。SQLite 默认关闭外键，
   不显式打开则 `ON DELETE CASCADE` 与外键约束全部是摆设，且失效时不会有任何报错。

---

## 7. 示例：成本聚合 SQL

用量查询分两类，都在 Go 里做最终聚合：

| 方法 | 口径 | 返回 |
| --- | --- | --- |
| `UsageByWork` | 单个 Work | `UsageSummary` |
| `UsageByRun` | 单个 Run | `UsageSummary` |
| `UsageSince` | 某时间点之后 | `UsageSummary` |
| `UsageByBot` | 按 bot（经 `runs` 关联） | `[]CostBucket` |
| `UsageByRoom` | 按房间（经 `runs` 关联） | `[]CostBucket` |
| `UsageByDay` | 按 UTC 自然日 | `[]CostBucket` |

为什么不在 SQL 里 `GROUP BY`：要同时区分"金额已知"与"金额未知"、并按币种分组
（不同币种绝不能相加），还要保证"总额"与"分桶之和"口径完全一致。逐行累加逻辑
只写在 `usageAcc.add` 一处，单桶与分桶共用，避免两套口径慢慢漂移。
单机规模下全扫一遍没有性能问题，也避免了三段 `UNION` 的复杂度。

两个刻意的取舍：

- `UsageByBot` / `UsageByRoom` 用 `LEFT JOIN runs`：`usage` 表没有 `bot_id` /
  `room_id`，只能经 Run 关联。用 `LEFT` 是为了让 `run_id` 为空或 Run 已不存在的
  记录仍计入总账，归到空 `Key` 桶，而不是被 `INNER JOIN` 静默丢掉。
- `UsageByDay` 按 **UTC** 切日（`strftime(..., 'unixepoch')`）。库里时间统一是
  UTC 毫秒，若按本地时区切，"某条记录算哪一天"会随部署机器时区变化，换台机器
  对账就对不上。UI 若要展示本地日期，由前端换算，不要改这里的口径。

如果需要在 SQL 里直接看，等价查询：

如果需要在 SQL 里直接看，等价查询：

```sql
-- 按 Work 的成本汇总：已知金额按币种分组，未知金额单独计数
SELECT
    work_id,
    COALESCE(currency, '')                              AS currency,
    COUNT(*)                                            AS calls,
    SUM(CASE WHEN amount IS NOT NULL THEN 1 ELSE 0 END)  AS known_calls,
    SUM(CASE WHEN amount IS NULL     THEN 1 ELSE 0 END)  AS unknown_calls,
    COALESCE(SUM(amount), 0)                             AS amount_micros,
    COALESCE(SUM(tokens_in), 0)                          AS tokens_in,
    COALESCE(SUM(tokens_out), 0)                         AS tokens_out,
    COALESCE(SUM(cached_tokens), 0)                      AS cache_read_tokens
FROM usage
WHERE work_id = ?
GROUP BY work_id, COALESCE(currency, '')
ORDER BY currency;
```

注意 `COALESCE(SUM(amount), 0)` 里的 0 只用于**展示空分组**，与"把未知写成 0"
是两件事：`unknown_calls` 必须一起显示。UI 上的正确文案是
"已知 1.23 元，另有 12 次调用金额未知"，而不是"共 1.23 元"。

按 Run 与按时间窗的查询只是把 `WHERE work_id = ?` 换成
`WHERE run_id = ?` / `WHERE created_at >= ?`。

按 bot / 房间 / 天聚合（对应 `UsageByBot` / `UsageByRoom` / `UsageByDay`）：

```sql
-- 按 bot：usage 没有 bot_id，必须经 runs 关联；LEFT JOIN 保证关联不到的仍计入
SELECT
    COALESCE(r.bot_id, '')                              AS bot_id,
    COALESCE(u.currency, '')                            AS currency,
    COUNT(*)                                            AS calls,
    SUM(CASE WHEN u.amount IS NULL THEN 1 ELSE 0 END)   AS unknown_calls,
    COALESCE(SUM(u.amount), 0)                          AS amount_micros,
    COALESCE(SUM(u.tokens_in), 0)                       AS tokens_in,
    COALESCE(SUM(u.tokens_out), 0)                      AS tokens_out
FROM usage u
LEFT JOIN runs r ON r.id = u.run_id
WHERE u.created_at >= ?
GROUP BY COALESCE(r.bot_id, ''), COALESCE(u.currency, '')
ORDER BY bot_id, currency;

-- 按房间：把 r.bot_id 换成 r.room_id 即可
-- 按天（UTC）：不 JOIN，直接按 created_at 切日
SELECT
    strftime('%Y-%m-%d', created_at / 1000, 'unixepoch') AS day,
    COALESCE(currency, '')                               AS currency,
    COUNT(*)                                             AS calls,
    SUM(CASE WHEN amount IS NULL THEN 1 ELSE 0 END)      AS unknown_calls,
    COALESCE(SUM(amount), 0)                             AS amount_micros
FROM usage
WHERE created_at >= ?
GROUP BY day, COALESCE(currency, '')
ORDER BY day, currency;
```

预留与实际偏差的核对（预留额是否严重高估/低估）：

```sql
SELECT request_id, provider, model, reserved_amount, amount, status
FROM usage
WHERE status = 'settled' AND amount IS NOT NULL AND reserved_amount > 0
  AND (amount > reserved_amount * 2 OR amount * 2 < reserved_amount)
ORDER BY created_at DESC
LIMIT 100;
```

---

## 8. 测试

`schema_test.go` 覆盖：

| 测试 | 验证内容 |
| --- | --- |
| `TestMigrateCreatesExpectedSchema` | 15 张表全部存在；Routine 表未被提前创建 |
| `TestMigrateIsIdempotent` | 重复迁移不报错、`schema_version` 不多出行 |
| `TestSchemaTooNewRejected` | 版本过高时拒绝启动，错误信息说明原因 |
| `TestUniqueConstraints` | 7 项唯一约束逐条以"插重复必须失败"验证 |
| `TestForeignKeysEnforced` | 外键真的生效（DSN 的 `foreign_keys(1)` 没丢） |
| `TestUnknownUsageIsNull` | 未知用量写 NULL 而不是 0（§9.6） |
| `TestCostAggregationByBotRoomDay` | 按 bot / 房间 / 天聚合：未知不折成 0、关联不到的落空桶、分桶之和等于总账 |
| `TestBackupCreatesConsistentSnapshot` | 备份是可独立打开的完整库 |

**未运行**：本机无 Go 工具链（COORDINATION.md §5），以上测试已编写但未执行过。
有工具链后跑 `CGO_ENABLED=0 go test ./internal/store/`。

唯一约束的验证方式是"插重复行必须失败"，而不是去解析 `sqlite_master` /
`PRAGMA index_list`：`UNIQUE` 生成的自动索引名（`sqlite_autoindex_xxx_n`）不是稳定接口，
按名字断言等于在测实现细节。

---

## 9. 已知方案张力（按方案原文实现，不自作主张修正）

1. **§9.1 备份要求 vs 存储层职责边界**：方案要求备份含配置与产物清单，
   存储层只做数据库快照。分工见 5.8，由调用方补齐。
2. **`contract` 包无 `Artifact` / `Review` 写入结构**：契约包定义了 `ArtifactRef`、
   `Review` 但未定义"存储层写入产物"的完整入参，本版本先只建表，
   写入方法等上层仓储需求明确后再补，不预设字段。
3. **`memory` 表的向量列在 P0 无写入路径**：`embedding` / `embedding_model` /
   `dimensions` 先留空，P1 接入记忆检索时再填。
