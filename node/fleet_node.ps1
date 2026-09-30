# ============================================================================
# fleet_node.ps1 —— 机端（node/executor）：跑在目标机上
#
# 职责：① 本机驱动 maa 任务（复用 ops\rogue_cli_ops.ps1，零重写）
#       ② 向中心上报（注册/心跳/状态/事件增量）
#       ③ 从中心拉指令并执行、回执（"机端拉取"模型：机端只做出站连接）
#
# 用法：
#   .\fleet_node.ps1 run        # 常驻（建议 Start-Process 后台拉起）
#   .\fleet_node.ps1 once       # 单轮调试：注册+状态+一次拉令
#   .\fleet_node.ps1 status     # 本机看自己的运行态
#   .\fleet_node.ps1 stop       # 停止
#
# 配置：node\conf.json
#   {
#     "nodeId":      "host-mrfz0000",                 // 稳定节点 id
#     "centerUrl":   "http://<后端内部地址>:8790",     // 内部地址（可跨机）
#     "token":       "<center/state/token.txt>",
#     "opsScript":   "<项目>\ops\rogue_cli_ops.ps1",
#     "logFile":     "<项目>\ops\rogue_cli_ops.log",   // 事件增量源
#     "heartbeatSec": 30, "stateSec": 60
#   }
# ============================================================================
param([Parameter(Position=0)][string]$Cmd = "status")

$ErrorActionPreference = "Continue"
$NodeDir  = Split-Path -Parent $PSCommandPath
$RootDir  = Split-Path -Parent $NodeDir
$ConfFile = Join-Path $NodeDir "conf.json"
$PidFile  = Join-Path $NodeDir "node.pid"
$StopFile = Join-Path $NodeDir "node.stop"
$NodeLog  = Join-Path $NodeDir "node.log"

function Log($msg) {
  $line = "[" + (Get-Date -Format "MM-dd HH:mm:ss") + "] " + $msg
  Add-Content -Path $NodeLog -Value $line -Encoding UTF8
  Write-Host $line
}

function Get-Conf {
  if (-not (Test-Path $ConfFile)) { Log "缺少配置：$ConfFile"; exit 2 }
  $c = Get-Content $ConfFile -Raw -Encoding UTF8 | ConvertFrom-Json
  foreach ($k in @("nodeId", "centerUrl", "token")) {
    if (-not $c.$k) { Log "配置缺字段：$k"; exit 2 }
  }
  if (-not $c.opsScript) { $c | Add-Member -NotePropertyName opsScript -NotePropertyValue (Join-Path $RootDir "ops\rogue_cli_ops.ps1") -Force }
  if (-not $c.logFile)   { $c | Add-Member -NotePropertyName logFile   -NotePropertyValue (Join-Path $RootDir "ops\rogue_cli_ops.log") -Force }
  if (-not $c.heartbeatSec) { $c | Add-Member -NotePropertyName heartbeatSec -NotePropertyValue 30 -Force }
  if (-not $c.stateSec)     { $c | Add-Member -NotePropertyName stateSec     -NotePropertyValue 60 -Force }
  return $c
}

function Invoke-Center($conf, [string]$Method, [string]$Path, $Body = $null, [int]$TimeoutSec = 40) {
  $uri = $conf.centerUrl.TrimEnd("/") + $Path
  $headers = @{ "X-Fleet-Token" = [string]$conf.token }
  try {
    if ($Method -eq "GET") {
      return Invoke-RestMethod -Uri $uri -Method Get -Headers $headers -TimeoutSec $TimeoutSec
    }
    $json = if ($Body) { $Body | ConvertTo-Json -Depth 8 -Compress } else { "{}" }
    return Invoke-RestMethod -Uri $uri -Method Post -Headers $headers -Body $json `
      -ContentType "application/json; charset=utf-8" -TimeoutSec $TimeoutSec
  } catch {
    return [pscustomobject]@{ ok = $false; error = $_.Exception.Message }
  }
}

# ---- 子进程执行（文件重定向 + 硬超时；不走管道，防 EOF 死等） ----------------
function Invoke-ChildPs([string[]]$Argv, [int]$TimeoutSec = 300) {
  $tag = [guid]::NewGuid().ToString('N').Substring(0, 8)
  $out = Join-Path $env:TEMP ("fleetnode_" + $tag + ".out")
  $err = Join-Path $env:TEMP ("fleetnode_" + $tag + ".err")
  $exe = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe"
  $p = Start-Process -FilePath $exe -ArgumentList $Argv `
       -RedirectStandardOutput $out -RedirectStandardError $err -WindowStyle Hidden -PassThru
  if (-not $p.WaitForExit($TimeoutSec * 1000)) {
    & taskkill /PID $p.Id /T /F 2>$null | Out-Null
    Remove-Item $out, $err -Force -ErrorAction SilentlyContinue
    return @{ ok = $false; timeout = $true; output = ("timeout: " + $TimeoutSec + "s 未返回，已强杀") }
  }
  $o = (Get-Content $out -Raw -ErrorAction SilentlyContinue)
  $e = (Get-Content $err -Raw -ErrorAction SilentlyContinue)
  Remove-Item $out, $err -Force -ErrorAction SilentlyContinue
  $txt = ("" + $o) + "`n" + ("" + $e)
  return @{ ok = $true; rc = $p.ExitCode; output = $txt.Trim() }
}

# ---- 本机状态采集（解析 ops status 文本） -----------------------------------
function Get-LocalState($conf) {
  $r = Invoke-ChildPs @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $conf.opsScript, "status") 200
  $raw = if ($r.output) { [string]$r.output } else { "" }
  if ($r.timeout) { Log "状态采集超时（200s）→ 本轮跳过" }
  $machines = @{}
  foreach ($ln in ($raw -split "`r?`n")) {
    $m = [regex]::Match($ln, '\[(\S+ \S+)\]\s+(l-\d)\s+\|\s+maa=(\S+)\s+\|\s+日志年龄=([\d.]+)分\s+\|\s+隧道=(\S+)\s+\|\s+游戏=(\S+)\s+\|\s+(\S+)')
    if ($m.Success) {
      $machines[$m.Groups[2].Value] = @{
        maa        = $m.Groups[3].Value
        logAgeMin  = [double]$m.Groups[4].Value
        tunnel     = $m.Groups[5].Value
        game       = $m.Groups[6].Value
        health     = $m.Groups[7].Value
        sampledAt  = $m.Groups[1].Value
      }
    }
  }
  return @{ machines = $machines; statusText = ($raw.Trim()) }
}

# ---- 事件增量（ops 日志新增行） ---------------------------------------------
function Get-NewEvents($conf, [ref]$offset) {
  if (-not (Test-Path $conf.logFile)) { return @() }
  $fi = Get-Item $conf.logFile
  if ($fi.Length -lt $offset.Value) { $offset.Value = 0 }          # 轮转/截断
  if ($fi.Length -eq $offset.Value) { return @() }
  $fs = [System.IO.File]::Open($conf.logFile, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
  try {
    [void]$fs.Seek($offset.Value, [System.IO.SeekOrigin]::Begin)
    $sr = New-Object System.IO.StreamReader($fs)
    $txt = $sr.ReadToEnd()
    $offset.Value = $fs.Position
  } finally { $fs.Close() }
  $lines = $txt -split "`r?`n" | Where-Object { $_ -ne "" }
  return @($lines | Select-Object -Last 100)
}

# ---- 指令执行 ---------------------------------------------------------------
$WRITE = @("daily", "rogue", "chain", "cycle", "cycle-stop", "watch", "watch-stop", "recover", "fix", "fix1", "fix2", "fix3")
$VALID = @("status") + $WRITE

function Invoke-NodeCommand($conf, $item) {
  $cmd = [string]$item.cmd
  $machine = [string]$item.machine
  $kind = [string]$item.kind
  $apply = [bool]$item.apply
  if ($VALID -notcontains $cmd) { return @{ ok = $false; error = "unknown_cmd:$cmd" } }
  $argv = @($conf.opsScript, $cmd)
  if ($machine) { $argv += $machine }
  if ($kind)    { $argv += $kind }
  $plan = ($cmd + " " + ($machine) + $(if ($kind) { " " + $kind } else { "" })).Trim()
  if (($WRITE -contains $cmd) -and (-not $apply)) {
    Log "指令 $($item.id)：dry-run（$plan）"
    return @{ ok = $true; dry_run = $true; cmdline = $plan; note = "写指令需 apply=true 才执行" }
  }
  Log "指令 $($item.id)：执行 $plan"
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  $argv2 = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File") + $argv
  $r = Invoke-ChildPs $argv2 300
  $sw.Stop()
  if ($r.timeout) {
    Log ("指令 " + $item.id + " 超时强杀（300s）→ 回报失败")
    return @{ ok = $false; timeout = $true; cmdline = $plan; output = $r.output }
  }
  return @{ ok = $true; rc = $r.rc; ms = $sw.ElapsedMilliseconds; cmdline = $plan; output = $r.output }
}

# ---- 单轮 -------------------------------------------------------------------
function Step($conf, [ref]$offset, [ref]$regAt) {
  $swStep = [System.Diagnostics.Stopwatch]::StartNew()
  # 注册（每 10 分钟刷新一次）
  if (((Get-Date) - $regAt.Value).TotalMinutes -ge 10) {
    $st = Get-LocalState $conf
    $r = Invoke-Center $conf "POST" "/register" @{
      node = @{ id = $conf.nodeId; ver = "0.1"; host = $env:COMPUTERNAME
                machines = ($st.machines.Keys | ForEach-Object { $_ }) }
    }
    if ($r.ok) { $regAt.Value = Get-Date; Log "已注册（$($st.machines.Count) 台机）" }
  }
  # 状态上报
  $st = Get-LocalState $conf
  [void](Invoke-Center $conf "POST" "/report" @{ node_id = $conf.nodeId; kind = "state"; data = $st })
  # 事件增量
  $ev = Get-NewEvents $conf $offset
  if ($ev.Count -gt 0) {
    [void](Invoke-Center $conf "POST" "/report" @{ node_id = $conf.nodeId; kind = "event"; data = @{ lines = $ev } })
  }
  if ($swStep.Elapsed.TotalSeconds -gt 90) { Log ("慢阶段：状态采集耗时 " + [int]$swStep.Elapsed.TotalSeconds + "s") }
  # 拉令（长轮询）
  $p = Invoke-Center $conf "GET" ("/poll?node_id=" + $conf.nodeId) $null 35
  if ($p.ok -and $p.commands) {
    foreach ($item in $p.commands) {
      $res = Invoke-NodeCommand $conf $item
      [void](Invoke-Center $conf "POST" "/result" @{
        node_id = $conf.nodeId; id = $item.id; ok = $res.ok; rc = $res.rc; output = ($res | ConvertTo-Json -Depth 6)
      })
    }
  }
}

# ---- 常驻 -------------------------------------------------------------------
function Run-Node($conf) {
  if (Test-Path $StopFile) { Remove-Item $StopFile -Force }
  ("$PID") | Out-File -FilePath $PidFile -Encoding ascii
  $offset = [ref]0
  if (Test-Path $conf.logFile) { $offset.Value = (Get-Item $conf.logFile).Length }
  $regAt = [ref][datetime]::MinValue
  Log ("机端启动：node=" + $conf.nodeId + " -> " + $conf.centerUrl + "（pid=" + $PID + "）")
  while (-not (Test-Path $StopFile)) {
    try { Step $conf $offset $regAt }
    catch { Log ("step 异常：" + $_.Exception.Message) }
    Start-Sleep -Seconds ([int]$conf.heartbeatSec)
  }
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
  Log "机端已停止"
}

# ---- CLI --------------------------------------------------------------------
switch ($Cmd.ToLower()) {
  "run"    { $c = Get-Conf; Run-Node $c }
  "once"   { $c = Get-Conf; $o = [ref]0; if (Test-Path $c.logFile) { $o.Value = (Get-Item $c.logFile).Length }; $r = [ref][datetime]::MinValue; Step $c $o $r; Log "once 完成" }
  "status" {
    $c = Get-Conf
    $alive = if (Test-Path $PidFile) { "pid=$(Get-Content $PidFile -Raw)" } else { "未运行" }
    Log "node=$($c.nodeId) center=$($c.centerUrl) 状态=$alive"
    $h = Invoke-Center $c "GET" "/health" $null 8
    Log ("center /health → " + ($h | ConvertTo-Json -Compress))
  }
  "stop"   { "stop" | Out-File -FilePath $StopFile -Encoding ascii
             if (Test-Path $PidFile) { $p = (Get-Content $PidFile -Raw).Trim(); Stop-Process -Id ([int]$p) -Force -ErrorAction SilentlyContinue }
             Log "停止信号已发" }
  default  { Write-Host "用法: fleet_node.ps1 run | once | status | stop" }
}
