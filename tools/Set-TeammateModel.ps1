# Set-TeammateModel.ps1 — 修改 DSH Agent Teams 新成员的大模型路由
#
# 原理（从 DSH 0.2.2-rc.1 实现反查）：
#   spawn_teammate 工具创建成员时 provider 取自 agent-team bundle 的配置项
#   freshProvider / forkProvider（默认分别是 "spawn" / "fork" 路由）。
#   这两个配置项可以在 profile 的 cordis.patch.yml 里按 bundle 名覆盖。
#   本工具就是对 cordis.patch.yml 做"行级"增删改——不做 YAML 对象重序列化，
#   以免破坏文件里的 {{API_KEY_...}} 运行时占位符与注释。
#
# 生效条件（必须如实知道）：
#   1. 需要**重启 DSH** 才生效——bundle 配置在进程启动时装载。
#   2. 只影响**之后新创建**的成员；已存在成员的 provider 记录在创建时就写进
#      团队日志，无法事后改（这也是 8 人名额里旧成员占坑不改模型的原因）。
#   3. provider 改成 "magpie" 后，新成员的模型 = magpie 路由的默认模型，
#      即 cordis.patch.yml 里 agent-default-model 段的 provider/model 组合。
#
# 用法示例：
#   pwsh -File Set-TeammateModel.ps1 -List                    # 查看当前状态
#   pwsh -File Set-TeammateModel.ps1 -FreshProvider magpie -ForkProvider magpie
#   pwsh -File Set-TeammateModel.ps1 -Restore                 # 恢复默认 spawn/fork
#
# 修改前自动备份到 cordis.patch.yml.bak-teammate-model。

[CmdletBinding(DefaultParameterSetName = 'set')]
param(
    # 目标 provider 路由名。可选值见 ~/.dsh/settings.yaml 的 providers 段，
    # 例如 magpie / magpie-anthropic / kimi-coding；"spawn" 是默认继承路由。
    [Parameter(ParameterSetName = 'set', Mandatory)]
    [string]$FreshProvider,

    [Parameter(ParameterSetName = 'set')]
    [string]$ForkProvider,

    # 删除覆盖条目，恢复 bundle 默认（fresh=spawn 继承、fork=fork）。
    [Parameter(ParameterSetName = 'restore')]
    [switch]$Restore,

    # 只显示当前状态，不做修改。
    [Parameter(ParameterSetName = 'list')]
    [switch]$List,

    # patch 文件路径，默认当前用户 desktop profile 的 cordis.patch.yml。
    [string]$PatchPath = "$env:USERPROFILE\.dsh\profiles\desktop\cordis.patch.yml",

    # 覆盖条目的定位键（cordis loader 的 name 匹配）。
    # 默认是 agent-team profile bundle；若重启后成员 provider 仍是 spawn，
    # 换成 tool 插件包名再试：-EntryName '@deepseek-ai/dsh-experimental-tool-agent-team'
    [string]$EntryName = '@deepseek-ai/dsh-experimental-agent-team-profile',

    # 本工具写入的条目标签（用于定位与恢复，不影响路由行为）。
    [string]$EntryId = 'teammate-model-override'
)

$ErrorActionPreference = 'Stop'

# ---------- 基础检查 ----------

if (-not (Test-Path -LiteralPath $PatchPath)) {
    Write-Error "找不到 patch 文件: $PatchPath"
    return
}

# 行级读入（保住注释、占位符、原始缩进）。
$lines = [System.IO.File]::ReadAllLines($PatchPath, [System.Text.Encoding]::UTF8)

# 定位本工具管理的覆盖条目块：
#   - id: <EntryId>
#     name: <EntryName>
#     config:
#       freshProvider: ...
#       forkProvider: ...
# 块的边界 = 下一个顶层 "- " 条目或文件尾。
function Find-EntryBlock {
    param([string[]]$FileLines)

    for ($i = 0; $i -lt $FileLines.Count; $i++) {
        if ($FileLines[$i] -match "^\-\s+id:\s*$([regex]::Escape($EntryId))\b") {
            $end = $FileLines.Count
            for ($j = $i + 1; $j -lt $FileLines.Count; $j++) {
                if ($FileLines[$j] -match '^\-\s') { $end = $j; break }
            }
            return [pscustomobject]@{ Start = $i; End = $end }
        }
    }
    return $null
}

function Get-BlockValue {
    param([string[]]$FileLines, $Block, [string]$Key)
    for ($i = $Block.Start; $i -lt $Block.End; $i++) {
        if ($FileLines[$i] -match "^\s+$Key`:\s*(.+?)\s*$") { return $Matches[1] }
    }
    return $null
}

$block = Find-EntryBlock -FileLines $lines

# ---------- -List ----------

if ($List) {
    "patch 文件: $PatchPath"
    "覆盖条目:  $(if ($block) { "存在（行 $($block.Start + 1) 起）" } else { '不存在（成员走 bundle 默认路由）' })"
    if ($block) {
        "  定位 name:       $(Get-BlockValue $lines $block 'name')"
        "  freshProvider:   $(Get-BlockValue $lines $block 'freshProvider')"
        "  forkProvider:    $(Get-BlockValue $lines $block 'forkProvider')"
    } else {
        "  默认行为:        fresh 成员走 spawn 路由（继承会话默认），fork 成员走 fork 路由"
    }
    "生效条件:  重启 DSH；只影响之后新创建的成员"
    return
}

# ---------- 备份 ----------

$backup = "$PatchPath.bak-teammate-model"
Copy-Item -LiteralPath $PatchPath -Destination $backup -Force
"已备份原文件到: $backup"

# ---------- -Restore ----------

if ($Restore) {
    if (-not $block) {
        "覆盖条目不存在，无需恢复。"
        return
    }
    $new = @($lines[0..($block.Start - 1)]) + @($lines[$block.End..($lines.Count - 1)])
    [System.IO.File]::WriteAllLines($PatchPath, $new, [System.Text.Encoding]::UTF8)
    "已删除覆盖条目（$($block.End - $block.Start) 行），重启 DSH 后新成员恢复默认路由。"
    return
}

# ---------- 设置 ----------

$fresh = $FreshProvider.Trim()
$fork  = if ($ForkProvider) { $ForkProvider.Trim() } else { $fresh }

if ($block) {
    # 原地更新块内的两行；没有的行插到 config: 段末尾。
    $newLines = [System.Collections.Generic.List[string]]::new()
    $inBlock = $false
    $configIndentSeen = $false
    $freshDone = $false; $forkDone = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $inBlock = ($i -ge $block.Start -and $i -lt $block.End)
        $line = $lines[$i]
        if ($inBlock -and $line -match '^\s+freshProvider\s*:') {
            $newLines.Add("    freshProvider: $fresh"); $freshDone = $true; continue
        }
        if ($inBlock -and $line -match '^\s+forkProvider\s*:') {
            $newLines.Add("    forkProvider: $fork"); $forkDone = $true; continue
        }
        $newLines.Add($line)
        if ($inBlock -and $line -match '^\s+config\s*:\s*$') { $configIndentSeen = $true }
        # config 段结束（下一行回到 name/id 层级或块尾）时补齐缺失的键。
        $nextInBlock = ($i + 1 -lt $block.End)
        if ($inBlock -and $configIndentSeen -and -not $nextInBlock) {
            if (-not $freshDone) { $newLines.Add("    freshProvider: $fresh") }
            if (-not $forkDone)  { $newLines.Add("    forkProvider: $fork") }
        }
    }
    [System.IO.File]::WriteAllLines($PatchPath, $newLines, [System.Text.Encoding]::UTF8)
    "已更新覆盖条目（$EntryName）：freshProvider=$fresh forkProvider=$fork"
} else {
    # 追加新条目到文件尾。
    $entry = @(
        "- id: $EntryId",
        "  # Set-TeammateModel.ps1 管理：新成员的模型路由。重启 DSH 生效。",
        "  name: `"$EntryName`"",
        '  config:',
        "    freshProvider: $fresh",
        "    forkProvider: $fork"
    )
    $new = @($lines) + $entry
    [System.IO.File]::WriteAllLines($PatchPath, $new, [System.Text.Encoding]::UTF8)
    "已追加覆盖条目（$EntryName）：freshProvider=$fresh forkProvider=$fork"
}

""
"下一步：重启 DSH 生效。之后 spawn_teammate 创建的成员将走 $fresh 路由；"
"已存在的成员不受影响（provider 在创建时已固化到团队日志）。"
"验证方法：重启后让 Lead 跑一次 list_agents，看新成员的 provider 字段。"
