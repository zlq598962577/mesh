# app — 组装层（task-7 / P0 集成验收）

把 store / orchestrator / registry / web 按依赖关系装配成一个可运行的本机服务：
`Build`（装配）→ `Run`（阻塞运行）→ 退出（LIFO 收尾）。本层只做装配、生命周期与
web 桥接，不做业务决策；编排逻辑在 orchestrator，持久化在 store，HTTP 在 web，
注册表在 registry，桌面原语在 desktop。

权威来源：`F:\AIWORKSPACE\agent_harness\多Agent协作平台技术方案_v4.md`
（§5.6 投递事务、§5.7 Work 状态机、§5.8 控制与分工、§9.2 进程生命周期、
§9.3 审批边界、§9.6 用量与成本、§10.1 验收场景）。

---

## 0. 编译与测试状态（先读这条）

- `go build ./...` 与 `go test ./...` **通过**（go1.27.4 windows/amd64，2026-11-14 实跑）；
  `go vet ./...` 在既有代码上有 4 处 "possible misuse of unsafe.Pointer" 告警
  （`internal/probe/uia.go` ×3、`internal/desktop/win32.go` ×1，Win32 句柄转换的既有写法，
  不属于本批改动，如实记录、未处理）。
- 本包 `acceptance_test.go` 共 **18 个用例，全部通过**（见第 6 节对照表）。
- **真实客户端端到端从未跑过**：本包所有验收都在可编程 fake 引擎上完成
  （实现 `contract.Engine` 全部八方法，绝不 import adapters 的真实实现）。
  依赖真实桌面会话的条目见对照表末尾的"需人工"清单。

## 1. 启动参数

由 `config.go` 的 `ParseArgs` 解析，参数表刻意很小。未知参数报错退出（exit 2）。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-data-dir` | `F:\AIWORKSPACE\mesh-data` | 数据目录，所有持久化状态的根 |
| `-listen` | `127.0.0.1:0` | 本机服务监听地址，**只接受回环**（127.0.0.0/8、::1、localhost）；端口 0 由内核分配，启动后打印真实地址 |
| `-room` | `room-main` | 房间 id（首版单房间） |
| `-room-title` | `主房间` | 房间标题 |
| `-max-active-runs` | `2` | 同时活跃 Run 上限（§5.6） |
| `-shutdown-grace` | `20s` | 优雅退出总预算 |

启动期硬红线（`Resolve` 检查，违反即拒绝启动，不做"尽力而为"的降级）：

1. `-listen` 非回环（`0.0.0.0`、局域网地址等）直接拒绝——本机服务绝不暴露到局域网。
2. 临时目录解析到系统盘（C:）**拒绝启动**——临时文件一律在 F 盘（默认 `F:\AIWORKSPACE\temp\mesh`）。
3. 临时目录与数据目录互相包含拒绝启动——备份/清理时容易互相误伤。
4. bot id 只允许 `[a-z0-9][a-z0-9_-]{0,63}`（`sanitizeBotID`），它会被直接拼进工作区路径，是路径穿越的第一道闸。

以上红线均由 `TestAccStartupArgsAndPathRedLines` 自动验证。

## 2. 目录布局

第一次启动自动创建（`EnsureDirs`，幂等），之后只校验不覆盖：

```
<data_dir>/                     默认 F:\AIWORKSPACE\mesh-data
  mesh.db                       SQLite 主库（含 -wal / -shm）
  mesh.lock.db                  单实例锁（BEGIN IMMEDIATE 长期持有，进程退出自动释放）
  credential                    本机访问凭据（32 字节 hex，0600；控制台 URL 里的 ?t= 就是它）
  agents/                       registry 的用户层 agent manifest
  bots/                         bot 配置（JSON 语法，后缀 .json 或 .yaml）
  backups/                      store.Backup 的落点
  logs/                         运行日志
  workspaces/<bot_id>/          workspace.mode=auto 时每个 bot 的私有工作区
```

平台自有的临时目录固定为 `F:\AIWORKSPACE\temp\mesh`（`FixedTempDir`，产品决定不是配置项）：
所有平台子进程的 `TEMP/TMP/TMPDIR` 都被 `ChildEnv` 改写到这里——不改的话，客户端 helper
会把中间产物写进 C 盘用户临时目录，违反红线。

**C 盘红线**：解析出的临时目录若落在系统盘（`SystemDrive` 环境变量，缺省 C:），
`Resolve` 直接返回错误拒绝启动，绝不悄悄写到系统盘。

## 3. 组装流程与凭据

`Build`（`app.go`）按依赖顺序装配，任何一步失败都返回带上下文的错误，绝不 panic：

1. `cfg.Resolve()` + `cfg.EnsureDirs()`：路径解析为绝对真实路径（Clean / Abs / 解符号链接）、建目录；
2. `store.Open`：实例锁 → 迁移 → 串行写队列；
3. `registry.New`：内嵌 catalog + `<data_dir>/agents` 两层加载（探测层非 Windows 或失败时如实缺席）；
4. `engineMux` + `orchestrator.New`：多 bot 引擎分发收口；
5. `loadOrCreateCredential`：复用已存在凭据（重启后浏览器 URL 仍有效），否则 `crypto/rand`
   生成 32 字节 hex 写入 `<data_dir>/credential`，权限 **0600**；
6. `newWebBridge` + `web.New`：五个窄接口注入 HTTP 服务。

**engineMux**：orchestrator 只接受一个 `contract.Engine`，多 bot 多客户端的分发在这里收口。
会话用 `muxSession` 包装记住归属 bot；**未注入引擎的 bot 拿到 `unavailableEngine`**——
所有方法返回 `*EngineMissingError`（"适配器未接入"），`Events` 立即关闭，绝不伪造成功。
`Health` 只要有一个 bot 的引擎注入就算整体可用，个别缺失由快照的能力受限列表如实展示。

**webBridge**（`webbridge.go`）：web 包不 import orchestrator / store，这层胶水把两者桥接成
web 要求的五个窄接口——`StateSource`（快照）、`EventStream`（rowid 游标补齐 + 实时扇出）、
`CostSource`（bot/room/day 三维成本）、`ControlSink`（界面控制动词）、`MessageSink`（发消息）。
每类实现遵守同一类诚实规则：不知道的显示"未知"，没发生的绝不伪造。界面指定的执行者/审查者
会并进消息文本（"改由 X 执行"），与聊天指令走同一条编排层解析路径，产生同一份分工记录。

`cmd/mesh/main.go` 是进程入口：解析参数 → `Build` → 交给 `Lifecycle` 管理。
**P0 以"零 bot"状态启动**（`BuildOptions{}`）：bot 配置文件加载与真实适配器注入属适配器
阶段接线，界面会如实显示能力受限，不会伪装出可用的 agent。

## 4. 退出行为

Ctrl+C / SIGTERM 由 `lifecycle.go` 接管（`WatchSignals`）：

- 收尾步骤按 **LIFO** 执行（后注册先执行，等同 defer 语义）。`main.go` 的注册顺序是
  store → web，因此实际收尾顺序为：
  1. **停 web** —— `Web.Shutdown` 先掐断全部 SSE 长连接（否则 `http.Server.Shutdown`
     会被长连接吊住），再关 HTTP；
  2. 逐条打印「未确认收尾的外部副作用」提醒（`UnsettledEffects`，按原样记录，不假装已停止）；
  3. **关库** —— `Store.Close` 停写队列、释放单实例锁。
- 宽限预算（默认 20s，`-shutdown-grace` 可调）内收尾；**第二个信号放弃宽限立即收尾**
  （收尾卡住时用户再按一次 Ctrl+C 就是唯一出路，不响应进程会吊到宽限结束）。
- 单个收尾步骤失败/超时会记入聚合错误（panic 也转成错误），但进程仍会退出——
  收尾失败只影响"退得干不干净"，不影响"该不该退"。
- 退出码：`2` = 参数解析/配置校验失败；`1` = 装配失败或收尾失败；`0` = 正常退出。
- 平台退出只释放自己持有的连接、句柄与辅助进程；**绝不触碰用户自己的客户端窗口**
  （§9.2，收尾不把用户客户端加入 kill-on-close 名单）。

`App.Shutdown` 提供同序收尾（停 web → 副作用提醒 → 关库）的可直接调用版本，供不经过
信号路径的调用方使用。

## 5. P0 已知限制（诚实边界，不许在别处假装已解决）

1. **消息历史是内存环形缓冲**（`webbridge.go` `ringCap=200`）：重启后聊天历史只显示新消息；
   `messages` 表里有落库数据，但存储层没有时间线查询方法，P0 不为此临时加宽接口。
2. **bot 状态行只有「空闲 / 思考中」两态**（`Snapshot`）：当前委托的执行者显示"思考中"，
   其余"空闲"；等授权/操作浏览器等细粒度状态依赖引擎事件流，适配器落地后接入。
3. **适配器未接入时 `unavailableEngine` 明确报错**：没注入引擎的 bot，所有引擎调用返回
   `*EngineMissingError`，事件通道立即关闭，快照的能力受限列表逐个如实列出——绝不伪造成功。
4. **成本面板是未知口径**：四个桌面客户端都不上报逐次 token 用量，金额与 token 显示为
   "未知"（`unknownAmountCalls` 单独计数，不并入已知总额），token/金额硬顶**无法由本机强制**，
   护栏只按步数与时长生效。
5. **P0 没有"聊天建委托"的派发器**：组装层还没有"从聊天消息建 Work / 派发 worker 驱动引擎"
   的循环，`SendMessage` 构造的消息也不携带 `WorkID`。验收测试按 §5.6 顺序由测试先 seed
   Work（`accSeedWork`）再投消息驱动真实链路（唤醒路由 → `ClaimDelivery` → `engineMux` 送达）。
6. **审批不代答**：界面审批动作一律引导用户去客户端窗口确认（§9.3：语义与目标无法验证时不桥接）。

## 6. P0 集成验收对照表（§10.1）

验收测试：`acceptance_test.go`（真实 store + 真实编排层 + 真实 web 中间件链 + 可编程 fake 引擎，
不 import adapters）。真实运行环境：Windows / go1.27.4 windows/amd64，2026-11-14，
`go test ./internal/app/ -v` —— **18 个用例全部通过**。

| # | 用例（§10.1 场景） | 已自动验证的内容 |
| --- | --- | --- |
| 1 | `TestAccDelegationWakesExecutorAndEngineGetsTask`（委托） | 新委托只唤醒执行者；任务消息经 `engineMux` 原文送达引擎、审查者收不到；成功判定绑定 `SuccessSignal`（fake 客户端退出码恒为 0，不可信）；`Cancel` 有调用记录 |
| 2 | `TestAccExecutorSubmitWakesOnlyReviewer`（跨对象角色） | 执行者交产物后 Work 转 reviewing，只唤醒审查者（review_ready），发送者不自激 |
| 3 | `TestAccAutoReviewCapTurnsWaitingUser`（修改循环） | 自动修改 2 轮封顶（`MaxAutoReviewRounds`），第 3 次 changes_requested 转 waiting_user 并给用户说明；3 条审查记录、3 个产物版本如实入账 |
| 4 | `TestAccStaleApprovalCannotPassNewVersion`（旧版本结果） | 旧版本 approved 被拒并点名"旧审批"、如实入账但不推进状态；最新版本 approved 放行 → completed 且 Final 绑定被批准的版本 |
| 5 | `TestAccMentionConsultDoesNotTouchAssignment`（咨询） | @B 咨询不改分工、不产生分工版本行；唤醒执行者 + 被咨询者 |
| 6 | `TestAccUIAndChatProduceSameAssignmentRecord`（界面与聊天同路） | 界面 replace 与聊天"改由 X 执行"进同一份 assignments 历史（v2、v3 递增、来源同为 user_chat）；界面下拉选择合并出的指令文本与聊天写法同形 |
| 7 | `TestAccDuplicateDeliveryIsIdempotent`（重复消息） | 同 `ClientMessageID` 重投被拒（`ErrDuplicateClientMessage`）；`input_seen` 登记幂等；同消息二次进编排层被护栏去重、不再唤醒 |
| 8 | `TestAccEventCursorReplayNoGapNoDup`（断线补齐） | rowid 游标补齐不重不漏；(run_id, seq) 重复写入幂等跳过；`webBridge.Replay` 断线补齐的并集恰好是全部唯一事件 |
| 9 | `TestAccControlStopKeepsUnconfirmedUntilClientConfirms`（停止） | Control(stop) → cancelled + 「停止待确认」文案；未确认副作用留待交代列表且状态为 unknown，绝不当成已停止 |
| 10 | `TestAccPauseThenResumeReturnsToDispatchable`（暂停/继续） | 聊天「暂停」→ paused（有回执、不唤醒任何人）；Control(resume) → 回 queued 可重新派发 |
| 11 | `TestAccControlPauseMovesWorkToPaused`（界面暂停） | Control(pause) 必须真的把 Work 置为 paused。该用例曾暴露 pause 译文不匹配命令表的接线缺陷，译文已修复为整句「暂停」，当前通过 |
| 12 | `TestAccControlReplaceReassignsExecutor`（换人） | 换人回执交代新分工并显式取消自审；聊天角色互换版本递增；换审查者后旧 agent 的迟到结论被拒且不改分工版本 |
| 13 | `TestAccHandoffDepthGuardBlocksAndNotifies`（交接深度） | 深度恰为 6 放行、深度 7 被 `GuardDepth` 拦截，并留下用户可见 notice（不许静默丢弃） |
| 14 | `TestAccLocalEntrySecurity`（本机入口） | 回环绑定；无/错凭证 401；跨源 403；无 CSRF 的写操作 403；取 CSRF 后带令牌写放行并真实落库 |
| 15 | `TestAccUnknownEffectSurfacesAsStopUnconfirmed`（外部副作用） | unknown 副作用进待核实列表、快照 `StopUnconfirmed=true`；适配器未接入的 bot 列入能力受限；bot 状态行两态如实 |
| 16 | `TestAccUnavailableEngineFailsHonestly`（诚实占位） | 未接入的适配器返回 `*EngineMissingError` 并指明适配器；空引擎表的 `Health` 也报缺失 |
| 17 | `TestAccStartupArgsAndPathRedLines`（参数与红线） | 六个参数生效；未知参数报错；非回环 listen 拒绝；系统盘 temp 拒绝启动；temp/data 互相包含拒绝 |
| 18 | `TestAccRestartRecoversPendingDelivery`（重启恢复） | 消息已落库未执行时退出：重启后 Run 标 interrupted（不得当成功）、投递重新排队、可再次领取且对准原消息 |

**必须人工在真实客户端验证**（自动化边界，如实分开——真实客户端端到端从未跑过）：

- **锁屏 / 窗口焦点 / DPI 缩放**下的行为（真实桌面会话）。
- **真实客户端会话恢复**（进程重启、断线后的重新关联）。
- **真实审批桥接**：客户端窗口内的授权由用户本人点"允许"，平台不代答（§9.3）。
- **真实成本对账**：客户端不上报用量时，成本面板的"未知"展示与真实账单的口径核对。

## 7. 构建命令

```powershell
cd F:\AIWORKSPACE\agent_harness\mesh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o mesh.exe ./cmd/mesh
```

- `CGO_ENABLED=0`：单二进制，不依赖 gcc/MSVC（方案 §11）；唯一第三方依赖
  `modernc.org/sqlite` 是纯 Go SQLite 驱动，已在 `go.mod` 里。
- **已在本机 Go 1.27.4（windows/amd64）真实构建通过**（2026-11-14）。
- 测试：`go test ./internal/app/...`（本包 18 个验收用例）或全量 `go test ./...`。
