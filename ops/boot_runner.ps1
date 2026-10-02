# ============================================================================
# boot_runner.ps1 —— 外部只管 runner 进程在不在 / 卡没卡（幂等）
#
# 与 boot_node.ps1 同一套路：只保证"runner 进程在岗且还在干活"，
# 槽位调度 / 模拟器 / maa / 停机 全部由 runner 自己管（runner/runner.json 配置）。
#
# 由计划任务调用：
#   FleetRunner-AutoStart   AtLogon（重启/登录后立即恢复）
#   FleetRunner-Watchdog    每 10 分钟（进程意外退出 / 卡死 兜底）
#
# 判活两道：
#   ① 进程在不在（认进程身份，不靠单一 PID —— 防 PID 复用误判）；
#   ② 心跳文件 runner\heartbeat.txt（runner 每轮 tick 重写）：mtime 超过 StaleMin = 主循环卡死 → 杀掉重拉。
#      （2026-10-02 踩过：进程活着但车道卡死，只判进程的看门狗看不见。）
# ============================================================================
param([int]$StaleMin = 6)
$ErrorActionPreference = "Continue"
$OpsDir = Split-Path -Parent $PSCommandPath
$Root   = Split-Path -Parent $OpsDir
$Exe    = Join-Path $Root "fleet.exe"
if (-not (Test-Path $Exe)) { $Exe = Join-Path $Root "dist\fleet.exe" }   # 仓库布局在 dist\，目标机布局在根目录
$Conf   = Join-Path $Root "runner\runner.json"
$Heart  = Join-Path $Root "runner\heartbeat.txt"
$Log    = Join-Path $OpsDir "boot_runner.log"

function RLog($m) {
  Add-Content -Path $Log -Value ("[" + (Get-Date -Format "MM-dd HH:mm:ss") + "] " + $m) -Encoding UTF8
}

if (-not (Test-Path $Exe)) { RLog "找不到 runner 可执行文件：$Exe"; exit 2 }

# 人工停：stop 文件在 → 不拉（尊重手动停止）
$stopFile = Join-Path $Root "runner\runner.stop"
if (Test-Path $Conf) {
  try {
    $c = Get-Content $Conf -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($c.stopFile) { $stopFile = [string]$c.stopFile }
    if ($c.stateFile) { $Heart = Join-Path (Split-Path -Parent ([string]$c.stateFile)) "heartbeat.txt" }
  } catch { }
}
if (Test-Path $stopFile) { RLog "runner.stop 在（人工停）→ 跳过"; exit 0 }

# 已在跑？（认进程身份：fleet.exe 且命令行是 runner 常驻；排除 -status/-once 这类一次性命令；
# 不靠单一 PID，防 PID 复用误判——2026-10-01 l-4 就是被 nvcontainer 顶了 PID）
$running = @(Get-CimInstance Win32_Process -Filter "Name='fleet.exe'" -ErrorAction SilentlyContinue |
  Where-Object {
    $_.CommandLine -and ($_.CommandLine -match '\brunner\b') -and
    ($_.CommandLine -notmatch '-(status|once|now|enqueue|cancel|reset|enable|disable)\b')
  })
if ($running.Count -gt 0) {
  # ② 心跳：主循环 heartbeat.txt + 各肉鸽车道 hb_rogue_*.txt
  #    任一太久没更新 = 卡死（进程活着不干活）→ 杀掉重拉
  $stale = $false; $age = 0.0; $why = ""
  if (Test-Path $Heart) {
    $age = ((Get-Date) - (Get-Item $Heart).LastWriteTime).TotalMinutes
    if ($age -gt $StaleMin) { $stale = $true; $why = "主循环心跳 " + [math]::Round($age, 1) + " 分钟" }
  }
  Get-ChildItem (Join-Path $Root "runner\hb_rogue_*.txt") -ErrorAction SilentlyContinue | ForEach-Object {
    $a = ((Get-Date) - $_.LastWriteTime).TotalMinutes
    if ($a -gt 15) { $stale = $true; $why = $why + " 车道心跳 " + $_.Name + " " + [math]::Round($a, 1) + " 分钟" }
  }
  if (-not $stale) { RLog ("runner 已在跑（pid=" + $running[0].ProcessId + "，主心跳 " + [math]::Round($age, 1) + " 分钟前），跳过"); exit 0 }
  RLog ("runner 进程在（pid=" + $running[0].ProcessId + "）但" + $why + " → 判卡死，杀掉重拉")
  foreach ($p in $running) { & taskkill /PID $p.ProcessId /T /F 2>$null | Out-Null }
  Start-Sleep -Seconds 3
}

# 拉起（WMI：脱离本进程树，计划任务/ssh 会话结束也不影响）
$cmd = '"' + $Exe + '" runner -conf "' + $Conf + '"'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
  CommandLine      = $cmd
  CurrentDirectory = $Root
}
RLog ("runner 已拉起 pid=" + $r.ProcessId + " rc=" + $r.ReturnValue)
