# MAS 盘点 / 调度 Plan

> 2026-10-23 修订：**MAS 不是过渡执行器，而是长期日常管理/执行平台**。
> 边界：MAA 负责游戏内自动化；MAS 负责账号/脚本/队列/历史/通知；本项目只做调度、首次登录和资源编排。
> 本工具默认**只读**，不改 MAS、不 dispatch。

## 用途

- **inventory**：同时看 MAS 脚本/账号和 fleet 账号，回答“谁在谁手里、今天清了没有”；
  MAS 的“用户”是**脚本内的用户记录**，不等于唯一账号；看板分别显示脚本数、用户记录、唯一账号 ID、启用唯一 ID、共享脚本数。
- **plan**：给一份 desired JSON，输出 `CREATE_USER / CREATE_SCRIPT / UPDATE_USER / UPDATE_SCRIPT / UPDATE_QUEUE / NOOP / BLOCKED`，只做计划，不落地。
  会检查：账号是否存在、`Emulator.Index`、`Info.Path`（MAA 槽位）、`Run` 参数、`Server/Stage/Annihilation/Task`、脚本在哪个队列、队列 `TimeEnabled`。

## 肉鸽 / 日常循环

`inventory` 现在把直接调度 MAA 的肉鸽车道也纳入同一张表：

- 数据源：`runner/state.json` 的 `daily:<id>`、`rogue:<id>`，以及 `runner/hb_rogue_<id>.txt` 车道心跳；
- `loopState`：`rogue`（肉鸽跑着）、`paused`（日常暂停肉鸽）、`daily`（日常插队）、
  `resuming`、`stopped`、`stale`（车道心跳 >15 分钟）、`disabled`；
- `todayState`：`cleared` / `not_cleared` 只表示**日常**；纯肉鸽号是 `no_daily`，不再算“未清/未知”；
- 新 runner 会在 `rogue:<id>` 写 `phase / phaseAt / lastResult / restarts`；旧 state 仍可读，只是没有这些新字段。

看板卡片与筛选：肉鸽运行、肉鸽暂停/插队、车道心跳异常、肉鸽/循环。

## 运行

在机端（有 `E:\AUTO-MAS` 和 `E:\maa-cli-fleet` 的机器）上：

```powershell
$env:PYTHONIOENCODING = "utf-8"
E:\AUTO-MAS\environment\python\python.exe tools\mas_inventory.py `
  --fleet-root E:\maa-cli-fleet `
  --mas-api http://127.0.0.1:36163 `
  --mas-history E:\AUTO-MAS\history `
  --out E:\maa-cli-fleet\dist\mas_inventory.html `
  --json E:\maa-cli-fleet\dist\mas_inventory.json `
  --print
```

Plan：

```powershell
E:\AUTO-MAS\environment\python\python.exe tools\mas_inventory.py `
  --fleet-root E:\maa-cli-fleet `
  --desired config\mas-desired.example.json `
  --plan-out E:\maa-cli-fleet\dist\mas_plan.json
```

## 输出

- HTML：按归属/今天没清/冲突筛选；手机号、邮箱默认掩码；
- JSON：给后续看板/告警用；
- Plan JSON：每行 `key / action / scriptId / userId / changes / reason`。

## 判据

| 项 | 判据 |
|---|---|
| fleet 今日已清 | `state.json` 的 `daily:<id>` 为 `done` 且 `day == 游戏日`（04:00 切日） |
| MAS 今日已清 | `E:\AUTO-MAS\history\<游戏日>\<账号名>\*.log` 或 `*.json` 存在；API 的 `LastProxyDate` 仅作兜底（当前多为空） |
| MAS 最近 | history 目录里该账号最近一个有日志的日期 + 累计天数 |
| 冲突 | fleet 启用 + MAS 用户存在且 `Status=true` |
| 归属 | `FLEET / MAS / BOTH / NONE`；MAS 用户按名称优先匹配 fleet，其次模拟器 |
| MAS 自动队列 | `并行1..7` 等 `TimeEnabled=true` 队列；有 `*` 标记 |

## MAS API 事实（源码确认）

- `POST /api/scripts/get`：脚本（MAA 脚本含 `Info.Path`、`Emulator.Index`）；
- `POST /api/scripts/user/get`：账号（`Info` / `Data` / `Task`）；
- `POST /api/dispatch/start {"mode":"ScriptConfig","taskId":"<userId>"}`：直接跑一个账号，`queue_id=None`，不走 QueueConfig；返回 `taskId`；
- `POST /api/dispatch/stop`：停止任务；
- 账号/脚本的创建与修改：`/api/scripts/add|update`、`/api/scripts/user/add|update`（本工具**不调用**）。

## 安全

- 只读接口；不调用 add/update/dispatch；
- 不输出密码；手机号/邮箱只输出掩码；
- Plan 中 `accountId` 为占位符/掩码时，缺账号会返回 `BLOCKED`，不会计划创建半成品；
- 真正 Apply 前必须：备份 `ScriptConfig.json` / 用户数据、dry-run、单账号 canary、读回校验。
