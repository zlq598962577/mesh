# mesh — 单聊天框多 Agent 协作平台（P0）

mesh 是一个**单聊天框多 Agent 协作平台**：驱动本机已装的 Windows 桌面客户端
（Codex / DSH / Trae CN / 豆包）互相协作——用户在一个聊天框里指定谁做谁审，
平台负责唤醒路由、投递、Work 状态机、审查绑定产物版本、循环护栏与外部副作用的
诚实呈现。**不做云、不做多租户**：只监听本机回环，浏览器是唯一入口。

权威方案：`F:\AIWORKSPACE\agent_harness\多Agent协作平台技术方案_v4.md`
（§5 交互协议、§9 权限与生命周期、§10 验收）。实现与规格冲突时按方案原文实现，
矛盾如实记录在各包 README（例如 `internal/orchestrator/README.md` 的"方案内部矛盾"一节）。

---

## 1. 快速开始

**前置**：Windows，且本机已安装 Codex / DSH / Trae / 豆包 **任一**桌面客户端。

**构建**（已在本机 Go 1.27.4 / windows/amd64 真实构建通过）：

```powershell
cd F:\AIWORKSPACE\agent_harness\mesh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o mesh.exe ./cmd/mesh
```

**首次运行**：

```powershell
.\mesh.exe                          # 默认 -data-dir F:\AIWORKSPACE\mesh-data
.\mesh.exe -data-dir <目录>          # 自定义数据目录
```

**打开控制台**：启动后打印形如
`mesh 已启动：用浏览器打开 http://127.0.0.1:<端口>?t=<见 <data_dir>\credential> …` 的地址，
用浏览器打开 `http://127.0.0.1:PORT/?t=<凭据>`。凭据是 32 字节 hex，写在
`<data_dir>\credential`（0600），重启不变，浏览器里的 URL 持续有效。

诚实预期：**P0 以"零 bot"状态启动**——真实适配器注入属适配器阶段接线，界面会如实显示
"适配器未接入"的能力受限项，不会伪装出可用的 agent。完整启动参数、目录布局与退出行为见
[internal/app/README.md](internal/app/README.md)。

## 2. 包结构一览

每个包的职责、已验证声明与已知限制都写在自己的 README 里，此处只给一句话与入口：

| 目录 | 一句话职责 | 详细文档 |
| --- | --- | --- |
| `internal/contract` | 唯一跨模块类型层（Engine 八方法契约、Message / Work / Review / Event），**契约冻结**，全仓只读 | — |
| `internal/orchestrator` | 编排层：唤醒路由、Work 状态机、分工版本、四道循环护栏、四个协作工具、外部副作用五态 | [README](internal/orchestrator/README.md) |
| `internal/store` | SQLite 存储层：版本化迁移、消息+投递同事务、事件游标补读、用量记账（未知 ≠ 0）、单实例锁 | [README](internal/store/README.md) |
| `internal/web` | 本机回环 Web UI 服务端：Host/凭证/Origin/CSRF 四道闸、SSE、五个窄接口（不 import 业务层） | [README](internal/web/README.md) |
| `internal/desktop` | 桌面操作原语：全局桌面操作锁（段落式持有）、焦点管理、文本注入、三阶段发送协议 | [README](internal/desktop/README.md) |
| `internal/adapters` | 四个桌面客户端适配器（Codex / DSH / Trae CN / 豆包），统一到 `contract.Engine` 八方法后面 | [README](internal/adapters/README.md) |
| `internal/registry` | 四层 agent 注册表：内嵌 catalog + `<data_dir>/agents` 目录 + Windows 探测，"新增 agent = 丢一个文件" | [README](internal/registry/README.md) |
| `internal/probe` | 客户端可行性探测：只读测出四个客户端的可控程度，如实报告 verified / unverified / unavailable | [README](internal/probe/README.md) |
| `internal/app` | 组装层：参数 → 目录 → 存储 → 注册表 → 引擎分发 → 凭据 → web 桥接，加进程生命周期与 P0 集成验收测试 | [README](internal/app/README.md) |
| `cmd/mesh` | 进程入口：解析参数 → Build → Lifecycle（Ctrl+C 优雅退出） | — |

## 3. 构建与验证

```powershell
cd F:\AIWORKSPACE\agent_harness\mesh
go build ./...
go vet ./...
go test ./...
```

当前真实状态（2026-11-14，go1.27.4 windows/amd64）：

- `go build ./...` 与 `go test ./...` **通过（全绿）**；`go vet ./...` 在既有代码上有
  4 处 `unsafe.Pointer` 用法告警（`internal/probe/uia.go` ×3、`internal/desktop/win32.go` ×1，
  非本批改动，如实记录）。
- 有单元测试的包：`desktop` / `orchestrator` / `registry` / `store` / `web` / `app`
  （app 含 18 个 P0 集成验收用例，对照表见 [internal/app/README.md](internal/app/README.md) 第 6 节）。
- `internal/adapters` 的四个子包**编译通过但无单元测试**；`contract` / `probe` / `cmd` 亦无测试文件。
- **真实客户端端到端从未跑过**：全部自动化验收基于可编程 fake 引擎 + 真实 web 中间件链；
  锁屏/焦点/DPI、真实客户端会话恢复、真实审批桥接、真实成本对账**必须人工验证**，
  清单见 [internal/app/README.md](internal/app/README.md) 第 6 节。因此不宣称"生产就绪"。

## 4. 设计红线（摘要）

完整论证分散在各包 README，此处是全仓适用的五条硬线：

1. **客户端退出码不可信，成功必须绑定 `SuccessSignal`**。实测 DSH 模型调用失败时进程仍
   退出 0；成功与否只由 `Final.Success + SuccessSignal` 判定，拿不到可靠信号就标
   `Unverified`，绝不报成功。
2. **未知绝不写 0**。token / 金额 / 操作结果未知就写 NULL 并显示"未知"
   （`Usage.Known=false`、`OpUnknown`、`unknownAmountCalls` 单独计数），未知折成 0
   会把假数字一路流到用户面前。
3. **绝不把用户客户端纳入 kill-on-close**。退出只释放平台自己持有的连接、句柄与辅助
   进程，用户自己的客户端窗口与进程永不触碰；取消也绝不靠杀进程。
4. **绝不读客户端内部数据库或提取 token**。登录态只判凭据文件存在性，不读内容、不上报
   账号标识；探测层全部只读（不点击、不注入、不重启客户端、不抢焦点）。
5. **临时文件绝不落 C 盘**。temp_dir 固定 `F:\AIWORKSPACE\temp\mesh`，解析到系统盘直接
   拒绝启动；平台子进程的 `TEMP/TMP/TMPDIR` 一律改写到该目录。

## 5. 权威规格与协作规则

- **技术方案 v4**：`F:\AIWORKSPACE\agent_harness\多Agent协作平台技术方案_v4.md` 为权威规格。
  未标为验证结果的内容均须在实现中验收；各包 README 里凡"已验证"声明都对应真实跑过的
  命令，凡"待验证 / 需人工"都不得当成已通过引用。
- P0 集成验收对照表（含自动化边界与人工验证清单）：
  [internal/app/README.md](internal/app/README.md) 第 6 节。
- 协作与协调记录：`docs/COORDINATION.md`。
