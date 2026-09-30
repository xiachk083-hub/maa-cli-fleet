# ============================================================================
# install_target.ps1 —— 在【目标机】上布点机端（node/executor）
#
# 做四件事：① 从 GitHub 拉最新项目包（免慢速 ssh 传输）
#           ② 解压到项目目录（保留本地配置：fleet.local.json / node\conf.json 都不在包里）
#           ③ 写配置：config\fleet.local.json（本机模式）+ node\conf.json（机端）
#           ④ adb connect 各模拟器 + 用 WMI 拉起机端常驻（可脱离 ssh 会话存活）
#
# 用法（在目标机上跑；也可从别处 ssh 进来跑）：
#   .\install_target.ps1 -NodeId host-mrfz0000 -CenterUrl http://10.0.0.1:8790 -Token <token>
#   .\install_target.ps1 ... -MachinesJson C:\fleet_machines.json      # 自定义机器表
#   .\install_target.ps1 ... -SkipNodeStart                            # 只布点不起机端
#
# 机器表 JSON 形如：[{"name":"l-1","emu":"25","state":"state_l1",
#                    "rogue":"rogue_sami_l1","daily":"daily_l1","local":"17184"}, ...]
# ============================================================================
param(
  [Parameter(Mandatory=$true)][string]$NodeId,
  [Parameter(Mandatory=$true)][string]$CenterUrl,
  [Parameter(Mandatory=$true)][string]$Token,
  [string]$ProjectDir = "E:\maa-cli-fleet",
  [string]$AdbPath    = "E:\MuMu Player 12\shell\adb.exe",
  [string]$MaaExe     = "",
  [string]$MumuManager = "E:\MuMu Player 12\nx_main\MuMuManager.exe",
  [string]$MachinesJson = "",
  [string]$RepoZip    = "https://codeload.github.com/xiachk083-hub/maa-cli-fleet/zip/refs/heads/master",
  [switch]$SkipNodeStart
)
$ErrorActionPreference = "Continue"
$ProgressPreference = "SilentlyContinue"

function Step($m) { Write-Host ("[install] " + $m) }

# ---- 默认机器表（按目标机实测端口改） --------------------------------------
$DefaultMachines = @(
  [pscustomobject]@{ name="l-1"; emu="25"; state="state_l1"; rogue="rogue_sami_l1";    daily="daily_l1"; local="17184" },
  [pscustomobject]@{ name="l-2"; emu="28"; state="state_l2"; rogue="rogue_sarkaz_l2";  daily="daily_l2"; local="17280" },
  [pscustomobject]@{ name="l-4"; emu="9";  state="state_l4"; rogue="rogue_mizuki_l4";  daily="daily_l4"; local="16672" },
  [pscustomobject]@{ name="l-5"; emu="34"; state="state_l5"; rogue="rogue_mizuki_l5";  daily="daily_l5"; local="16452" },
  [pscustomobject]@{ name="l-7"; emu="52"; state="state_l7"; rogue="rogue_mizuki_l7";  daily="daily_l7"; local="17028" }
)
$machines = $DefaultMachines
if ($MachinesJson -and (Test-Path $MachinesJson)) {
  $machines = Get-Content $MachinesJson -Raw -Encoding UTF8 | ConvertFrom-Json
}
if (-not $MaaExe) {
  # 同机已有 maa-cli 就直接复用（避免搬 8MB+ 可执行文件）
  foreach ($cand in @("$ProjectDir\bin\maa.exe", "E:\MAA-CLI\bin\maa.exe")) {
    if (Test-Path $cand) { $MaaExe = $cand; break }
  }
}
Step ("node=$NodeId center=$CenterUrl adb=$AdbPath maa=$MaaExe 机数=$(@($machines).Count)")

# ---- ① 拉包 ----------------------------------------------------------------
if (-not (Test-Path $ProjectDir)) { New-Item -ItemType Directory -Path $ProjectDir -Force | Out-Null }
$zip = Join-Path $ProjectDir "fleet.zip"
Invoke-WebRequest -Uri $RepoZip -OutFile $zip -UseBasicParsing
Step ("已下载 " + (Get-Item $zip).Length + " 字节")

# ---- ② 解压（覆盖代码，不动本地配置） --------------------------------------
$x = Join-Path $ProjectDir "_x"
Expand-Archive -Path $zip -DestinationPath $x -Force
$inner = (Get-ChildItem $x -Directory | Select-Object -First 1).FullName
Copy-Item "$inner\*" $ProjectDir -Recurse -Force
Remove-Item $x -Recurse -Force
Remove-Item $zip -Force
Step "已解压到 $ProjectDir"

# ---- ③ 写配置 ---------------------------------------------------------------
$prof = Join-Path $ProjectDir "config\profiles\default.toml"
if (Test-Path $prof) {
  (Get-Content $prof -Raw -Encoding UTF8) -replace 'adb_path\s*=\s*"[^"]*"', ('adb_path = "' + ($AdbPath -replace '\\','/') + '"') |
    Set-Content $prof -Encoding UTF8
  Step "profile.adb_path = $AdbPath"
}
# 资源目录（maa-cli 按 MAA_DATA_DIR 找 lib/resource；缺失会报 "Resource directory not found!"）
# 目标机通常已有现成的（安装过 maa-cli 的机器），用 junction 指过去，别复制几百 MB。
$dataDir = Join-Path $ProjectDir "data"
if (-not (Test-Path $dataDir)) { New-Item -ItemType Directory -Path $dataDir -Force | Out-Null }
$resCands = @(
  (Join-Path $env:APPDATA "loong\maa\dataesource"),
  (Join-Path $ProjectDir "coreesource"),
  (Join-Path (Split-Path -Parent $MaaExe) "..\coreesource")
)
$libCands = @(
  (Join-Path $env:APPDATA "loong\maa\data\lib"),
  (Join-Path $ProjectDir "core\data\lib"),
  (Join-Path (Split-Path -Parent $MaaExe) "..\core\data\lib")
)
foreach ($pair in @(@{n="resource";c=$resCands}, @{n="lib";c=$libCands})) {
  $dst = Join-Path $dataDir $pair.n
  if (Test-Path $dst) { Step ("data\" + $pair.n + " 已存在"); continue }
  $src = $null
  foreach ($c in $pair.c) { if ($c -and (Test-Path $c)) { $src = (Resolve-Path $c).Path; break } }
  if ($src) {
    cmd /c "mklink /J `"$dst`" `"$src`"" | Out-Null
    Step ("data\" + $pair.n + " -> " + $src + "（junction）")
  } else {
    Step ("警告：找不到 " + $pair.n + " 的来源，maa 可能报 Resource directory not found")
  }
}

$localConf = [pscustomobject]@{
  localMode = $true
  adbPath   = $AdbPath
  maaExe    = $MaaExe
  mumuManager = $MumuManager
  machines  = $machines
}
$localConf | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $ProjectDir "config\fleet.local.json") -Encoding UTF8

$nodeConf = [pscustomobject]@{
  nodeId      = $NodeId
  centerUrl   = $CenterUrl
  token       = $Token
  opsScript   = (Join-Path $ProjectDir "ops\rogue_cli_ops.ps1")
  logFile     = (Join-Path $ProjectDir "ops\rogue_cli_ops.log")
  heartbeatSec = 30
  stateSec     = 60
}
$nodeConf | ConvertTo-Json -Depth 4 | Set-Content (Join-Path $ProjectDir "node\conf.json") -Encoding UTF8
Step "已写 fleet.local.json + node\conf.json"

# ---- ④ adb connect + 起机端 -------------------------------------------------
foreach ($m in @($machines)) {
  if ($m.local) { & $AdbPath connect "127.0.0.1:$($m.local)" 2>$null | Out-Null }
}
Step ("adb 已连 " + (@($machines).Count) + " 台")

if (-not $SkipNodeStart) {
  Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" |
    Where-Object { $_.CommandLine -like "*fleet_node.ps1*" } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
  Remove-Item (Join-Path $ProjectDir "node\node.pid") -Force -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 2
  $cmd = 'powershell -NoProfile -ExecutionPolicy Bypass -File "' + (Join-Path $ProjectDir "node\fleet_node.ps1") + '" run'
  $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmd }
  Step ("机端已拉起 pid=" + $r.ProcessId + "（node.log 可查）")
}
Step "完成"
