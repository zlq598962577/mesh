# web —— 本机回环 Web 界面

`mesh/internal/web` 把 mesh 的状态暴露成一个只在本机可访问的网页。

它**不做业务**：不碰数据库、不调引擎、不管调度。它只做三件事：

1. 把 `app` 组装好的状态渲染成前端能直接吃的 JSON；
2. 把事件流实时推给浏览器；
3. 把用户的操作转成调用，交给注入进来的接口。

所有依赖都是窄接口（见下文 `Options`），所以本包不 import
`internal/store`、`internal/orchestrator`、`internal/adapters` 里任何一个具体实现。

---

## 一、安全模型

服务只监听回环地址，但"只监听本机"不等于安全：任何本机进程、任何网页都能往
`127.0.0.1:端口` 发请求。所以四道闸门一起用。

### 1. 只绑定回环

`New` 拒绝一切非回环的 `Addr`（`0.0.0.0`、`:`、局域网 IP、域名、缺端口、端口越界全部报错）。
`net.Listen` 之后还会**再校验一次实际绑定的地址**，防止 `Addr` 通过某种解析绕过。

默认 `127.0.0.1:0` 让系统分配端口，`Server.Addr()` 返回真实端口，由 `app` 打印给用户。

### 2. 访问凭证

凭证由 `app` 用 `crypto/rand` 生成 32 字节十六进制串，写进 `<data_dir>/web-token`（权限 0600），
通过 `Options.Credential` 交给本包。**本包只校验，不生成、不落盘、不打印。**

三种携带方式，按优先级：

| 方式 | 用途 |
| --- | --- |
| Cookie `mesh_token` | 正常使用期间；`HttpOnly` + `SameSite=Strict` |
| 请求头 `X-Mesh-Token` | 脚本/调试 |
| 查询参数 `?t=` | 仅首次打开 |

首次访问 `GET /?t=<凭证>` 时，服务下发会话 cookie 并 **302 跳到去掉 `t` 的干净 URL**，
避免凭证留在地址栏、浏览历史、Referer 里。跳转后的地址不含凭证。

服务端**不保存凭证原文**：cookie 里存的是 `SHA-256("mesh-web:cookie:" + 凭证)`，
CSRF 令牌是 `SHA-256("mesh-web:csrf:" + 凭证)`。比对一律走 `crypto/subtle` 常数时间比较。

凭证不出现在：日志、SSE 流、`/api/state` 响应、错误信息里。测试 `TestStateSnapshotShape`
和 `TestSSEInitialStateFrame` 会扫响应体确认这一点。

### 3. 中间件顺序

`chain()` 固定顺序，不能改：

```
Host 校验 → 凭证 → Origin 校验 → CSRF → 业务
```

- **Host 校验**（`hostAllowed`）：只接受回环主机名。挡 DNS rebinding —— 攻击者把自己的域名
  解析到 `127.0.0.1`，浏览器就会认为同源，从而绕过同源策略。
- **凭证**：见上。
- **Origin 校验**（`originAllowed`）：带 `Origin` 头的请求必须来自回环源。空 `Origin` 放行
  （同源 GET、非浏览器客户端不发 `Origin`）。
- **CSRF**：只对写方法（POST/PUT/PATCH/DELETE）要求 `X-Mesh-CSRF` 头与派生令牌一致。
  令牌由 `GET /api/state` 的 `session.csrf` 下发（`app.js` 在每次 POST 时回传）。

任何一步失败都**不进业务逻辑**，直接 401/403 + JSON `{"error": "..."}`。

### 4. 响应头

`setSecurityHeaders` 对**所有**响应（含错误响应）设置：

```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: no-referrer
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
                         img-src 'self' data:; connect-src 'self'; base-uri 'none';
                         form-action 'none'; frame-ancestors 'none'
```

CSP **不含 `unsafe-inline` / `unsafe-eval`**。这能成立是因为 `assets/` 里没有内联
`<style>`、没有 `style=` 属性、只有一个 `<script src>`，`app.js` 也不用 `eval`/`new Function`。
以后改前端时要守住这条，否则 CSP 会真的挡掉样式。

---

## 二、接口

只有前端实际用到的这些，没有多余的。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/` | `assets/index.html` |
| GET | `/assets/{index.html,app.js,style.css}` | 白名单内的静态资源 |
| GET | `/api/state` | 完整快照 |
| GET | `/api/events?cursor=N` | SSE 事件流 |
| GET | `/api/cost?dim=bot\|room\|day` | 成本报表 |
| POST | `/api/message` | 发消息 |
| POST | `/api/work/{id}/control` | 暂停/继续/停止/换人/审批/忽略 |

静态资源走 `embed.go` 的白名单（`assetWhitelist`），文件名不在表里一律 404，
所以不可能通过 `/assets/../../etc/passwd` 之类的路径读到别的东西。

### `/api/state` 的契约

顶层七个键，缺一个前端就静默不渲染（`app.js:214 applySnapshot` 直接读）：

```json
{
  "session":      {"csrf": "...", "roomId": "room-main"},
  "current":      {"workId": "...", "goal": "...", "state": "executing",
                   "executor": {"name": "A", "version": 2},
                   "reviewer": {"name": "B", "version": 3},
                   "rounds": 1, "maxRounds": 2,
                   "stopUnconfirmed": false, "stopDetail": ""},
  "bots":         [{"id": "...", "name": "...", "adapter": "...", "status": "idle",
                    "statusDetail": "", "workGoal": "", "caps": [...]}],
  "pending":      [{"id": "...", "kind": "approval", "title": "...", "detail": "...",
                    "options": ["允许一次"], "workId": "...", "approvalId": "..."}],
  "capabilities": [{"scope": "...", "label": "...", "detail": "..."}],
  "notices":      [{"text": "...", "level": "info"}],
  "messages":     [{"id": "...", "author": "...", "authorKind": "bot", "kind": "chat",
                    "text": "...", "createdAtMs": 1700000000000,
                    "roleVersion": 2, "version": 1,
                    "streaming": false, "unverified": false,
                    "operationState": "unknown", "unknownCost": true}]
}
```

`current` 为 `null` 表示当前没有进行中的委托，前端据此禁用控制按钮。

**五个列表字段永远不是 `null`，是 `[]`**（`nonNil*` 系列）。前端会对它们直接 `.length`
和 `for` 循环，`null` 会抛 `TypeError` 并让整个界面白屏。测试
`TestStateEmptySlicesNotNull` 守着这条。

### `/api/cost` 的契约

前端 `renderCost`（`app.js:675`）要求全部这些字段存在：

```json
{
  "unknownAmountCalls": 3,
  "unknownReason": "该通道不提供计费信息",
  "byCurrency": [{"currency": "CNY", "amountMicros": 1250000}],
  "calls": 10, "callsWithTokens": 7,
  "tokensIn": 1000, "tokensOut": 200, "cacheReadTokens": 0,
  "reservedCalls": 0,
  "rows":   [{"label": "A", "key": "bot-a", "calls": 10, "unknownAmountCalls": 3,
              "unknownReason": "...", "note": "", "callsWithTokens": 7,
              "tokensIn": 1000, "tokensOut": 200, "byCurrency": [...]}],
  "limits": [{"label": "每日", "limit": "", "note": "客户端不提供"}]
}
```

`dim` 只接受 `""`（默认按 bot）、`bot`、`room`、`day`，其他值 400。

### `/api/work/{id}/control`

请求体 `{"action": "...", "executor": "", "approvalId": "", "option": "", "pendingId": ""}`，
`action` 只能是 `pause` / `resume` / `stop` / `replace` / `approval` / `dismiss_pending`。

响应 `{"note": "..."}`，**`note` 由前端原样显示**（`app.js:944`）。这是有意的：
动作没真正生效时（比如客户端没回应停止请求），`note` 必须说实话，而不是让前端显示"已停止"。

`work id` 从 `r.URL.EscapedPath()` 解析并 `url.PathUnescape` 还原，因为前端会
`encodeURIComponent`。**不能用 `r.URL.Path`**：`ServeMux` 已经把 `%2F` 解码成 `/`，
用解码后的路径取 id 会把含斜杠的 id 切断。

---

## 三、SSE 为什么不用 WebSocket

需求只要求单向推送（服务端 → 浏览器），浏览器 → 服务端的操作都是普通 POST。
在这个前提下 SSE 比 WebSocket 更合适：

- 纯标准库。WebSocket 需要 `golang.org/x/net` 或手写帧解析，项目红线是不引第三方依赖。
- 走普通 HTTP，不需要协议升级，不需要处理 `Upgrade` 握手失败的各种边界。
- 浏览器原生 `EventSource` 自带断线重连，`app.js` 只需要处理 `error` 事件退避重连。
- 单向流的心智负担小：不存在"连接半死"的双向状态不一致。

代价是单向、且只能发文本 —— 但这两点在本场景都不是限制。

### 三条硬规则

**1. 慢消费者只能被丢弃，绝不能阻塞广播。**

每个连接有自己的带缓冲通道（`sseBuffer = 64`）。`hub.broadcast` 用非阻塞发送；
缓冲满了说明这个客户端跟不上，直接把它从订阅表里摘掉并关闭通道，让它的 handler
`select` 醒来退出。广播本身永远不阻塞，所以一个卡死的浏览器不会拖慢其他浏览器。

**2. 必须保活。**

中间的代理、防火墙、NAT 会掐掉长时间没数据的连接。每 20 秒发两样东西：
一行 `: keepalive` 注释（SSE 规范里注释即心跳）和一个具名 `ping` 事件
（前端收到后刷新"实时"状态）。两者都要，因为注释对某些中间层不透明，而
`ping` 事件让前端能确认链路真的活着。

**3. `Shutdown` 必须主动关掉所有 SSE 连接。**

SSE 是长连接，`http.Server.Shutdown` 会一直等它们自然结束，等于永远不返回。
所以 `Shutdown` 先 `hub.closeAll()` 把所有连接掐断，再 `baseCancel()` 停掉后台订阅
协程，最后才 `httpServer.Shutdown`。测试 `TestSSEShutdownClosesConnections` 守着这条。

### 补读与去重

`?cursor=N` 表示"我最后看到的是 N"，服务端补发 `seq > N` 的事件。

- 先订阅，再补读。顺序不能反：反过来的话，补读期间产生的事件会永久丢失。
  代价是补读与实时流可能重叠，靠去重解决。
- 去重键是 `(RunID, Seq)`，同一个键只发一次，且要求 `seq` 严格递增（`acceptEvent`）。
  `seq <= 0` 的事件直接丢弃：没有 seq 就无法去重、也无法推进游标。
- 补读分批，每批 `replayLimit = 2000` 条，最多 8 批，防止一次重连把几万条历史全灌进浏览器。
- 补读结束后推一帧完整 `state`，让前端在追平历史后与服务器对齐。
- 回给前端的游标取所有 seq 的最大值：前端只有一个 `cursor` 变量（`app.js:141`），
  所以游标必须全局单调，不能按 Run 分开。

### 写超时

客户端假死（TCP 零窗口，既不读也不关）时，写会永久阻塞。这时广播侧的"摘掉慢消费者"
救不了它 —— 那个 goroutine 不在 `select` 上等，于是连接、订阅者和 goroutine 一起泄漏。
`sseWriter.refreshDeadline` 给每次写设 `sseWriteTimeout = 10s` 超时，每帧刷新，
健康的长连接不受影响。

---

## 四、三种"未知"分别在哪渲染

需求里反复强调的三类"不知道"，在界面上必须显示成"未知"，**绝不能显示成 0 或"已完成"**。
它们的渲染位置分别是：

### ① 费用未知

- 数据来源：`app` 侧把 `usage.Known == false` 的调用单独计数，填进
  `CostReport.UnknownAmountCalls` / `UnknownReason`，**不并入已知总额**。
- 服务端：`server.go handleCost` 保证 `UnknownReason` 非空（缺省填"该通道不提供计费信息"）。
- 渲染：`assets/app.js` 的 `renderCost`（约 675 行）把未知调用数单独显示成
  `另有 N 次调用金额未知（原因），实际花费可能高于下方金额。`；
  `fmtAmount` / `fmtMoneyList`（约 53 行）对非数字、非有限、负数一律返回 `未知`；
  跨币种只并列不求和（`fmtMoneyList` 用 `' + '` 连接）。
- 单条消息上的未知：`MessageView.UnknownCost` → 消息脚注"本次费用未知"（`appendMessage`）。

**为什么重要**：`docs/COORDINATION.md` §8④ 记录了硬矛盾 —— §9.6 要求成本硬上限，
但主通道不返回计费信息，硬上限在数学上不可能生效。所以界面必须显示真相
（"可能更高"），不能显示一个假的硬上限。

### ② 停止未确认

- 数据来源：`WorkView.StopUnconfirmed` / `StopDetail`。
- 渲染：`renderWork` 调 `renderStopFlag`（`app.js:293`），
  显示 `停止待确认`（有 `stopDetail` 时追加 `：<detail>`）。
- 另外 `POST /api/work/{id}/control` 的 `note` 会被原样显示（`sendControl`，约 916 行），
  所以"停止请求已发出但客户端没回应"必须由 `app` 在 `note` 里如实写明，
  前端不会替它撒谎。

### ③ 能力受限

- 数据来源：`CapView{Scope, Label, Detail}`，来自 probe 的能力探测结果。
- 渲染：`renderCapabilities`（`app.js:479`）填 `#cap-list`，并把 `#flag-cap` 显示成
  `能力受限 N 项`；无受限项时隐藏该标记并显示 `未发现受限能力`。
- 单个 bot 的受限能力：`BotView.Caps` → `buildBotRow`（`app.js:429`）在 bot 行下
  列出受限项；`renderRoleOptions` 还会在受限 bot 的下拉项后缀加 `（能力受限）`。

---

## 五、依赖注入接口

```go
type Options struct {
    Credential string        // app 生成，本包只校验
    Addr       string        // 只允许回环；默认 127.0.0.1:0
    State      StateSource   // Snapshot(ctx) (Snapshot, error)
    Events     EventStream   // Replay(ctx, cursor, limit) / Subscribe(ctx)
    Cost       CostSource    // Cost(ctx, dim) (CostReport, error)
    Control    ControlSink   // Control(ctx, workID, req) (note string, err error)
    Message    MessageSink   // SendMessage(ctx, req) (MessageView, error)
}
```

五个依赖**都必填**，缺一个 `New` 直接报错（`ErrMissingDependency`），
不做静默降级 —— 少一个数据源的界面会显示错误的状态，比不启动更糟。

错误处理分两类：

- `NewPublicError(...)` 包出来的错误，文案会原样进响应体（用户该看到的提示）。
- 其他错误只写 `stderr`，响应体是通用文案。内部细节（SQL、路径、凭证）绝不出门。
  测试 `TestMessageInternalErrorHidesDetail` 守着这条。

---

## 六、已自动验证 / 需人工验证

`server_test.go` 用假数据源跑真实 `httptest` 服务器（走完整中间件链），
`go test ./internal/web/...` 全部通过（25 个用例，`-count=3` 无抖动）。

| 项目 | 状态 |
| --- | --- |
| 非回环 `Addr` 被拒绝；`Addr()` 返回真实端口 | 已自动验证 |
| 无凭证 / 错凭证 → 401，且错误体用 `error` 键 | 已自动验证 |
| `?t=` 首屏下发 HttpOnly+SameSite=Strict cookie、302 跳干净 URL、cookie 不存凭证原文 | 已自动验证 |
| 跨源 `Origin` → 403；回环 `Origin` 放行 | 已自动验证 |
| 非回环 `Host` → 403（DNS rebinding） | 已自动验证 |
| 写操作缺 `X-Mesh-CSRF` → 403 且不进业务逻辑 | 已自动验证 |
| 安全响应头齐全，CSP 不含 `unsafe-*` | 已自动验证 |
| 快照七个顶层键齐全、`session.csrf` 非空、五个列表非 `null` | 已自动验证 |
| 快照 / SSE 流中不出现访问凭证 | 已自动验证 |
| 成本未知调用单独计数、原因非空、跨币种不合并、非法 `dim` → 400 | 已自动验证 |
| 消息正文去空白、`clientMessageId`/`executor`/`reviewer` 透传、空正文 400、坏 JSON 400、错方法 405 | 已自动验证 |
| 内部错误细节不进响应体；`PublicError` 原样透出 | 已自动验证 |
| 控制动作六个取值全部透传；未知动作 400；空 work id 404；含斜杠 id 正确还原 | 已自动验证 |
| 静态资源 Content-Type 正确、白名单外 404、静态资源也要凭证 | 已自动验证 |
| SSE 补读 `cursor` 语义、补读后推 `state` 帧、event 帧字段齐全 | 已自动验证 |
| SSE 实时事件送达、非法 `cursor` 400 | 已自动验证 |
| `Shutdown` 能掐断 SSE 长连接 | 已自动验证 |
| 慢消费者被摘除且广播不阻塞；`(RunID, Seq)` 去重与 seq 回退丢弃 | 已自动验证 |
| **真实浏览器里页面能正常渲染、SSE 自动重连、点击控制按钮** | **需人工验证** |
| **真实客户端下的审批弹窗、停止确认文案** | **需人工验证** |
| **长时间（>1 小时）挂着 SSE 的保活与内存稳定** | **需人工验证** |
| **真实数据的成本数字与客户端账单对得上** | **需人工验证** |

`go test -race` 在本机不可用（`-race` 需要 cgo，而项目红线是禁用 cgo），
所以并发正确性靠设计约束（非阻塞广播、`sync.Once` 关闭、锁外关通道）和重复运行保证，
**不是**靠竞态检测器证明的。

---

## 七、文件

| 文件 | 内容 |
| --- | --- |
| `server.go` | 构造、路由、中间件、五个 API handler、静态资源 |
| `sse.go` | 订阅中心（`sseHub`）、`handleEvents`、补读、帧写出 |
| `embed.go` | 静态资源白名单与 `go:embed` |
| `server_test.go` | 上表的自动验证用例 |
| `assets/` | 前端（`index.html` / `app.js` / `style.css`），不属于本包职责 |

改前端时注意：`app.js` 是接口契约的**唯一**权威。字段名、`note` 原样显示、
未知值显示"未知"这些约定都在前端，改后端前先读 `app.js` 对应函数。
