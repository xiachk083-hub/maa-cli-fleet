# MAS 日常 canary（单账号脚本执行）

> 2026-10-23。`tools/mas_daily.py`：只对**已存在的 MAS 账号**做 direct dispatch，
> 默认 dry-run；`--apply` 才真执行。不创建/修改账号或脚本。

## 为什么不是直接批量

- MAS 的 `并行1..7` 队列 `TimeEnabled=true`，会自动跑；
- 直接 `POST /api/dispatch/start`（`mode=ScriptConfig`）虽然不走 QueueConfig，但**不会绕过 MAS 的锁**；
- 如果账号同时在自动队列里，手动 dispatch 可能和定时任务重复；
- 当前盘点：**active MAS 账号全部在自动队列里**，所以工具默认 `BLOCKED`。

## 用法

```powershell
# dry-run（默认）：只检查，不发
python tools\mas_daily.py --account a01 `
  --fleet-root E:\maa-cli-fleet `
  --mas-api http://127.0.0.1:36163 `
  --mas-history E:\AUTO-MAS\history

# 真执行 + 等 history
python tools\mas_daily.py --account a01 --apply --wait --timeout 1800
```

## 安全闸

| 条件 | 结果 |
|---|---|
| fleet 同账号 `enabled=true` | `BLOCKED`（双控） |
| MAS `Status=false` | `BLOCKED` |
| 脚本在 `TimeEnabled=true` 队列 | `BLOCKED`（防与定时队列撞车） |
| 当前游戏日已有 history | `BLOCKED`（防重复刷） |
| 15 分钟内有 history 活动 | `BLOCKED`（可能在跑） |

`--force` 只应作为人工确认后的例外；它不会绕过 fleet 双控/Status 检查。

## 执行链路

1. 读 MAS scripts/users + history + queue；
2. 找到唯一 user/script；
3. 检查安全闸；
4. `POST /api/dispatch/start {"mode":"ScriptConfig","taskId":"<userId>"}`；
5. 返回 `taskId`，写 `dist/mas_daily_ledger.json`；
6. `--wait` 轮询 `history/<游戏日>/<账号>/*.json`，解析 `maa_result` / `sanity`。

## 第一次 canary 的三种选择

1. **推荐**：04:00 换游戏日后，临时把 canary 的脚本从自动队列里移除（或暂停该队列项），
   直接 dispatch，验证后恢复队列；只影响一个账号。
2. 现在直接 `--force`：接受该账号当天重复跑一次日常（浪费理智），用于验证链路。
3. 用 `Status=false` 的账号：MAS 定时队列不会跑它，但 direct dispatch 是否会被 MAS 跳过需要实测。

当前不建议 2/3；先做 1，并备份 `QueueConfig.json`。

## 结果

- history JSON 字段：`maa_result`（完成/失败）、`sanity`、`drop_statistics`；
- 台账：`dist/mas_daily_ledger.json`（account / scriptId / userId / taskId / status / historyFile / maaResult / sanity）；
- 后续看板/通知直接读这份台账。
