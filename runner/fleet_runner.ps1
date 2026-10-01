# ============================================================================
# fleet_runner.ps1 —— 槽位轮转调度器（全量账号：一天内把每台的日常跑完）
#
# 用法：
#   .\fleet_runner.ps1            # 常驻（由开机自启/机端拉起）
#   .\fleet_runner.ps1 -Once      # 单轮：填一次槽就退出（调试）
#   .\fleet_runner.ps1 -Status    # 看一眼状态（不动手）
#
# 模型：N 个槽（accounts.json 的 slots）→ 每槽一次跑一个账号的一次日常（+到期的剿灭）
#       队列：当天（游戏日 04:00 边界）还没跑完的账号，按"上次跑完时间"最旧的优先。
# 状态：state.json（每账号 doneDate / 上次结果 / 失败次数）；结果由 run_slot.ps1 写 result_<id>.json
# ============================================================================
param([switch]$Once, [switch]$Status)

$ErrorActionPreference = "Continue"
$RunnerDir = Split-Path -Parent $PSCommandPath
$Root      = Split-Path -Parent $RunnerDir
$AccFile   = Join-Path $RunnerDir "accounts.json"
$StateFile = Join-Path $RunnerDir "state.json"
$LogFile   = Join-Path $RunnerDir "runner.log"
$SlotScript= Join-Path $RunnerDir "run_slot.ps1"
$MaxFailPerDay = 3

function RLog($m) {
  $line = "[" + (Get-Date -Format "MM-dd HH:mm:ss") + "] " + $m
  Add-Content -Path $LogFile -Value $line -Encoding UTF8
  if ($Status -or $Once) { Write-Host $line }
}

function Load-Json($p, $default) {
  if (-not (Test-Path $p)) { return $default }
  try { return (Get-Content $p -Raw -Encoding UTF8 | ConvertFrom-Json) } catch { return $default }
}

function GameDay { return (Get-Date).AddHours(-4).ToString('yyyy-MM-dd') }

function Running-Slots {
  $procs = Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
           Where-Object { $_.CommandLine -like "*run_slot.ps1*" }
  $ids = @()
  foreach ($p in $procs) {
    $m = [regex]::Match($p.CommandLine, 'run_slot\.ps1\s+"?([A-Za-z0-9_-]+)"?')
    if ($m.Success) { $ids += $m.Groups[1].Value }
  }
  return $ids
}

# ---- 汇总 result → state ----------------------------------------------------
function Merge-Results($state) {
  $changed = $false
  foreach ($f in (Get-ChildItem (Join-Path $RunnerDir 'result_*.json') -ErrorAction SilentlyContinue)) {
    $r = Load-Json $f.FullName $null
    if (-not $r) { continue }
    $s = $state.accounts.($r.id)
    if (-not $s) { $state.accounts | Add-Member -NotePropertyName $r.id -NotePropertyValue ([pscustomobject]@{}) -Force; $s = $state.accounts.($r.id) }
    if ($s.lastResult -eq $r.ended) { Remove-Item $f.FullName -Force -ErrorAction SilentlyContinue; continue }
    $day = (Get-Date $r.ended).AddHours(-4).ToString('yyyy-MM-dd')
    $s | Add-Member -NotePropertyName lastEnd -NotePropertyValue $r.ended -Force
    $s | Add-Member -NotePropertyName lastResult -NotePropertyValue $r.ended -Force
    $s | Add-Member -NotePropertyName lastDailyOk -NotePropertyValue ([bool]$r.dailyOk) -Force
    $s | Add-Member -NotePropertyName lastNote -NotePropertyValue ([string]$r.note) -Force
    if ($r.dailyOk) {
      $s | Add-Member -NotePropertyName doneDate -NotePropertyValue $day -Force
      $s | Add-Member -NotePropertyName fails -NotePropertyValue 0 -Force
      RLog ("[$($r.id)] 日常完成（$day）")
    } else {
      $n = ([int]$s.fails) + 1
      $s | Add-Member -NotePropertyName fails -NotePropertyValue $n -Force
      if ($n -ge $MaxFailPerDay) { $s | Add-Member -NotePropertyName doneDate -NotePropertyValue $day -Force }
      RLog ("[$($r.id)] 日常失败第 $n 次（$($r.note)）" + $(if ($n -ge $MaxFailPerDay) { " → 今日放弃" } else { "" }))
    }
    Remove-Item $f.FullName -Force -ErrorAction SilentlyContinue
    $changed = $true
  }
  return $changed
}

# ---- 单轮 -------------------------------------------------------------------
function Tick {
  $acc = Load-Json $AccFile $null
  if (-not $acc) { RLog "缺少 accounts.json（先跑 tools/gen_accounts.ps1）"; return }
  $state = Load-Json $StateFile ([pscustomobject]@{ accounts = [pscustomobject]@{} })
  if (-not $state.accounts) { $state = [pscustomobject]@{ accounts = [pscustomobject]@{} } }
  [void](Merge-Results $state)
  $state | ConvertTo-Json -Depth 8 | Set-Content $StateFile -Encoding UTF8

  $day = GameDay
  $running = Running-Slots
  $slots = [int]$acc.slots; if ($slots -le 0) { $slots = 8 }
  $free = $slots - $running.Count
  RLog ("游戏日 $day | 槽位 $($running.Count)/$slots | 在跑：" + $(if ($running.Count) { $running -join ',' } else { "-" }))
  if ($free -le 0) { return }

  $due = @()
  foreach ($a in $acc.accounts) {
    if (-not $a.enabled) { continue }
    $s = $state.accounts.($a.id)
    if ($s -and $s.doneDate -eq $day) { continue }
    if ($running -contains $a.id) { continue }
    $lastEnd = if ($s -and $s.lastEnd) { [string]$s.lastEnd } else { "2000-01-01T00:00:00" }
    $due += [pscustomobject]@{ acc = $a; lastEnd = $lastEnd }
  }
  $due = $due | Sort-Object lastEnd
  RLog ("待跑 " + $due.Count + " 台" + $(if ($due.Count) { "（下一个：" + $due[0].acc.id + "）" } else { "" }))
  foreach ($d in ($due | Select-Object -First $free)) {
    $cmd = 'powershell -NoProfile -ExecutionPolicy Bypass -File "' + $SlotScript + '" ' + $d.acc.id
    $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmd }
    RLog ("开槽 " + $d.acc.id + " -> pid=" + $r.ProcessId)
    Start-Sleep -Seconds 2
  }
}

if ($Status) {
  $acc = Load-Json $AccFile $null
  $state = Load-Json $StateFile ([pscustomobject]@{ accounts = [pscustomobject]@{} })
  $day = GameDay
  $running = Running-Slots
  $doneN = 0; $total = 0
  if ($acc) {
    foreach ($a in $acc.accounts) {
      if (-not $a.enabled) { continue }
      $total++
      $s = $state.accounts.($a.id)
      if ($s -and $s.doneDate -eq $day) { $doneN++ }
    }
    RLog ("状态：游戏日 $day | 已完成 $doneN/$total | 在跑 $($running.Count)/$($acc.slots)：" + ($running -join ','))
  } else { RLog "无 accounts.json" }
  return
}

if ($Once) { Tick; return }

RLog "调度器启动"
while ($true) {
  try { Tick } catch { RLog ("Tick 异常：" + $_.Exception.Message) }
  Start-Sleep -Seconds 60
}
