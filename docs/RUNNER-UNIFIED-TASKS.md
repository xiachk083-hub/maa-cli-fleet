# 三类任务统一到一个 runner（daily / ann / rogue）

> 定稿 2026-10-02。原则（用户定调）：**剿灭、日常、肉鸽本质只是三种任务**，
> 不该有两套引擎（Go runner 跑日常/剿灭、PS cycle/watch worker 跑肉鸽）。

## 模型

```
机端 = 一个调度器（fleet.exe runner）
任务 = {账号/实例, 类别 ∈ daily | ann | rogue, 任务文件, 参数}
  daily / ann  排队型：跑一次就结束，跑完按理智/周次排下一次（原有逻辑不动）
  rogue        常驻型：只要这台机在就一直刷——跑完/挂了/卡死 → 立刻重来；实例不关机
互斥：日常到点插队 → 暂停肉鸽（杀 maa、模拟器不动）→ 跑完日常 → 自动接回肉鸽
```

`runner/accounts.json` 里，肉鸽机就是一个带 `rogueTask` 的普通账号：

```json
{ "id": "l4", "name": "l-4 肉鸽机（日服·水月）", "emu": "9", "daily": "daily_l4",
  "rogueTask": "rogue_mizuki_l4", "resident": true,
  "stateDir": "E:/maa-cli-fleet/data/state_l4", "enabled": true }
```

- `rogueTask`：肉鸽任务文件名（有值 = 这台机常驻肉鸽）；
- `resident`：常驻机 → 永不参与"跑完停机"的轮转；
- `stateDir`：`MAA_STATE_DIR` 覆盖（肉鸽机沿用老目录 `data/state_l4`，不丢缓存）；
- 日常/剿灭字段照旧 —— 同一台机可以三种任务都有。

## 实现（Go 侧，commit `4b535c0`）

| 文件 | 改动 |
|---|---|
| `internal/model/model.go` | `Account` 加 `rogueTask` / `resident` / `stateDir` |
| `internal/runner/rogue.go`（新） | 常驻车道：每台肉鸽机一条 goroutine；起机→关游戏→发车→看护→重来 |
| `internal/runner/runner.go` | 车道启动/停止、`syncQueue` 建 rogue 台账、并发位只发 daily/ann、上报带肉鸽状态 |
| `internal/runner/slot.go` | `runTask` 对 resident 账号先暂停肉鸽、跑完放开；常驻机不关机；`startMaa`/`judgeOutFile` 拆出（肉鸽长跑不能用 75 分钟超时套） |

看护判据（不靠 PID、不靠固定超时）：
- maa 进程退出 → 判 out 文件（Completed/Error）→ 记结果 → 立刻重来；
- `asst.log` 5 分钟没更新 → 判卡死，杀 maa 重来；
- 连续两轮连设备都起不来 → 硬重启模拟器（沿用"坏 VM 阶梯"）。

## Canary：l-4 切到 runner（2026-10-02 06:38，TESTED）

切换动作（可回滚）：
1. 停 l-4 的 PS `cycle-worker`、删 `ops\cycle_l-4.pid`、杀它正在跑的肉鸽 maa；
2. `runner\accounts.json` 加 `l4`（如上）；
3. `runner\state.json` 种 `daily:l4`（PS worker 06:06 已跑完当天，`nextDueAt=22:15`，避免切换后重复日常）；
4. `node\conf.json` 的 `workers` 去掉 `l-4`（机端不再拉 PS worker），重启机端；
5. 换 `fleet.exe`（旧版存 `fleet.exe.bak`）→ `FleetRunner-Watchdog` 拉起 runner。

验收证据：
```
06:38:57 调度器启动：并发位 4，账号 50 个
06:39:03 [rogue:l4] 常驻肉鸽启动（任务 rogue_mizuki_l4，实例 9）
06:39:14 [rogue:l4] 已发车 rogue_mizuki_l4（pid=36404，日志 rogue_mizuki_l4_1002_063914.out）
```
- l-4 的 maa：`E:/maa-cli-fleet/bin/maa.exe --batch run rogue_mizuki_l4 -a 127.0.0.1:16672`；
- 状态目录复用成功：`data\state_l4\debug\asst.log` 持续更新（日志年龄 0 分）；
- 机端巡检（PS 侧不受影响）：`l-4 | maa=pid36404 | 日志年龄=0分 | 隧道=通 | 游戏=32660 | OK`；
- PS worker 只剩 l-1/l-2/l-5/l-7 四个（l-4 的已退役）。

回滚（一条即可）：杀掉 `fleet.exe` → `fleet.exe.bak` 换回 `fleet.exe` → 用旧 `fleet.exe runner` 起回 →
把 `l-4":"cycle"` 加回 `node\conf.json` 并重启机端 → `ops\rogue_cli_ops.ps1 cycle l-4` 拉回 PS worker。

### 日常插队 / 接回（10-02 08:21，TESTED）

`fleet.exe runner -enqueue daily:l4` 实测：

```
07:29:00 [daily:l4] 开始 l-4 肉鸽机（daily，第 1 次）
07:29:00 [rogue:l4] 日常插队 → 暂停肉鸽（杀 maa pid=30748）
08:21:28 [judge] daily_l4_1002_080036.out: completed=6 → 成功
08:21:28 [daily:l4] 理智 0/169 → 下次日常 2026-10-03 00:45:28（16.4 小时后，自循环）
08:21:28 [daily:l4] 常驻机 → 保持开机，肉鸽接回
08:21:28 [rogue:l4] 日常结束 → 放开肉鸽
08:21:37 [rogue:l4] 已发车 rogue_mizuki_l4（pid=35832）
```

- 暂停期间设备只给日常（只有一个 maa 进程，无双开）；日常跑完 9 秒内肉鸽接回；
- 下一次日常自动按理智排到 10-03 00:45（自循环，与 PS 版一致）。

### 顺手修掉的老 bug：判据永远判失败（10-02 08:2x，TESTED）

旧判据找 `"] Completed"` 这种连在一起的子串，而 MAA 实际输出是
`[开始唤醒] 07:29:41 - 07:32:03 (2m 21s) Completed` —— **永远匹配不上**：
所有日常都被判 ok=false → 重试 3 次后 failed → 队列只增不减（当时"今日日常完成 0/50"）。
改成正则核对 summary 时间行（`] HH:MM:SS - HH:MM:SS (耗时) Completed|Error`），保留"刷理智 Error 不算整轮失败"的容错。
修后实测：`[judge] daily_a46_...out: completed=6 → 成功`、`daily_l4: completed=6 → 成功`、`daily:a40/a49 → 成功`。

### 12 小时运行暴露的问题 + 修复（10-02 20:2x，TESTED）

| # | 现象 | 根因 | 修复 |
|---|---|---|---|
| 1 | l-4 肉鸽 14:03 停摆，车道 6 小时没反应 | 看护循环里 `case <-done` 已收过 `Wait()` 的值，后面又 `<-done` **永久阻塞** | 记 `exited` 标志，自己退的不再收第二次；顺带加"看护中"心跳行（每 10 分钟） |
| 2 | l-1 10:57 起静默不动 | PS `Watch-Once` 设备恢复计数 >4 后**什么都不做**（"冷却 30 分钟后重试"是空话） | 冷却到期重置计数并重试 |
| 3 | 每 20 分钟"关孤儿 idx=13"，且会**关错模拟器** | `info -v all` 是 JSON，字段字母序（adb_port/is_process_started 在 index 前）→ 按行解析把属性算到**上一个实例**头上 | 改为按对象切块正则解析（不能用 json.Unmarshal：实例名带未转义字符） |
| 4 | 体检误报 l-1/l-4"隧道断/maa 无" | 模拟器重启后 adb 端口漂移（16672→16673、17184→17185），机器表还是旧端口 | worker 的 recover 把新端口写回 `fleet.local.json`；体检用 `info -v all` 的实时端口 |
| 5 | 进程活着但卡死时没人管 | 外层看门狗只看"进程在不在" | runner 每轮 tick 写 `heartbeat.txt`、每个肉鸽车道写 `hb_rogue_<id>.txt`；`boot_runner.ps1` 发现心跳停 >6/15 分钟就杀掉重拉；`boot_node.ps1` 用体检日志新鲜度做同样的事 |


## 待办


1. 5 台全切（l-1/l-2/l-5/l-7）→ 退休 PS `cycle-worker`/`watch-worker`、机端 `Ensure-Workers`、`FleetNode-*` 任务。
2. `fleet gen` 要保留常驻账号：现在 gen 从 AUTO-MAS 重建账号表，会把手工加的 `l4` 冲掉（gen 已保留 emu 覆盖，但没保留 resident 条目）。
3. `fleet runner -status` 的"在跑 N"永远显示 0（进程启动时把读到的 running 当"重启回收"，只影响显示，不影响调度）——顺手修一下更好。
4. 队列排序：91 个待跑任务里，同优先级按 map 随机序领（BuildPlan 会把热改以外的优先级统一降到 10）——"越早 enqueue 越先跑"不明显，不值得急，但可整理。
