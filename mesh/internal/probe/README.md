# P0a 探测模块（`mesh/internal/probe`）

本包只做一件事：**在真实机器上把四个桌面客户端的可控程度测出来，并如实报告**。
它不下结论说"能接入"，只把每项检查的实测状态写成 `verified` / `unverified` / `unavailable`。

> **最重要的诚实性声明**
>
> 代码已编译通过、逻辑已实现，但**四个真实客户端的完整探测尚未跑完**。
> 本机只做了零散的手工取证（见下文"已实测"各条），`mesh-probe` 的完整输出还没有生成。
> 因此本文件里凡标"待实测"的结论，**都必须由人工运行 `mesh-probe` 后填写**，
> 不能当成已验证结果引用（技术方案第 5 行："本文是设计要求，未标为验证结果的内容均须在实现中验收"）。

---

## 1. 怎么跑

Go 在本机的位置是 `D:\Program Files\Go\bin\go.exe`，不在默认 PATH 里。

```powershell
# 构建（必须 CGO_ENABLED=0，§11 明确把引入 cgo 列为风险）
cd F:\AIWORKSPACE\agent_harness\mesh
$env:CGO_ENABLED='0'
& 'D:\Program Files\Go\bin\go.exe' build -o ..\..\temp\mesh\mesh-probe.exe .\cmd\mesh-probe

# 跑全部四个客户端，人读表格
& 'F:\AIWORKSPACE\temp\mesh\mesh-probe.exe'

# 出 JSON（给后续 registry / 适配器开发当输入）
& 'F:\AIWORKSPACE\temp\mesh\mesh-probe.exe' -json > F:\AIWORKSPACE\temp\mesh\probe.json

# 只测一个（省时间）
& 'F:\AIWORKSPACE\temp\mesh\mesh-probe.exe' -client dsh
```

可选参数：`-client all|codex|dsh|trae|doubao`、`-json`、`-timeout 30s`（默认 60s）。

**退出码只表示"探测本身跑没跑完"，完全不表示客户端可用**：

| 退出码 | 含义 |
| --- | --- |
| `0` | 探测跑完且每项检查都给出了确定结论 |
| `1` | 探测跑完，但有检查失败或无法定论 |
| `2` | 参数错误 |

这一点必须强调，因为 §4.3 第 242 行明确要求 `successSignal` 必填、不能只靠 exit code。

---

## 2. 每项检查的判定方法

所有检查都是**只读**的：不点击、不注入、不重启客户端、不抢焦点。

| 检查项 | 判定方法 | 拿不到时怎么降级 |
| --- | --- | --- |
| 安装与版本 | 已知安装根目录（显式写死，不递归扫盘，§9.5）→ 按有序相对路径定位主程序 → 读 exe 版本资源；卸载登记项 `DisplayVersion` 优先于 exe 资源；MSIX 客户端从带版本的包目录名解析 | `state=unavailable` + `reason` 写"未在已知安装路径中找到该客户端（不递归扫盘，见 §9.5）"；版本取不到写 `"unknown"` 并注明 `version_source=none` |
| 控件可达性 | COM 起 `IUIAutomation`，按 HWND 取元素，`FindAll(Descendants)` 枚举 + RawView 遍历测树深；按角色关键词匹配输入框/发送按钮/消息列表 | 无法枚举时**不写"客户端没有控件"**，而是 `unavailable` + 注明这是探测侧能力问题（见第 5 节基线实验） |
| 会话定位 | 只用「进程归属 + exe 安装路径」定位客户端；再记录窗口标题、PID 作辅助信号 | `unverified` + 说明"四个客户端均未暴露原生会话 ID 查询接口" |
| 重启后重关联 | 检查可用的稳定信号（进程路径、固定标题白名单），并明确标注不可靠信号 | 恒为 `unverified`——**探测过程不重启用户客户端**，只能给出信号可用性判断 |
| 发送链路（注入/触发/完成/失败/停止） | 控件可达后才可能测；不可达时整条链路 `unavailable` | 见下 |
| 焦点与锁屏 | 只读测量：`IsIconic`、输入桌面能否打开、显示器数量、DPI、前后台窗口是否被抢 | 条件无法探测时 `unverified` 并写明原因；聚合结论一律不报"可用" |
| 依赖清单 | 当前进程完整性级别/是否提权、进程树里的 helper 进程、运行时依赖（.NET/WebView2/VC++） | `unverified`；**绝不自提权去验证**（§9.5） |

### `unknown` 的降级输出格式

三态取值固定为 `verified` / `unverified` / `unavailable`，登录态另有一套：

```json
{
  "state": "unavailable",
  "reason": "未在已知安装路径中找到该客户端（不递归扫盘，见 §9.5）"
}
```

```json
{
  "login": { "state": "unknown", "reason": "未找到已知凭据文件；不代表未登录" }
}
```

**登录态只允许三种输出**：`存在` / `不存在` / `unknown`。
`过期` 在常量里保留但探测**从不输出**——判断是否过期必须读凭据内容，那是 §9.4 的红线。
凭据文件只判存在性，`evidence` 只写文件名，绝不写内容或账号标识。

---

## 3. 为什么禁止"几秒没新字 = 完成"

§4.2.2 第 218 行原文：

> 客户端未暴露可靠完成信号时，不按"几秒没新字"宣告成功；标为待核实或未达到自动接入验收。
> 错误、限额、重新登录、弹窗和工具审批均是独立状态。

沉默**无法区分**以下五种情况，它们对上层是完全不同的处理：

1. 模型还在生成（只是恰好停顿）；
2. 已生成完（真正的完成）；
3. 正在等用户批准工具调用（`approval` 事件，不是 `final`）；
4. 报错了但错误提示还没渲染出来；
5. 触发了限额 / 需要重新登录 / 弹了模态框。

把 3/4/5 当成"完成"会直接导致平台把失败当成功交付，这是最难排查的一类错误。
所以 `sendcheck.go` 里把这条禁令写成 `forbiddenCompletionJudgements()` 的 6 条清单，
随 JSON 一起输出（字段 `completion.forbidden`），让下游适配器作者看到原始禁令而不是只看结论。

同理，`cancel`/停止也一样：**客户端未确认停止，就不报已停止**（§10.1 第 644 行）。

---

## 4. 为什么不能信退出码

本机实测：**DSH 在模型调用失败时进程仍然退出 0**。
只看退出码会把一次失败的对话判成成功，并可能触发重试逻辑重复发送。

所以：

- `FinalInfo.SuccessSignal` 是必填字段，必须写清"凭哪个信号判定成功"；
- 拿不到可靠信号时 `FinalInfo.Unverified = true`，不报成功；
- 探测程序自己的退出码也只描述探测过程（见第 1 节表格）。

---

## 5. 已实测的关键事实（本机取证，非 `mesh-probe` 输出）

这些是手工用 PowerShell 只读查出来的，用来校正探测逻辑；它们**不能替代**完整探测。

### 5.1 Trae 没有任何标题含 "Trae" 的窗口

- 主窗口标题是**当前打开的项目名**；
- 智能体面板窗口标题是 `"Agent"`。

**后果**：按窗口标题找 Trae 必然漏判。本包定位客户端一律用「进程归属 + exe 安装路径」，
`session.go` 的固定标题白名单里**已移除 `"Agent"`**（它是个子面板，不是应用标识）。

实测安装：`F:\Trae CN\Trae CN.exe`（214 MB，`ProductVersion=3.4.18`）。
同目录还有 `unins000.exe`（Inno Setup 卸载器），绝不能被当成主程序选中。
注册表：`TRAE CN (User)` / `3.4.18` / `F:\Trae CN\`。

### 5.2 豆包：版本在启动器里，主程序里是 Chromium 版本（与最初设想相反）

| 文件 | `ProductName` | `ProductVersion` |
| --- | --- | --- |
| `E:\Doubao\Doubao.exe` | `Doubao Launcher` | **2.36.8** |
| `E:\Doubao\app\Doubao.exe` | `Doubao` | 147.0.7727.149 |

- 真正在跑的 8 个进程全是 `app\Doubao.exe`（所以**它才是主程序**，用于进程归属和 UIA 操作）；
- 但 `147.0.7727.149` 是 **Chromium 版本**，不是豆包版本；
- 应用版本 `2.36.8` 在根目录启动器里，且与卸载登记项 `DisplayVersion=2.36.8` 完全一致；
- 注册表 `豆包` 项的 `InstallLocation` 是**空的**，所以版本能靠注册表补，安装根目录不能。

**处理方式**：`clientSpec` 里把「主程序」(`Rels`) 和「版本来源」(`VersionRels`) 拆成两份清单，
`InstallProbe` 新增 `version_file` 字段说明版本取自哪个文件，报告可复核。

### 5.3 Codex 没有卸载登记项

HKCU / HKLM / WOW6432Node 三个位置都没有 Codex 条目 → 它是 MSIX/商店安装。
版本只能从 `WindowsApps` 下带版本号的包目录名解析，且**升级后路径会变**，
所以适配器必须固定已支持版本范围（§11 第 667 行：升级后探测失败就暂停该连接，不盲点坐标）。

### 5.4 DSH

`F:\deepseek\DeepSeek Harness.exe`：`ProductVersion=0.2.2.0`、`FileVersion=0.2.2-rc.1`；
注册表 `DeepSeek Harness` 登记 `0.2.2-rc.1`、安装位置 `F:\deepseek`。

### 5.5 控件枚举在本机拿不到（探测侧问题，不是客户端问题）

决定性对照实验：四个客户端 `FindAll(Descendants)` 全为空，但

- 资源管理器 `CabinetWClass` 也只返回 **1** 个后代；
- 桌面 `RootElement` 只有 14 个子元素，其中 Button=0、Edit=0。

一个普通资源管理器窗口不可能只有 1 个后代元素 → 问题在探测侧。
所以 `controls.go` 强制做基线对照（`baselineClass = "CabinetWClass"`），
并把 `baseline_ok` / `baseline_descendants` 同时写进 JSON 和表格。
**基线不通过时，绝不输出"该客户端未暴露控件"。**

顺带纠正一个曾经的错误归因：早期把"UIA 拿不到控件"归因于进程完整性级别低，
但实测本进程是 **Medium**，该归因不成立，已在 `win32.go` 注释里记录。

---

## 6. 三态汇总判定标准

`report.go` 的 `Summarize()` 给出每个客户端一个总判定：

| 判定 | 条件 |
| --- | --- |
| **可用** | 六项全部 `verified` |
| **不可用** | 至少一项 `unavailable` |
| **未知** | 没有 `unavailable`，但存在 `unverified` |

优先级：不可用 > 未知 > 可用。

为什么 `unverified` 不能算可用：本机控件枚举拿不到（5.5），如果把 `unverified` 当可用，
报告会显示四个客户端全部"可用"，而这是**明显错误**的。宁可报"未知"也不给假绿灯。

按当前实测状态，四个客户端的预期判定是 **未知或不可用**，不可能是"可用"。
这正是 P0a 要暴露的结论：**可靠入口是否缺失，必须明确记录阻碍**（§10）。

---

## 7. `go vet` 剩余告警为什么保留

`CGO_ENABLED=0 go build` 全绿；`go vet` 剩余 **3 条**，全部在 `uia.go`：

| 位置 | 告警 | 为什么保留 |
| --- | --- | --- |
| `uia.go:139` | `possible misuse of unsafe.Pointer` | `obj` 是 COM 接口指针，由 `ole32`/`uiautomationcore` 分配的原生内存，不在 Go 堆、不被 GC 移动。解引用取 vtable 首地址是 COM ABI 的必需操作 |
| `uia.go:144` | 同上 | `vtbl` 是接口方法表首地址，按索引偏移取函数地址。COM 的 vtable 调用本质上必须做整数偏移寻址，没有既满足 vet 又满足 ABI 的写法 |
| `uia.go:174` | 同上 | `b` 是 `SysAllocString` 分配的原生 BSTR 数据区首地址，不在 Go 堆 |

这三处属于 Go 官方文档承认的 **syscall 例外**：指针来自系统调用返回的原生地址，
而不是 Go 指针被降级成整数。vet 无法区分这两种情况，因此是误报。
每处都写了注释说明属于哪一类，避免后来者当成"可清理的坏味道"删掉——
**vtable 索引写错不是报错，是直接崩掉客户端进程。**

其余曾经告警的 5 处已按"Go 堆派生地址"正确处理（不是注释豁免）：

- `version.go` 的 `VerQueryValue` 回填指针、`win32.go` 的 `GetSidSubAuthority*` 回填指针，
  都指向调用方传入的 Go `[]byte` 缓冲区内部，属于真实的 GC 生命期问题。
  统一改为 `ptrInBuf()`：先算偏移，再用 `unsafe.Add` 从 `&buf[0]` 重新派生指针，
  使指针来源全程可被类型系统追踪，同时消除告警。这是**真修**，不是豁免。

---

## 8. 已知阻碍与未做事项

- **四个客户端的完整探测未跑**（第 0 节的声明）。需要人工运行 `mesh-probe` 并把结果补进本文件。
- **控件枚举在本机不可用**（5.5），根因未定位到具体是 COM 初始化、UIA 版本还是权限问题。
  在修好之前，发送链路（注入/触发/完成/失败/停止）全部只能报 `unavailable`。
- **重启后重关联无法实测**：探测过程不重启用户客户端。只能给出信号可用性分析，结论恒为 `unverified`。
- **锁屏/最小化/多显示器/DPI 的运行时行为未实测**：只测量了当前环境状态，没有在真锁屏下验证。
  §4.2.2 第 221 行要求这些场景分别测试，属于待补。
- **未做管理员权限验证**：§9.5 要求绝不以管理员身份启动平台，所以没有提权去测"客户端是否需要管理员"。
- **`go build ./...` 在本模块需要 `modernc.org/sqlite`**，而本次探测环境沙箱内 GOPATH 只读，
  只能验证 `./internal/probe/... ./cmd/mesh-probe/...`。probe 包本身零依赖（纯 stdlib），
  不受影响；整个模块的构建由 Lead 侧验证。

## 9. 排除项

`F:\AIWORKSPACE\CodexChinese\` **必须排除**：该目录的安装器会注册管理员计划任务。
`version.go` 的 `isExcluded()` 在 `globExisting` / `pickRel` / `describeInstallRoot` /
`versionFromRels` 四处都做了大小写不敏感过滤。

注意：`detect.exclude` 只是**过滤规则，不是安全边界**（§9.5）。
真正的保障是"只用显式写死的已知路径、绝不递归扫盘、绝不在磁盘上找到什么就执行什么"。

## 10. 文件分工

| 文件 | 职责 |
| --- | --- |
| `types.go` | 全部探测结果的数据结构与三态常量 |
| `probe.go` | 总调度：宿主 → 进程表 → 基线 → 安装 → 控件 → 会话 → 发送 → 焦点 → 依赖 |
| `host.go` | 宿主环境（OS build、完整性级别、会话、显示器、DPI、.NET 运行时、临时目录） |
| `win32.go` | Win32 只读封装（进程路径、窗口枚举、令牌、显示器、DPI） |
| `process.go` | Toolhelp 进程快照与按安装目录归属 |
| `version.go` | 已知路径定位、exe 版本资源、`ptrInBuf` |
| `registry.go` | 卸载登记项（三个 hive 都看） |
| `detect.go` | 每个客户端的静态配置与安装/版本装配 |
| `uia.go` | 手写 COM vtable 的 UI Automation 最小封装 |
| `controls.go` | 控件枚举、基线对照、角色匹配 |
| `session.go` | 会话定位与重关联信号分析 |
| `sendcheck.go` | 发送链路五项检查（含禁止判据清单） |
| `focus.go` | 最小化/锁屏/多显示器/DPI/焦点抢占 |
| `deps.go` | 依赖清单（管理员、helper 进程、运行时） |
| `report.go` | 汇总判定、JSON 输出、人读表格 |
