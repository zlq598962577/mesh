# 多 Agent 协作平台 · 技术方案 v3（定稿）

> **一句话**：把本机已装的各家 agent CLI（DeepSeek Harness、Codex、Kimi、Trae CN、Claude Code…）编排起来，做成一个像 `magpie` 那样的单文件可执行程序。
>
> **技术底座**：Go（`CGO_ENABLED=0`）· SQLite 单文件 · Docker 只用于 P2 沙箱。

---

## 1. 三条前提

| # | 前提 | 含义 |
|---|---|---|
| 1 | **Agent = 外部可执行程序** | 平台不定义 agent 的智能，只调度进程。不写 prompt，不做模型网关 |
| 2 | **Agent 集合是运行时可增删的注册表** | 绝不硬编码。装了就出现，卸了就消失 |
| 3 | **产物是一个可执行程序** | 对标 magpie：单文件、零外部依赖、双击即用 |

推论——平台的职责边界只有五件事：**进程编排 · 输出归一化 · 状态持久化 · 审批拦截 · 隔离执行**。模型调用发生在各 agent 内部，平台不在那条链路上，所以计费/路由/网关一律不做。

「某 agent 支不支持 headless」不是架构约束，而是注册表里的一个能力字段。不支持的标 `interactive-only`，不参与自动编排，但依然能注册、能手动调用、能记录。

---

## 2. 形态：对标 magpie

### 2.1 要复刻的形态

实测 `F:\AIWORKSPACE\magpie\magpie.exe`（95.6 MB，文件名 `magpie-windows-amd64.exe` 是 GoReleaser 命名，**magpie 本身是 Go 写的**）：

- 单个自包含 exe，无运行时依赖，双击即用
- 一个二进制覆盖全部入口：桌面窗口 / `tray` / `panel` / `tui` / `web --addr` / `autostart` / `uninstall`
- 程序与数据分离：程序单文件，可变状态全在 `~/.config/magpie/` 与 `%LOCALAPPDATA%\magpie\`
- 自带运维动词：`data move` / `backup` / `restore`（口令密封）/ `save|use|profiles`

要照抄的四条：**一个二进制多模式** · **状态外置** · **自带运维动词** · **自身可被脚本调用**。

### 2.2 命令面

产品代号暂定 `mesh`。注意 `agents` 是数据驱动的，不是枚举的：

```
mesh                        # 默认：托盘 + 窗口
mesh tray                   # 仅托盘
mesh web --addr 127.0.0.1:7788 [--lan] [--no-open] [--tls-cert f --tls-key f]
mesh autostart on|off
mesh uninstall
mesh data move <folder> | --home
mesh backup <file> [--passphrase]   /   mesh restore <file>

mesh agents                 # 列出「已探测到」的 agent（能力、可用性、版本）
mesh agents add <path|url>  # 装入一个 manifest（或 adapter 目录）
mesh agents rm <id>
mesh agents enable|disable <id>
mesh agents probe <id>      # 主动探测：--version + 空转，缓存能力矩阵
mesh agents inspect <id>    # 打印解析后的最终配置，含每个字段的来源

mesh run <agent-id> <task…> # 一次性 headless，--json 输出 NDJSON 事件流
mesh task <id> status|logs|cancel|approve|reject
```

`agents inspect` 打印「每个字段的来源」是刻意抄 magpie 的 `plugin dump`——**配置可解释是排障的地基**，缺了它动态注册表会变成黑箱。

### 2.3 平台自身必须 headless 可调用

`mesh run --json` 不是附赠功能，是必需品：自动化测试靠它（不依赖 GUI）；平台可被别的 agent 当成 agent 调用；且**它的输出格式就是它要求下游 agent 的格式**——同一套契约两个方向，免费获得一个参考实现。

---

## 3. 技术选型

### 3.1 为什么是 Go

| 理由 | 说明 |
|---|---|
| **并发模型天然匹配问题形状** | 本项目 = 一堆独立进程各自有生命周期 + 事件汇聚到单点写库。goroutine + channel 的教科书场景，不需要论证「不用 asyncio」 |
| **Windows 原生能力直接可用** | Job Object（`x/sys/windows`）、ConPTY（`creack/pty`）、文件监视（`fsnotify`）全是成熟库 |
| **单文件最彻底** | 真静态、零运行时、~0ms 冷启动。这不是审美——它决定分发体验 |

学习成本：Go 只有 25 个关键字，静态类型 + 接口 + goroutine 近似线程，**有 Java 底子语法层面一周上手**。真实成本在生态陌生（错误处理风格、要自己拼库），不在语言本身。

### 3.2 依赖清单（全纯 Go，无 cgo）

```go
// 核心
gopkg.in/yaml.v3              // manifest 解析
github.com/google/go-jsonpath // eventMap 投影
github.com/fsnotify/fsnotify  // 监视 ~/.mesh/agents/ 热增删
modernc.org/sqlite            // 纯 Go SQLite

// Windows 原生
golang.org/x/sys/windows      // Job Object
github.com/creack/pty         // ConPTY

// P1 起
github.com/go-chi/chi/v5      // 内嵌 Web API
embed                         // 标准库，前端打进二进制

// P2 起
go.opentelemetry.io/otel      // 追踪
github.com/getlantern/systray // 托盘

// 打包
goreleaser                    // 交叉编译 + 发布
```

> **硬约束：不许引入 cgo。** 一旦有 cgo 静态单文件就不成立——这是选 `modernc.org/sqlite` 而非 `mattn/go-sqlite3` 的唯一原因。CI 必须卡 `CGO_ENABLED=0 go build`。

### 3.3 开工前要补的环境

| 项 | 现状（实测） | 动作 |
|---|---|---|
| Go SDK | 未装 | 下载 zip 解压到 `F:\go`，加 PATH。**不需要管理员权限** |
| **git** | **未装** | 必须装——`git worktree` 是 §6.2 隔离方案的基础 |
| Docker | 客户端在，引擎 `com.docker.service` 为 Stopped，管道 Access denied | 本阶段不需要，P2 再验 |
| C 编译器 | 无 `cl`/`gcc`/`clang` | 不需要（全纯 Go + `CGO_ENABLED=0`） |

---

## 4. Agent 注册表

### 4.1 四层概念

| 层 | 是什么 | 存哪 | 谁改 |
|---|---|---|---|
| **Catalog** | 内置 agent 类型清单，每个一个 manifest | 编译进二进制（`embed.FS`），首次运行释放到磁盘 | 随版本升级 |
| **Registry** | 本机「已启用」的 agent 实例 | `~/.mesh/agents/*.yaml` | `agents add/rm`，或直接改文件 |
| **Detection** | 判断 manifest 对应的程序是否存在 | 运行时求值，结果缓存 | 自动（启动 + 定期 + 手动 probe） |
| **Instance** | 真正被 spawn 的进程 | 内存 + 状态库 | 运行时 |

**Catalog 内置 ≠ 写死**：内置的只是配方，配方是否生效取决于 Detection。装了 Codex，下次启动自动从 `disabled` 变 `detected`；卸了自动变 `missing`，任务给明确错误而不是崩。**新增一种 agent = 丢一个 manifest 文件，不重新编译。**

### 4.2 manifest schema（Tier-1，声明式，无需写代码）

```yaml
# ~/.mesh/agents/dsh.yaml
id: dsh
displayName: DeepSeek Harness
version: 1

# ── 探测：怎么知道这台机器上有它 ──────────────────────
detect:
  executable: ["dsh", "dsh.cmd"]          # PATH 解析顺序
  fallbackPaths:                          # PATH 找不到时的候选
    - "F:/deepseek/resources/runtime/cli/bin/dsh.cmd"
  configDirs: ["~/.dsh"]                  # 存在即认为装过
  versionCommand: ["--version"]
  versionRegex: "(\\d+\\.\\d+\\.\\d+)"
  exclude: []                             # 见 §6.5 黑名单

# ── 调用：怎么起进程 ──────────────────────────────────
launch:
  mode: oneshot                # oneshot | pty | http-endpoint | sidecar
  argv: ["--profile", "headless", "{{prompt}}"]
  promptDelivery: argv         # argv | stdin
  workdirFlag: null            # 支持 --cwd 的填 "--cwd"
  envPassthrough: ["DEEPSEEK_API_KEY", "HTTPS_PROXY"]
  timeoutSec: 1800
  killPolicy: job-object       # Windows: Job Object；Unix: 进程组

# ── 输出：怎么读它的流 ────────────────────────────────
output:
  format: jsonl                # text | jsonl | sse
  eventMap:
    session:  { match: '$.type == "session"',  sessionId: "$.sessionId", cwd: "$.cwd" }
    progress: { match: '$.type == "status"',   phase: "$.phase", step: "$.step" }
    final:    { match: '$.type == "final"',    text: "$.text", reason: "$.reason" }
  successSignal: '$.final.reason.kind == "success"'   # 唯一可靠的成功判据
  errorSignal:   '$.final.reason.kind == "error"'

# ── 续跑与审批 ────────────────────────────────────────
resume:
  supported: true
  flag: ["--session-id", "{{sessionId}}"]
approval:
  delegateToCli: true
  flags: ["--approval-policy", "ask"]     # 需 pty 模式才阻塞得住
  unattendedFlags: ["--approval-policy", "never"]

# ── 能力声明（供编排器决策，不参与架构裁剪）─────────────
capabilities:
  headless: true
  streaming: true
  resume: true
  interactiveOnly: false
  costReport: partial          # none | partial | full
concurrencyLimit: 4
```

> **`successSignal` 必填，不能靠 exit code。** 实测 DSH 在 LLM 调用失败时进程内已产生 `turn_end` 的 error 事件、`final.text` 为空，但退出码语义不可靠（协议层成功、业务层失败）。只有结构化事件里的 reason 字段能区分「跑完了」和「跑失败了」。

### 4.3 两级扩展

| 级别 | 形态 | 适用 | 要编译吗 |
|---|---|---|---|
| **Tier-1** | 纯 YAML manifest | 「起这个 argv，读这个流」能表达的一切 | 不需要 |
| **Tier-2** | **sidecar adapter 进程**，走 stdio JSON-RPC | 需要 PTY 交互、私有握手、非标准流 | 不需要（任何语言） |

Tier-2 刻意**不用 Go plugin**：`plugin` 包在 Windows 上基本不可用，且会把 adapter 锁死在 Go 上。改为独立子进程 + 固定协议，adapter 可以是 Go、Python、Node、Bash。协议只有 6 个方法：

```
initialize(manifest) -> capabilities
spawn(task)          -> {pid, sessionId}
send(sessionId, msg) -> ack          # PTY 场景
events(sessionId)    -> stream       # 归一化后的事件流
cancel(sessionId)    -> ack
health()             -> {ok, version}
```

### 4.4 热增删的语义

- **监听**：`fsnotify` 监视 `~/.mesh/agents/`，增删改即重载，无需重启
- **新增**：拷贝 manifest → 探测 → `detected` 或 `missing`
- **删除**：正在跑的任务**不杀**，标记 `orphaned`，照常收尾并落库；之后不再调度。要立刻杀用 `--cancel-running`
- **冲突**：两个 manifest 同 `id` → 拒绝加载并报错，不静默覆盖
- **损坏**：校验失败 → 该条进 `invalid` 状态红字显示，其余不受影响。**一个坏文件不能拖垮整个注册表**

---

## 5. 编排层

### 5.1 分层

```
┌─────────────────────────────────────────────┐
│ 接入层  Web(内嵌) / REST / WebSocket / CLI    │
├─────────────────────────────────────────────┤
│ 编排层  Coordinator · Scheduler · 审批节点     │
│         Task 状态机 · 重试/超时/熔断 · 配额    │
├─────────────────────────────────────────────┤
│ 适配层  Registry · Manifest · Adapter Runtime │
│         Detection · 事件归一化 · 进程生命周期  │
├─────────────────────────────────────────────┤
│ 基础层  SQLite 状态库 · 事件日志 · OTel 追踪   │
│         工作区隔离 · 密钥读取（只读）           │
└─────────────────────────────────────────────┘
```

没有模型网关层。工具/连接器与向量检索归 agent 侧：MCP server 和 skill 由各 agent 自己配置，平台只负责把它们**写进**每个 agent（§5.4），而不是替 agent 执行。

### 5.2 内部事件模型

所有 agent 无论输出 text / jsonl / sse，一律压成六种事件：

```
session   { sessionId, cwd, agentId }
progress  { phase, step?, message? }
output    { chunk }                    # 增量文本
approval  { id, kind, detail, options }
final     { text, reason: {kind, code?, message?} }
error     { code, message, retryable }
```

`eventMap` 用 JSONPath + 谓词把各家格式投影到这套模型上。**这是平台最核心的一段代码，也是唯一必须自己写、无法外包的部分。**

### 5.3 任务状态机

```
pending → planning → running → (awaiting_approval → running)*
                  ↘ retrying ↗
                              → succeeded | failed | cancelled | orphaned
```

- **重试**：指数退避，≤3 次，仅对 `retryable` 错误。`MISSING_CREDENTIAL` 这类**不重试**，直接失败并提示
- **超时**：三层——单 step、单 task 墙钟、全局 `max_wall_clock`
- **熔断**：某 agent 连续 N 次失败 → 临时摘出调度池，冷却后半开
- **硬顶**：`max_steps` / `max_tokens` / `max_wall_clock` / 单工具调用次数。**没有硬顶的多 agent 循环一定会失控**
- **防循环**：Coordinator 维护「已派发任务指纹集合」，同一指纹重复出现即判循环并中止；转派链深度上限 3

### 5.4 能力下发（抄 magpie 的 library）

magpie 会往 `~/.dsh/AGENTS.md` 写一个带标记的块：

```
<!-- magpie:begin · written by magpie from its Library -->
所有临时文件不许放C盘，都放在F:\AIWORKSPACE\temp路径下
<!-- magpie:end -->
```

**带标记 → 可重复同步、可精确回滚。** 平台照抄：

- `mesh library instructions set <file>` → 写进每个已启用 agent 的指令文件（`AGENTS.md` / `CLAUDE.md`），用 `<!-- mesh:begin -->` / `<!-- mesh:end -->` 包裹，**只替换标记内内容，绝不碰用户手写部分**
- `mesh library mcp add <name> <url|command>` → 写进各 agent 的 MCP 配置
- `mesh library sync` → 重新下发（新装 agent 后、用户手改后）
- 每个 agent 可单独勾选是否接收，`none` 表示不接收

> 下发是**写文件**，高风险操作。必须先备份 `*.mesh.bak`，失败可 `library rollback`。

---

## 6. 进程生命周期（Windows 是重点）

纯工程活，但最容易翻车。

| 问题 | 处理 | Go 实现 |
|---|---|---|
| **孤儿进程** | Windows 必须用 **Job Object**（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）。实测 DSH 子进程能活得比父进程久，只 `Process.Kill()` 收不干净。Unix 用 `setpgid` + `kill(-pgid)` | `x/sys/windows` 调 `CreateJobObject`/`AssignProcessToJobObject`/`SetInformationJobObject`；启动前设 `CREATE_SUSPENDED`，入 Job 后再 Resume |
| **取消** | 先温和信号，等 5s，再整组杀 | `GenerateConsoleCtrlEvent` → 超时 → `TerminateJobObject` |
| **输出背压** | stdout 单独 goroutine 读；消费慢时**丢 progress 不丢 final**，避免 OOM | 带缓冲 channel（cap≈1000）；progress 走 `default` 丢弃分支，final 阻塞发送 |
| **编码** | Windows CLI 输出可能是 GBK。统一 `chcp 65001` + UTF-8，失败按 locale 猜 | `x/text/encoding/simplifiedchinese.GBK` fallback。**实测 magpie 输出已出现 `鈥?` 乱码残渣，必须处理** |
| **stdin 阻塞** | `--approval-policy ask` 阻塞等输入 → 必须用 PTY 而非普通 pipe。manifest 用 `launch.mode: pty` 声明 | `creack/pty`（ConPTY）；审批决定写回 PTY stdin |
| **cwd 隔离** | 每次 run 一个独立工作目录；支持 git 的用 `git worktree add` | `exec.Cmd.Dir` + `os/exec` 调 git |
| **并发上限** | 每 agent 一个信号量（`concurrencyLimit`），全局一个 | 带缓冲 `chan struct{}` 当信号量 |
| **启动竞态** | spawn 后等首帧 `session` 事件（带超时）才算真启动，避免「进程在但没连上模型」被误判成功 | `select` + `time.After` |

**两个必须写测试的点**（不写一定出事）：

1. **Job Object 回收**：起一个 run，`taskkill /F` 杀 `mesh.exe`，确认没有残留 `dsh.exe`
2. **PTY 审批往返**：写一个假 agent（读 stdin 打印到 stdout），验证平台能阻塞、能收 approval 事件、能把决定写回

---

## 7. 状态、隔离与安全

### 7.1 状态归属

- 任务/运行/步骤/事件全落 **SQLite**（`~/.mesh/mesh.db`，WAL），驱动 `modernc.org/sqlite`
- agent CLI 的 `sessionId` 只是**续跑加速器**，不是真相。丢了它平台照样能重建上下文（历史事件回灌新 prompt）
- 事件流另存 JSONL（`~/.mesh/runs/<runId>/events.jsonl`），便于重放排障

**为什么不用 Docker 里的 Postgres**：平台启动前要先开 Docker Desktop，那它就不是自包含程序，而是「一个 exe + 一个 Docker」。分层策略：

| 阶段 | 存储 | 理由 |
|---|---|---|
| **P0–P2（默认）** | SQLite 单文件 | 零安装、可随 `backup` 打包。万级事件 WAL 完全够 |
| **向量检索** | SQLite 存 embedding + Go 标准库暴力余弦 | <10 万条毫秒级，不必上 pgvector |
| **P3（多租户）** | Postgres + pgvector（Docker） | 此时才真正需要并发写、RLS、向量索引 |

Docker 在本项目里**最有价值的用途不是装数据库，而是 P2 的 agent 沙箱化**。

### 7.2 隔离

- **工作区**：git worktree per agent，物理隔离写入范围
- **写域约定**：并行 agent 各自只写自己的目录；跨 agent 共享文件必须经平台提交
- **容器沙箱**（P2）：高风险 agent（能执行 shell、能联网）默认跑容器，挂载限定目录。引擎不可用时降级为「进程 + Job Object + 目录白名单」，UI 明确标注降级

### 7.3 审批

1. **委托给 CLI**（首选）：agent 自己有权限模型就用它的（`--approval-policy` / `--permission-mode` / `--add-dir`）。平台只负责把 approval 事件冒泡到 UI 并把决定写回 stdin
2. **平台兜底**：CLI 没有审批能力时，平台在 `approval` 事件上阻塞。触发条件写死：**花钱 / 对外发送 / 删除 / 改线上配置 / 写工作区外路径** → 一律挂起等人

### 7.4 密钥

- 平台**只读不写**各 agent 的凭据文件（`~/.dsh/.credentials.yaml`、`~/.codex/config.toml`…）
- **健康检查/诊断日志绝不允许 dump 凭据内容**。实测本机 `~/.codex/config.toml` 里有明文 bearer token 和 GitHub PAT——被 `agents inspect` 或日志带出来就是事故。诊断只输出「存在 / 不存在 / 过期」
- 平台自身不需要模型 key（它不是网关），这是本架构的安全红利

### 7.5 扫描黑名单

`CodexChinese` 这类安装**管理员权限每分钟计划任务**的程序，不得进入 agent 扫描目录——否则平台等于提供一个提权面。注册表用 `detect.exclude` 显式排除。

---

## 8. 交付分期

| 阶段 | 周期 | 交付 | 完成判据 |
|---|---|---|---|
| **P0 骨架** | 1–2 周 | Go 单文件 `mesh.exe`：`agents ls/add/rm`、manifest 解析、`run` 一次性 headless、事件归一化 | 跑通 DSH + 任意第二个 agent，输出统一事件流 |
| **P1 MVP** | +2–4 周 | SQLite 状态机、多 agent 串行编排、审批节点、内嵌 Web UI、`backup/restore` | 一条真实任务链（研究→写→审）端到端跑完，中断可恢复 |
| **P2 可用** | +4–8 周 | 并行编排、worktree 隔离、`library` 下发、OTel、熔断重试、托盘 | 连续跑 100 任务无失控、无孤儿进程 |
| **P3 产品化** | +6–12 周 | 多租户、`--lan` + TLS 远程模式、Tier-2 adapter SDK | 可交付他人使用 |

P0 里**没有**「Agent 定义编辑器」——那是伪需求。P1 的验收是「跑通一条真实任务链」，不是「能配出一个 agent」。

---

## 9. 关键风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| 各家 CLI 输出格式随意变动 | 归一化层失效 | `eventMap` 数据驱动 + `probe` 自检 + 每个 adapter 带 golden 事件样本做回归 |
| headless 能力参差 | 部分 agent 无法自动编排 | 能力字段 + `interactive-only` 降级，不阻塞架构 |
| 长时运行失控 | 费用与状态双爆 | 硬顶 + 循环指纹检测 + 熔断 |
| 审批窗口长 | 无人值守时卡死 | 超时自动拒绝并落 `awaiting_approval`，不静默等待 |
| 单文件体积膨胀 | 分发体验 | 前端 gzip + `-ldflags "-s -w"` + UPX（可选），目标 <40 MB |
| 平台自身成为提权面 | 安全事故 | §7.4 / §7.5 的只读与黑名单约束 |
| 引入 cgo | 静态单文件失效 | CI 卡 `CGO_ENABLED=0 go build` |

---

## 10. 需要拍板的三件事

1. **产品名**：暂用 `mesh`，是否更换
2. **托盘 GUI 还是纯 Web**：托盘 + 内嵌 Web 是 magpie 路线，体验完整但多 2–3 周。建议 **P0/P1 先纯 Web，P2 再补托盘**
3. **Tier-2 sidecar 参考实现语言**：建议 **Go 写协议骨架 + 附 Python/Node 最小示例**。协议是 JSON-RPC over stdio，任何语言都能实现
