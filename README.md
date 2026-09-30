# maa-cli-fleet — 明日方舟 CLI 机队（日常 + 肉鸽）

> 用 **maa-cli** 驱动明日方舟肉鸽机队：日常全链 + 肉鸽连刷，含**循环调度 / 卡死自愈 / 重启恢复**。
> **完全自包含**：二进制、核心库、资源、配置、状态、日志、文档全部在本项目目录内，可整体搬移。

---

## 快速开始

```powershell
# 1) 体检（五台：maa 进程 / 日志年龄 / 隧道 / 游戏）
.\ops\rogue_cli_ops.ps1 status

# 2) 启动循环（后台 worker：肉鸽↔日常自动衔接 + 自愈）
.\ops\rogue_cli_ops.ps1 cycle l-1     # 循环机（l-1/l-2/l-4/l-7）
.\ops\rogue_cli_ops.ps1 watch l-5     # 只自愈机（l-5）

# 3) 常用
.\ops\rogue_cli_ops.ps1 daily  <机|all>     # 只跑日常（自动先关游戏）
.\ops\rogue_cli_ops.ps1 rogue  <机|all>     # 只刷肉鸽
.\ops\rogue_cli_ops.ps1 recover <机|all>    # 主机重启/掉线后恢复（启模拟器+隧道+发任务）
.\ops\rogue_cli_ops.ps1 fix <机> [daily|rogue]   # 手动阶梯修复
```

**机队**：l-1(idx25/萨米) · l-2(idx28/萨卡兹) · l-4(idx9/水月) · l-5(idx34/水月) · l-7(idx52/水月)

---

## 目录结构

```
D:\maa-cli-fleet\
├── README.md                    ← 本文件
├── bin\
│   ├── maa.exe                  maa-cli 0.7.5（入口）
│   └── adb\adb.exe(+2 dll)      项目内 adb（连隧道设备用）
├── core\                        MaaCore 运行库 + resource（789M，从既有可用安装拷入）
├── config\                      =MAA_CONFIG_DIR
│   ├── profiles\default.toml     连接配置（adb 路径/global_resource=YoStarJP）
│   └── tasks\*.toml              任务文件（daily_l* / rogue_*）
├── data\                        =MAA_DATA_DIR（运行状态，全部在项目内）
│   ├── lib        → core        （目录联接，指向项目内 core）
│   ├── resource   → core\resource
│   ├── cache\                     =MAA_CACHE_DIR
│   └── state_l1/l2/l4/l5/l7\     每台独立状态（debug\asst.log 为 worker 判据源）
├── ops\                         运维目录
│   ├── rogue_cli_ops.ps1        ★ 主脚本（自定位项目根，可整体搬移）
│   ├── rogue_cli_ops.log         动作审计日志
│   ├── dailydate_l-*.txt         每台"最后完成日常的游戏日"
│   ├── cycle_*.pid/.stop         循环 worker 控制
│   ├── watch_*.pid/.stop         看守 worker 控制
│   └── logs\*.out                每次发车的 maa 输出（摘要/Explorations）
├── tools\                       预留（bootstrap / 同步脚本位）
└── docs\
    ├── FLEET_OPS.md              运维手册（架构/命令/worker 逻辑/证据标签/回滚）
    ├── maa-cli-daily-config.html 日常配置全参数
    ├── maa-cli-roguelike-config.html
    ├── maa-cli-multi-instance.html
    └── emulator-precise-launch.html
```

## 环境重定向（自包含的关键）
脚本启动时自动设置（子进程继承）：
```powershell
$env:MAA_CONFIG_DIR = <root>\config
$env:MAA_DATA_DIR   = <root>\data
$env:MAA_CACHE_DIR  = <root>\data\cache
```
另有 `MAA_STATE_DIR=<root>\data\state_lX`（由脚本按台指定）。

## 外部依赖（不在项目内的）
- **主机 MRFZ-0000 (100.79.173.69)**：MuMu 模拟器（5 台）+ ssh 通道（key: `C:\Users\xiach\.ssh\maaorch_target`）
- **ssh 隧道**（本机 → 主机）：16522→17184(l-1) · 16523→17280(l-2) · 16524→16672(l-4) · 16520→16452(l-5) · 16521→17028(l-7)
- AUTO-MAS（主机 36163 API）：机队账号日常已停用（改走本系统）

## 迁移说明
- 原零散位置：`D:\MAA-CLI\`（脚本+日志）、`%APPDATA%\loong\maa\`（config/data）→ 已全部收敛到本项目。
- 自 2026-10-01 起运行：**本项目为唯一事实源**；`D:\MAA-CLI` 与 `%APPDATA%\loong\maa` 仅剩历史残留（可清理）。
