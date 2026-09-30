> 2026-10-01：本项目已收敛为独立项目 `D:\maa-cli-fleet`（自包含：bin/core/config/data/ops/docs 全在项目内）；路径已更新。

# CLI 肉鸽机队运维手册（收敛版）

> 建立：2026-10-01 · 维护人：lead（会话） · 机器：本机 <LOCAL_PC> + 主机 <HOST>(<HOST_IP>)
> 一句话：**用 maa-cli 接管肉鸽机队（l-1/l-2/l-4/l-5/l-7）的「日常 + 肉鸽」，替代 fz-maa/MAS，含循环调度与自愈。**

---

## 1. 架构总览

```
┌─ 本机 <LOCAL_PC> ─────────────────────────────────────────────┐
│  D:\maa-cli-fleetin\maa.exe        maa-cli 0.7.5（连设备→驱动游戏）      │
│  D:\maa-cli-fleet\ops\rogue_cli_ops.ps1   运维脚本（调度/自愈/恢复，17 命令）    │
│  worker（后台 PS 进程）：cycle ×4 + watch ×1                        │
│  隧道 ssh -L：16522→17184(l-1) 16523→17280(l-2) 16524→16672(l-4)   │
│              16520→16452(l-5) 16521→17028(l-7)                     │
└───────────────┬────────────────────────────────────────────────────┘
                │ ssh（key: %USERPROFILE%\.ssh\<key>）
┌───────────────▼─ 主机 <HOST> (<HOST_IP>) ────────────────────┐
│  E:\MuMu Player 12\   5 台模拟器（vms\MuMuPlayer-12.0-{9,25,28,34,52}）│
│      shell\adb.exe · nx_main\MuMuManager.exe                        │
│  E:\AUTO-MAS\         生产 MAS（早/晚班）——机队 5 账号已 Status=false │
│  E:\MAA-Multi\MAA_1..12\ 产线日常槽（不受影响）                       │
└─────────────────────────────────────────────────────────────────────┘
```

**分层职责**：maa-cli 只管"连设备+跑任务"；`rogue_cli_ops.ps1` 管"什么时候跑什么 + 坏了怎么办"。

---

## 2. 文件树

```
D:\maa-cli-fleet\ops\                                   ← 运维根
├── rogue_cli_ops.ps1                         ★ 主脚本（~575 行）
├── rogue_cli_ops.log                         动作审计日志
├── cycle_l-1/l-2/l-4/l-7.pid (+.stop)        cycle worker 控制
├── watch_l-5.pid (+.stop)                    watch worker 控制
├── dailydate_l-*.txt                         每台"最后完成日常的游戏日"（自动生成）
├── dist\maa.exe                               maa-cli 二进制
├── logs\<任务>_<月日_时分秒>.out              每次发车的 stdout（摘要/Explorations）
└── tmp\ · *.log（历史调试，可清理）

%APPDATA%\loong\maa\
├── config\profiles\default.toml              连接（adb 路径/address/global_resource=YoStarJP）
├── config\tasks\
│   ├── daily_l1/l2/l4/l5/l7.toml             日常链：唤醒→刷理智(1-7,series=0)→公招→基建→信用→奖励
│   └── rogue_sami_l1 · rogue_sarkaz_l2 · rogue_mizuki_l4/l5/l7.toml   肉鸽连刷（99999 局）
└── data\state_l1/l2/l4/l5/l7\debug\asst.log  ★ 每台独立状态目录（日志隔离，worker 判据源）

<MAAOrch_REPO>\docs\
├── maa-cli-fleet-ops.md                      ← 本文（收敛手册）
├── maa-cli-daily-config.html                 日常配置全参数手册
├── maa-cli-roguelike-config.html             肉鸽参数速查
├── maa-cli-multi-instance.html               多实例/隔离/污染
└── emulator-precise-launch.html              MuMuManager 精准启动
```

---

## 3. 命令参考（`rogue_cli_ops.ps1`）

```powershell
# 体检：maa 进程/日志年龄/隧道/游戏 四查
.\rogue_cli_ops.ps1 status

# 两个独立功能（启动前都自动先关游戏 am force-stop，并停掉该机旧任务）
.\rogue_cli_ops.ps1 daily  <机|all>     # 只跑日常
.\rogue_cli_ops.ps1 rogue  <机|all>     # 只刷肉鸽

# 一键衔接：日常 → 关游戏 → 肉鸽（一次性）
.\rogue_cli_ops.ps1 chain  <机|all>

# 循环调度（后台 worker，kill 不随终端退出）
.\rogue_cli_ops.ps1 cycle  <机|all>     # 肉鸽↔日常：今日日常没做完→立即先日常；完了→按理智时钟等下一轮
.\rogue_cli_ops.ps1 cycle-stop <机|all> # 停止循环（也杀 worker 进程）
.\rogue_cli_ops.ps1 watch  <机|all>     # 只自愈，不跑日常
.\rogue_cli_ops.ps1 watch-stop <机|all>

# 主机重启/掉线恢复：按实例名找 idx → 启模拟器 → 自动查 adb 端口 → 主机/本机双 adb connect → 建隧道 → 发肉鸽
.\rogue_cli_ops.ps1 recover <机|all>

# 手动阶梯修复（自动认当前在跑的功能；可强制 daily|rogue）
.\rogue_cli_ops.ps1 fix  <机> [daily|rogue]   # ①→②→③ 自动升级
.\rogue_cli_ops.ps1 fix1 <机> …               # ①重发任务
.\rogue_cli_ops.ps1 fix2 <机> …               # ②关游戏重开+重发
.\rogue_cli_ops.ps1 fix3 <机> …               # ③重启模拟器+重建隧道+重发
```

---

## 4. Worker 逻辑

**cycle-worker**（l-1/l-2/l-4/l-7）：
```
循环：
  1) 设备检查：adb 不通 → 设备级恢复（启模拟器+双端 adb connect+隧道+发任务）
  2) 游戏日检查：本游戏日(以 04:00 为界)日常未完成 → 立即打断肉鸽→跑日常(错则关游戏重试1次)
                 完成→写 dailydate_<机>.txt；完成后回肉鸽
  3) 日常已完成 → 按理智时钟等待（(上限−当前)×6min − 30min）→ 日常 → 回肉鸽
  4) 睡觉间隙每 5 分钟自检（进程没了/日志>4min 不动/10min 内≥2 次 TaskChainError → 自愈）
```
**watch-worker**（l-5）：只有 1)+4)（只自愈，不跑日常）。
**自愈动作**：关游戏（am force-stop）→ 重发"当前那个功能"的任务（日常坏修日常、肉鸽坏修肉鸽）；连续 3 次无效 → 冷却 30 分钟后重试。

**理智推算**：读 `state_lX\debug\asst.log` 最后一条 `current_sanity/max_sanity`（MaaCore Fight 任务打的），按 1 点/6 分钟外推；若之后完成过 Fight 链则从链结束按 0 重算。

---

## 5. 五台现状（2026-10-01 04:31 实测）

| 机 | 模拟器 idx | 隧道 | 主题 | 当前 | worker |
|---|---|---|---|---|---|
| l-1 | 25 | 16522→17184 | 萨米 Sami (MAX) | 补 10-01 日常 | cycle |
| l-2 | 28 | 16523→17280 | 萨卡兹 Sarkaz (MAX) | 补日常（StartUp） | cycle |
| l-4 | **9**（编号已漂） | 16524→16672 | 水月 Mizuki (3) | 补日常（刷理智） | cycle |
| l-5 | 34 | 16520→16452 | 水月 Mizuki (3) | 肉鸽连刷（无日常） | watch |
| l-7 | 52 | 16521→17028 | 水月 Mizuki (3) | 补日常（刷理智） | cycle |

任务参数统一：`core_char=维什戴尔 · roles=先手必胜 · use_support+use_nonfriend_support · investment_enabled=false · starts_count=99999 · series=0(auto连战)`。

---

## 6. 证据标签

| 项 | 标签 | 证据 |
|---|---|---|
| maa-cli 部署+连通（本机） | `IMPLEMENTED`/`TESTED` | D:\maa-cli-fleetin\maa.exe 0.7.5；5 台设备经隧道 getprop=1（多轮实测） |
| 日常链（6 段）在 CLI 可跑通 | `TESTED` | l-1/l-2/l-7 2026-09-30 全链 Completed；10-01 04:2x 复跑中 |
| 肉鸽连刷（99999）可连续跑 | `TESTED` | l-5 单 run 2h43m/5 局（09-30 23:31→03:03） |
| 两功能独立 + 启动前关游戏 | `IMPLEMENTED` | daily/rogue/chain 命令；l-1/l-4 重启实测 |
| cycle 循环（理智时钟+游戏日） | `IMPLEMENTED`（部分 `TESTED`） | 10-01 04:26 实测：l-1/l-2/l-4/l-7 四台因"本游戏日日常未完成"被立即转日常 |
| 自愈（关游戏重开+重发） | `IMPLEMENTED` | 09-30 弹窗事件后人工处置验证；自动路径待实战 |
| 设备级自愈（启模拟器+双端 connect+隧道） | `IMPLEMENTED` | 10-01 主机重启后手工同序操作验证；`Recover-Machine` 代码已就位 |
| AUTO-MAS 5 账号停用（走 CLI） | `TESTED` | 36163 API + 磁盘 ScriptConfig.json 双确认 Status=false |

---

## 7. 已知缺口 / 待办（诚实清单）

1. **`recover` 效率**：等待逻辑为串行 + 每 5s ssh 轮询（冷启可达 15min/台）。改进方向：实例在线则跳过等待、多台并行、adb connect 重试短路。（`PROPOSED`）
2. **自动自愈未完整实战**：10-01 主机重启后的恢复是人工编排（手工隧道/connect）；设备级自愈代码已就位但**尚未在真实重启场景全程自动验证**。（`UNVERIFIED`）
3. **游戏弹窗类卡死**（如日服"データが更新されました。データ同期を行います"）：本次靠人工关游戏解决；worker 的 error-loop/no-task 判据理论上能兜住，但**未复现验证**。（`UNVERIFIED`）
4. **l-5 无日常**：按早前指示"只刷肉鸽"。若要它清日常：给它 `cycle l-5`（daily_l5.toml 已备）。（待拍板）
5. **隧道依赖本机 SSH 长连**：本机重启/断网后需重建（`recover` 或手工 ssh -L）。（`DEPLOYED` 现状）
6. **l-4 的 AUTO-MAS 旧记录（<账号4>/官服档案）已停用**；现机为日服、实例号已漂到 idx9。恢复 MAS 侧如需另行确认。（`UNVERIFIED`）
7. **理智读数依赖日志**：账户理智 > 上限（如 l-1 曾 333/165）时读数为 clamp 值，等待时间按满算（保守，不误伤）。（`IMPLEMENTED`）

---

## 8. 回滚手册

| 回到 | 操作 |
|---|---|
| 单台停 CLI 肉鸽 | `rogue_cli_ops.ps1 cycle-stop/watch-stop <机>` + 杀该机 maa 进程 |
| 恢复该机 MAS 日常 | `python tools/auto_mas_toggle_user.py --name <账号> --enable`（l-1 <账号1> / l-2 <账号2> / l-4 <账号4> / l-5 <账号5> / l-7 <账号7>） |
| 恢复 fz-maa（GUI 肉鸽） | 主机：`schtasks /change /tn MAAOrch-StartOne-<idx> /enable`（idx：l-1=25 / l-2=28 / l-4=31(已失效) / l-5=34 / l-7=52） |
| 全清 CLI 侧 | 杀 worker（cycle/watch-stop）+ 杀 maa 进程 + 关隧道（杀 ssh 进程） |

---

## 9. 变更时间线（2026-09-30 → 10-01）

1. **09-30 白天**：maa-cli 三份 HTML 手册（日常/肉鸽/多实例）；l-5 首迁受阻（ConfigFile 闸）→ 已修（`t_036b0386`）。
2. **09-30 晚**：l-5/l-7 切 CLI 水月（关游戏重开法解决"其他主题进行中"锁）。
3. **09-30 深夜**：发现并处置 MAA 日志中的"2 局即停"问题（`starts_count=99999`）；五台机队全部切 CLI（l-1 萨米 / l-2 萨卡兹 / l-4 水月 / l-5 水月 / l-7 水月）。
4. **10-01 凌晨**：日常链定稿（唤醒→刷理智→公招→基建→信用→奖励，`series=0` auto 连战）；五台 MAS 日常停用；`rogue_cli_ops.ps1` 建成（17 命令 + 3 worker + 自愈/恢复/阶梯修复）。
5. **10-01 04:01 主机重启**：暴露"重启后恢复流程"缺口 → 补 设备级自愈 + 游戏日日常检查（"本游戏日日常未完成 → 立即先日常"），10-01 04:26 实测生效（四台自动转日常）。
