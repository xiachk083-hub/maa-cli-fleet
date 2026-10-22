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
{ "id": "l4", "name": "l-4 肉鸽机（日服·萨卡兹）", "emu": "9", "daily": "daily_l4",
  "rogueTask": "rogue_sarkaz_l4", "resident": true,
  "stateDir": "<项目>/data/state_l4", "enabled": true }
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
- l-4 的 maa：`<项目>/bin/maa.exe --batch run rogue_mizuki_l4 -a 127.0.0.1:16672`；
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


### 全量迁移：5/5（10-02 20:45，TESTED）

l-4 canary 稳住后，其余 4 台一次性切完（同一套动作）：

```
20:45:26 kill node（改 conf 前）→ workers 清空 → boot_node 拉起
20:45:29-30 退役 4 台 PS worker + 杀它们的 maa（l-1/l-2/l-5/l-7）
20:45:34 accounts.json 加 4 个常驻账号（共 54 个：49 日常 + 5 肉鸽）
20:45:35 state.json 种 daily（l-1→10-03 09:22、l-2→01:24、l-7→01:09，取自 PS worker 自己的理智推算；
          l-5 是 watch 机，按现状不带日常）
20:45:36 调度器启动：账号 54 个
20:45:36 [rogue:l1/l2/l4/l5/l7] 常驻肉鸽启动
20:45:42-59 5 条车道全部发车
20:49 体检 5 台全 OK，5 条车道日志年龄 0 分，hb_rogue_l1/l2/l4/l5/l7 心跳都在跳
```

- 附带修复：`syncQueue` 支持 `daily=""`（= 这台机不跑日常，如 l-5 只刷肉鸽）；
- l-5 首次发车 20 秒后自行退出（游戏重启时序），车道 15 秒后自动重发并稳定 —— 正是肉鸽车道该有的行为；
- PS worker 归零（`rogue_cli_ops.ps1 *-worker` 无进程），机端 `workers` 为空表。


## 剿灭（ann）修复（10-02 21:4x，TESTED）

45 个账号的剿灭一整周全灭，是两个坑叠加：

### 坑 1：stage 写法（任务加载即失败）
`gen` 从 AUTO-MAS 抄了**中文显示名** `龙门市区`。MaaCore 的 `Fight.stage` 必须是 resource 里的**任务名**（实测 maa-cli 0.7.5，用假设备地址做任务加载试错）：

| stage 写法 | 结果 |
|---|---|
| `龙门市区` / `LungmenDowntown` / `Chernobog` | ❌ `Unknown task … Cannot set stage` |
| `LungmenDowntown@Annihilation` | ✅ 过加载（随后正常连设备） |
| `Annihilation` | ✅ 过加载（不切关卡，只打当前选中的剿灭） |

映射：龙门市区=`LungmenDowntown@Annihilation`、龙门外环=`LungmenOutskirts@Annihilation`、切尔诺伯格=`Chernobog@Annihilation`。
关卡切换的 OCR 文本在各客户端资源里已本地化（JP：`チェルノボーグ`/`龍門郊外`/`龍門市街`），所以 **stage 用英文任务名是客户端无关的**。

### 坑 2：缺"开始唤醒"（跑 6~35 秒就挂）
原来的 `ann_*.toml` 只有一段 `Fight`。剿灭跑的时候模拟器通常是**冷启**（刚起机、游戏没开）→ MAA 在 `StageBegin` 里 5 次重试都找不到任何作战入口 → `Fight Error`。

修复：`gen` 生成 ann = `开始唤醒`（StartUp，含 client_type / 官服账号掩码，与 daily 同一段）+ `Fight(stage=<关卡>@Annihilation)`；
存量 45 个 `ann_*.toml` 按同规则重建（StartUp 块从各自的 daily toml 抄），`state.json` 里本周 45 条 failed 记录清掉重排。

实测：
```
21:47:13 [ann:a40] 完成 ok=true 耗时=4m16s
21:48:04 [ann:a11] 完成 ok=true 耗时=4m7s
21:49     done=2 / running=3 / queued=39（队列消化中）
```

注意：剿灭有**每周上限**，跑满后 MAA 自己会识别"本周已完成"；runner 每周只排一次（`weekKey`）。

### 10-02 21:53 起：先不刷剿灭（用户决定）
`runner/runner.json` 加 `"annDisabled": true`（代码提交 `3bfb692`）→ runner 不再排任何 ann 任务；
同时把 state.json 里当批 44 条 ann 记录清掉、在跑的 ann maa 一并停掉。
**恢复方式**：把 `annDisabled` 去掉或置 false，重启 runner（`FleetRunner-Watchdog` 会拉起）即可；
任务文件（`ann_*.toml`，已修好）和账号表的 `annTask` 都原样保留。


## 主题切换：l-4 水月 → 萨卡兹（10-03 08:30，TESTED）

用户 2026-10-03 指示「L-4 的水月刷完了，接下来刷萨卡兹」。只改数据、不改代码：

1. 新增 `config/tasks/rogue_sarkaz_l4.toml` —— 与 l-2 同配（`theme=Sarkaz` / `difficulty=2147483647` /
   维什戴尔 / 先手必胜 / 借干员+非好友助战 / 不投资），沿用既定口径「水月·傀影=3、其余=MAX」；
2. `runner/accounts.json`：l4 的 `rogueTask` → `rogue_sarkaz_l4`、`name` → `l-4 肉鸽机（日服·萨卡兹）`
   （改动前备份 `runner/accounts.json.bak-20261003-081955`，替换后逐字节核对：只有这两处 + 中文长度差，46 个账号条目未动）；
3. 优雅重启：落 `runner/runner.stop` → runner 08:20:14 收到停止信号、停 5 条肉鸽车道、drain 在场任务后退出；
   `FleetRunner-Watchdog` 08:30:24 拉起新 runner。

实测证据：

```
08:30:34 [rogue:l2] 已发车 rogue_sarkaz_l2（pid=10052）
08:30:37 [rogue:l4] 已发车 rogue_sarkaz_l4（pid=11072，日志 rogue_sarkaz_l4_1003_083037.out）
maa 命令行：<项目>/bin/maa.exe --batch run rogue_sarkaz_l4 -a 127.0.0.1:16672
state.json：{"key":"rogue:l4","file":"rogue_sarkaz_l4","state":"running","note":"肉鸽跑着"}
hb_rogue_l4.txt 心跳正常；车道实例 emu9 / 16672 不变（缓存目录仍 data/state_l4）
```

回滚：`rogueTask` 改回 `rogue_mizuki_l4`（任务文件仍在）+ 同法重启 runner。另 4 台主题未动：l1=萨米、l2=萨卡兹、l5/l7=水月。

### ⚠️ 真正的坑：换主题前必须先结算/放弃「进行中的探索」（MAA 不会自己干）

重启后 l4 并没有开始刷萨卡兹——两次发车都是 3.5 分钟就「退出」，asst.log 里是：

```
Sarkaz@Roguelike@SelectTheme  OcrDetect [{ text: 仮説集, rect: [806,409,51,18] }]  →  ClickSelf ×20
「exec times exceeded the limit」→ Roguelike 链结束（judge 还把它判成 ok=true completed=2）
```

抓屏看得清楚：日服 IS 主题列表里，l4 的**水月还有一局探索进行中**（卡片显示「探索中」），
其余三张卡（萨米/萨卡兹/界园）全是「他のテーマ進行中につき現在進入不可」——**游戏根本不让进**，
MAA 点 20 次也进不去。查 MAA 资源确认它没有这个场景的处理（全库搜不到「進入不可」，
`Sarkaz@Roguelike@SelectTheme` 只是「OCR 到仮説集→点它」）→ **换主题必须先把当前主题的探索结算掉**。

人工结算（08:42-08:44，adb 点屏，实测通过）：

| 步 | 操作 | 坐标（1920×1080） |
|---|---|---|
| 1 | 主题列表点水月卡下的「テーマへ »」 | (480, 997) |
| 2 | 水月主题页点「探索を続ける」 | (1680, 845) |
| 3 | 结算页确认（✓） | (960, 973) → (960, 888) |
| 4 | 返回（←）主题列表 → 四张卡全部恢复「テーマへ »」（无 探索中 / 進入不可） | (65, 60) |

（第 2 步进去就是**结算页**——因为之前 maa 被停掉时那局其实已经结束了，只是没结算。）

**做人工操作时怎么让车道别抢设备**：常驻车道每轮都会 force-stop 游戏 + 起 maa，人工点屏会被冲掉。
临时把 `<MuMu 安装>\nx_main\MuMuManager.exe` 改名（本次 `.paused-tmp`）→ `MuMuInfo` 空 →
`DeviceUp` 转 90×5s 空转（**不 force-stop、不发 maa**），车道被“冻”约 7.5 分钟，人工点屏不被打断；
点完改回名字，车道下一次 DeviceUp 立刻恢复（08:44:03 自己发车）。

⚠️ 副作用（改名窗口内）：所有机端设备操作一起被冻 —— 期间只有 1 条日常因此记一次「设备未就绪」
（`daily:a30`，emu30 当日本来就在反复起不来，见下）；**别在忙时段用这招**，用完立刻改回名字。

修好后的实测：

```
08:44:03 [rogue:l4] 已发车 rogue_sarkaz_l4（pid=7004）
08:51:56 asst.log: Roguelike theme Sarkaz | mode 0 | difficulty 2147483647
08:47:40 cur_task = Sarkaz@StartExplore@Roguelike@RecruitOther / ChooseOperFlag   ← 真在萨卡兹里招人开局
```

（期间 maa 还因为一次读屏踩到游戏 loading 页 → `NextLevel`/`AbandonConfirm` 20 次失败 → 车道自动重发，
这是肉鸽车道该有的行为；重发后仍回到 Sarkaz 主题继续跑。）

**建议**：把「切主题」做成两步前置检查 —— ① 主题列表里当前主题是否「探索中」；有则先跑一个
「结算/放弃」任务模板（MAA 里等价的是进主题页 → 放弃），② 再切目标主题。否则任何主题切换都会踩这个坑。

### 坑：drain 超时的在场日常会变孤儿（本次暴露，未修）

`stop` 的 drain 上限 10 分钟；超时后 runner 直接退出，**在场 maa 不被杀**。本次 `daily:a27` 就是这样
（08:25:50 起跑的 pid 28192 没被杀），新 runner 的 `recoverResidual` 又把同一任务重排 → 同一模拟器
（17248）上同时两个 maa —— 手工 `taskkill /PID 28192 /T /F` 清掉后恢复正常。
**建议**：drain 超时就由 runner 亲自杀掉在场 maa（或重启前先确认无在场任务）。

## 主题切换：l-5 水月 → 萨米（2026-10-05 09:06，TESTED）

用户指示「L-5 水月刷完了换成萨米」。没重启 runner（其它车道不动）。

1. 新增 `config/tasks/rogue_sami_l5.toml`，配装同 l-1（Sami / difficulty=MAX / 维什戴尔 / 先手必胜 / 借干员+非好友 / 不投资）。
2. 机端 `runner/accounts.json`：l5 `rogueTask` → `rogue_sami_l5`，名字改为「日服·萨米·只肉鸽」。备份 `accounts.json.bak-20261005-l5sami`。只动这两处。
3. 当前车道内存里任务名仍是 `rogue_mizuki_l5`，所以同内容写进该文件（备份 `rogue_mizuki_l5.toml.bak-20261005-l5sami`）。下次 runner 重启会改走 `rogue_sami_l5`。
4. 只杀 l-5 的 maa，车道自己重发。其它车道未杀。

实测：

```
08:59:51 已发车 rogue_mizuki_l5 pid=34116（文件 theme=Sami）
09:06:03 FoldartalGainNextLevel → CloseSamiIntro 点掉开局说明
09:06:22 WaitForStartButtonClicked
09:07 截图：OPERATION ISW-NO / 死刑囚の夜（萨米作战加载）
进程仍存活；命令行在 runner 重启前仍显示 rogue_mizuki_l5
```

中途坑（已处理）：

- 换主题后第一局在「密文板下一层」被萨米开局说明弹窗挡住（`閉じる`）。MAA 认不出，20 次后整链失败，留下未结算探索。
- 人工点掉结算页后回到「探索開始」。弹窗每局还会再出。
- 在**实际加载的**资源 `<项目>/data_fresh/resource/global/YoStarJP/resource/tasks/Roguelike/Sami.json` 加了 `CloseSamiIntro`（JustReturn + ClickRect `[115,96,94,28]`，挂在 `FoldartalGainNextLevel.next` 最后）。备份 `Sami.json.bak-20261005-l5intro`。
- 先写过 OCR 版，ROI 读不到「閉じる」；无 algorithm 的版本会被当成模板，报 `Templ load failed` 并让 l-5 空转。两版都已换掉。
- `data/resource/...` 那份不是运行时加载的，已还原。

回滚：还原上述三个 bak，再杀 l-5 的 maa。l-1 下次重发也会读到这个 JP 补丁；它只在密文板按钮认不出时才点一下关闭坐标。

## 主题切换：l-7 萨卡兹 → 萨米（2026-10-11 02:30，TESTED）

用户指示「l-7 的萨卡兹刷完了换萨米刷」。没重启 runner（其它车道不动）。

1. 新增 `config/tasks/rogue_sami_l7.toml`，配装同 l-1（Sami / difficulty=MAX / 维什戴尔 / 先手必胜 / 借干员+非好友 / 不投资）。
2. 机端 `runner/accounts.json`：l7 `rogueTask` → `rogue_sami_l7`，名字改为「日服·萨米」。备份 `accounts.json.bak-20261011-l7sami`。
3. 当前车道内存里任务名仍是 `rogue_mizuki_l7`，所以同内容写进该文件（备份 `rogue_mizuki_l7.toml.bak-20261011-l7sami`）。下次 runner 重启改走 `rogue_sami_l7`。
4. 换主题前萨卡兹还有一局「探索中」（卡在「いつしかの時」，放弃确认会提示进不了结算页）。点「探索を諦める」确认后主题页恢复「探検開始」，主题列表不再锁其它卡。
5. 只杀 l-7 的 maa，车道重发。期间 `MuMuManager.exe` 被改名成 `MuMuManager.exe.paused-l2-20261011`，DeviceUp 空转；已改回原名（`paused-tmp` 副本仍在）。

实测：

```
02:17:08 Roguelike theme Sami | mode 0 | difficulty 2147483647
02:30:10 Sami@Roguelike@StartExplore → SquadConfirm → RecruitOther
02:31 截图：初霜の湖沼 / 死刑囚の夜（萨米地图，啓示板）
进程仍存活；命令行在 runner 重启前仍显示 rogue_mizuki_l7
```

回滚：还原上述 bak，再杀 l-7 的 maa。`CloseSamiIntro` 补丁 10-05 已在，本轮未改资源。

## 主题切换：l-2 水月 → 傀影（2026-10-11 02:51，TESTED）

用户指示「l-2 的水月刷完了要换成傀影」。没重启 runner。

1. 新增 `config/tasks/rogue_phantom_l2.toml`：`theme=Phantom` / `difficulty=3` / 维什戴尔 / 先手必胜 / 借干员+非好友 / 不投资。
2. 机端 `runner/accounts.json`：l2 `rogueTask` → `rogue_phantom_l2`，名字改为「日服·傀影」。备份 `accounts.json.bak-20261011-l2phantom`。
3. 车道内存仍是 `rogue_mizuki_l2`，同内容写进该文件（备份 `rogue_mizuki_l2.toml.bak-20261011-l2phantom`）。只杀 l-2 的 maa。

坑：水月还有未结算探索，傀影卡显示「他のテーマ進行中につき現在進入不可」。进水月结算掉已结束的局（干潮に消える灯 / UNCOMPLETED SIMULATION）后才进得去。傀影首次进主题有开场剧情，现有 `DialogSkip` 模板认不出右上角 `SKIP`。在运行时资源 `<项目>/data_fresh/resource/global/YoStarJP/resource/tasks/Roguelike/Phantom.json` 加了 `SkipOpening`（OCR `SKIP`）和 `SkipOpeningConfirm`（点勾后回 `Begin`，不然整链会被判完成）。备份 `Phantom.json.bak-20261011-l2skip`。

实测：

```
02:48:06 Roguelike theme Phantom | mode 0 | difficulty 3
02:51:16 Phantom@StartExplore 招募，截图为傀影古城作战，场上ウィシャデル（维什戴尔）
maa pid=29644 仍在 127.0.0.1:17280；asst.log 年龄 0 秒
其余车道未杀：l1/l5/l7 的 maa 仍在
```

回滚：还原上述 bak，再杀 l-2 的 maa。命令行在 runner 重启前仍显示 `rogue_mizuki_l2`（内容已是 Phantom）。

## 主题切换：l-7 萨米 → 界园（2026-10-21 22:0x，TESTED）

用户指示「1-7 这个账号的萨米刷完了（等级已满），换成界园」。没重启 runner。

1. 新增 `config/tasks/rogue_jiegarden_l7.toml`：`theme=JieGarden` / difficulty=MAX / 维什戴尔 / 先手必胜 / 借干员+非好友 / 不投资。
2. 机端 `runner/accounts.json`：l7 `rogueTask` → `rogue_jiegarden_l7`，名字改为「l-7 肉鸽机（日服·界园）」。备份 `accounts.json.bak-20261021-l7garden`；解析核对 55 个账号只有 l7.name / l7.rogueTask 两处变化。
3. 车道内存里任务名仍是 `rogue_sami_l7`，所以同内容写进该文件（备份 `rogue_sami_l7.toml.bak-20261021-l7garden`）。下次 runner 重启走 `rogue_jiegarden_l7`。
4. **只暂停 l-7 车道**：沿用 10-12 已部署的 pause shim（`bin\maa.exe` 实为 shim，正常时透传 `maa.real.exe`）——
   写 `runner\pause_l7` 标记 → 杀 l7 的 maa → 车道重发，shim 识别 l7 进入 Hold：不碰模拟器、每 20 秒刷 asst.log，
   5 分钟 stale / 8 分钟 spin 两个看护判据都不会误杀。好处：不用 NtSuspend、也不用改名 `MuMuManager.exe`，其余 4 条车道完全不受影响。
5. 手动放弃进行中的萨米探索（adb，1280×720；暂停期间游戏被 force-stop，手动 monkey 拉起）：
   标题 TAP TO START → 大厅「端末」(994,201) → 终端「統合戦略」卡 (1080,255) → 萨米主题页「探索を諦める」(1130,340)
   → 确认弹窗红勾 (842,495) → 结算页（风雪蔽目 / LEVEL MAX）点 (640,620) 退出 → 主题页变「探索開始」，
   主题列表四张卡（萨米/萨卡兹/**界園志異 LV1 来園記念**/…）全部解锁、无「探索中 / 進入不可」。
6. 恢复：删 `runner\pause_l7` → shim 退出 → 车道重发，无标记的 shim 透传 `maa.real.exe`。只杀 l-7 的进程。

实测证据：

```
22:02:06 [rogue:l7] 已发车 rogue_sami_l7（pid=58340）        ← 暂停前
22:09:46 [rogue:l7] 肉鸽退出：ok=false completed=0 errored=0 → 立即重来
22:10:02 [rogue:l7] 已发车 rogue_sami_l7（pid=61064）        ← 恢复后（文件内容已是 JieGarden）
22:10:03 asst.log: Roguelike theme JieGarden | mode 0 | difficulty 2147483647
```

备注：命令行在 runner 重启前仍显示 `rogue_sami_l7`（内存名=兼容名）；runner 重启后按 accounts.json 走 `rogue_jiegarden_l7`。
回滚：还原 `accounts.json.bak-20261021-l7garden` + `rogue_sami_l7.toml.bak-20261021-l7garden`，删 `pause_l7` 标记，杀 l-7 的 maa 等车道重发。

## 空转重发（2026-10-05 16:06，DEPLOYED）

看护原来只认「`asst.log` 5 分钟没更新」。l-7 从 11:32 停在「作战终了」，`RoguelikeBattleExitBegin` 每秒写一条日志，看起来一直健康，直到 15:44 人工杀掉。

`internal/runner/rogue.go`：同一 `cur_task` 连续 8 分钟 → 杀 maa → 车道按原任务重发（关游戏再拉起）。阈值依据当天健康局：l-1/l-5 同一任务最长 2.5 分钟（`TESTED` `go test ./internal/runner`，`lastCurTask`）。尾巴里没有 `cur_task` 不据此杀。

机端 `fleet.exe` 已换（备份 `fleet.exe.bak-20261005-spin`），runner pid=1668 于 16:06:17 拉起。下次同类空转大约 8 分钟内会自己重来，不用等人看。

## 肉鸽任务热切换（2026-10-22，setRogue）

以前换主题要改 `runner/accounts.json` + 换任务文件 + 杀 maa/重启 runner。现在机端本地可以一条命令热切换：

```powershell
fleet.exe runner -conf runner\runner.json -set-rogue l7:rogue_jiegarden_l7
```

链路：

1. CLI 往 `runner/queue.in` 写 `{"op":"setRogue","account":"l7","rogueTask":"rogue_jiegarden_l7"}`；
2. runner 下一轮 tick（≤30 秒）校验 `config/tasks/<task>.toml` 存在；
3. 更新内存里的车道任务 + `runner/accounts.json`（先备份 `accounts.json.bak-<时间>`）；
4. 只杀掉 **l7 车道**当前的 maa；其他车道不动；
5. 车道立刻用新任务重发（跳过旧任务的 out 判定和 10 秒等待）。

边界：

- 只对**已存在的常驻肉鸽车道**生效；账号没配 `rogueTask` 会报错，不会偷偷新建车道；
- 任务名只接受简单文件名（可带 `.toml`），不允许路径；
- **换主题前仍要先结算/放弃当前主题里进行中的探索**——这是游戏限制，不是 runner 能绕过的；
- 回滚：`-set-rogue l7:<旧任务>`，或还原 `accounts.json.bak-<时间>` 后重启 runner。

## 待办


1. 退休遗留：机端 `Ensure-Workers` 代码（workers 已空，暂留无害）、`ops
ogue_cli_ops.ps1` 里的 `cycle/watch` 那套（留作手动兜底）。
2. `fleet gen` 要保留常驻账号：现在 gen 从 AUTO-MAS 重建账号表，会把手工加的 `l4` 冲掉（gen 已保留 emu 覆盖，但没保留 resident 条目）。
3. `fleet runner -status` 的"在跑 N"永远显示 0（进程启动时把读到的 running 当"重启回收"，只影响显示，不影响调度）——顺手修一下更好。
4. 队列排序：91 个待跑任务里，同优先级按 map 随机序领（BuildPlan 会把热改以外的优先级统一降到 10）——"越早 enqueue 越先跑"不明显，不值得急，但可整理。
5. 停机 drain 超时会留孤儿 maa（见「主题切换」小节）：drain 超时后应杀掉在场 maa（否则与新进程重排的同名任务抢同一台模拟器）。
6. 肉鸽换主题缺前置：切主题前得先确认「当前主题没有进行中的探索」（日服会直接锁住其他主题卡，MAA 自己不会结算/放弃，见「主题切换」小节）——建议做成任务制作器的前置检查 / 一个「结算/放弃」任务模板。
7. emu 5/13/14/30 起不来（10-03 实测：`MuMuManager control --vmindex 5 launch` 返 errcode=0，75s 后仍 `is_process_started=false`；其余 51 台正常，内存余 31GB）→ 这 4 号今天日常全灭（a05/a13/a14/a30，每号已满 3 次重试）。待查：这 4 个实例是不是坏 VM / 要不要重装。
