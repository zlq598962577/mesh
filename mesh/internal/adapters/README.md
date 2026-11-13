# internal/adapters — 四个 Windows 桌面客户端适配器

本包把四个桌面客户端（Codex / DSH / Trae CN / 豆包）统一到
`mesh/internal/contract.Engine` 八方法契约后面。平台其余部分只认这个接口，
不认具体客户端。

**诚实性声明**：本文档里的每一个"能 / 不能 / 待验证"都有来源标注——
**[P0a 实测]** 表示探测包 `mesh/internal/probe` 在本机真机测过；
**[待验证]** 表示代码路径已实现但端到端没验证过，或结论是同族推断。
禁止把"待验证"改写成"已实现"。

---

## 1. 包结构

```
internal/adapters/
├── common.go           公共层：事件归一化、完成判定、能力声明、错误分类、
│                       DesktopEngine（四客户端共用的 contract.Engine 实现）
├── locate_windows.go   定位层：manifest → 探测 → 主窗口定位（HWND+PID）
├── codex/engine.go     身份 + 能力声明 + 实测差异注释
├── dsh/engine.go       同上
├── trae/engine.go      同上
└── doubao/engine.go    同上
```

四个子包**只承载差异**（适配器名、能力布尔值、该客户端的实测陷阱注释、
构造函数）。所有编排逻辑在 `DesktopEngine` 里只有一份：

| contract.Engine 方法 | 实现位置 | 说明 |
|---|---|---|
| `Open` | `DesktopEngine.Open` | 校验 `engine.adapter` 与 `session_mode=dedicated` → 探测定位主窗口 → 建专用会话绑定（HWND+PID）→ `session` 事件 → 注册表 `Attach` |
| `Send` | `DesktopEngine.Send` | desktop 三段协议（见 §3） |
| `Events` | `BaseEngine.Events` | 返回会话事件通道；输出观察见 §4 |
| `Cancel` | `DesktopEngine.Cancel` | `desktop.PressEscape`，两态结论（见 §5） |
| `ResolveApproval` | `BaseEngine.ResolveApproval` | 不代答（§9.3），返回错误引导用户回客户端 |
| `Suspend` | `BaseEngine.Suspend` | 只标记状态 |
| `Close` | `DesktopEngine.Close` | 发 final（Unverified）→ 注册表 `Detach` → 只释放平台侧簿记，**绝不动客户端窗口/进程** |
| `Health` | `DesktopEngine.Health` | 探测可达性，错误信息分清探测侧 / 客户端侧 |

新增第五个客户端 = 复制任一子包改三个标识符 + 丢一份 manifest，不需要碰
公共层。

## 2. 能力对照表

图例：✅ 能（且实现/实测） · ❌ 不能（实测确认做不到） · ❓ 待验证 ·
标注 **[P0a]** 的是真机实测结论，标注 **[假设]** 的是同族推断。

| 能力 | Codex | DSH | Trae CN | 豆包 |
|---|---|---|---|---|
| 存在性探测（进程/路径） | ✅ [P0a] | ✅ [P0a] | ✅ [P0a] | ✅ [P0a] |
| 版本号来源 | ✅ [P0a] MSIX 包目录名（如 26.1106.6628.0），无卸载登记项可查 | ✅ [P0a] exe ProductVersion 0.2.2.0（FileVersion 0.2.2-rc.1 是构建号，不回落） | ✅ [P0a] exe ProductVersion 3.4.18（FileVersion 是内嵌 Electron 版本） | ✅ [P0a] **启动器** `E:\Doubao\Doubao.exe` 的 2.36.8（主程序带的是 Chromium 147.x） |
| 主窗口定位 | ✅ [P0a] 进程 + 窗口标题 | ✅ [P0a] 进程 + 窗口标题 | ✅ [P0a] **仅进程 + exe 路径**：无任何标题含 "Trae"（主窗口=项目名，面板="Agent"） | ✅ [P0a] 主程序 `app\Doubao.exe` 的进程 |
| 文本发送（Send） | ❓ 已实现（剪贴板注入，P0 主路径），端到端待真机验证 | ❓ 同左 | ❓ 同左 | ❓ 同左 |
| 发送核实（VerifyText） | ❌ [P0a] UIA 控件枚举不可达 → 恒 Unknown，禁止重发 | ❌ [P0a] 同左 | ❓ [假设] 同族推断 UIA 同样不可达 | ❌ [P0a] 同左 |
| 流式输出（StreamOutput） | ❌ [P0a] | ❌ [P0a] GUI 无 stdout 事件流 | ❌（无替代通道） | ❌ [P0a] |
| 输出读取（读消息列表） | ❓ 整条路径待真机验证，当前不可用（§4） | ❓ 同左 | ❓ 同左 | ❓ 同左 |
| 完成判定 | ❌ `unknown_only`：无可靠完成信号 [P0a] | ❌ 同左（模型调用失败仍 exit 0，退出码不可信 [P0a]） | ❌ 同左（编辑器常驻不退出） | ❌ 同左 |
| 取消（Cancel 发出 Esc） | ❓ 已实现，端到端待真机验证 | ❓ 同左 | ❓ 同左 | ❓ 同左 |
| 取消核实（CancelVerified） | ❌ 只能报"停止待确认 / 未发送"两态，没有"已停止" | ❌ 同左 | ❌ 同左 | ❌ 同左 |
| 审批桥接（ApprovalBridge） | ❌ 审批弹窗无可定位控件，语义与目标不可验证 → 不代答（§9.3） | ❌ 同左 | ❌ 同左 | ❌ 同左 |
| 文件传输 | ❌ 未实现 | ❌ 同左 | ❌ 同左 | ❌ 同左 |
| 原生会话 ID / 会话恢复 | ❌ `resume.mode=none` → `NativeID` 留空、`Recovered=false`（如实） | ❌ 同左 | ❌ 同左 | ❌ 同左 |
| 只读复核（ReadOnlyReview） | ❌ 客户端无只读验证通道，不向审查者承诺只读会话内容 | ❌ 同左 | ❌ 同左 | ❌ 同左 |
| 健康检查（HealthCheck） | ✅ 已实现（探测可达性，区分探测侧/客户端侧） | ✅ 同左 | ✅ 同左 | ✅ 同左 |

**本机实测前提**（P0a）：四个客户端的主窗口类都是 `Chrome_WidgetWin_1`，
UIA 深度 6 走查只拿到 Window/Pane/TitleBar，`Chrome_RenderWidgetHostHWND`
之下没有可读控件，且 `SPI_GETSCREENREADER=false`。这是上表所有 ❌ 的共同
根源。

## 3. Send：三段协议与"绝不重发"

发送走 `desktop` 包的三段协议（Prepare 聚焦校验 → Submit 注入 → Verify 核实），
锁按**段落式持有**：只在聚焦+注入+核实的瞬间持全局桌面锁，等待生成完全在锁外。

三条从结构上锁死的纪律：

1. **注入走剪贴板**：`desktop.Target` 不带 `TextTarget`，`desktop.Inject`
   直接落剪贴板路径。控件直设（UIA ValuePattern）本机实测不可达，适配器
   也不许自己实现 UIA——所以不存在"先试一下直设"的姿态性代码。剪贴板注入
   会**借用并完整归还**用户剪贴板（`InjectViaClipboard` 内置备份/恢复）。
2. **发送器构造为 `NewSenderNoResend`**：重发能力在构造上不存在。桌面客户端
   没有幂等键，`Unknown` 时重发可能把同一句话提交两次（客户端生成两份回答，
   或两段文本被拼接成语义错乱的一段）。
3. **verify 不通过必须落 Unknown，绝不重发**：`HonestVerifier.VerifyText`
   恒返回 `desktop.Unknown`（不返回 error——Unknown 是合法的诚实答案）。
   Send 的返回值语义：
   - `Verified` → `(turn, nil)`，随后事件流里发 progress 说明"完成状态未知"；
   - `Unknown` → `(turn, err)`：消息可能已在客户端里跑起来，turn 非 nil；
     err 明确告诉上层**禁止重发**；
   - `NotVerified` → `(nil, err)`：确定没送达（例如聚焦预检 fail-closed），
     上层可安全重试。

**禁止的启发式**（§4.2.2 明令，任何维护者不得以"提高体验"为由加回）：
"N 秒无新字符 = 生成完成"。模型的思考停顿与生成结束在屏幕上不可区分，
该启发式会把"未知"伪装成"成功"。本包没有、也不允许出现任何静默期计时代码。

## 4. Events 与输出读取：为什么整条标"待验证"

`contract.Event` 六类事件在桌面通道的实际含义：

| Kind | 何时发出 |
|---|---|
| `session` | Open 成功：`SessionInfo{Adapter, ClientPID, WindowTitle, NativeID:"", Recovered:false}` |
| `progress` | Send 核实通过后的状态说明（"完成状态未知"）；读取到文本但无完成信号时的说明 |
| `output` | 仅当未来接入真实 `OutputReader` 且读到文本时 |
| `approval` | 当前不会发出：审批不可桥接，用户直接回客户端处理 |
| `final` | 仅在 `Close` 时发出（`Unverified=true`：会话关闭 ≠ 生成结束/成功） |
| `error` | 输出不可读的状态说明（每会话一次）、发送/取消失败的原因 |

输出读取的接入点是 `adapters.OutputReader` 接口（`common.go`）。当前没有
任何实现，原因有二，均为本机事实：

1. **UIA 控件枚举不可达** [P0a]：probe 连资源管理器基线窗口都枚举不到
   后代控件，四个客户端的渲染子树全空，"读消息列表"无从谈起。
2. **desktop 包未导出剪贴板读取原语**：manifest 的 `output.mode=clipboard`
   需要"聚焦 → 全选/复制 → 读剪贴板"路径，而 `desktop` 包的
   `readClipboardText` 等符号未导出（未在代码里绕过它，遵守"适配器只调
   desktop 原语"的边界）。

因此 `Events` 在每轮 Send 核实通过后发出一条 error 事件说明上述原因与
用户动作（每会话只发一次，防刷屏），**不产出 final**——没有证据就不许下
任何结论，包括"失败"这个结论。

## 5. Cancel：两态诚实结论

`Esc` 经 `desktop.PressEscape` 发出（持锁 + 聚焦 + 按键前复核，焦点纪律
全部内建）。`keybd_event` 是 fire-and-forget，客户端是否真的停了无法核实：

- 发送失败 → error 事件 + `ErrFocus/ErrLock` 错误："未发送，请手动停止"
  （明确失败，可重试）；
- 发送成功 → error 事件 + `adapters.StopUnconfirmedError`（`ErrStopUnconfirmed`，
  `NeedsUserAttention=true`）："停止待确认"。

**两种情况都不会被标成"已停止"**：把"键发出去了"说成"停了"，用户会
放心走开，而客户端可能还在消耗额度。取消也绝不通过杀客户端实现（§4.2.2）。

## 6. 各客户端实测陷阱（适配器依赖的探测事实）

这些事实全部由内置 manifest（`internal/registry/manifests/*.json`）驱动，
适配器代码里没有客户端特有的路径/窗口类/版本逻辑：

- **Codex**：MSIX 安装，三个注册表 hive 无卸载登记项；版本只从包目录名
  解析（`version_from=msix_package`）；`F:\AIWORKSPACE\CodexChinese\` 在
  `exclude_paths`（其安装脚本会注册管理员计划任务），命中即整条判不可用。
- **DSH**：GUI 客户端不是 CLI——`dsh.cmd` 只是启动器，**不能沿用 CLI 的
  事件假设**（GUI 无 stdout 事件流）；探测/启动目标是 `DeepSeek Harness.exe`
  本体，Electron 的 `resources\app.asar(.unpacked)` 装配物不是探测目标。
- **Trae CN**：主程序 `F:\Trae CN\Trae CN.exe`；无任何标题含 "Trae" 的
  窗口，定位只靠进程 + exe 路径；同目录 `unins000.exe` 是 Inno Setup
  卸载器，绝不能被探测/启动选中（manifest 只写完整文件名，无通配）。
- **豆包**：版本读**启动器** `E:\Doubao\Doubao.exe`（2.36.8），窗口定位用
  **主程序** `E:\Doubao\app\Doubao.exe`（带的是 Chromium 147.x）；运行时
  约 8 个同名 `Doubao.exe` 进程，只能靠镜像路径区分。

## 7. 与 manifest / 注册表的关系

- manifest 的 `capabilities` 段是**声明**，子包 `caps` 是**实现**；两者
  不一致时以更保守的一方为准，并视为需要修的 bug。
- `Open` 成功后向注册表第 4 层 `Attach`（`registry.Instance`，含 ClientPID、
  `Recovered=false`），`Close` 时 `Detach`。reg 为 nil（单测场景）跳过记账。
- `Health` 复用 `registry.WindowsDetector` 探测，探测侧失败（进程枚举没做成）
  与客户端侧未运行严格分开上报——"没探测成"报成"没安装"会让用户去重装
  一个本来开着的客户端。

## 8. 已知阻碍与待办

| 项 | 状态 |
|---|---|
| UIA 控件枚举本机不可达（probe 基线也失败） | 环境事实，P0a 已确认；待真机复验后重评 |
| desktop 包缺剪贴板**读取**导出 | 阻碍输出路径实现；需 desktop 层新增导出（本包无权添加） |
| Send / Cancel 端到端真机验证 | 待做：probe 明确"不点击发送、不注入测试文本"，故只能标待验证 |
| Trae UIA 不可达是同族推断 | 四客户端中唯一未经本机实测确认的结论 |
| 事件信封 WorkID/RunID 为空 | `Open(BotSpec)` 无 Run 概念，由 orchestrator 落库时归位 |
