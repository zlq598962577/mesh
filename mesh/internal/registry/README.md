# internal/registry — 四层 agent 注册表

实现方案 §8.2 的 Tier-1 声明式注册表："新增一种 agent = 丢一个文件，不重新
编译"。本包只做数据与探测；与客户端的实际连接由 `internal/adapters` 完成。

---

## 1. manifest 完整示例

manifest 是纯 JSON（原因见 §4）。下面是一份可解析的完整示例，六个段落
（`detect` / `launch` / `output` / `resume` / `approval` / `capabilities`）
全部出现；以内置 `manifests/codex.json` 为蓝本做了通用化改写，字段取值
规则见 §2 的字段说明：

```json
{
  "id": "codex",
  "name": "Codex",
  "description": "OpenAI Codex 桌面客户端。",
  "tier": 1,

  "detect": {
    "exe_paths": [
      "%LOCALAPPDATA%\\Microsoft\\WindowsApps\\chatgpt-desktop.exe",
      "C:\\Program Files\\WindowsApps\\OpenAI.Codex_*\\app\\ChatGPT.exe"
    ],
    "process_names": ["ChatGPT.exe"],
    "window_class": [],
    "window_title": [],
    "exclude_paths": [
      "F:\\AIWORKSPACE\\CodexChinese"
    ],
    "version_path": "",
    "version_from": "msix_package",
    "notes": "给人看的维护说明，不参与逻辑。"
  },

  "launch": {
    "exe_path": "%LOCALAPPDATA%\\Microsoft\\WindowsApps\\chatgpt-desktop.exe",
    "args": [],
    "work_dir": "",
    "window_wait_ms": 30000
  },

  "output": {
    "mode": "clipboard",
    "control": "",
    "completion": "unknown_only",
    "success_signal": "",
    "notes": "无可靠完成标志时必须写 unknown_only。"
  },

  "resume": {
    "mode": "none",
    "native_id_source": ""
  },

  "approval": {
    "bridged": false,
    "notes": "语义与目标不可验证时平台不代答（§9.3）。"
  },

  "capabilities": {
    "send": true,
    "stream_output": false,
    "cancel": true,
    "cancel_verified": false,
    "approval_bridge": false,
    "file_transfer": false,
    "native_session_id": false,
    "read_only_review": false,
    "health_check": true
  }
}
```

解析器 `ParseManifest` 开启 `DisallowUnknownFields`：字段名拼错会得到带
文件路径与行号的明确报错，而不是静默忽略后"看起来生效了"。错误一律是
`*registry.ManifestError`，带 `Path / Line / Column / Field`。

## 2. 字段说明与硬约束

### detect — 怎么判断客户端在本机存在

| 字段 | 说明 |
|---|---|
| `exe_paths` | 候选可执行文件绝对路径，支持 `%ENV%` 展开与**单路径分量内**的 `*` 通配（不跨目录、不递归扫盘，§9.5）。逐条按序取第一个存在者 |
| `process_names` | 进程名（不含路径）。任一条件命中即"可用"；探测优先走进程（进程映像路径不受 WindowsApps 目录枚举限制） |
| `window_class` / `window_title` | 仅作窗口定位参考，**不参与**"是否安装"判定（Trae 的主窗口标题是项目名，按标题判必然漏判） |
| `exclude_paths` | 命中即整条判不可用，优先于一切条件。这是防事故拦截（如 CodexChinese 会装管理员计划任务），不是路径过滤 |
| `version_path` | 版本号从哪个文件读。默认用探测命中的 exe；豆包必须指向启动器（主程序带的是 Chromium 版本） |
| `version_from` | `exe`（FileVersion 优先回落 ProductVersion）/ `exe_product`（只读 ProductVersion，DSH/Trae 用）/ `msix_package`（从包目录名第 2 段解析，Codex 用） |

### launch / output / resume / approval

| 段 | 硬约束 |
|---|---|
| `launch` | `exe_path` 必填。平台默认不主动启动客户端；启动只发生一次，不接管生命周期 |
| `output` | `mode` 只能是 `control`（必须给 `control` 标识）或 `clipboard`；`completion` 只能是 `success_signal`（必须给 `success_signal`）或 `unknown_only`（禁止再写 success_signal，两者矛盾）。**没有第三种取值**——不存在"看退出码"这个选项（§4.2.2 / §4.3） |
| `resume` | `mode` 是 `native_id`（必须说明来源）或 `none`。`none` 时调用方必须置 `Recovered=false`，不许假装恢复成功 |
| `approval` | `bridged=false` 时平台无权代答，审批请求只透传给用户；不得自动点"全部允许" |

### capabilities 的自洽校验

以下组合在 `Manifest.Validate` 里直接报错，不是风格问题：

- `cancel_verified=true` 但 `cancel=false`；
- `approval_bridge` 与 `approval.bridged` 必须一致；
- `native_session_id=true` 必须搭配 `resume.mode=native_id`，反之亦然。

## 3. 四层加载顺序

```
第 1 层 Catalog    embed.FS 内置 manifests/*.json，只读，随二进制走
第 2 层 Registry   <data_dir>/agents/*，用户可增删改，热加载
第 3 层 Detection  运行时探测结果的缓存（默认 TTL 30s）
第 4 层 Instance   实际连接实例（session id / 客户端 PID / Recovered）
```

四层是**逐层补充**，不是逐层覆盖：

- 第 1+2 层共同决定"有哪些 agent、怎么探测"。
- 第 3 层只回答"这台机器上它现在在不在、在哪"。
- 第 4 层只回答"我们当前连着哪个会话"。

不合成一个大结构的理由：探测会失败、实例会断开，而 manifest 是静态的；
混在一起会让"manifest 没错但探测失败"看起来像配置错误，排查会往错的方向走。

**加载与覆盖规则**（`Registry.Reload`，整表重建）：

1. 先读第 1 层（`LoadCatalog`），再扫第 2 层（`loadUserDir`）。
2. **跨层重复 id**：用户层覆盖内置层（分层的本意——用户可修正内置定义
   而不必等发版），被覆盖的内置定义保留在 `Entry.Overrides`，UI 可显示
   "已由用户覆盖"。
3. **同层重复 id**：硬拒绝，两个文件都标 invalid 并互相指出对方路径——
   无论保留哪个都是在猜。
4. **坏文件不拖累别人**：任何一个 manifest 解析失败只把那一项标
   `EntryInvalid`（仍出现在列表里，让人看得见自己写错了），其余照常加载。
5. **孤儿**：定义被删除但仍有未分离实例的条目转 `EntryOrphaned`，引用它的
   Run 必须被如实标记失败/中断，不许当作完成。
6. 重载时按 id 保留已有 Detection 缓存与 Instance，避免 UI 闪"未探测"。

`Entry.Usable()` = 定义有效 ∧ 探测确认可用 ∧ 非孤儿。它不看 Instance：
"没连着"不算不可用，只是还没连。

### Detection 的三态语义（第 3 层最容易误读的地方）

- `Available=true`：客户端在运行/已安装。
- `Available=false` 且 `Err==""`：探测成功，结论是"本机没有"。
- `Available=false` 且 `Err!=""`：**探测本身没做成**（权限、超时）——
  这不等于"没装"，UI 必须显示"未确认"，不能显示"未安装"。

缓存规则：TTL 默认 30 秒（`DefaultDetectionTTL`）；同一 adapter 的并发探测
合并成一次（inflight 去重）；探测失败（Err 非空）**不写缓存**，下次重试；
`Options.DetectionTTL < 0` 表示永不缓存。

### Instance（第 4 层）

`Attach` 只记账不连接（连接是适配器的事）。`NativeID` 为空时 `Attach` 兜底
置 `Recovered=false`——contract 的约定不能只靠调用方自觉。`Detach` 只清
平台侧记账，不关用户窗口。

## 4. 为什么用 JSON 而不是 YAML

方案 §8.2 写的是"Tier-1 纯 YAML manifest"，本实现改用 JSON。这是一次
被环境逼出来的取舍，理由与代价都要说清楚：

**理由**：解析 YAML 必须引入第三方库（如 `gopkg.in/yaml.v3`），而本项目
硬性约束是"纯标准库 + syscall/unsafe，不许 cgo"，且构建环境无网络、
无法拉依赖。`encoding/json` 是标准库，零依赖完成同一件事。

**代价**：manifest 作者要写 JSON 而不是 YAML——影响面被限制在"写文件的
手感"上。字段集合、语义、四层加载顺序、热增删行为都与方案一致。

**兼容处理**：用户目录下 `*.json / *.yaml / *.yml` 都会被扫描（保持
方案里 `*.yaml` 习惯不至于直接失效），但 `.yaml` 文件的**内容必须是 JSON
语法**；不是的话，解析错误会明确写出"本实现不解析 YAML"，而不是让人对
着一句看不懂的语法错误发呆。

## 5. 热增删行为：`registry.Watch` 的 mtime 轮询

`<data_dir>/agents/*` 的增删改不需要重启平台。实现是**定时轮询**而不是
文件系统事件监听，三条理由按重要性排序：

1. **依赖**：fsnotify 是第三方库，纯标准库约束下加了就永远编译不过。
2. **正确性**：`ReadDirectoryChangesW` 在网络盘、被挂起的进程、某些杀毒
   软件介入时会丢事件或干脆不投递。丢事件的表现是"用户改了 manifest 但
   没生效"且无法自查——它本来就不该漏。轮询最坏只是慢一个周期，不会漏。
3. **成本可控**：监听目录里是个位数到几十个几 KB 的 JSON 文件，
   `os.ReadDir + os.Stat` 是几十微秒级；默认 2 秒一次（`DefaultPollInterval`）
   对桌面机是噪声级开销。

轮询的**代价**也如实说：变化最多延迟一个周期才生效；空闲时周期性唤醒。
对本场景（人改配置文件不会掐秒表）都可忽略。

**指纹与触发**：每个文件记 `size + mtime` 指纹（某些编辑器保存会保留 mtime，
单看 mtime 会漏）；目录自身 mtime 兜底"新增/删除文件"。指纹有变化 →
`Registry.Reload()` **整表重建**——不做增量合并：id 在文件内容里不在文件名上，
改名/换 id/两个文件交换 id 的增量状态机复杂一个数量级，而这个规模下整表
重建根本不需要优化，且不会留下"删掉的文件还在表里"的残留。

**失败语义**：加载失败（用户存盘存了一半）不会让 Watcher 停止——先记下
新指纹再加载，下个周期用户修好文件后指纹再变自然重试；`OnError` 回调
知情，`OnReload` 回调拿到 `ReloadReport`。`Watcher.PollOnce()` 供"刷新
按钮"立即比对，`Stop()` 幂等收尾。

## 6. 与适配器的衔接

- 适配器（`internal/adapters`）通过 `ManifestFor / DetectClient /
  LocateClientWindow` 读取**内置层** manifest 并用 `WindowsDetector` 探测；
  WindowsDetector 复用 `internal/probe` 的进程/窗口/版本原语，不另造
  Win32 调用。
- 会话建立后由适配器 `Attach` 第 4 层 Instance；`Close` 时 `Detach`。
- 探测器的三态语义同样约束适配器 `Health`：探测侧失败（Err 非空）与
  客户端侧未运行（Available=false）必须分开上报。
