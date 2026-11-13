# mesh 模块协调说明

本文件由 Lead 维护，不属于任何单个模块的写作用域。各模块负责人**只读**，需要变更请通知 Lead。

## 1. 契约冻结规则

`mesh/internal/contract` 是唯一的跨模块类型层，Lead 已冻结。三条铁律：

1. 跨模块传递的类型必须定义在 contract 包，其它包不得自建同名结构体。
2. contract 只依赖 Go 标准库。
3. 字段的 json/yaml 名字是对外协议，改动等于改协议。

需要新增跨模块类型时，向 Lead 提出，由 Lead 统一加入 contract，不要各自在自己包里定义后再互相适配——那正是这个项目最容易烂掉的方式。

## 2. 写作用域划分（互不重叠）

| 任务 | 负责人 | 写作用域 |
|---|---|---|
| task-1 P0a 探测 | probe | `mesh/cmd/mesh-probe/`、`mesh/internal/probe/` |
| task-2 桌面原语 | desktop | `mesh/internal/desktop/` |
| task-3 存储层 | store | `mesh/internal/store/` |
| task-4 编排层 | orchestrator | `mesh/internal/orchestrator/` |
| task-5 UI 与本机服务 | web | `mesh/internal/web/` |
| task-6 四适配器+注册表 | desktop（第二阶段） | `mesh/internal/adapters/`、`mesh/internal/registry/` |
| task-7 组装与打包 | 待定 | `mesh/cmd/mesh/`、`mesh/internal/app/`、`mesh/README.md` |
| task-8 P1 浏览器 | web（第二阶段） | `mesh/internal/browser/` |

唯一例外：`mesh/go.mod` 由 Lead 维护。需要加依赖时向 Lead 报，由 Lead 统一改，避免多方同时编辑同一个文件。

## 3. 依赖关系

```
task-1 (probe)      ─────────────────────┐
task-2 (desktop)    ──► task-6 (adapters) ┤
task-3 (store)      ─────────────────────┼──► task-7 (app 组装)
task-4 (orchestr)   ─────────────────────┤
task-5 (web)        ──► task-8 (browser) ┘
```

- task-6 必须等 task-2：适配器只许调用 desktop 包暴露的原语，不许自己再造锁和发送逻辑。
- task-7 必须等 task-3/4/5：组装需要三层的真实接口。
- task-8 必须等 task-5，且属 P1，不要抢跑。

## 4. 层间边界（重要）

- 编排层不操作窗口、不写 SQL。只依赖 `contract.Engine` 和自己定义的 `orchestrator.Store` 小接口。
- web 层通过接口注入编排层，不 import 具体实现。
- adapters 层调用 desktop 原语，不自己实现锁、焦点、发送协议。
- store 层的 SQLite 驱动隔离在单独 driver 文件里，便于替换。

违反这些边界会导致包循环依赖，且本机无 Go 工具链无法及时发现——请务必自觉。

## 5. 环境事实（影响所有人的交付方式）

**本机没有 Go 工具链**：`go` / `git` / `node` 均不在 PATH，常见安装路径也没有。
**也无法联网下载**：`proxy.golang.org` 超时，`goproxy.cn` 与 `mirrors.aliyun.com` TLS 失败，`go.dev` 超时。

因此：
1. **任何代码都无法编译验证。** 不许在任何交付物里声称"已测试通过"、"编译通过"、"运行正常"。
2. 单元测试可以写，但必须注明"未运行"。
3. 第三方依赖（SQLite 驱动、chromedp）无法拉取，README 必须写明首次构建需联网执行 `go mod tidy` 并给出命令。
4. 方案 §10 的全局声明同样适用于代码交付：「本文是设计要求，未标为验证结果的内容均须在实现中验收。」代码里的未验证结论必须如实标注。

## 6. 硬性技术红线（所有人）

- 语言 Go，不许改回 Python。
- 不许 cgo。整个项目必须能 `CGO_ENABLED=0 go build` 通过。
- 不许引入 `golang.org/x/sys`；Win32/COM 用 `syscall.NewLazyDLL` + `unsafe` 手写。
- 不许留 TODO 占位。要么实现，要么在 README 写明"不实现及原因"。
- 凭据正文（token / key / password）绝不出现在日志、导出、错误信息、UI 任何位置。
- 临时文件不许放 C 盘，一律 `F:\AIWORKSPACE\temp`。
- 不把用户已有的四个客户端纳入 Job Object 的 kill-on-close 名单。

## 7. 提交前

代码每次 PR 之前需调用代码审查技能（`ponytail-review`）先完成一次审查。Lead 会在汇总阶段统一执行。

## 8. 已知的方案内部张力（供各模块负责人参考，不要自行"修正"方案）

这些是 Lead 读方案时识别出的、实现时必然撞上的矛盾点。遇到时**按方案原文实现，并在 README 里记录**，不要自作主张改设计：

1. **§4.2.2 全局桌面操作锁 vs 并发**：锁是全局单例，但 §5.6 允许最多 2 个活跃 Run。两个 Run 同时要操作桌面时必然排队，方案的"跨 Work 公平轮转"在锁层面没有对应机制。
2. **§4.2.2 专用会话 vs 用户自己也在用同一个客户端**：平台专用会话和用户手动操作同一个客户端会互相干扰，方案只说了"默认不干扰用户原有会话"，没有说用户切走了怎么办。
3. **§5.3 护栏 vs §4.2.2 客户端内部步骤不可见**：护栏只能约束平台可见的派发，客户端内部循环平台看不到，方案已承认这点，实现时不要假装能封顶。
4. **§9.6 成本硬顶 vs 桌面客户端无法计量 token**：四个客户端首版都不提供逐 Run token，所以"日-月硬顶"实际无法对主通道生效，只能靠唤醒次数/时间/轮次间接控制。UI 必须如实显示，不能显示一个假的硬顶。
5. **§4.2.1 "可先用两个接通验证闭环" vs "四者验收前只算阶段性原型"**：这两个说法并存，意味着 task-6 可以分批交付，但不得宣称四者已接入。
