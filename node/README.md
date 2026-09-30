# node —— 机端（跑在目标机上）

三层里的 **① 机端**：贴着模拟器干活，向中心上报、从中心拉令。
（结构全文见 `docs/ARCHITECTURE.md`）

```
中心(center, 常开)◄── 注册/心跳/状态/事件 ── 机端(node, 目标机)
        ▲                    └── 拉令/回执 ──►（执行 ops 命令，驱动模拟器）
        └── REST ── 前端(console, 暂缓)/CLI/Agent
```

## 文件

| 文件 | 作用 |
|---|---|
| `fleet_node.ps1` | 机端本体：`run`（常驻）/ `once`（单轮）/ `status` / `stop` |
| `conf.json` | 机端配置（含 token，**不入库**；模板见 `conf.example.json`） |
| `install_target.ps1` | 一键布点：拉 GitHub 包 → 解压 → 写配置 → adb connect → 拉起机端 |
| `node.log` / `node.pid` / `node.stop` | 运行态 |

## 机端在干什么（每轮）

1. **注册**（每 10 分钟）：`node_id` / 机型表 → 中心；
2. **状态上报**：跑一次 `ops status`（本地模式=直连模拟器）→ 结构化上报（maa/日志年龄/连通/游戏 pid/健康）；
3. **事件增量**：把 `ops\rogue_cli_ops.log` 新增行同步给中心；
4. **拉令**（长轮询 ≤25s）：拿中心指令 → 执行 → 回执。**写指令需 `apply=true` 才真跑**（否则只回 dry-run 计划）。

## 布点（目标机）

```powershell
# 目标机上（管理员 PowerShell）
.\install_target.ps1 -NodeId host-mrfz0000 `
  -CenterUrl http://<中心内部地址>:8790 -Token <center/state/token.txt 的内容>
```

要点：
- **资源目录**：目标机的 `<项目>\data` 下必须有 `lib` 与 `resource`（maa-cli 按 `MAA_DATA_DIR` 找）。
  安装过 maa-cli 的机器一般已有现成的（`%APPDATA%\loong\maa\data\{lib,resource}`）——
  脚本会自动建 **junction** 指过去（别复制几百 MB）；缺了 maa 会报 `Resource directory not found!` 秒退。
- **包走 GitHub**（`codeload.../zip/refs/heads/master`），不走慢速 ssh；
- 解压**不会覆盖**本地配置（`config/fleet.local.json`、`node/conf.json` 都不在包里）；
- 同机已有 maa-cli（如 `E:\MAA-CLI\bin\maa.exe`）会被**复用**，不搬大文件；
- 常驻进程用 **WMI `Win32_Process Create`** 拉起（`Start-Process` 起的子进程会随 ssh 会话结束被杀）。

## 本机模式（目标机上不再走隧道）

`config/fleet.local.json`：

```json
{
  "localMode": true,
  "adbPath": "E:\\MuMu Player 12\\shell\\adb.exe",
  "maaExe":  "E:\\MAA-CLI\\bin\\maa.exe",
  "machines": [ {"name":"l-1","emu":"25","state":"state_l1","rogue":"rogue_sami_l1","daily":"daily_l1","local":"17184"} ]
}
```

- `localMode=true` → `Invoke-HostPs` 本地跑、`Test-Tunnel` 改判 adb 直连、`recover` 不再建 ssh 隧道；
- `machines` 可覆盖机器表（本机模式下 `local` = 模拟器在该机上的 adb 端口）；
- 默认（不设 localMode）仍是"本机经 ssh 隧道驱动"的老模式，两种模式同一份 ops 脚本。


## 开机自恢复（目标机重启/蓝屏后无人介入）

```
开机/登录 → 计划任务 FleetNode-AutoStart → opsoot_node.ps1 → 机端
                                                              ↓ Ensure-Workers
                                        cycle/watch worker（缺失就拉起）
                                                              ↓ Watch-Once 设备级自愈
                                        模拟器 → adb connect → maa 任务 → 上报中心
```

- **计划任务**（照抄本机 AUTO-MAS 那套可用配置）：
  `Administrator / Interactive / Highest / AtLogon` →
  `powershell -NoProfile -ExecutionPolicy Bypass -File <项目>\opsoot_node.ps1`
- `boot_node.ps1`：幂等；只保证"机端进程在"（已在跑就跳过），其余交给机端/worker。
- 机端 `conf.json` 加 `workers` 映射（例：`{"l-1":"cycle",...,"l-5":"watch"}`）：
  机端启动时 + 每 10 步检查 worker pid，不在岗就拉起。
- **实测（2026-10-01）**：杀掉机端+全部 worker → 跑 boot 脚本 → 机端 1 + worker 5 全自动回来。
- 蓝屏后实测：07:31:36 拉起 → 07:33:28 五台模拟器+任务全部恢复（约 2 分钟）。

### 部署坑（重要）
- **`.ps1` 必须带 UTF-8 BOM**：PowerShell 5.1 读无 BOM 文件按本地编码解析，中文字符串会被读坏
  （症状：WMI 拉起机端返回 `rc=21 无效参数`）。仓库内 .ps1 已统一带 BOM；
  从 GitHub raw 下载后建议再强制转一次 BOM：
  `[IO.File]::WriteAllText($p, [IO.File]::ReadAllText($p,[Text.Encoding]::UTF8), (New-Object Text.UTF8Encoding($true)))`

## 当前部署（2026-10-01）

| 机端 | 机器 | 中心 | 备注 |
|---|---|---|---|
| `local-desktop` | 本机（隧道模式，驱动全队） | `127.0.0.1:8790` | 现役控制方 |
| `host-mrfz0000` | 主机 MRFZ-0000（本机模式） | `http://100.111.209.108:8790` | 已布点、只读上报（尚未接管任务） |
