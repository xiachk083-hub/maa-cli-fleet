# MAS 只读盘点 / 接管 Plan

> 2026-10-23。MAS 是过渡执行器，项目最终接管；本工具**只读**，不改 MAS、不 dispatch。

## 用途

- **inventory**：同时看 MAS 脚本/账号和 fleet 账号，回答“谁在谁手里、今天清了没有”；
- **plan**：给一份 desired JSON，输出 `CREATE_USER / CREATE_SCRIPT / UPDATE_USER / UPDATE_SCRIPT / NOOP / BLOCKED`，只做计划，不落地。

## 运行

在机端（有 `E:\AUTO-MAS` 和 `E:\maa-cli-fleet` 的机器）上：

```powershell
$env:PYTHONIOENCODING = "utf-8"
E:\AUTO-MAS\environment\python\python.exe tools\mas_inventory.py `
  --fleet-root E:\maa-cli-fleet `
  --mas-api http://127.0.0.1:36163 `
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
| MAS 今日已清 | `Data.LastProxyDate` 以今天开头 |
| 冲突 | fleet 启用 + MAS 用户存在且 `Status=true` |
| 归属 | `FLEET / MAS / BOTH / NONE`；MAS 用户按模拟器索引优先匹配 fleet，其次昵称 |

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
