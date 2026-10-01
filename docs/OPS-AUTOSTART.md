# 开机 / 意外退出 自动恢复（外层在岗保证）

> 定稿 2026-10-02。分工原则：
> - **外层（计划任务）只管"进程在岗"**：谁不在拉谁，不碰槽位/模拟器/任务；
> - **进程自己管自己的活**：机端（node）自己拉 worker/设备/任务；runner 自己排槽位/设备/maa/停机；
> - **中心**只收上报 / 下发指令。
>
> 机器 MRFZ-0000 比较老、会随机蓝屏重启，这层就是为它兜底的。

## 计划任务清单（MRFZ-0000）

| 任务 | 触发器 | 动作 | 管的进程 |
|---|---|---|---|
| `FleetNode-AutoStart` | AtLogon（Administrator / Interactive / Highest） | `ops\boot_node.ps1` | 机端 `fleet_node.ps1` |
| `FleetNode-Watchdog` | 每 10 分钟、无限重复、IgnoreNew | 同上 | 机端 |
| `FleetRunner-AutoStart` | AtLogon（同上主体） | `ops\boot_runner.ps1` | runner `fleet.exe` |
| `FleetRunner-Watchdog` | 每 10 分钟、无限重复、IgnoreNew | 同上 | runner |

- 两个 boot 脚本都是**幂等**的：进程在 → 只写一行"跳过"日志；不在 → WMI 拉起（脱离计划任务进程树）。
- 都**认进程身份，不只看 PID**（2026-10-01 l-4 被 `nvcontainer` 复用 PID 顶掉，机端误判"在岗"，停了 7 小时）：
  - `boot_node.ps1` / 机端 `Ensure-Workers`：`powershell.exe` + 命令行含 `fleet_node.ps1` / `rogue_cli_ops.ps1 <kind>-worker <machine>`；
  - `boot_runner.ps1`：`fleet.exe` + 命令行是 runner 常驻（排除 `-status/-once/-now/...` 一类一次性命令）。
- `boot_runner.ps1` 尊重人工停：`runner\runner.stop` 存在则不拉。

## 恢复链（重启/蓝屏后无人干预）

```
开机 → 自动登录（~4s）→ FleetNode-AutoStart / FleetRunner-AutoStart（~5s）
     → boot_node / boot_runner → 机端 / runner
         ├ 机端：Ensure-Workers → cycle/watch worker → 模拟器 → adb → maa 任务（5 台肉鸽机）
         └ runner：槽位调度（4 并发）→ 起模拟器 → 日常/剿灭 → 停机（49 个日常账号）
```

## 实测证据

| 时间 | 事项 | 证据 |
|---|---|---|
| 10-01 16:48\22:50 | 两次蓝屏重启后自动登录 + AtLogon 任务自动恢复 | Security 4624（Administrator type-2，boot 后 ~4s）；TaskScheduler/Operational `FleetNode-AutoStart` 16:48:53→16:50:14、22:50:31→22:52:00，均 rc=0 |
| 10-02 06:10 | 机端 worker 自愈（PID 复用场景演习） | `node.log`：`worker pid 失效（pid=4688 不是 cycle l-4，疑似 PID 复用）→ 清理后重拉`；新 worker 58 秒上岗 |
| 10-02 06:14 | `FleetNode-Watchdog` 注册 + 试跑 | `LastResult=0`；`boot_node.log`：`机端已在跑（pid=30120），跳过` |
| 10-02 06:28 | `FleetRunner-*` 注册 + 触发恢复 runner | `boot_runner.log`：`runner 已拉起 pid=30248 rc=0`；`fleet.exe` session=1 |
| 10-02 06:30 | runner 恢复工作 | 中心台账 `host-mrfz0000-runner.last_seen=06:30:54`；4 个槽位在跑（a18/a32/a40/a50） |

## 已知边界

- 计划任务是 `AtLogon`（Interactive）——**依赖自动登录**；这台机实测 boot 后 ~4 秒自动登录（`AutoLogonCount=9999999`），历史两次蓝屏重启均自动恢复。
- 蓝屏本身不修（老机器）；这层只保证"再蓝屏也能自己回来"。开机慢时（07:30 那次）从 boot 到机端约 6.5 分钟。
- `a06` 属于 runner 的日常槽位（`daily_a06`+`ann_a06`，**不是肉鸽机、不需要 worker**）；平时关机是正常的，`ops status` 对它的 `!!需处理` 是巡检口径噪音。
