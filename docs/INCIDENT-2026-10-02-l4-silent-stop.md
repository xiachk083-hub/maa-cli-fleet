# 事件复盘：MRFZ-0000 `l-4` 肉鸽静默停摆（2026-10-01 22:44 → 2026-10-02 05:38）

> 证据标签：本文所有"实测"均注明时间与命令/路径。
> `TESTED` = 本机/目标机实际跑过验证；`UNVERIFIED` = 仅推断，未验证。

## 1. 现象

- 机器 `MRFZ-0000`（Tailscale `100.79.173.69`，项目在 `E:\maa-cli-fleet`）上，`l-4`（MuMu 实例 9，日服，水月肉鸽连刷 `rogue_mizuki_l4`）从 **10-01 22:44** 起停止：无 maa 进程、模拟器关闭、`asst.log` 不再增长。
- 其余 4 台（`l-1/l-2/l-5/l-7`）在 10-01 22:52 机端重启后全部自动恢复，**只有 l-4** 没起来。
- 机端健康巡检（`E:\maa-cli-fleet\ops\rogue_cli_ops.log`，每 ~57s 一次）从 22:44 起持续报：
  `l-4 | maa=无 | 日志年龄=4xx分 | 隧道=断 | 游戏=(隧道断) | !!需处理`（只上报，不自动修）。

## 2. 时间线（实测）

| 时间 | 事件 | 证据 |
|---|---|---|
| 10-01 16:50:07 | 蓝屏 #1（`0x0000003b`，`MEMORY.DMP`） | 主机 System 日志 事件 1001 |
| 10-01 16:50:14 | 重启后机端拉起，**l-4 worker pid=9408**（pid 文件写于 16:50:16） | `node\node.log`、`ops\cycle_l-4.pid` |
| 10-01 22:44:43 | l-4 最后一次 `asst.log` 写入（肉鸽战斗中），随后机器卡死 | `data\state_l4\debug\asst.log` mtime |
| 10-01 22:50:16 | 蓝屏 #2（`0x0000003b`）后重启；22:50:31 `nvcontainer.exe` 拿到 **PID 9408** | 主机 System 日志；`Get-Process -Id 9408` |
| 10-01 22:52:00 | 机端重启（pid=10800），`node.log` 记录 `worker 缺失 → 拉起 cycle l-1 / l-2 / watch l-5 / cycle l-7`，**唯独没有 l-4** | `node\node.log` |
| 10-01 23:54 → 10-02 05:36 | l-4 一直 `!!需处理`，日志年龄从 70 分涨到 412 分（无人修） | `ops\rogue_cli_ops.log` |

## 3. 根因

### 根因 1（主因）：`Ensure-Workers` 判活只看 PID 存在 → 撞上 PID 复用
`node\fleet_node.ps1` 原逻辑：

```powershell
if ($x -match '^\d+$' -and (Get-Process -Id ([int]$x) -ErrorAction SilentlyContinue)) { $alive = $true }
```

- 蓝屏重启后系统会重新分配低号 PID，这次是 `nvcontainer.exe` 在 22:50:31 拿到了 9408；
- `cycle_l-4.pid` 里还是 9408 → 机端/`boot_node.ps1` 的 `Ensure-Workers` 判"l-4 worker 在岗"→ 永久跳过重拉；
- 于是 l-4 的 "worker→模拟器→adb→任务" 这条链整条缺失，而机端只负责上报、不自愈。

### 根因 2（次生）：`Test-OutError` 把"空输出"当成功
`ops\rogue_cli_ops.ps1` 原逻辑：`return [bool]($txt -match '\]\s+Error')`。

- 现场修复时 l-4 worker 首发车时设备还没起来，`daily_l4` **8 秒空转退出、.out 文件为空**；
- 判据没匹配到 `Error` → 被当成"日常成功" → 写下 `dailydate_l-4.txt = 2026-10-02`，当天真实日常会被跳过（已回滚，见 §4）。

### 触发条件：机器本身两次蓝屏
`0x0000003b SYSTEM_SERVICE_EXCEPTION (0xc0000005)`，10-01 一天内两次（16:50、22:50）。**不稳定的是机器，不是任务逻辑**；本事件只是把判活缺陷暴露了出来。

## 4. 处置（全部实测）

### 4.1 现场抢修（10-02 05:37～05:38，TESTED）
1. 确认 `cycle_l-4.pid` 的 9408 = `nvcontainer.exe`（非 worker）后删除该失效 pid 文件；
2. 用 WMI `Win32_Process Create` 拉起 `ops\rogue_cli_ops.ps1 cycle-worker l-4`（脱离 ssh 会话）；
3. worker 自检发现设备不可达 → `recover` 拉起 MuMu 实例 9 → adb `127.0.0.1:16672` boot=1 → 发车肉鸽：
   `[10-02 05:38:20] l-4 已发车 pid=28284 (任务 rogue_mizuki_l4)`。

### 4.2 纠正被误标的日常（10-02 05:40，TESTED）
1. `dailydate_l-4.txt` 回滚为 `2026-10-01`，重启 l-4 cycle worker；
2. worker 按设计"本游戏日日常未完成 → 立即先跑日常"：
   `[10-02 05:40:23] cycle：开跑日常（daily_l4）`；
3. 日常完整跑完（`ops\logs\daily_l4_1002_054028.out`）：
   开始唤醒 2m10s / 刷理智 1-7×28 9m54s / 公招 1m8s / 基建 8m35s / 信用 3m25s / 奖励 31s —— **全部 Completed**；
4. `[10-02 06:06:24] 日常完成标记：游戏日 2026-10-02` → `[06:06:27] 已发车 rogue_mizuki_l4 pid=25736`，06:07 健康 OK。

### 4.3 代码修复（commit `a168c9e`，已推 GitHub master；目标机已同步，TESTED）
1. `node\fleet_node.ps1` `Ensure-Workers`：判活改为**进程身份校验**——
   `powershell.exe` 且命令行含 `rogue_cli_ops.ps1 <kind>-worker <machine>`；不符则清理 pid 文件并重拉，`node.log` 记原因；
2. `ops\rogue_cli_ops.ps1` `Test-OutError`：空 `.out`、或没有 summary 时间行（`\"04:58:24 - 05:00:22\"`）一律判失败；
3. 部署：目标机 `E:\maa-cli-fleet\{node,ops}` 同名文件 SHA256 与仓库一致
   （`ops=94C04835…`、`node=E63641F0…`），语法检查 0 错误；重启机端（新 pid=23948）后 `node.log` **没有**误重拉（新判活正确认下 5 个 worker）；
4. 冒烟：`rogue_cli_ops.ps1 status` 正常输出，5 台 `OK`。

## 5. 验收证据（10-02 06:07，TESTED）

- 机端健康：`l-4 | maa=pid25736 | 日志年龄=0分 | 隧道=通 | 游戏=13435 | OK`；
- 中心台账 `center\state\registry.json` → `host-mrfz0000.state.machines.l-4` = `health OK / tunnel 通`；
- l-4 任务链：`rogue_mizuki_l4`（肉鸽连刷），日常标记 `2026-10-02` 为真实完成。

### 5.1 「重启后自愈」等价演习（10-02 06:10，TESTED）

不真重启机器，只复现当时坏掉的那一段：杀 l-4 worker + 把活着的非 worker 进程（`nvcontainer.exe` pid=4688）PID 写进 `cycle_l-4.pid`，再重启机端（= 重启后 `boot_node.ps1` 走的路）。

```
[10-02 06:10:43] 机端启动：node=host-mrfz0000（pid=30120）
[10-02 06:10:44] worker pid 失效（pid=4688 不是 cycle l-4，疑似 PID 复用）→ 清理后重拉
[10-02 06:10:44] worker 缺失 → 拉起 cycle l-4
```

新 worker pid=27988 上岗、pid 文件同步更新，l-4 的肉鸽（maa pid=25736）全程未被打断。
即：**同一故障场景现在会自动恢复**。

另：计划任务 `FleetNode-AutoStart`（Administrator / Interactive / Highest / AtLogon，动作 `boot_node.ps1`）确认在岗，10-01 22:50 蓝屏重启后 **22:50:50 自动触发、LastResult=0** —— 重启→自愈链路的"自动触发"这一环本来就是好的，坏的是 `Ensure-Workers` 的判活。

### 5.2 恢复链全链路证据（10-02 06:15，TESTED）

| 环节 | 证据 |
|---|---|
| ① 自动登录 | 两次蓝屏重启：`System 6005` 16:48:46 → Administrator type-2 登录 16:48:50；22:50:25 → 22:50:29（Security 4624）；`query user` 当前会话登录时间 = 10-01 22:50 |
| ② AtLogon 计划任务 | TaskScheduler/Operational：16:48:53 启动 → 16:50:14 完成 rc=0；22:50:31 启动 → 22:52:00 完成 rc=0 |
| ③ boot_node → 机端 | `ops\boot_node.log`：16:50:14 / 22:52:00 `机端已拉起 … rc=0` |
| ④ 机端 → worker 自愈 | §5.1 演习（PID 复用场景自动清理 + 重拉） |
| ⑤ worker → 模拟器 → 任务 | §4.1：90 秒内 MuMu 实例 9 起来 → adb boot=1 → 肉鸽发车 |

**结论：机器再蓝屏/重启，全链路无人干预可恢复；唯一慢的是老机器开机本身（07:30 那次从 boot 到机端用了 6.5 分钟）。**

### 5.3 加固：机端看门狗（10-02 06:14，TESTED）

原链路只覆盖「重启后恢复」；机端进程**自身**意外死掉（没重启机器）时没人拉它（worker 还能跑，但再死就没补位）。新增：

| 任务 | 触发器 | 动作 | 说明 |
|---|---|---|---|
| `FleetNode-Watchdog` | 每 10 分钟、无限重复（IgnoreNew） | `boot_node.ps1`（幂等） | 机端不在了就拉起 → Ensure-Workers → worker/模拟器/任务全回来 |

实测：注册后试跑 `LastResult=0`，`boot_node.log` 记 `[10-02 06:14:21] 机端已在跑（pid=30120），跳过` —— 无副作用。

## 6. 遗留 / 待办（UNVERIFIED / 未做）

1. **蓝屏本身没修**：`0x3b` 一天两次（`C:\Windows\MEMORY.DMP`）。需要单独立项：查 dump、排除驱动/内存/模拟器高负载；否则同类停摆还会再来（判活修复只保证"再来也能自动恢复"）。
2. `a06`（官方服，`daily_a06`）长期 `!!需处理`：`fleet.local.json` 里有这台，但机端 `node\conf.json` 的 `workers` 没给它配 worker → **属于设计缺口**，需要决定是否补 `cycle`/`watch`。
3. `runner\run_slot.ps1` 的 `Test-OutErr` 是同款"只匹配 `] Error`"判据（空输出=成功），未改；AUTO-MAS 槽位流程（`a**`）若有同类误判，可参照 §4.3-2 修。
4. 目标机 `E:\maa-cli-fleet` **不是 git 检出**（GitHub zip 布点），本次靠人工同步文件；后续改动建议走"提交→目标机覆盖同名文件"流程，避免目标机再出现与仓库口径不一致的旧快照。
5. 本轮未动：`l-1/l-2/l-5/l-7` 的任务配置、AUTO-MAS（`E:\AUTO-MAS`）槽位流程。
