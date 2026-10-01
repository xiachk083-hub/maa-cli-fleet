# ============================================================================
# gen_accounts.ps1 —— 从 AUTO-MAS 配置生成我们的账号表与任务文件（在目标机上跑）
#
# 输入：E:\AUTO-MAS\config\ScriptConfig.json（每个账号一份：服务器/关卡/任务开关）
# 输出（全部为“可重建产物”，不进 git）：
#   <项目>\runner\accounts.json          账号表（id/名字/服务器/关卡/剿灭/实例/端口）
#   <项目>\config\tasks\daily_<id>.toml  日常任务文件（6 段链，按账号的服务器/关卡）
#   <项目>\config\tasks\ann_<id>.toml    剿灭作战任务（仅开启剿灭的账号）
#   <项目>\config\profiles\<client>.toml 每服务器一份 profile（global_resource 不同）
#
# 注意：账号凭据（手机号/邮箱）在 ScriptConfig 的 Notes 里 —— **一律不写入任何产物**。
#
# 用法： .\gen_accounts.ps1 [-AutoMasDir 'E:\AUTO-MAS'] [-ProjectDir 'E:\maa-cli-fleet'] [-DryRun]
# ============================================================================
param(
  [string]$AutoMasDir = 'E:\AUTO-MAS',
  [string]$ProjectDir = 'E:\maa-cli-fleet',
  [string[]]$OurIds = @('a25','a28','a31','a34','a52'),   # 这 5 个是已接管的 l-1/l-2/l-4/l-5/l-7（跳过）
  [switch]$DryRun
)
$ErrorActionPreference = 'Stop'

$script:D = Join-Path $ProjectDir 'data'

function New-DailyToml([string]$client, [string]$stage, [string]$fb) {
  $s = ''
  $s += "[[tasks]]`nname = `"开始唤醒`"`ntype = `"StartUp`"`nparams = { client_type = `"$client`", start_game_enabled = true }`n`n"
  $s += "[[tasks]]`nname = `"刷理智`"`ntype = `"Fight`"`nparams = { stage = `"$stage`", medicine = 0, stone = 0, series = 0 }`n`n"
  $s += "[[tasks]]`nname = `"公开招募`"`ntype = `"Recruit`"`nparams = { refresh = true, select = [4, 5], confirm = [3, 4], times = 4, skip_robot = true }`n`n"
  $s += "[[tasks]]`nname = `"基建换班`"`ntype = `"Infrast`"`nparams = { facility = [`"Mfg`",`"Trade`",`"Power`",`"Control`",`"Reception`",`"Office`",`"Dorm`"], drones = `"Money`", threshold = 0.3, dorm_trust_enabled = true }`n`n"
  $s += "[[tasks]]`nname = `"信用购物`"`ntype = `"Mall`"`nparams = { visit_friends = true, shopping = true, buy_first = [`"招聘许可`"], blacklist = [`"加急许可`",`"家具零件`"] }`n`n"
  $s += "[[tasks]]`nname = `"领取奖励`"`ntype = `"Award`"`nparams = { award = true, mail = true }`n"
  return $s
}

function New-AnnToml([string]$stage) {
  return "[[tasks]]`nname = `"剿灭作战`"`ntype = `"Annihilation`"`nparams = { stage = `"$stage`" }`n"
}

function New-ProfileToml([string]$client) {
  $s = '[connection]' + "`n"
  $s += 'adb_path = "<ADB_PATH>"' + "`n"          # 由部署时按目标机实际路径替换
  $s += 'address = "127.0.0.1:16384"' + "`n"
  $s += 'config = "General"' + "`n`n"
  $s += '[resource]' + "`n"
  $s += "global_resource = `"$client`"`n"
  $s += 'user_resource = false' + "`n`n"
  $s += '[instance_options]' + "`n"
  $s += 'touch_mode = "MaaTouch"' + "`n"
  return $s
}

$scriptJson = Join-Path $AutoMasDir 'config\ScriptConfig.json'
if (-not (Test-Path $scriptJson)) { throw "找不到 $scriptJson" }
$j = Get-Content $scriptJson -Raw -Encoding UTF8 | ConvertFrom-Json

$accounts = @()
foreach ($pr in $j.PSObject.Properties) {
  if ($pr.Name -eq 'instances') { continue }
  $o = $pr.Value
  $ud = $o.SubConfigsInfo.UserData
  if (-not $ud -or -not $ud.instances) { continue }
  $u = $ud.($ud.instances[0].uid)
  if (-not $u) { continue }
  $info = $u.Info; $task = $u.Task
  $idx = [string]$o.Emulator.Index
  $id  = 'a' + $idx.PadLeft(2, '0')
  if ($OurIds -contains $id) { continue }                     # 已接管
  if (-not $task.IfStartUp -and -not $task.IfFight) { continue }   # 空脚本跳过
  $accounts += [pscustomobject]@{
    id       = $id
    name     = [string]$info.Name
    client   = [string]$info.Server
    stage    = [string]$info.Stage
    mode     = [string]$info.StageMode
    fallback = '1-7'
    ann      = if ($info.Annihilation -ne 'Close') { '龙门市区' } else { '' }
    emu      = $idx
    port     = ''
    daily    = "daily_$id"
    annTask  = if ($info.Annihilation -ne 'Close') { "ann_$id" } else { '' }
    state    = "state_$id"
    enabled  = $true
  }
}

Write-Host ("账号数（待迁）：" + $accounts.Count)
$byClient = $accounts | Group-Object client | ForEach-Object { $_.Name + '=' + $_.Count }
Write-Host ("按服务器：" + ($byClient -join ', '))
Write-Host ("剿灭开启：" + (@($accounts | Where-Object { $_.ann }).Count))

if ($DryRun) {
  $accounts | Select-Object id, name, client, stage, emu, ann | Format-Table -AutoSize | Out-String | Write-Host
  return
}

# ---- 写产物 -----------------------------------------------------------------
$runnerDir = Join-Path $ProjectDir 'runner'
$tasksDir  = Join-Path $ProjectDir 'config\tasks'
$profDir   = Join-Path $ProjectDir 'config\profiles'
foreach ($d in @($runnerDir, $tasksDir, $profDir)) { if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null } }

foreach ($a in $accounts) {
  $body = "# $($a.id) · $($a.name) · $($a.client) · 刷 $($a.stage)（由 gen_accounts.ps1 从 AUTO-MAS 配置生成）`n"
  $body += New-DailyToml $a.client $a.stage $a.fallback
  [System.IO.File]::WriteAllText((Join-Path $tasksDir ("daily_{0}.toml" -f $a.id)), $body, (New-Object System.Text.UTF8Encoding($false)))
  if ($a.annTask) {
    $ab = "# $($a.id) · $($a.name) · 剿灭作战 $($a.ann)`n" + (New-AnnToml $a.ann)
    [System.IO.File]::WriteAllText((Join-Path $tasksDir ("ann_{0}.toml" -f $a.id)), $ab, (New-Object System.Text.UTF8Encoding($false)))
  }
}
foreach ($cl in ($accounts | Group-Object client)) {
  $pf = Join-Path $profDir ("{0}.toml" -f $cl.Name)
  if (-not (Test-Path $pf)) {
    [System.IO.File]::WriteAllText($pf, (New-ProfileToml $cl.Name), (New-Object System.Text.UTF8Encoding($false)))
  }
}
[System.IO.File]::WriteAllText(
  (Join-Path $runnerDir 'accounts.json'),
  (([pscustomobject]@{ generatedAt = (Get-Date).ToString('s'); slots = 8; accounts = $accounts }) | ConvertTo-Json -Depth 6),
  (New-Object System.Text.UTF8Encoding($false)))
Write-Host ("已写：" + $accounts.Count + " 份日常任务 + " + (@($accounts | Where-Object { $_.annTask }).Count) + " 份剿灭任务 + profile " + (@($accounts | Group-Object client).Count) + " 份 + accounts.json")
