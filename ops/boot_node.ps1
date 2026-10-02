# ============================================================================
# boot_node.ps1 —— 目标机开机/登录自启：确保机端在跑（幂等）
#
# 由计划任务调用（推荐：Administrator / Interactive / Highest / 登录触发 + 启动触发）：
#   powershell -NoProfile -ExecutionPolicy Bypass -File <项目>\ops\boot_node.ps1
#
# 职责很窄：只保证"机端进程在"。机端起来后会 Ensure-Workers：
#   拉起 cycle/watch worker → worker 自愈时会把模拟器、adb、任务全拉起来。
# 即：主机蓝屏/重启后无人干预也能自愈。
# ============================================================================
$ErrorActionPreference = "Continue"
$OpsDir = Split-Path -Parent $PSCommandPath
$Root   = Split-Path -Parent $OpsDir
$NodeScript = Join-Path $Root "node\fleet_node.ps1"
$Log    = Join-Path $OpsDir "boot_node.log"

function BLog($m) {
  Add-Content -Path $Log -Value ("[" + (Get-Date -Format "MM-dd HH:mm:ss") + "] " + $m) -Encoding UTF8
}

if (-not (Test-Path $NodeScript)) { BLog "找不到机端脚本：$NodeScript"; exit 2 }

# 给桌面/网络留点时间（登录后立刻起模拟器容易抢资源）
Start-Sleep -Seconds 20

$running = Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" |
  Where-Object { $_.CommandLine -like "*fleet_node.ps1*" }
if ($running) {
  # 判活两道：进程在 + 还在干活。
  # node 每轮都会跑 ops status（往 rogue_cli_ops.log 追加体检行）→ 日志太久没新行 = 主循环卡死
  # （2026-10-02 踩过：进程活着但车道卡死，只看进程的看门狗看不见）
  $opsLog = Join-Path $OpsDir "rogue_cli_ops.log"
  $age = 0.0; $stale = $false
  if (Test-Path $opsLog) {
    $age = ((Get-Date) - (Get-Item $opsLog).LastWriteTime).TotalMinutes
    if ($age -gt 8) { $stale = $true }
  }
  if (-not $stale) { BLog ("机端已在跑（pid=" + (@($running)[0].ProcessId) + "，心跳 " + [math]::Round($age, 1) + " 分钟前），跳过"); exit 0 }
  BLog ("机端进程在（pid=" + (@($running)[0].ProcessId) + "）但体检日志停了 " + [math]::Round($age, 1) + " 分钟 → 判卡死，杀掉重拉")
  foreach ($p in $running) { Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue }
  Start-Sleep -Seconds 3
}

# 用 WMI 拉起（脱离本进程树，计划任务结束也不影响）
$cmd = 'powershell -NoProfile -ExecutionPolicy Bypass -File "' + $NodeScript + '" run'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmd }
BLog ("机端已拉起 pid=" + $r.ProcessId + " rc=" + $r.ReturnValue)
