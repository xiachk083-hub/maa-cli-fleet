# node —— 机端（跑在目标机上）

> 2026-10-02 收敛定稿：**机端只有三件事 —— 管理环境、执行任务、上报。**
> 收敛前的"接令代理 / worker 守夜 / 隧道驱动"已全部删除（见文末"已退役"）。

## 三件事，一个进程

机端 = **`fleet.exe`**（一个二进制，跑在贴着模拟器的那台机器上）：

| 职能 | 命令 | 说明 |
|---|---|---|
| **管理环境** | `fleet setup` / `fleet env` / `fleet gen` | maa-cli 安装、模拟器实例配置收敛、账号表/任务文件生成 |
| **执行任务** | `fleet runner` | daily / ann 排队 + rogue 常驻车道 + 设备生命周期 + 自愈；**唯一常驻进程** |
| **上报** | （runner 内建） | 启动注册、每 60s 状态（调度口径 + 设备体检）、任务事件；只做出站连接 |

设备体检（随状态上报）：maa pid（runner 自己的发车记录）、状态日志年龄（`asst.log` mtime）、
游戏 pid（adb pidof，只对"常驻机 + 有任务在跑的机"查）。

**边界**（不做的事）：不渲染界面；不做全局策略裁决；不驱动别的机器；不接中心遥控指令
（任务的增删改在机端本地做：`fleet runner -enqueue/-cancel/-now/...`）。

```
                       ┌─────────────── 目标机 ───────────────┐
   center(常开) ◄────────┤ fleet.exe runner（调度 + 上报）      │
   :8790                │   └─ MuMuManager / adb / maa.exe     │
   （只收上报，无指令通道）└────────────────────────────────────┘
```

## 目录

| 文件 | 作用 |
|---|---|
| `install_target.ps1` | 一键布点：拉包 → 放 `fleet.exe` → 写 `runner\runner.json` → 注册计划任务 → 拉起 |
| （运行时）`runner\runner.json` | 机端配置（本机路径 / center 地址 / nodeId；**不入库**） |
| （运行时）`ops\boot_runner.ps1` | 开机自启 + 看门狗：只保证 runner 进程在岗且没卡死（心跳超时则杀掉重拉） |

## 布点

```powershell
# 目标机上（管理员 PowerShell）
.\install_target.ps1 -NodeId node-01 -CenterUrl http://<后端内部地址>:8790 -Token <center/state/token.txt 的内容>
```

计划任务（由 install_target 注册，两件套）：

| 任务 | 触发器 | 动作 |
|---|---|---|
| `FleetRunner-AutoStart` | AtLogon（当前用户 / Interactive / Highest） | `ops\boot_runner.ps1` |
| `FleetRunner-Watchdog` | 每 10 分钟、无限重复、IgnoreNew | 同上（幂等） |

判活两道：**进程在**（认命令行身份，不靠单一 PID——防 PID 复用误判）+ **还在干活**
（`runner\heartbeat.txt` mtime 超时 = 主循环卡死 → 杀掉重拉）。人工停用 `runner\runner.stop`（存在即不拉）。

## 运行

```powershell
fleet.exe runner -conf runner\runner.json     # 常驻（计划任务负责，不必手起）
fleet.exe runner -status                      # 看一眼队列（不启动任务）
fleet.exe runner -enqueue daily:l4            # 手工插一次日常
fleet.exe runner -set-rogue l7:rogue_jiegarden_l7  # 热切换 l7 的肉鸽任务（只动该车道）
fleet.exe runner -cancel ann:l4 / -now daily:a07 / -disable daily:a07 / -enable daily:a07
```

数据归属：账号表 / 状态 / 任务文件 / 日志全在本机（`runner\*`、`config\tasks\*`、`data\*`）；
center 只持有它上报的台账与事件。

## 已退役（2026-10-02 收敛时删除）

| 对象 | 曾经的职责 | 为什么不需要 |
|---|---|---|
| `fleet_node.ps1`（PS 机端代理） | 注册/心跳/状态/事件 + **拉中心指令** | 上报已由 runner 内建；指令通道自建成后 **0 使用**（events.jsonl 无 command 记录），删 |
| `ops\boot_node.ps1` + `FleetNode-*` 计划任务 | 保证机端进程在岗 | 机端只剩 runner 一个进程，`boot_runner.ps1` 已覆盖 |
| `internal/node`（Go 代理包） | PS 桥：调 `ops status` 采集设备状态 | 设备状态改由 runner 原生采集（`internal/runner/health.go`），不再经 PS |
| `Ensure-Workers` / cycle/watch worker | 机端守夜拉起 PS worker | PS worker 全量退役（10-02 20:45 5/5 迁入 runner） |
| 隧道驱动（本机 ssh 连目标机） | 从别的机器驱动模拟器 | 机端只在**本机**驱动；跨机=再布一台机端 |

## 坑（保留，仍在用）

- **`.ps1` 必须带 UTF-8 BOM**：PowerShell 5.1 读无 BOM 文件按本地编码解析，中文字符串会被读坏。
  仓库内 `.ps1` 已统一带 BOM；从 GitHub raw 下载后建议再强制转一次 BOM。
- 常驻进程用 **WMI `Win32_Process Create`** 拉起（`Start-Process` 的子进程会随 ssh 会话结束被杀）。
