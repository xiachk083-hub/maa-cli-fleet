# maa-cli-fleet

用 **maa-cli** 管一支明日方舟「肉鸽机队」：**日常全链 + 肉鸽连刷**，带循环调度、卡死自愈、重启恢复。

## 这是什么

每台机器跑一条任务链；一个调度器决定「什么时候跑什么」：

- **日常**：唤醒 → 刷理智(1-7，自动连战) → 公开招募 → 基建换班 → 信用购物 → 领取奖励
- **肉鸽**：连刷（水月 / 萨米 / 萨卡兹…，`starts_count=99999`，不主动停）
- **节奏**：肉鸽一直刷 → 理智快回满前插入一次日常（不浪费回复）→ 回肉鸽；约 16 小时一轮
- **自愈**（每 5 分钟自检）：
  - 进程没了 / 日志停转 / 连环报错 → 关游戏重开 + 重发当前功能
  - 设备掉线 / 主机重启 → 启模拟器 + 自动查端口 + 两端 adb connect + 重建隧道 + 发任务
  - 连续 3 次救不回 → 冷却 30 分钟再试（并写日志）
- **两个功能独立**：日常与肉鸽是两条任务文件；每次启动前都会先关游戏，从干净状态拉起。

## 目录结构

```
maa-cli-fleet/
├── bin/          maa.exe + adb（不入库，见 bin/README.md）
├── core/         MaaCore 运行库 + resource（不入库，见 core/README.md）
├── config/       =MAA_CONFIG_DIR
│   ├── profiles/    连接配置（adb 路径 / 全局资源）
│   ├── tasks/       日常与肉鸽任务文件（TOML）
│   ├── fleet.example.json   连接信息模板（ssh 主机/密钥）
│   └── fleet.local.json     本地实配（不入库）
├── data/         =MAA_DATA_DIR（运行状态/缓存，不入库）
├── ops/          运维脚本与运行产物
│   └── rogue_cli_ops.ps1    ★ 主脚本（调度 / 自愈 / 恢复）
├── docs/         文档（运维手册、参数手册、循环图）
└── tools/        装配脚本（预留）
```

## 快速开始

```powershell
# 1) 就位：按 bin/README.md 与 core/README.md 放置 maa.exe / MaaCore+resource
# 2) 连接信息：复制模板并填你的 ssh 目标
copy config\fleet.example.json config\fleet.local.json
# 3) 机表：编辑 ops\rogue_cli_ops.ps1 顶部 $Machines（名 / 实例 idx / 隧道口 / 任务名）
# 4) 体检
.\ops\rogue_cli_ops.ps1 status
# 5) 开循环（后台 worker：肉鸽↔日常 + 自愈）
.\ops\rogue_cli_ops.ps1 cycle l-1
```

## 常用命令

| 命令 | 作用 |
|---|---|
| `status [机]` | 体检：maa 进程 / 日志年龄 / 隧道 / 游戏 |
| `daily <机\|all>` | 只跑日常（自动先关游戏） |
| `rogue <机\|all>` | 只刷肉鸽 |
| `chain <机\|all>` | 一次性：日常 → 关游戏 → 肉鸽 |
| `cycle <机> / cycle-stop` | 日常↔肉鸽循环（含自愈），后台 worker |
| `watch <机> / watch-stop` | 只自愈，不跑日常 |
| `recover <机\|all>` | 主机重启/掉线后恢复（启模拟器+隧道+发任务） |
| `fix / fix1 / fix2 / fix3 <机> [daily\|rogue]` | 手动阶梯修复：重发 → 关游戏重开 → 重启模拟器 |

## 文档

- `docs/maa-cli-fleet-ops.md` — 运维手册（架构 / 命令 / worker 逻辑 / 回滚）
- `docs/fleet-daily-cycle.html` — 24 小时循环图（理智曲线 / 日常节拍 / 自愈）
- `docs/maa-cli-daily-config.html` — 日常任务全参数
- `docs/maa-cli-roguelike-config.html` — 肉鸽参数
- `docs/maa-cli-multi-instance.html` — 多实例隔离与配置机制
- `docs/emulator-precise-launch.html` — 模拟器精准启动（MuMuManager）

## 说明

- 本仓库只含**脚本 + 配置 + 文档**；二进制（bin/）、核心库（core/）、运行状态（data/、ops 产物）通过 `.gitignore` 排除，按各目录 README 自行获取。
- maa-cli 版本锚点：`0.7.5`（联网验证于 2026-10）。
