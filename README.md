# maa-cli-fleet

用 [maa-cli](https://github.com/MaaAssistantArknights/maa-cli) 管一支明日方舟「肉鸽机队」：
**日常 / 剿灭 / 肉鸽三类任务统一调度**，带设备生命周期、卡死自愈、重启恢复和中心台账。

> 当前实现：Go（`fleet.exe`），机端一个常驻进程即可跑完「环境 → 发车 → 判定 → 上报」。
> 旧 PowerShell 单体栈保留为手动兜底，见文末「旧栈」。
>
> License：**AGPL-3.0**（与 MAA 相同）。

---

## 特性

- **三类任务一个模型**
  - `daily` / `ann`：排队型，跑一次结束，按理智或周次排下一次；
  - `rogue`：常驻型，跑完 / 挂了 / 卡死就立刻重来，模拟器不关机。
- **日常插队**：日常到点会暂停肉鸽（只杀 maa，不动模拟器），跑完自动接回。
- **设备自愈**：起模拟器 → 等 boot → adb connect → 端口漂移识别 → 发车；连续失败会硬重启模拟器。
- **看护判据**：进程退出 / `asst.log` 5 分钟不更新 / 同一 `cur_task` 空转 8 分钟 → 杀 maa 重来。
- **班表（可选）**：把账号均匀铺在 16h 轮盘上，按网格点发车，避免所有号挤在同一时段。
- **资源闸门**：空闲内存 / CPU / 并发位不足时停止入队，等在场任务收工。
- **中心（可选）**：`fleet center` 收各机端上报，提供 `/fleet`、`/health` 只读视图；**机端只做出站连接**。
- **不依赖 PowerShell**：机端只需要 `fleet.exe` + maa-cli + MuMu。

---

## 架构

```
┌──────────────┐   HTTP（只上报）   ┌──────────────────────────────┐
│ fleet center │ ◄──────────────── │ fleet.exe runner（机端常驻） │
│ :8790        │                   │  ├─ 队列 / 班表 / 资源闸门    │
│ 台账 + 事件  │                   │  ├─ MuMuManager / adb         │
└──────────────┘                   │  └─ maa.exe → 模拟器           │
                                   └──────────────────────────────┘
```

- **机端**：管理本机环境、执行任务、上报；不驱动别的机器，不接中心遥控指令。
- **中心**：只收 `register` / `report`，聚合台账与事件；没有 `/command` 通道。
- **跨机**：每台机器各布一个机端；中心可以跑在任意一台常开机上。

设计背景与边界：
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)、[`docs/ARCHITECTURE-NODE-SELF.md`](docs/ARCHITECTURE-NODE-SELF.md)。

---

## 环境要求

- Windows 10 / 11
- [MuMu Player 12](https://mumu.163.com/) + 至少一个 Android 实例
- [maa-cli](https://github.com/MaaAssistantArknights/maa-cli) 与 MaaCore 资源（`fleet setup` 可自举）
- Go 1.24+（仅从源码构建时需要）

---

## 构建

```powershell
git clone https://github.com/xiachk083-hub/maa-cli-fleet.git
cd maa-cli-fleet
go build -o fleet.exe ./cmd/fleet
.\fleet.exe version
```

测试：

```powershell
go test ./...
```

---

## 快速开始（单机）

### 1. 安装 MAA

```powershell
.\fleet.exe setup                 # 下载 maa-cli，准备 data\lib + data\resource
# 网络受限时：
.\fleet.exe setup -direct         # 直连 GitHub 下载 MaaCore 整包
```

### 2. 配置机端

```powershell
copy runner\runner.example.json   runner\runner.json
copy runner\accounts.example.json runner\accounts.json
copy runner\roster.example.json   runner\roster.json   # 可选；不用班表可删
notepad runner\runner.json
notepad runner\accounts.json
```

必须按本机情况修改：

- `runner.json` 里的 `mumuManager`、`adbExe`、`maaExe` 路径；
- `accounts.json` 里的 `emu`（MuMu 实例编号）、`daily` / `rogueTask`（任务文件名）；
- 如果要接中心，填 `centerUrl` 与 `nodeId`，并把 token 写到 `runner/center.token`。

任务文件放在 `config/tasks/`，一个任务一个 TOML（示例见该目录）。

### 3. 试跑

```powershell
.\fleet.exe runner -conf runner\runner.json -status   # 只看队列，不发车
.\fleet.exe runner -conf runner\runner.json -once     # 只跑一轮 tick
.\fleet.exe runner -conf runner\runner.json           # 常驻
```

常驻进程建议由计划任务拉起（见 [`docs/OPS-AUTOSTART.md`](docs/OPS-AUTOSTART.md)）：

```powershell
.\ops\boot_runner.ps1
```

### 4. 可选：中心服务

```powershell
.\fleet.exe center -addr 0.0.0.0:8790
# token 自动生成到 center\state\token.txt；机端用 runner/center.token 对上
```

---

## 任务模型

| 类型 | 行为 | 账号字段 |
|---|---|---|
| `daily` | 跑一次结束；理智快满 / 班表到点再跑 | `daily` |
| `ann` | 每周一次；`annDisabled=true` 可整体关闭 | `annTask` |
| `rogue` | 常驻；退出/卡死立刻重来，不被并发位限制 | `rogueTask` + `resident` |

- **互斥**：同一账号同一时刻只跑一个任务；日常插队会暂停该账号的肉鸽车道。
- **重试**：每个任务最多 3 次；失败超过次数后等下一个游戏日/周再排。
- **班表**：`roster.json` 不存在或 `enabled=false` 时退回「理智自循环」。
- 详细口径见 [`docs/RUNNER-UNIFIED-TASKS.md`](docs/RUNNER-UNIFIED-TASKS.md) 与
  [`docs/DAILY-ROSTER.md`](docs/DAILY-ROSTER.md)。

---

## 常用命令

| 命令 | 作用 |
|---|---|
| `fleet setup [-direct]` | 安装 / 修复 maa-cli 与 MaaCore |
| `fleet env show / check / converge` | 查看 / 检查 / 收敛 MuMu 实例与端口 |
| `fleet env set -emu 1 -key width -value 1280` | 修改单个实例配置 |
| `fleet gen -automas <AUTO-MAS 目录>` | 从 AUTO-MAS 配置导入账号表与任务（临时 ETL） |
| `fleet center [-addr :8790]` | 启动中心服务 |
| `fleet runner -conf runner\runner.json` | 常驻调度器 |
| `fleet runner -status` | 查看队列 / 班表 / 资源 |
| `fleet runner -once` | 只跑一轮 |
| `fleet runner -enqueue daily:a01` | 手工插队一次日常 |
| `fleet runner -set-rogue l7:rogue_jiegarden_l7` | 热切换某账号的肉鸽任务（只动该车道） |
| `fleet runner -now daily:a01` | 把已排任务提前到现在 |
| `fleet runner -cancel daily:a01` | 取消任务 |
| `fleet runner -reset daily:a01` | 重置失败次数并重排 |
| `fleet runner -disable a01` / `-enable a01` | 停用 / 启用账号 |
| `fleet runner -maintenance on|off|status` | 维护模式（不派车、不扫模拟器） |
| `fleet version` | 版本 |

> 热改通过 `runner/queue.in` 合并，runner 每 30 秒 tick 生效；不需要重启进程。

---

## 目录

```
maa-cli-fleet/
├── cmd/fleet/            CLI 入口
├── internal/
│   ├── runner/           调度器（队列 / 班表 / 肉鸽车道 / 设备 / 体检 / 资源闸门）
│   ├── center/           中心服务（台账 / 事件 / 只读 API）
│   ├── setup/            maa-cli / MaaCore 自举
│   ├── env/              MuMu 实例与端口
│   ├── gen/              AUTO-MAS 导入器（临时）
│   ├── model/            共用数据结构
│   └── netproxy/         代理感知 HTTP
├── config/
│   ├── tasks/            任务 TOML（daily / ann / rogue）
│   ├── profiles/         maa-cli 连接 profile（default 为模板）
│   └── fleet.example.json  旧 PS 兜底栈配置模板
├── runner/               运行时（账号表 / 状态 / 班表；默认不入库）
│   ├── runner.example.json
│   ├── accounts.example.json
│   └── roster.example.json
├── ops/                  boot / 兜底脚本与运行日志
├── tools/                辅助脚本（班表生成、截图、MCP 等）
└── docs/                 架构、运维、参数与复盘
```

---

## 安全与隐私

账号表、日志和截图会包含**手机号、游戏昵称、服务器、模拟器端口**等个人信息。仓库已用
`.gitignore` 排除以下运行时文件，**提交前请确认不要强制添加**：

```
runner/accounts.json      # 账号表（手机号/昵称）
runner/state.json         # 任务台账
runner/runner.json        # 本机路径 / center 地址 / nodeId
runner/center.token       # 中心共享密钥
config/fleet.local.json   # 旧栈 SSH 主机与 key 路径
center/state/             # 中心 token / 台账 / 事件
data/、ops/logs/、shots/  # 状态、日志、截图
```

- 截图对外分享时请给 `shot_annotate.py` 加 `--mask-phone`；
- 不要把 `fleet.local.json`、`accounts.json`、`center.token` 粘到 issue / 聊天里；
- 发现仓库里出现凭据或个人信息，请按 [`SECURITY.md`](SECURITY.md) 处理。

---

## 文档

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — 三层结构（历史设计 + 收敛记录）
- [`docs/ARCHITECTURE-NODE-SELF.md`](docs/ARCHITECTURE-NODE-SELF.md) — 机端边界铁律
- [`docs/RUNNER-UNIFIED-TASKS.md`](docs/RUNNER-UNIFIED-TASKS.md) — daily / ann / rogue 统一模型与实测记录
- [`docs/DAILY-ROSTER.md`](docs/DAILY-ROSTER.md) — 16h 连续班表
- [`docs/OPS-AUTOSTART.md`](docs/OPS-AUTOSTART.md) — 开机 / 看门狗
- [`docs/TUTORIAL-AUTOCLOSE.md`](docs/TUTORIAL-AUTOCLOSE.md) — 教程弹窗自动关闭（可选）
- [`docs/LOBBY-SHOT-STANDARD.md`](docs/LOBBY-SHOT-STANDARD.md) — 大厅截图凭证流程（可选）
- [`docs/maa-cli-fleet-ops.md`](docs/maa-cli-fleet-ops.md) — **旧 PS 栈**运维手册（仅兜底参考）

---

## 旧栈（可选）

2026-10-02 之前的 PowerShell 机端 / worker / SSH 隧道仍保留在 `ops/rogue_cli_ops.ps1`，
用于手动排障与回滚；它不是新部署的必需组件。

- `tools/fleet_mcp.py` 是包在旧栈外面的 MCP 网关（11 个工具，写动作默认 dry-run）；
- `center/center.py` 是旧 Python 中心，已被 `internal/center` 的 Go 实现取代；
- `tools/gen_accounts.ps1` 是旧账号表生成器，已被 `fleet gen` 取代。

---

## 说明

- 本项目与 Hypergryph / 鹰角网络、MAA 官方无隶属关系；请遵守游戏与 MAA 的使用条款。
- 仅用于自己账号的自动化；不要用于代练、账号交易或其他违反服务条款的场景。
- 感谢 [MaaAssistantArknights](https://github.com/MaaAssistantArknights/MaaAssistantArknights)
  与 [maa-cli](https://github.com/MaaAssistantArknights/maa-cli)。

---

## License

[GNU Affero General Public License v3.0](LICENSE)（AGPL-3.0），与 MAA 相同。

如果你修改本程序并通过网络向他人提供服务，AGPL 要求你向使用者提供对应的源代码。
