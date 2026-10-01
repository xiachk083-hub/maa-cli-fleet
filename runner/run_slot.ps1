# ============================================================================
# run_slot.ps1 —— 槽位执行器：一个账号的一次"起机→跑日常(→剿灭)→停机"
#
# 由 fleet_runner.ps1 拉起（每槽一个进程）：
#   powershell -NoProfile -ExecutionPolicy Bypass -File run_slot.ps1 <accountId>
#
# 流程：起模拟器 → 等 adb_port + boot → adb connect → ops run <id> daily_<id>
#       → 等 maa 退出 → 查 .out 是否 Error →（剿灭到期则 ann_<id>）→ 停模拟器 → 写 result
# ============================================================================
param([Parameter(Mandatory=$true)][string]$AccountId)

$ErrorActionPreference = "Continue"
$RunnerDir = Split-Path -Parent $PSCommandPath
$Root      = Split-Path -Parent $RunnerDir
$OpsDir    = Join-Path $Root "ops"
$OpsScript = Join-Path $OpsDir "rogue_cli_ops.ps1"
$AccFile   = Join-Path $RunnerDir "accounts.json"
$ConfFile  = Join-Path $Root "config\fleet.local.json"
$ResultFile= Join-Path $RunnerDir ("result_{0}.json" -f $AccountId)
$LogFile   = Join-Path $RunnerDir "slot.log"

function SLog($m) {
  $line = "[" + (Get-Date -Format "MM-dd HH:mm:ss") + "][$AccountId] " + $m
  Add-Content -Path $LogFile -Value $line -Encoding UTF8
}

$MumuDir  = 'E:\MuMu Player 12'
$MumuMgr  = Join-Path $MumuDir 'nx_main\MuMuManager.exe'
$Adb      = Join-Path $MumuDir 'shell\adb.exe'

function Invoke-Cim($cmdline, $timeoutSec = 60) {
  $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmdline }
  return $r
}

function Get-InstanceInfo([string]$idx) {
  try { return (& $MumuMgr info -v $idx 2>&1 | Out-String) } catch { return "" }
}

function Get-MaaProc([string]$port) {
  $needle = [regex]::Escape("-a 127.0.0.1:" + $port)
  return @(Get-CimInstance Win32_Process -Filter "Name='maa.exe'" -ErrorAction SilentlyContinue |
           Where-Object { $_.CommandLine -and ($_.CommandLine -match $needle) })
}

function Test-OutErr([string]$task) {
  $f = Get-ChildItem (Join-Path $OpsDir "logs\$task`_*.out") -ErrorAction SilentlyContinue |
       Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if (-not $f) { return $true }
  $txt = Get-Content $f.FullName -Raw -Encoding UTF8
  return [bool]($txt -match '\]\s+Error')
}

function Set-MachineEntry($acc, [string]$port) {
  # 把该账号的机器条目写进 ops 的机器表（fleet.local.json），供 ops 命令使用
  $lock = Join-Path $RunnerDir "machines.lock"
  $held = $false
  for ($i = 0; $i -lt 60 -and -not $held; $i++) {
    try { $fs = [IO.File]::Open($lock, 'CreateNew', 'Write', 'None'); $held = $true }
    catch { Start-Sleep -Milliseconds 500 }
  }
  try {
    $cfg = Get-Content $ConfFile -Raw -Encoding UTF8 | ConvertFrom-Json
    $list = @($cfg.machines)
    $entry = [pscustomobject]@{
      name = $acc.id; emu = [string]$acc.emu; state = [string]$acc.state
      rogue = ""; daily = [string]$acc.daily; local = [string]$port
      fallback = [string]$acc.fallback; client = [string]$acc.client; pkg = ""; profile = [string]$acc.client
    }
    $list = @($list | Where-Object { $_.name -ne $acc.id }) + $entry
    $cfg | Add-Member -NotePropertyName machines -NotePropertyValue $list -Force
    $tmp = $ConfFile + ".tmp"
    [IO.File]::WriteAllText($tmp, ($cfg | ConvertTo-Json -Depth 8), (New-Object System.Text.UTF8Encoding($false)))
    Move-Item -Force $tmp $ConfFile
  } finally {
    if ($fs) { $fs.Close(); $fs.Dispose() }
    Remove-Item $lock -Force -ErrorAction SilentlyContinue
  }
}

# ---- 载入账号 ---------------------------------------------------------------
$acc = (Get-Content $AccFile -Raw -Encoding UTF8 | ConvertFrom-Json).accounts | Where-Object { $_.id -eq $AccountId }
if (-not $acc) { SLog "账号不存在：$AccountId"; exit 2 }
$res = [ordered]@{ id = $AccountId; name = $acc.name; started = (Get-Date).ToString('s'); dailyOk = $false; annOk = $false; annSkipped = $true; port = ""; note = "" }
SLog ("开始：name=" + $acc.name + " client=" + $acc.client + " stage=" + $acc.stage + " emu=" + $acc.emu)

# ---- 1) 起模拟器 + 等端口 ---------------------------------------------------
$info = Get-InstanceInfo $acc.emu
if ($info -notmatch '"is_process_started":\s*true') {
  Invoke-Cim ('"' + $MumuMgr + '" control --vmindex ' + $acc.emu + ' launch') | Out-Null
  SLog "已发启动"
}
$port = ""
for ($i = 0; $i -lt 90 -and -not $port; $i++) {
  Start-Sleep -Seconds 5
  $inf = Get-InstanceInfo $acc.emu
  $mp = [regex]::Match($inf, '"adb_port":\s*(\d+)')
  if ($mp.Success) {
    $cand = $mp.Groups[1].Value
    & $Adb connect "127.0.0.1:$cand" 2>$null | Out-Null
    $boot = (& $Adb -s "127.0.0.1:$cand" shell getprop sys.boot_completed 2>$null | Out-String).Trim()
    if ($boot -eq "1") { $port = $cand }
  }
}
if (-not $port) {
  $res.note = "模拟器未就绪"
  SLog "模拟器未就绪，放弃本次"
  $res | ConvertTo-Json | Set-Content $ResultFile -Encoding UTF8
  exit 1
}
$res.port = $port
SLog ("设备就绪：127.0.0.1:$port")
Set-MachineEntry $acc $port

# ---- 2) 跑日常 --------------------------------------------------------------
& powershell -NoProfile -ExecutionPolicy Bypass -File $OpsScript run $acc.id $acc.daily | Out-Null
$deadline = (Get-Date).AddMinutes(75)
while ((Get-Date) -lt $deadline -and (Get-MaaProc $port).Count -gt 0) { Start-Sleep -Seconds 20 }
if ((Get-MaaProc $port).Count -gt 0) { $res.note = "日常超时" ; SLog "日常超时（75min）" }
else {
  $res.dailyOk = -not (Test-OutErr $acc.daily)
  SLog ("日常结束 dailyOk=" + $res.dailyOk)
}

# ---- 3) 剿灭（按周：本周没做就做） ------------------------------------------
if ($acc.annTask) {
  $weekKey = (Get-Date).AddHours(-4).Date.AddDays(-(([int](Get-Date).AddHours(-4).DayOfWeek + 6) % 7)).ToString('yyyy-MM-dd')
  $stFile = Join-Path $RunnerDir "annweek_$AccountId.txt"
  $done = if (Test-Path $stFile) { (Get-Content $stFile -Raw).Trim() } else { "" }
  if ($done -ne $weekKey) {
    $res.annSkipped = $false
    & powershell -NoProfile -ExecutionPolicy Bypass -File $OpsScript run $acc.id $acc.annTask | Out-Null
    $dl2 = (Get-Date).AddMinutes(75)
    while ((Get-Date) -lt $dl2 -and (Get-MaaProc $port).Count -gt 0) { Start-Sleep -Seconds 20 }
    $res.annOk = -not (Test-OutErr $acc.annTask)
    if ($res.annOk) { Set-Content -Path $stFile -Value $weekKey -Encoding UTF8 }
    SLog ("剿灭结束 annOk=" + $res.annOk)
  } else { SLog "剿灭本周已完成（$weekKey），跳过" }
}

# ---- 4) 停模拟器（释放内存） -------------------------------------------------
Invoke-Cim ('"' + $MumuMgr + '" control --vmindex ' + $acc.emu + ' shutdown') | Out-Null
Start-Sleep -Seconds 5
SLog "已发停机"
$res.ended = (Get-Date).ToString('s')
$res | ConvertTo-Json | Set-Content $ResultFile -Encoding UTF8
SLog "完成"
