# ============================================================================
# rogue_cli_ops.ps1 — CLI 肉鸽机队·主动运维（手动触发，无定时、无 fz-maa 兜底）
#
# 用法（本机 PowerShell）：
#   powershell -ExecutionPolicy Bypass -File D:\maa-cli-fleet\ops\rogue_cli_ops.ps1 status
#       → 体检四台：maa 进程 / asst.log 新鲜度 / 游戏进程 / 隧道
#   powershell -ExecutionPolicy Bypass -File D:\maa-cli-fleet\ops\rogue_cli_ops.ps1 fix1 l-1
#       → 阶梯①：重发任务（kill 卡住的 maa → 重新发车）
#   ... fix2 l-1   → 阶梯②：①不行则 关游戏重开（StartUp 自动拉起）+ 重发
#   ... fix3 l-1   → 阶梯③：②不行则 重启模拟器 + 重建隧道 + 重发（端口漂移自动处理）
#   ... fix  l-1   → ①→②→③ 自动升级；  fix all → 四台逐个跑阶梯
#
# 判据：state_lX/debug/asst.log 最后修改 < StaleMin(默认3分钟) = 有进展；否则判卡。
# ============================================================================
param(
  [Parameter(Position=0)][string]$Cmd = "status",
  [Parameter(Position=1)][string]$Target = "all",
  [Parameter(Position=2)][string]$TaskKind = "",     # 可选：daily / rogue（指定修复哪个功能）
  [int]$StaleMin = 3
)

$ErrorActionPreference = "Continue"
# ── 项目根自定位：脚本在 <root>\ops\ 下，整个项目可整体搬移 ──────────────
$OpsDir    = Split-Path -Parent $PSCommandPath
$RootDir   = Split-Path -Parent $OpsDir
$MaaExe    = Join-Path $RootDir "bin\maa.exe"
$StateRoot = Join-Path $RootDir "data"
$LogDir    = Join-Path $OpsDir "logs"
$Adb       = Join-Path $RootDir "bin\adb\adb.exe"
# ── 连接信息从本地配置读取（config\fleet.local.json，不入库；模板 config\fleet.example.json）──
$SshKey  = "~/.ssh/id_ed25519"
$SshHost = "user@host"
$FleetLocal = Join-Path $RootDir "config\fleet.local.json"
$LocalMode = $false          # true = 本机模式：脚本就跑在目标机上，直连模拟器（不走 ssh/隧道）
$MaaExeOverride = ""
$MachinesOverride = $null
if (Test-Path $FleetLocal) {
  try {
    $fl = Get-Content $FleetLocal -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($fl.sshKey)  { $SshKey  = [string]$fl.sshKey }
    if ($fl.sshHost) { $SshHost = [string]$fl.sshHost }
    if ($fl.localMode) { $LocalMode = [bool]$fl.localMode }
    if ($fl.adbPath)   { $Adb = [string]$fl.adbPath }
    if ($fl.maaExe)    { $MaaExeOverride = [string]$fl.maaExe }
    if ($fl.machines)  { $MachinesOverride = $fl.machines }
  } catch { }
}
if ($MaaExeOverride) { $MaaExe = $MaaExeOverride }
$OpsLog    = Join-Path $OpsDir "rogue_cli_ops.log"
# ── maa-cli 三目录全部重定向到项目内（config/data/cache）──────────────────
$env:MAA_CONFIG_DIR = Join-Path $RootDir "config"
$env:MAA_DATA_DIR   = Join-Path $RootDir "data"
$env:MAA_CACHE_DIR  = Join-Path $RootDir "data\cache"
if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

# ── 服务器/包名：按机解析（配置优先 → 客户端映射 → adb 探测）────────────────
$ClientPkg = @{
  'Official' = 'com.hypergryph.arknights'
  'Bilibili' = 'com.hypergryph.arknights.bilibili'
  'YoStarEN' = 'com.YoStarEN.Arknights'
  'YoStarJP' = 'com.YoStarJP.Arknights'
  'YoStarKR' = 'com.YoStarKR.Arknights'
  'txwy'     = 'com.wayi.arknights'
}
$script:PkgCache = @{}
function Get-GamePkg($m) {
  if ($m.Pkg) { return [string]$m.Pkg }
  if ($script:PkgCache.ContainsKey($m.Name)) { return $script:PkgCache[$m.Name] }
  $c = if ($m.Client -and $ClientPkg.ContainsKey([string]$m.Client)) { $ClientPkg[[string]$m.Client] } else { 'com.YoStarJP.Arknights' }
  try {
    $out = (& $Adb -s "127.0.0.1:$($m.Local)" shell "pm list packages" 2>$null | Out-String)
    $hits = [regex]::Matches($out, "(?i)package:(com\.[a-z0-9._]*arknights[a-z0-9._]*)") | ForEach-Object { $_.Groups[1].Value }
    if ($hits -and ($hits -notcontains $c) -and @($hits).Count -eq 1) { $c = $hits[0] }
  } catch { }
  $script:PkgCache[$m.Name] = $c
  return $c
}

# 自愈巡检节奏（秒）：能检测到卡死/掉线就立刻治，不拖到下一轮；本机模式可更激进
$HealthSec = 30
if ($fl -and $fl.healthSec) { $HealthSec = [int]$fl.healthSec }

$Machines = @(
  [pscustomobject]@{ Name="l-1"; Emu="25"; State="state_l1"; Rogue="rogue_sami_l1";    Daily="daily_l1"; Local="16522"; Fallback="1-7"; Client="YoStarJP"; Pkg=""; Profile="" },
  [pscustomobject]@{ Name="l-4"; Emu="9";  State="state_l4"; Rogue="rogue_mizuki_l4";  Daily="daily_l4"; Local="16524"; Fallback="1-7"; Client="YoStarJP"; Pkg=""; Profile="" },
  [pscustomobject]@{ Name="l-2"; Emu="28"; State="state_l2"; Rogue="rogue_sarkaz_l2";  Daily="daily_l2"; Local="16523"; Fallback="1-7"; Client="YoStarJP"; Pkg=""; Profile="" },
  [pscustomobject]@{ Name="l-5"; Emu="34"; State="state_l5"; Rogue="rogue_mizuki_l5";  Daily="daily_l5"; Local="16520"; Fallback="1-7"; Client="YoStarJP"; Pkg=""; Profile="" },
  [pscustomobject]@{ Name="l-7"; Emu="52"; State="state_l7"; Rogue="rogue_mizuki_l7";  Daily="daily_l7"; Local="16521"; Fallback="1-7"; Client="YoStarJP"; Pkg=""; Profile="" }
)
if ($MachinesOverride) {
  # 机器表可被 config\fleet.local.json 的 machines 覆盖（本机模式/其它部署）
  $Machines = @($MachinesOverride | ForEach-Object {
    [pscustomobject]@{ Name=[string]$_.name; Emu=[string]$_.emu; State=[string]$_.state
                       Rogue=[string]$_.rogue; Daily=[string]$_.daily; Local=[string]$_.local
                       Fallback=$(if ($_.fallback) { [string]$_.fallback } else { "1-7" })
                       Client=$(if ($_.client) { [string]$_.client } else { "YoStarJP" })
                       Pkg=[string]$_.pkg; Profile=[string]$_.profile }
  })
}

function Log($msg) {
  $line = "[" + (Get-Date -Format "MM-dd HH:mm:ss") + "] " + $msg
  Write-Host $line
  Add-Content -Path $OpsLog -Value $line -Encoding UTF8
}

function Invoke-HostPs([string]$Script, [int]$TimeoutSec = 90) {
  $b64 = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($Script))
  if ($LocalMode) {
    # 本机模式：目标机就是本机，"主机侧"操作直接本地跑
    $out = & powershell -NoProfile -EncodedCommand $b64 2>&1
    return ($out | Out-String)
  }
  $out = & ssh -i $SshKey -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=15 $SshHost "powershell -NoProfile -EncodedCommand $b64" 2>&1
  return ($out | Out-String)
}

function Get-MaaProc($m) {
  # 按设备端口认领（不依赖当前跑的是日常还是肉鸽任务）
  $needle = [regex]::Escape("-a 127.0.0.1:" + $m.Local)
  Get-CimInstance Win32_Process -Filter "Name='maa.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -and ($_.CommandLine -match $needle) }
}

# 生成"兜底关卡"日常任务文件（对应 AUTO-MAS 的 Stage_Remain：主关卡不开/不可用 → 改刷兜底关）
function New-FallbackTask($m, [string]$Stage) {
  $src = Join-Path $RootDir ("config\tasks\{0}.toml" -f $m.Daily)
  if (-not (Test-Path $src)) { return $null }
  $body = Get-Content $src -Raw -Encoding UTF8
  $body = [regex]::Replace($body, '(stage\s*=\s*)"[^"]*"', ('$1"' + $Stage + '"'))
  $name = $m.Daily + "_fb"
  $dst = Join-Path $RootDir ("config\tasks\{0}.toml" -f $name)
  [System.IO.File]::WriteAllText($dst, $body, (New-Object System.Text.UTF8Encoding($false)))
  return $name
}

# 事件级等待：有任务在跑就"等它退出"（进程退出=结束通知，零延迟唤醒）；没任务/超时则立即返回
function Wait-MaaSignal($m, [int]$TimeoutSec = 10) {
  $w = Get-MaaProc $m
  if (-not $w) { return "none" }
  try { $p = [System.Diagnostics.Process]::GetProcessById([int]$w.ProcessId) } catch { return "none" }
  try { if ($p.WaitForExit($TimeoutSec * 1000)) { return "exited" } } catch { }
  return "timeout"
}

function Get-CurrentTask($m) {
  # 从命令行解析该机当前任务名（daily_lX / rogue_*）
  $proc = Get-CimInstance Win32_Process -Filter "Name='maa.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match [regex]::Escape("-a 127.0.0.1:" + $m.Local) }
  if ($proc -and $proc.CommandLine -match '--batch run (\S+)') { return $Matches[1] }
  return $null
}

function Resolve-Task($m) {
  # 修复目标：显式指定 > 当前正在跑的同类任务 > 默认肉鸽
  if ($TaskKind -eq "daily") { return $m.Daily }
  if ($TaskKind -eq "rogue") { return $m.Rogue }
  $cur = Get-CurrentTask $m
  if ($cur) { return $cur }
  return $m.Rogue
}

function Get-LogAgeMin($m) {
  $p = Join-Path $StateRoot ("$($m.State)\debug\asst.log")
  if (-not (Test-Path $p)) { return 9999 }
  return [math]::Round(((Get-Date) - (Get-Item $p).LastWriteTime).TotalMinutes, 1)
}

function Test-Tunnel($m) {
  if ($LocalMode) {
    # 本机模式：无隧道，判据 = adb 已连上该模拟器（device 状态）
    $dev = (& $Adb devices 2>$null | Out-String)
    return [bool]($dev -match ([regex]::Escape("127.0.0.1:$($m.Local)") + "\s+device"))
  }
  return [bool](Get-NetTCPConnection -LocalPort $m.Local -State Listen -ErrorAction SilentlyContinue)
}

function Get-GamePid($m) {
  $r = (& $Adb -s "127.0.0.1:$($m.Local)" shell ("pidof " + (Get-GamePkg $m)) 2>$null | Out-String).Trim()
  return $r
}

function Show-Status {
  Log "===== 体检 ====="
  foreach ($m in $Machines) {
    $proc  = Get-MaaProc $m
    $age   = Get-LogAgeMin $m
    $tun   = Test-Tunnel $m
    $game  = if ($tun) { Get-GamePid $m } else { "(隧道断)" }
    $health = if ($proc -and $age -lt $StaleMin -and $tun -and $game) { "OK" } else { "!!需处理" }
    Log ("{0} | maa={1} | 日志年龄={2}分 | 隧道={3} | 游戏={4} | {5}" -f `
      $m.Name, $(if ($proc) { "pid$($proc.ProcessId)" } else { "无" }), $age, `
      $(if ($tun) { "通" } else { "断" }), $(if ($game) { $game } else { "无" }), $health)
  }
}

function Start-Task($m, $TaskName) {
  $env:MAA_STATE_DIR = Join-Path $StateRoot $m.State
  $outFile = Join-Path $LogDir ("{0}_{1}.out" -f $TaskName, (Get-Date -Format "MMdd_HHmmss"))
  $workDir = if ($LocalMode) { Split-Path -Parent $MaaExe } else { Join-Path $RootDir "bin" }
  $mArgs = @('--batch','run',$TaskName,'-a',"127.0.0.1:$($m.Local)")
  if ($m.Profile) { $mArgs += @('-p', [string]$m.Profile) }
  $p = Start-Process -FilePath $MaaExe `
    -ArgumentList $mArgs `
    -WorkingDirectory $workDir -RedirectStandardOutput $outFile -WindowStyle Hidden -PassThru
  Log ("{0} 已发车 pid={1} (任务 {2}, 日志 {3})" -f $m.Name, $p.Id, $TaskName, $outFile)
  return [pscustomobject]@{ Pid = $p.Id; Out = $outFile }
}

function Wait-TaskEnd($m, [int]$TimeoutMin = 90) {
  $deadline = (Get-Date).AddMinutes($TimeoutMin)
  $i = 0
  while ((Get-Date) -lt $deadline) {
    $sig = Wait-MaaSignal $m 20                       # 任务退出 → 立即知道
    if ($sig -eq "none" -or $sig -eq "exited") { Start-Sleep -Seconds 3; return $true }
    $i++
    if (($i % 2) -eq 0) { Watch-Once $m | Out-Null }   # 等的同时也守着（~40s）
  }
  return $false
}

# ---------- 卡死自检与自愈（弹窗/异常画面/进程消失/连环报错） ----------
$script:StuckFixCount = @{}
$script:StuckGiveUpAt = @{}

function Test-Stuck($m) {
  # 返回异常原因（字符串）或 $null=健康
  $proc = Get-MaaProc $m
  if (-not $proc) { return "no-task（任务已不在）" }
  $age = Get-LogAgeMin $m
  if ($age -gt 4) { return ("log-stale（日志 {0} 分未更新）" -f $age) }
  $log = Join-Path $StateRoot ("$($m.State)\debug\asst.log")
  if (-not (Test-Path $log)) { return "no-log" }
  $cut = (Get-Date).AddMinutes(-10)
  $n = 0
  foreach ($ln in (Get-Content -Path $log -Tail 4000 -Encoding UTF8 -ErrorAction SilentlyContinue)) {
    if ($ln -match 'TaskChainError') {
      $mm = [regex]::Match($ln, '\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)')
      if ($mm.Success) {
        $ts = [datetime]::ParseExact($mm.Groups[1].Value, 'yyyy-MM-dd HH:mm:ss', $null)
        if ($ts -gt $cut) { $n++ }
      }
    }
  }
  if ($n -ge 2) { return ("error-loop（10 分钟内 {0} 次 TaskChainError）" -f $n) }
  return $null
}

function Watch-Once($m) {
  # 0) 设备层：隧道通 + adb 在线；否则设备级恢复（启动模拟器 + 重建隧道 + 发任务）
  $boot = ""
  try { $boot = (& $Adb -s "127.0.0.1:$($m.Local)" shell getprop sys.boot_completed 2>$null | Out-String).Trim() } catch {}
  if ($boot -ne "1") {
    $cnt0 = ([int]$script:StuckFixCount[$m.Name]) + 1
    $script:StuckFixCount[$m.Name] = $cnt0
    if ($cnt0 -le 3) {
      Log ("[{0}] 自检：设备不可达（boot={1}）→ 第 {2}/3 次设备级恢复（启动模拟器+隧道+发任务）" -f $m.Name, $boot, $cnt0)
      Recover-Machine $m
    } elseif ($cnt0 -eq 4) {
      Log ("[{0}] 自检：设备级恢复已尝试 3 次仍不可达 → 冷却 30 分钟后重试" -f $m.Name)
    }
    return $true
  }
  # 异常 → 关游戏重开 + 重发当前任务；连续 3 次仍异常 → 冷却 30 分钟后再试
  $why = Test-Stuck $m
  if (-not $why) { $script:StuckFixCount[$m.Name] = 0; $script:StuckGiveUpAt.Remove($m.Name) | Out-Null; return $false }
  $cnt = ([int]$script:StuckFixCount[$m.Name]) + 1
  if ($cnt -gt 3) {
    $give = $script:StuckGiveUpAt[$m.Name]
    if (-not $give) {
      $script:StuckGiveUpAt[$m.Name] = Get-Date
      Log ("[{0}] 自检连续异常（{1}）—— 自愈 3 次无效，冷却 30 分钟后再试（也可人工 fix {0}）" -f $m.Name, $why)
      return $false
    } elseif (((Get-Date) - $give).TotalMinutes -gt 30) {
      $cnt = 1; $script:StuckGiveUpAt.Remove($m.Name) | Out-Null
      Log ("[{0}] 自检：冷却结束，重新尝试自愈" -f $m.Name)
    } else { return $false }
  }
  $script:StuckFixCount[$m.Name] = $cnt
  Log ("[{0}] 自检异常：{1} → 第 {2}/3 次自愈（关游戏重开+重发当前任务）" -f $m.Name, $why, $cnt)
  $task = Get-CurrentTask $m
  if (-not $task) { $task = $m.Rogue }
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Start-Sleep -Seconds 3
  Start-Task $m $task | Out-Null
  return $true
}

function Test-OutError($outFile) {
  # summary 行形如 "[任务名] 10:00:00 - 10:05:00 (5m) Error"
  if (-not (Test-Path $outFile)) { return $true }
  $txt = Get-Content -Path $outFile -Raw -Encoding UTF8
  return [bool]($txt -match '\]\s+Error')
}

# 游戏日（以 04:00 为界）：用于判断"今日日常是否已完成"
function Get-GameDay { return (Get-Date).AddHours(-4).ToString('yyyy-MM-dd') }

function Test-DailyDone($m) {
  $f = Join-Path $OpsDir ("dailydate_{0}.txt" -f $m.Name)
  if (-not (Test-Path $f)) { return $false }
  return ((Get-Content $f -Raw).Trim() -eq (Get-GameDay))
}

function Set-DailyDone($m) {
  $f = Join-Path $OpsDir ("dailydate_{0}.txt" -f $m.Name)
  (Get-GameDay) | Out-File -FilePath $f -Encoding ascii
  Log ("[{0}] 日常完成标记：游戏日 {1}" -f $m.Name, (Get-GameDay))
}

function Get-SanityNow($m) {
  # 从 state 日志最后一条 sanity 读数 + 时间推算当前值（恢复 1 点/6 分钟）
  $log = Join-Path $StateRoot ("$($m.State)\debug\asst.log")
  if (-not (Test-Path $log)) { return @{ cur = 0; max = 135; src = "default" } }
  $pat = '\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\.\d+\].*"current_sanity":(\d+),"max_sanity":(\d+)'
  $last = Select-String -Path $log -Pattern $pat -AllMatches -Encoding UTF8 -ErrorAction SilentlyContinue | Select-Object -Last 1
  if (-not $last) { return @{ cur = 0; max = 135; src = "default" } }
  $m2 = [regex]::Match($last.Line, $pat)
  $ts = [datetime]::ParseExact($m2.Groups[1].Value, 'yyyy-MM-dd HH:mm:ss', $null)
  $cur0 = [int]$m2.Groups[2].Value
  $max = [int]$m2.Groups[3].Value
  # 若读数之后又完成过一场 Fight（理智已被清空）→ 从该链结束时刻按 0 重算
  $fpat = '\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\.\d+\].*TaskChainCompleted \{"taskchain":"Fight"'
  $fight = Select-String -Path $log -Pattern $fpat -AllMatches -Encoding UTF8 -ErrorAction SilentlyContinue | Select-Object -Last 1
  if ($fight) {
    $mf = [regex]::Match($fight.Line, $fpat)
    $tsF = [datetime]::ParseExact($mf.Groups[1].Value, 'yyyy-MM-dd HH:mm:ss', $null)
    if ($tsF -gt $ts) {
      $est = [math]::Min($max, [math]::Floor(((Get-Date) - $tsF).TotalMinutes / 6))
      return @{ cur = $est; max = $max; src = ("postFight@" + $tsF.ToString('MM-dd HH:mm')) }
    }
  }
  $elapsedMin = ((Get-Date) - $ts).TotalMinutes
  $est = [math]::Min($max, $cur0 + [math]::Floor($elapsedMin / 6))
  return @{ cur = $est; max = $max; src = ("log@" + $ts.ToString('MM-dd HH:mm')) }
}

# 「循环」：肉鸽 ↔ 日常 —— 等到理智快回满（上限×6min − 余量）→ 跑日常 → 回肉鸽 → 循环
function Cycle-Worker($m, [int]$MarginMin = 30) {
  $pidFile  = Join-Path $OpsDir ("cycle_{0}.pid"  -f $m.Name)
  $stopFile = Join-Path $OpsDir ("cycle_{0}.stop" -f $m.Name)
  if (Test-Path $stopFile) { Remove-Item $stopFile -Force }
  ("$PID") | Out-File -FilePath $pidFile -Encoding ascii
  Log ("[{0}] cycle 启动（pid={1}，余量 {2}min）" -f $m.Name, $PID, $MarginMin)
  # 启动守卫：若此刻正在跑日常，先等它自然结束（不打断当天日常）
  $curTask = Get-CurrentTask $m
  if ($curTask -eq $m.Daily) {
    Log ("[{0}] cycle：检测到日常在跑（{1}），等它结束…" -f $m.Name, $curTask)
    Wait-TaskEnd $m 90 | Out-Null
  }
  # 若此刻没有任何任务在跑（含刚跑完日常的情况）→ 先起肉鸽，避免空等
  if (-not (Get-MaaProc $m)) {
    Log ("[{0}] cycle：当前无任务 → 先起肉鸽（{1}）" -f $m.Name, $m.Rogue)
    Stop-MaaQuiet $m
    & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
    Start-Sleep -Seconds 2
    Start-Task $m $m.Rogue | Out-Null
  }
  $firstIter = $true
  $cap = 135
  while (-not (Test-Path $stopFile)) {
    if ($firstIter) {
      $s = Get-SanityNow $m
      $cur = $s.cur; $cap = $s.max; $src = $s.src
    } else {
      # 刚跑完日常（Fight 把理智打到 ≤5）→ 按 0 起算
      $cur = 0; $src = "daily-end"
    }
    $firstIter = $false
    if (-not (Test-DailyDone $m)) {
      $waitMin = 0
      Log ("[{0}] cycle：本游戏日（{1}）日常尚未完成 → 立即先跑日常" -f $m.Name, (Get-GameDay))
    } else {
      $waitMin = [int](($cap - $cur) * 6 - $MarginMin)
      if ($waitMin -lt 5) { $waitMin = 5 }
      Log ("[{0}] cycle：理智≈{1}/{2}（{3}）→ 等待 {4:N1} 小时后下次日常" -f $m.Name, $cur, $cap, $src, ($waitMin / 60))
    }
    $end = (Get-Date).AddMinutes($waitMin)
    while ((Get-Date) -lt $end -and -not (Test-Path $stopFile)) {
      $sig = Wait-MaaSignal $m $HealthSec   # 任务退出=立刻醒；没任务/超时才走下一轮
      if (Test-Path $stopFile) { break }
      Watch-Once $m | Out-Null              # 退出/卡死 → 立刻重发（不等下一轮）
    }
    if (Test-Path $stopFile) { break }
    # 日常：① 原关卡 → ② 失败则换兜底关卡重跑 → ③ 再失败计数，连续 3 次冷却 60 分钟（期间跑肉鸽）
    if (-not $script:DailyFailCount) { $script:DailyFailCount = @{} }
    $dailyOk = $false
    $stages = @($null)
    if ($m.Fallback) { $stages += $m.Fallback }
    foreach ($stage in $stages) {
      if (Test-Path $stopFile) { break }
      $taskName = $m.Daily
      if ($stage) {
        $taskName = New-FallbackTask $m $stage
        if (-not $taskName) { break }
        Log ("[{0}] cycle：日常改刷兜底关卡 {1}（{2}）" -f $m.Name, $stage, $taskName)
      } else {
        Log ("[{0}] cycle：开跑日常（{1}）" -f $m.Name, $taskName)
      }
      Stop-MaaQuiet $m
      & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
      Start-Sleep -Seconds 2
      $r = Start-Task $m $taskName
      if (-not (Wait-TaskEnd $m 90)) { Log ("[{0}] cycle：日常超时（{1}），跳过本轮" -f $m.Name, $taskName); continue }
      if (-not (Test-OutError $r.Out)) { $dailyOk = $true; break }
      Log ("[{0}] cycle：日常报错（{1}）" -f $m.Name, $taskName)
    }
    if (-not $dailyOk) {
      $script:DailyFailCount[$m.Name] = ([int]$script:DailyFailCount[$m.Name]) + 1
      $n = [int]$script:DailyFailCount[$m.Name]
      Log ("[{0}] cycle：日常本轮失败（第 {1} 次）" -f $m.Name, $n)
      if ($n -ge 3) {
        Log ("[{0}] cycle：日常连续失败 3 次 → 冷却 60 分钟（先回肉鸽，稍后再试；若是活动关已关请改关卡）" -f $m.Name)
        Stop-MaaQuiet $m
        & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
        Start-Sleep -Seconds 2
        Start-Task $m $m.Rogue | Out-Null
        $co = (Get-Date).AddMinutes(60)
        while ((Get-Date) -lt $co -and -not (Test-Path $stopFile)) { Start-Sleep -Seconds $HealthSec; Watch-Once $m | Out-Null }
        $script:DailyFailCount[$m.Name] = 0
      }
      continue
    }
    $script:DailyFailCount[$m.Name] = 0
    Set-DailyDone $m
    # 回肉鸽
    Log ("[{0}] cycle：日常完成 → 关游戏 → 回肉鸽（{1}）" -f $m.Name, $m.Rogue)
    Stop-MaaQuiet $m
    & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
    Start-Sleep -Seconds 2
    Start-Task $m $m.Rogue | Out-Null
  }
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
  Log ("[{0}] cycle 已停止（stop 标志存在）" -f $m.Name)
}

# 「重启恢复」：主机重启后找回机队——按实例名定位 index → 确保运行 → 重建隧道 → 发任务
function Get-InstanceIndexByName([string]$prefix) {
  $raw = Invoke-HostPs '& "E:\MuMu Player 12\nx_main\MuMuManager.exe" info -v all 2>&1 | Out-String'
  $curIdx = ""
  foreach ($ln in ($raw -split "`r?`n")) {
    if ($ln -match '"index":\s*"(\d+)"') { $curIdx = $Matches[1] }
    if ($ln -match ('"name":\s*"' + [regex]::Escape($prefix))) { return $curIdx }
  }
  return $null
}

function Recover-Machine($m) {
  Log ("[{0}] recover：查找实例（name 前缀 {0}-）…" -f $m.Name)
  $idx = Get-InstanceIndexByName ("$($m.Name)-")
  if (-not $idx) { Log ("[{0}] recover：MuMu 里找不到实例" -f $m.Name); return }
  Log ("[{0}] recover：实例 index = {1}" -f $m.Name, $idx)
  $info = Invoke-HostPs ("& 'E:\MuMu Player 12\nx_main\MuMuManager.exe' info -v $idx | Out-String")
  if ($info -notmatch '"is_process_started":\s*true') {
    $launchPs = @'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = '"E:\MuMu Player 12\nx_main\MuMuManager.exe" control --vmindex __IDX__ launch' }
Write-Output ('LAUNCH_PID=' + $r.ProcessId)
'@
    $launchPs = $launchPs.Replace('__IDX__', "$idx")
    Invoke-HostPs $launchPs | Out-Null
    Log ("[{0}] recover：已发启动（index {1}）" -f $m.Name, $idx)
  }
  # 主机侧：自动查 adb 端口 + 自动 adb connect（主机重启后必须重连）
  $hostPort = ""
  for ($i = 0; $i -lt 108 -and -not $hostPort; $i++) {
    Start-Sleep -Seconds 5
    $inf = Invoke-HostPs ("& 'E:\MuMu Player 12\nx_main\MuMuManager.exe' info -v $idx | Out-String")
    $mp = [regex]::Match($inf, '"adb_port":\s*(\d+)')
    if ($mp.Success) {
      $cand = $mp.Groups[1].Value
      $bootPs = "& 'E:\MuMu Player 12\shell\adb.exe' connect 127.0.0.1:$cand | Out-Null; (& 'E:\MuMu Player 12\shell\adb.exe' -s 127.0.0.1:$cand shell getprop sys.boot_completed)"
      if (((Invoke-HostPs $bootPs).Trim()) -eq "1") { $hostPort = $cand }
    }
  }
  if (-not $hostPort) { Log ("[{0}] recover：模拟器未就绪，未接隧道" -f $m.Name); return }
  Log ("[{0}] recover：adb 端口 = {1}" -f $m.Name, $hostPort)
  if ($LocalMode) {
    # 本机模式：无隧道；直接 adb connect（实测端口优先，防端口漂移）
    if ($hostPort -ne $m.Local) { Log ("[{0}] recover：端口漂移 配置={1} 实测={2}（本次按实测走）" -f $m.Name, $m.Local, $hostPort); $m.Local = [string]$hostPort }
    & $Adb connect "127.0.0.1:$($m.Local)" 2>$null | Out-Null
    Start-Sleep -Seconds 2
    $lboot = (& $Adb -s "127.0.0.1:$($m.Local)" shell getprop sys.boot_completed 2>$null | Out-String).Trim()
    Log ("[{0}] recover：本机直连 127.0.0.1:{1}（adb boot={2}）" -f $m.Name, $m.Local, $lboot)
  } else {
    $old = Get-NetTCPConnection -LocalPort $m.Local -State Listen -ErrorAction SilentlyContinue
    if ($old) { Stop-Process -Id $old.OwningProcess -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 2 }
    Start-Process ssh -ArgumentList '-i',$SshKey,'-o','StrictHostKeyChecking=no','-N','-L',"127.0.0.1:$($m.Local):127.0.0.1:$hostPort",$SshHost -WindowStyle Hidden | Out-Null
    Start-Sleep -Seconds 4
    # 本机侧：自动 adb connect 到隧道口（本机 adb 也可能丢掉设备）
    & $Adb connect "127.0.0.1:$($m.Local)" 2>$null | Out-Null
    Start-Sleep -Seconds 2
    $lboot = (& $Adb -s "127.0.0.1:$($m.Local)" shell getprop sys.boot_completed 2>$null | Out-String).Trim()
    Log ("[{0}] recover：隧道本机 {1} -> 主机 {2}（本机 adb boot={3}）" -f $m.Name, $m.Local, $hostPort, $lboot)
  }
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Start-Sleep -Seconds 2
  Start-Task $m $m.Rogue | Out-Null
}

# 「看守」：只自愈，不跑日常（适合只刷肉鸽的机，如 l-5）
function Watch-Worker($m) {
  $pidFile  = Join-Path $OpsDir ("watch_{0}.pid"  -f $m.Name)
  $stopFile = Join-Path $OpsDir ("watch_{0}.stop" -f $m.Name)
  if (Test-Path $stopFile) { Remove-Item $stopFile -Force }
  ("$PID") | Out-File -FilePath $pidFile -Encoding ascii
  Log ("[{0}] watch 启动（pid={1}；只自愈，不跑日常）" -f $m.Name, $PID)
  while (-not (Test-Path $stopFile)) {
    $sig = Wait-MaaSignal $m $HealthSec
    if (Test-Path $stopFile) { break }
    Watch-Once $m | Out-Null
  }
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
  Log ("[{0}] watch 已停止" -f $m.Name)
}

# 「联接」：日常（独立功能）跑完 → 自动关游戏 → 起肉鸽（仍为独立任务文件）
function Chain-Worker($m) {
  Log ("[{0}] chain：先跑日常（{1}）" -f $m.Name, $m.Daily)
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Start-Sleep -Seconds 2
  $r = Start-Task $m $m.Daily
  if (-not (Wait-TaskEnd $m 90)) { Log ("[{0}] 日常超时未结束，chain 停止" -f $m.Name); return }
  if (Test-OutError $r.Out) {
    Log ("[{0}] 日常有错误 → 关游戏重试一次（按对应处理方式）" -f $m.Name)
    Stop-MaaQuiet $m
    & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
    Start-Sleep -Seconds 3
    $r = Start-Task $m $m.Daily
    if (-not (Wait-TaskEnd $m 90)) { Log ("[{0}] 日常重试超时，chain 停止" -f $m.Name); return }
    if (Test-OutError $r.Out) { Log ("[{0}] 日常重试仍报错，chain 停止（请用 fix {1} daily 处理）" -f $m.Name, $m.Name); return }
  }
  Set-DailyDone $m
  Log ("[{0}] 日常完成 → 关游戏 → 起肉鸽（{1}）" -f $m.Name, $m.Rogue)
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Start-Sleep -Seconds 2
  Start-Task $m $m.Rogue | Out-Null
  Log ("[{0}] chain 完成：肉鸽已起" -f $m.Name)
}

# 「日常」与「肉鸽」两个功能：启动前统一先关游戏（am force-stop），再拉起任务
function Start-One($m, $TaskName) {
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Log ("{0} 已关游戏（启动 {1} 前）" -f $m.Name, $TaskName)
  Start-Sleep -Seconds 2
  Start-Task $m $TaskName | Out-Null
}

function Stop-MaaQuiet($m) {
  $p = Get-MaaProc $m
  if ($p) {
    Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
    Log ("{0} 已停旧 maa pid={1}" -f $m.Name, ($p.ProcessId -join ","))
    Start-Sleep -Seconds 2
  }
}

function Wait-Fresh($m, [int]$TimeoutSec = 120, [datetime]$Since) {
  $deadline = (Get-Date).AddSeconds($TimeoutSec)
  $logPath = Join-Path $StateRoot ("$($m.State)\debug\asst.log")
  while ((Get-Date) -lt $deadline) {
    if (Get-MaaProc $m) {
      if (Test-Path $logPath) {
        if ((Get-Item $logPath).LastWriteTime -gt $Since) { return $true }   # 发车后有新写入 = 真推进
      }
    } else {
      Start-Sleep -Seconds 10
      if (-not (Get-MaaProc $m)) { return $false }   # 进程已退 = 失败
    }
    Start-Sleep -Seconds 10
  }
  return $false
}

function Fix-Step1($m, $task) {
  Log ("--- {0} 阶梯①：重发任务（{1}）---" -f $m.Name, $task)
  Stop-MaaQuiet $m
  $t0 = Get-Date
  Start-Task $m $task | Out-Null
  if (Wait-Fresh $m 150 $t0) { Log ("{0} ①成功：恢复推进" -f $m.Name); return $true }
  Log ("{0} ①未见推进" -f $m.Name); return $false
}

function Fix-Step2($m, $task) {
  Log ("--- {0} 阶梯②：关游戏重开 + 重发（{1}）---" -f $m.Name, $task)
  Stop-MaaQuiet $m
  & $Adb -s "127.0.0.1:$($m.Local)" shell ("am force-stop " + (Get-GamePkg $m)) 2>$null | Out-Null
  Log ("{0} 已关游戏" -f $m.Name)
  Start-Sleep -Seconds 3
  $t0 = Get-Date
  Start-Task $m $task | Out-Null
  if (Wait-Fresh $m 180 $t0) { Log ("{0} ②成功：恢复推进" -f $m.Name); return $true }
  Log ("{0} ②未见推进" -f $m.Name); return $false
}

function Fix-Step3($m, $task) {
  Log ("--- {0} 阶梯③：重启模拟器 + 重建隧道 + 重发（{1}）---" -f $m.Name, $task)
  Stop-MaaQuiet $m
  # 1) 关模拟器
  $shutPs = @'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = '"E:\MuMu Player 12\nx_main\MuMuManager.exe" control --vmindex __EMU__ shutdown' }
Write-Output ('SHUT_PID=' + $r.ProcessId)
'@
  $shutPs = $shutPs.Replace('__EMU__', "$($m.Emu)")
  Invoke-HostPs $shutPs | Out-Null
  Log ("{0} 模拟器 shutdown 已发" -f $m.Name)
  Start-Sleep -Seconds 15
  # 2) 启动模拟器
  $launchPs = @'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = '"E:\MuMu Player 12\nx_main\MuMuManager.exe" control --vmindex __EMU__ launch' }
Write-Output ('LAUNCH_PID=' + $r.ProcessId)
'@
  $launchPs = $launchPs.Replace('__EMU__', "$($m.Emu)")
  Invoke-HostPs $launchPs | Out-Null
  Log ("{0} 模拟器 launch 已发，等待就绪…" -f $m.Name)
  # 3) 找新 adb 端口（VMM 监听口里试连通）
  $portsPs = @'
Get-CimInstance Win32_Process -Filter "Name='MuMuVMMHeadless.exe'" | ForEach-Object {
  if ($_.CommandLine -match '12\.0-__EMU__\b') { (Get-NetTCPConnection -OwningProcess $_.ProcessId -State Listen).LocalPort }
}
'@
  $portsPs = $portsPs.Replace('__EMU__', "$($m.Emu)")
  $hostPort = ""
  for ($i = 0; $i -lt 24 -and -not $hostPort; $i++) {
    Start-Sleep -Seconds 5
    $ports = (Invoke-HostPs $portsPs).Trim() -split "\s+"
    foreach ($pp in $ports) {
      if ($pp -notmatch "^[0-9]+$") { continue }
      $bootPs = "& 'E:\MuMu Player 12\shell\adb.exe' -s 127.0.0.1:$pp shell getprop sys.boot_completed"
      $ok = (Invoke-HostPs $bootPs).Trim()
      if ($ok -eq "1") { $hostPort = $pp; break }
    }
  }
  if (-not $hostPort) { Log ("{0} ③失败：模拟器未就绪/端口未找到" -f $m.Name); return $false }
  Log ("{0} 模拟器就绪，adb 端口 = {1}" -f $m.Name, $hostPort)
  # 4) 重建隧道（杀掉监听该本地口的旧 ssh）
  $old = Get-NetTCPConnection -LocalPort $m.Local -State Listen -ErrorAction SilentlyContinue
  if ($old) { Stop-Process -Id $old.OwningProcess -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 2 }
  Start-Process ssh -ArgumentList '-i',$SshKey,'-o','StrictHostKeyChecking=no','-N','-L',"127.0.0.1:$($m.Local):127.0.0.1:$hostPort",$SshHost -WindowStyle Hidden | Out-Null
  Start-Sleep -Seconds 5
  Log ("{0} 隧道重建 本机:{1} -> 主机:{2}" -f $m.Name, $m.Local, $hostPort)
  # 5) 重发
  $t0 = Get-Date
  Start-Task $m $task | Out-Null
  if (Wait-Fresh $m 180 $t0) { Log ("{0} ③成功：恢复推进" -f $m.Name); return $true }
  Log ("{0} ③未见推进（需人工）" -f $m.Name); return $false
}

function Fix-Machine($m) {
  $task = Resolve-Task $m
  if ((Get-MaaProc $m) -and ((Get-LogAgeMin $m) -lt $StaleMin)) {
    Log ("{0} 正在推进（日志 {1} 分前），无需处置" -f $m.Name, (Get-LogAgeMin $m)); return
  }
  if (Fix-Step1 $m $task) { return }
  if (Fix-Step2 $m $task) { return }
  Fix-Step3 $m $task | Out-Null
}

# ---------------------------------------------------------------------------
switch ($Cmd.ToLower()) {
  "status" { Show-Status }
  "daily"  { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Start-One $m $m.Daily } }
  "rogue"  { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Start-One $m $m.Rogue } }
  # chain：日常跑完自动接肉鸽（两个功能仍是独立任务文件）——后台 worker，不占终端
  "chain"  { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) {
               Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File',$PSCommandPath,'chain-worker',$m.Name -WindowStyle Hidden | Out-Null
               Log ("{0} chain worker 已启动（后台：日常→肉鸽）" -f $m.Name) } }
  "chain-worker" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Chain-Worker $m } }
  # cycle：肉鸽↔日常 循环（到理智快回满才跑日常）——后台 worker
  "cycle"  { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) {
               Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File',$PSCommandPath,'cycle-worker',$m.Name -WindowStyle Hidden | Out-Null
               Log ("{0} cycle worker 已启动（到理智快满自动日常→回肉鸽）" -f $m.Name) } }
  "cycle-worker" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Cycle-Worker $m } }
  "cycle-stop" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) {
               $stopFile = Join-Path $OpsDir ("cycle_{0}.stop" -f $m.Name)
               "stop" | Out-File -FilePath $stopFile -Encoding ascii
               $pidFile = Join-Path $OpsDir ("cycle_{0}.pid" -f $m.Name)
               if (Test-Path $pidFile) { $cpid = (Get-Content $pidFile -Raw).Trim(); Stop-Process -Id ([int]$cpid) -Force -ErrorAction SilentlyContinue }
               Log ("{0} cycle 停止信号已发" -f $m.Name) } }
  # recover：主机重启/大规模掉线后一键找回（按实例名定位 index → 启动 → 隧道 → 发肉鸽）
  "recover" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Recover-Machine $m } }
  # watch：只自愈（进程没了/卡死 → 关游戏重开+重发当前任务），不跑日常
  "watch"  { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) {
               Start-Process powershell -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File',$PSCommandPath,'watch-worker',$m.Name -WindowStyle Hidden | Out-Null
               Log ("{0} watch worker 已启动（只自愈）" -f $m.Name) } }
  "watch-worker" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Watch-Worker $m } }
  "watch-stop" { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) {
               $stopFile = Join-Path $OpsDir ("watch_{0}.stop" -f $m.Name)
               "stop" | Out-File -FilePath $stopFile -Encoding ascii
               $pidFile = Join-Path $OpsDir ("watch_{0}.pid" -f $m.Name)
               if (Test-Path $pidFile) { $cpid = (Get-Content $pidFile -Raw).Trim(); Stop-Process -Id ([int]$cpid) -Force -ErrorAction SilentlyContinue }
               Log ("{0} watch 停止信号已发" -f $m.Name) } }
  "fix1"   { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Fix-Step1 $m (Resolve-Task $m) | Out-Null } }
  "fix2"   { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Fix-Step2 $m (Resolve-Task $m) | Out-Null } }
  "fix3"   { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Fix-Step3 $m (Resolve-Task $m) | Out-Null } }
  "fix"    { foreach ($m in ($Machines | Where-Object { $Target -eq "all" -or $_.Name -eq $Target })) { Fix-Machine $m } }
  default  { Write-Host "用法: rogue_cli_ops.ps1 status | fix1|fix2|fix3|fix <l-1|l-2|l-5|l-7|all>" }
}
