# ============================================================================
# boot_runner.ps1 —— 外部只管 runner 进程在不在（幂等）
#
# 与 boot_node.ps1 同一套路：只保证"runner 进程在岗"，
# 槽位调度 / 模拟器 / maa / 停机 全部由 runner 自己管（runner/runner.json 配置）。
#
# 由计划任务调用：
#   FleetRunner-AutoStart   AtLogon（重启/登录后立即恢复）
#   FleetRunner-Watchdog    每 10 分钟（进程意外退出兜底）
# ============================================================================
$ErrorActionPreference = "Continue"
$OpsDir = Split-Path -Parent $PSCommandPath
$Root   = Split-Path -Parent $OpsDir
$Exe    = Join-Path $Root "fleet.exe"
if (-not (Test-Path $Exe)) { $Exe = Join-Path $Root "dist\fleet.exe" }   # 仓库布局在 dist\，目标机布局在根目录
$Conf   = Join-Path $Root "runner\runner.json"
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
  } catch { }
}
if (Test-Path $stopFile) { RLog "runner.stop 在（人工停）→ 跳过"; exit 0 }

# 已在跑？（认进程身份：fleet.exe 且命令行是 runner 常驻；排除 -status/-once 这类一次性命令；
# 不靠单一 PID，防 PID 复用误判——2026-10-01 l-4 就是被 nvcontainer 顶了 PID）
$running = Get-CimInstance Win32_Process -Filter "Name='fleet.exe'" -ErrorAction SilentlyContinue |
  Where-Object {
    $_.CommandLine -and ($_.CommandLine -match '\brunner\b') -and
    ($_.CommandLine -notmatch '-(status|once|now|enqueue|cancel|reset|enable|disable)\b')
  }
if ($running) { RLog ("runner 已在跑（pid=" + (@($running)[0].ProcessId) + "），跳过"); exit 0 }

# 拉起（WMI：脱离本进程树，计划任务/ssh 会话结束也不影响）
$cmd = '"' + $Exe + '" runner -conf "' + $Conf + '"'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
  CommandLine      = $cmd
  CurrentDirectory = $Root
}
RLog ("runner 已拉起 pid=" + $r.ProcessId + " rc=" + $r.ReturnValue)
