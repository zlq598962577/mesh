# 编排层（internal/orchestrator）

这一层只干四件事：**记住一个委托走到哪了**、**决定该叫醒谁**、**拦住打转**、**接住四个协作工具**。

不做的事（架构边界，见 `docs/COORDINATION.md` §4）：

- 不操作窗口、不发键鼠、不读屏幕 —— 那是 `desktop` / `adapters` 的事。
- 不写 SQL —— 持久化只通过本包自己声明的 `Store` 小接口（`store.go`），实现方在 `store` 包，由 app 层注入。
- 不 import `store` 包的具体实现，也不 import `desktop` / `adapters`。依赖只有 `mesh/internal/contract`（已冻结）和标准库。

## 文件

| 文件 | 内容 |
| --- | --- |
| `store.go` | `Store` 接口（声明，不含实现）、`ErrNotFound`、`Pending` |
| `work.go` | Work 状态机：迁移表、产物提交、审查结论 |
| `roles.go` | 分工优先级、自审拦截、聊天指令识别（咨询 / 改分工 / 暂停继续停止）、换人时撤销待处理项 |
| `route.go` | `Orchestrator` 本体 + 唤醒路由 + `OnUserMessage` 聊天入口 |
| `guard.go` | 四道循环护栏 + 预算 |
| `tools.go` | 四个协作工具的参数校验与落地 |
| `effect.go` | §5.8 外部副作用的五种状态，以及"停止能不能确认" |
| `schedule.go` | §5.6 执行名额：每 bot 串行、全局并发上限、跨 Work 轮转 |
| `work_test.go` | 状态机 / 分工 / 工具 / 护栏 / 副作用 / 名额的单元测试（56 个，全绿） |

## 一、三个概念不许混

方案最容易写错的地方是把"活干完了"和"这次执行完了"当成一件事。这里严格分开：

| 概念 | 回答的问题 | 载体 |
| --- | --- | --- |
| **Work** | 这件差事办成了没有 | `contract.Work.State` |
| **Run** | 这一次执行跑完了没有 | `contract.Event` / `FinalInfo`（本层不建模） |
| **Review** | 这一个产物版本过没过 | `contract.Review`（带 `ArtifactVersion`） |

一个 Work 可以有多个 Run，也可以有多个产物版本、多轮审查。Run 成功 ≠ Work 完成；审查通过才算完成。

## 二、Work 状态机

```
                    ┌──────────────────────────────┐
                    │                              │
  (创建)            ▼                              │
   └──────► queued ──────► executing ──────► reviewing
              │  ▲            │  ▲               │  │
              │  │            │  └───────────────┘  │
              │  │            │     changes_requested│
              │  │            │     (轮次 ≤ 2)       │
              │  │            ▼                     ▼
              │  │         completed            waiting_user
              │  │            ▲                     │  │
              │  │            │                     │  │
              │  └────────────┴─────────────────────┘  │
              │        paused ◄───────────────────────┘
              │          │
              └──────────┴──► cancelled   failed ──► queued / cancelled
```

允许的迁移（`work.go` 的 `workStateTransitions`，唯一权威）：

| 从 | 可以到 |
| --- | --- |
| `queued` | executing, paused, waiting_user, cancelled |
| `executing` | reviewing, completed, paused, waiting_user, failed, cancelled |
| `reviewing` | executing, completed, paused, waiting_user, cancelled |
| `paused` | queued, executing, waiting_user, cancelled |
| `waiting_user` | queued, executing, completed, cancelled |
| `failed` | queued, cancelled |
| `completed` | —（终态） |
| `cancelled` | —（终态） |

不在表内的迁移一律返回 `*WorkError`，不做"顺手帮忙"的隐式修正。同状态迁移是幂等的（返回 nil），便于重复投递。

### 完成必须绑定产物版本

`Review` 带 `ArtifactVersion`。`onReviewVerdict` 处理 `approved` 时的判定顺序：

1. 不在 `reviewing` 状态 → 只当历史记录存下来，**不改状态**。
2. `r.ArtifactVersion != latestVersion(w)` → 返回 `*WorkError`：`approved 绑定版本 N，但最新版本是 M，旧审批不得放行新版本`。
3. 版本号越界（`<1` 或 `>len(Artifacts)`）→ 同样报错。
4. 通过以上检查，才把 `w.Final` 指向**那个版本**并迁移到 `completed`。

这样"审查者批的是 v1，执行者已经交了 v2"不会误放行 v2。这是方案 §11 点名的风险项，实现上必须显式挡。

### 自动返修轮次

`changes_requested` → `ReviewRounds++`；超过 `contract.MaxAutoReviewRounds`（2）→ `waiting_user`，并带一句提示。不无限自动往返。

### 解析失败永不等于通过

`parseReviewArgs` 对空结论、拼错的结论（`lgtm` / `pass` / `ok`）、缺字段的结论一律返回 `*ToolError`，绝不兜底成 `approved`。`onReviewVerdict` 的 `default` 分支也是转 `waiting_user`，不是 `completed`。

## 三、分工优先级

| 优先级 | `RoleSource` | 来源 | 说明 |
| --- | --- | --- | --- |
| 1（最高） | `SourceUserChat` | 用户在聊天里明确说 | 最新一句覆盖旧的 |
| 2 | `SourceUserUI` | 启动时 UI 上的选择 | 与聊天产生同样的分工变更记录，按版本号排序 |
| 3 | `SourceRoomDefault` | 房间默认 | |
| 4（最低） | `SourceSoulSuggest` | agent 自己的"人格"建议 | 只作建议，不得推翻上面任何一条 |

规则在 `resolveAssignment`：来源等级低于当前记录 → 直接忽略（返回 `changed=false`），版本号不涨。同等级或更高级 → `Version = prev.Version + 1`；如果人没变（`sameAssignment` 忽略 Version/Source/At），版本号涨但 `changed=false`，不触发撤销动作。

`validateAssignment` 两条硬线：

- 执行者不能为空。
- **执行者不能同时是审查者**：返回 `*RoleError{NeedUser: true}`，文案 `X 既是执行者又是审查者，平台不把自审当作独立审查`。用户确实想要"自己干自己验"，得明确说不审查（空审查者），而不是把一个人填两次。

### 咨询 ≠ 换人

`interpretRoleInstruction` 的默认行为是**不改分工**：

| 用户说法 | 判定 | 动作 |
| --- | --- | --- |
| `@B 看看这个` | `InstrConsult` | 只唤醒 B 给意见，分工不动，`KeepExisting=true` |
| `改由 B 审查` | `InstrRoleChange` | 审查者换成 B |
| `改由 B 做` | `InstrRoleChange` | 执行者换成 B |
| `不用审查` | `InstrSkipReview` | 清空审查者（这是用户明确选择，不是"忘了填"） |
| `@B @C 你们改一下` | `InstrRoleChange` + `Ambiguous` | 只问"谁执行谁审查"，已有委托继续按原分工跑 |

`Ambiguous` 非空时**只就模糊的那部分提问**，绝不自行挑一个当执行者。@ 到不存在的人同样进 `Ambiguous`，不会猜一个替身。

### 换人时撤销待处理项

`changeAssignment` 先写分工版本记录，再撤销旧角色名下、且新分工里已无角色的 `Pending`（`PendingDelivery` / `PendingApproval`）。顺序不能反：反了会在崩溃时丢版本号。

**边界**：只能撤销"还没产生外部副作用"的投递和审批。已经发进客户端、已经开始跑的动作平台收不回来 —— 见下面「方案内部的矛盾」第 3 条。

## 四、唤醒路由

`Route` 的判定（`route.go`）：

| 消息 | 唤醒谁 |
| --- | --- |
| `MsgTask`，或带排队中 Work 的 `MsgChat` | 只唤醒**执行者**（`new_work`） |
| `MsgTask` / `MsgChat`，但 Work 处于执行中 | 只唤醒**执行者**（`revise`）—— 这是审查退回后的返修任务 |
| `MsgResult`（产物已提交） | 只唤醒**审查者**；没有审查者则 Work 已直接完成，不唤醒任何人 |
| `MsgHandoff` | 唤醒 `ToID` |
| 任意消息的 `Mentions` | 并入唤醒集合（咨询性质） |
| `MsgChat`（普通聊天，无排队 Work） | 不按角色唤醒，只看 @ |

过滤顺序（`filterTargets`）：去空白 → 按 bot 去重 → 剔除发送者自己（防自激）→ 剔除不在房间的 bot（记一条 notice，不猜替身）→ 逐目标跑护栏。

最后一条兜底：`MsgTask` / `MsgHandoff` 走完过滤后一个目标都没剩下 → 写一条 notice 说明"本次没有唤醒任何人"。**不允许静默失败**。

**没有实现的两类唤醒，以及为什么**：审批（`approval`）和"用户点名让人处理"（`user_ask`）没有做成独立的 `WakeReason`。审批走的是引擎的 `ResolveApproval` 通道，唤醒谁由审批发起方决定，路由层不该二次决定；用户点名本来就被 `Mentions` 覆盖。声明了却永不产生的常量是假接口，所以直接删掉，不留在代码里充数。

## 五、四道循环护栏

全部在 `guard.go`，P0 强制，`Check` 里按"便宜的先判"排序：

| # | 护栏 | 阈值 | 拦下时的提示（原文） |
| --- | --- | --- | --- |
| 1 | 幂等去重 | 同一 `(message_id, bot_id)` 只投一次 | 无（那条消息已经送到过，用户没丢东西） |
| 2 | 交接深度 | `MaxHandoffDepth = 6` | 这次交接已经连续转了 N 层（上限 6 层），我先停在这里。要它继续，请直接说明下一步由谁做什么。 |
| 3 | 累计唤醒 | `MaxWakeupsPerWork = 20` | 这个委托已经互相唤醒了 N 次（上限 20 次），看着像在原地打转，我先暂停自动流转。请给一句明确的下一步，或者换个执行者。 |
| 4 | 冷却静音 | 60 秒内 > 6 次 → 静音 5 分钟 | X 在 60s 内被唤醒了 N 次（上限 6 次），已临时静音 5m0s。这通常是两个 bot 在互相推活，请指定一个人收尾。 |
| 5 | 预算 | 见下 | 见下 |

去重是唯一允许 `Notice` 为空的拦截（`GuardVerdict` 的注释写明了这一点）。其余任何"因为超限所以不做了"都必须留下用户看得见的提示 —— 平台不能悄悄停。

去重落库（`Store.MarkInputSeen` 必须是原子操作，靠唯一索引或事务），这样重启后不会重复唤醒同一个 bot。其余护栏状态是纯内存（`guardState`），重启从零开始：护栏防的是同一进程内的循环风暴，落库反而会造成"重启后还被静音"的困惑。

### 预算的诚实边界（重要）

`checkBudget` 只对**可观测**的部分生效：

- `MaxStepsPerTurn`：每轮步数，平台数得清。
- `MaxTurnSeconds`：每轮时长，平台掐得了表。
- `DailyTokens`：**只在 `Usage.Known == true` 时生效**。

四个桌面客户端（Codex / DSH / Trae / 豆包）都不上报 token 用量，`contract.Usage.Known` 是 `false`。所以配了 `DailyTokens` 也**卡不住真实消耗**。这种情况下 `checkBudget` 会给出一次性的明确提示：

> 这个客户端不上报 token 用量，我没法把 token 上限当硬线来卡。目前只按步数和时长控制；要精确控制请改用能上报用量的内置引擎。

宁可说清楚，也不假装有一道不存在的硬线。

## 六、四个协作工具

`tools.go`，P0 只有这四个，不做自由协议：

| 工具 | 谁能调 | 关键校验 |
| --- | --- | --- |
| `collaboration.send` | 任何 bot | `to` 必须在房间内；`text` 不能空 |
| `collaboration.handoff` | 任何 bot | 不能交接给自己；交接产生的分工变更记为 `SourceSoulSuggest`（bot 不能推翻用户的明确指定） |
| `work.submit` | **只有执行者**，且状态为 `executing` | `summary` / `kind` / (`ref` 或 `files`) 必填；**版本号由平台分配**，模型传 `version` 会被 `DisallowUnknownFields` 拒掉 |
| `review.submit` | **只有审查者** | `approved` 必须带 `artifact_version` 且写 `findings`；结论不认识就报错，不兜底 |

`strictUnmarshal` 用 `DisallowUnknownFields` + 拒绝尾随内容，目的是让模型没法塞越权字段（尤其是伪造版本号）。

工具失败返回 `*ToolError`，是给模型看的、让它重试的错误，**不是**"通过"。

## 七、方案内部的矛盾 / 不好实现的地方

按 `docs/COORDINATION.md` §8 的要求：按方案原文实现，把冲突记在这里，不自行改设计。

1. **§5.3 要求给单次 Run 卡死步数和时长，同时又承认客户端内部的步骤平台看不见。** 两者不能同时成立。桌面客户端的内部循环（它自己想了几轮、点了多少下）没有可观测出口，平台只能在"投递—回传"这个边界上计数。所以本实现的 `MaxStepsPerTurn` 数的是**平台可见的步数**，不是客户端真实步数。方案如果要真正的硬上限，得让 adapter 上报心跳或步骤事件 —— 那是 adapters 层的活，不在本层。

2. **§9.6 的 token / 金额硬上限在四个桌面客户端上根本绑不住。** 原因同上：`Usage.Known=false`。本层没有假装它生效（见「预算的诚实边界」）。真要落地，只有两条路：改用内置引擎，或者让 adapter 抓客户端自己的用量显示 —— 后者依赖屏幕读取，脆弱且不可靠。

3. **§5.8「换人时撤销旧角色的待处理投递与审批」和"已经发出去的外部副作用"冲突。** 消息一旦进了客户端窗口、开始执行，平台无法回滚（这正是 `OperationState` 里 `OpUnknown` 存在的理由）。本实现只撤销尚未送达的 `Pending`，已发生的如实告知用户，不写"撤销成功"。方案原文那句"撤销"需要限定为"撤销尚未产生外部副作用的动作"。

4. **`RoleAssignment.Reviewer` 空字符串既是"用户明确选择不审查"，也是"没设置"的零值。** 语义撞车。`contract` 里另有 `Review.Skipped` 字段，但 `RoleAssignment` 没有对应的 `ReviewerCleared bool`。本实现的处理：空 = 不审查，且在这种情况下 `toolWorkSubmit` 会明确提示"这个委托没有独立审查者，产物未经审查即为最终结果"，绝不伪造一条 `approved`。建议后续在 contract 里加一个显式标志位。

5. **§5.6 允许全局 2 个并发 Run，但桌面客户端是单实例独占的。** 两个 Work 抢同一个客户端时，串行是客户端本身强制的，不是本层的锁能协调的。"跨 Work 公平轮转"在桌面场景下只能做到"投递队列公平"，做不到"执行公平"—— 真正的执行顺序由客户端窗口决定。本层不假装自己排得动。

6. **"两个客户端就能验证完整循环"与"验收要求四客户端全跑通"不一致。** 两个客户端只够验证"执行—审查"最小回路；交接深度、并发、静音这些护栏在四客户端下才有真正的相互推活场景。按方案实现，不缩减验收范围。

7. **本层注入了 `contract.Engine` 却一次也没调用它。** 这是有意为之，不是漏写：编排层只做"该唤醒谁、状态能不能走、护栏过不过"的决策，真正的开窗、投递、取消、审批应答全部由 app/scheduler 层执行。好处是这一层可以纯单测（`fakeStore` 就够，不需要模拟客户端）；代价是**"谁负责真正调 `Engine`"在方案里没有明确归属**，读代码的人容易误以为这里是端到端闭环。方案需要补一句职责边界，否则 P0 联调时会出现两处都在投递、或者两处都不投递。

## 八、调用方必须补的六件事（否则护栏是哑的）

这一层是纯决策层，有六处能力它自己不产生数据、或者根本没人调用，只能由调用方（app/scheduler）接上。写在这里，免得联调时以为护栏已经生效：

1. **`Message.ReplyTo` 必须由调用方填。** 交接深度护栏（≤6）靠 `depthOf` 沿 `ReplyTo` 因果链回溯；本包里没有任何代码写 `ReplyTo`。调用方不填，`depthOf` 恒为 0，深度护栏永远不会触发 —— 看起来有四道护栏，实际只有三道。
2. **`RecordUsage` / `StartTurn` / `ForgetWork` / `Unmute` 必须由调度器调用。** 它们导出在 `guard.go`，但包内没有调用点。不调 `RecordUsage` 就没有用量，预算护栏只剩"用量未知"的诚实提示；不调 `StartTurn` 就不计步数；不调 `ForgetWork`，`wakes` / `spent` 两个 map 会随 Work 数量单调增长（内存泄漏，长跑必然暴露）；不调 `Unmute` 则静音只能等 5 分钟自然过期。
3. **`work.submit` / `review.submit` 不往房间发消息。** 它们只改 Work 状态并返回文本给模型。要让用户和其他 bot 在聊天里看到"v2 已提交，等 trae 审查"，得由调用方拿 `ToolResult.Text` 自己发一条 `MsgResult`。本层不发，是为了不替 app 层决定消息排版。
4. **`MsgTask` 和 `MsgResult` 本包不产生。** `Route` 的两条主分支（新委托唤醒执行者、产物提交唤醒审查者）都依赖这两种消息，但包内只写 `MsgChat` / `MsgHandoff` / `MsgNotice`。调用方不合成 `MsgTask` / `MsgResult`，自动流转根本不会启动 —— 只有 `@` 会唤醒人。这是这一层最大的隐性依赖，必须在 app 层落地时先写测试确认。
5. **`CallTool` 没有包内调用点。** §5.5 的四个协作工具入口就是这个函数，但真正把它接到模型输出上的是 app 层（拿到 bot 的工具调用意图 → 解析名字和参数 → 调 `CallTool`）。不接，则 `work.submit` / `review.submit` 永远不会被调用，Work 永远停在 `executing`。
6. **`interpretRoleInstruction` 只在测试里被调用，聊天里的"暂停 / 继续 / 停止"没有实现。** §12 的优先级阶梯（用户聊天指令 > UI > 房间默认 > soul 建议）目前只有测试在走；生产路径上改分工的唯一入口是 `collaboration.handoff`，而它固定用 `SourceSoulSuggest`（最低优先级）。也就是说：**在调用方接上 `interpretRoleInstruction` 之前，用户打字改分工完全不生效**，而 `SourceUserChat` 这个最高优先级形同虚设。另外 `InstructionKind` 只有 `InstrNone` / `InstrConsult` / `InstrRoleChange` / `InstrSkipReview` 四种，没有暂停/继续/取消，`WorkPaused` / `WorkCancelled` 只能靠直接调 `transition` 到达 —— 方案里的用户级"暂停 / 继续 / 停止"控制在本层是缺的，需要补一个 `InstrPause` / `InstrResume` / `InstrCancel` 分支（同样要 `SourceUserChat`）。

## 九、测试状态

`work_test.go` 覆盖：状态机正常路径、无审查者直接完成、旧版本审批被拒、版本号越界、自动返修轮次上限、blocked 交回用户、未知结论不放行、非法迁移、终态不可离开、全状态可达、分工优先级、未知来源被拒、同人变更不触发撤销、自审被拒、空执行者被拒、helpers 去重、咨询不改分工、明确改分工、模糊只问模糊部分、多目标歧义、跳过审查、普通聊天无指令、审查解析失败不放行、`work.submit` 拒绝无法定位的产物、深度/唤醒数/去重/冷却/预算（已知与未知用量两种）。

**编写环境**：这台机器上没有 Go 工具链（`go` 不在 PATH），也没有网络可以装，所以这些测试是我按人工阅读保证类型正确性的。**已由 Lead 在其环境执行 `go test ./internal/orchestrator/` 通过（0.828s）**，包能编译、用例全绿。后续改动仍应本地跑 `go vet ./internal/orchestrator/` + `go test ./internal/orchestrator/`。
