# MAS 日常 canary（单账号脚本执行）

> ⚠️ **2026-10-23 修订：direct `ScriptConfig` 已弃用。**
> 实测它会绕过 MAS 调度台：只启动 MAA 进程，不启动模拟器，也不做队列的设备准备。
> 正确路径是 **`AutoProxy` + `taskId=<scriptId>`**（MAS 调度器按脚本单独调度，不需要临时队列）；
> `mas_daily.py` 默认走 AutoProxy，`--unsafe-direct` 仅保留作诊断。

> 2026-10-23。`tools/mas_daily.py`：只对**已存在的 MAS 账号**做检查；
> 默认 dry-run；不再默认直接 dispatch。

## 正确执行路径（已改为 AutoProxy）

```text
POST /api/dispatch/start {"mode":"AutoProxy","taskId":"<scriptId>"}
  → MAS 调度台按脚本调度（启动模拟器 / 加载账号 / 跑日常）
  → 等 history
```

**不需要临时队列**：MAS 调度器支持按脚本单独调度。`ScriptConfig` 直跑才会绕过调度台，
导致模拟器不启动；已默认禁用，仅 `--unsafe-direct` 保留作诊断。

### AutoProxy 是“脚本级”，不是“账号级”

2026-10-23 实测：`AutoProxy + taskId=<scriptId>` 会跑该脚本内**所有启用账号**
（`Status=true` 且 `RemainedDay != 0`），按用户列表顺序逐个跑，不是只跑 Plan 匹配到的那个账号。

a19 的脚本有多个启用账号；第一次 canary 实际先跑了另一个账号，`v` 排在后面，
30 分钟 timeout 时还没轮到 `v`，所以 `history/2026-10-23/v/` 为空——不是 MAS 没跑，而是跑的是整脚本。

因此：

- `mas_daily.py` 默认在“脚本内启用账号 >1”时 `BLOCKED`，避免误跑整脚本；
- `--script` 才明确允许整脚本执行（耗时 = 账号数 × 单账号时间）；
- 单账号执行需要**专用脚本**（推荐），或 `--unsafe-direct` + 先启动模拟器（仅诊断）；
- 不要用“暂停其他账号”做 canary，容易忘记恢复。

## 为什么不是直接批量

- MAS 的 `并行1..7` 队列 `TimeEnabled=true`，会自动跑；
- **正确路径**是 `AutoProxy` + `taskId=<scriptId>`：MAS 调度器按脚本单独调度，
  不需要临时队列，也会负责启动模拟器、加载账号；
- 脚本锁会让“手动按脚本调度”和“自动队列”串行，不会双开；
- `ScriptConfig` 直跑只启动 MAA 进程，不负责设备，已弃用。

## 用法

```powershell
# dry-run（默认）：只检查，不发
python tools\mas_daily.py --account a19 `
  --fleet-root E:\maa-cli-fleet `
  --mas-api http://127.0.0.1:36163 `
  --mas-history E:\AUTO-MAS\history

# 真执行 + 等 history（AutoProxy 按脚本调度）
python tools\mas_daily.py --account a19 --apply --wait --timeout 1800
```

## 安全闸

| 条件 | 结果 |
|---|---|
| fleet 同账号 `enabled=true` | `BLOCKED`（双控） |
| MAS `Status=false` | `BLOCKED` |
| 脚本在 `TimeEnabled=true` 队列 | AutoProxy 允许；仅 `--unsafe-direct` 时 `BLOCKED`（脚本锁串行） |
| 当前游戏日已有 history | `BLOCKED`（防重复刷） |
| 15 分钟内有 history 活动 | `BLOCKED`（可能在跑） |

`--force` 只应作为人工确认后的例外；它不会绕过 fleet 双控/Status 检查。

## 执行链路

1. 读 MAS scripts/users + history + queue；
2. 找到唯一 user/script；
3. 检查安全闸；
4. `POST /api/dispatch/start {"mode":"AutoProxy","taskId":"<scriptId>"}`；
5. 返回 `taskId`，写 `dist/mas_daily_ledger.json`；
6. `--wait` 轮询 `history/<游戏日>/<账号>/*.json`，解析 `maa_result` / `sanity`；
7. 超时（默认 1800s）会尝试 `POST /api/dispatch/stop`，台账记 `timeout`，不把任务留在后台。

## 第一次 canary

不需要动队列：对已存在、绑定正确的脚本直接 `AutoProxy` 调度即可。
`a19` 的 Plan 是 `NOOP`，`AutoProxy` 会走 MAS 调度台；如果脚本也在自动队列里，
脚本锁会让两者串行，不会双开。

`--unsafe-direct` 只用于诊断，不建议在 canary/生产使用。

## 结果

- history JSON 字段：`maa_result`（完成/失败）、`sanity`、`drop_statistics`；
- 台账：`dist/mas_daily_ledger.json`（account / scriptId / userId / taskId / status / historyFile / maaResult / sanity）；
- 后续看板/通知直接读这份台账。
