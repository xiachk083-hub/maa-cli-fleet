# 全量接管 AUTO-MAS 账号 —— 迁移方案（PROPOSED）

> 2026-10-01 · 状态：**待拍板**（本文件只写方案与规模，不含任何账号凭据）
> 背景：AUTO-MAS 已退役，但它在管的不止我们 5 台——共有 **54 个账号**需要迁到我们的栈。

---

## 1. 规模（主机 `E:\AUTO-MAS\config\ScriptConfig.json` 实测）

| 项 | 实测值 |
|---|---|
| 账号总数 | **54**（含我们已成接管的 5 台）→ 待迁 **49** |
| 服务器 | **4 种**：Official（官服，多数）、Bilibili（B服）、YoStarJP（日服，我们的 5 台）、txwy（台服） |
| 关卡 | 多数 `SR-7`（当期活动关）、其余 `SR-5`、`SR-8`、`1-7`、`CE-6`；模式全为 Fixed |
| 任务链 | 与我们一致：StartUp / Fight / Recruit / Infrast / Mall / Award |
| 额外任务 | **剿灭作战（Annihilation）多数账号开启**（我们 5 台是关的）；森空岛签到全关；保全/仓库/基建自定义全关；肉鸽**全关** |
| 模拟器实例 | MuMu 共 **55 个**（54 方舟 + 1 碧蓝航线）；索引可能与 ScriptConfig 记录漂移（例：l-4 记 31、实际 9） |
| 账号凭据 | 藏在 ScriptConfig 的 Notes（手机号/邮箱）→ **一律不进 git/文档**，只留主机本地 |

## 2. 承载瓶颈（实测 2026-10-01 07:5x）

| 项 | 实测 |
|---|---|
| 主机 | 36 逻辑核 / 63.8GB RAM，空闲 38.3GB |
| 当前负载 | 6 个模拟器 + 5 个 maa → CPU 66%、RAM 用掉 ~25GB |
| 结论 | **同时只能跑 8-12 个实例** → 54 台**不可能常驻**，必须"槽位轮转" |

> 这正是 AUTO-MAS 当年的做法：12 个 MAA GUI 槽位 + 早班/晚班分批（`RunTimesLimit=3`、`RoutineTimeLimit=10`、`TaskTransitionMethod=ExitEmulator`）。

## 3. 要加的三件东西

### ① 多服务器支持（现在是写死 YoStarJP）
每台机新增：`Client`（Official/Bilibili/YoStarJP/txwy）、游戏包名（force-stop 用）、MAA resource（`global_resource`）、任务文件里的 `client_type`。
- 现有硬编码点：`ops/rogue_cli_ops.ps1` 的 `com.YoStarJP.Arknights`、`config/profiles/default.toml` 的 `global_resource`、各 `daily_*.toml` 的 `client_type`。

### ② 槽位轮转器（新组件，跑在机端）
```
队列（按"理智将满时间"排序）
  → 取一台 → 起模拟器 → 等 boot + adb connect
  → 跑日常（含到期的剿灭）→ 记状态 → 停模拟器 → 释放槽位 → 下一台
```
- 槽位数可配（建议先 **8**，稳了再提到 12）；
- 每台每天至少完成 1 次日常；完成时间按理智时钟自然错峰；
- 我们的 5 台**不受影响**（继续常驻 cycle/watch worker，含肉鸽）。

### ③ 剿灭作战任务
MaaCore 原生类型（`Annihilation`）✓；按 AUTO-MAS 的周跟踪语义：每周做满（`AnnihilationCompletedWeek`），做完当周不再跑。

## 4. 阶段

| 阶段 | 内容 | 验收 |
|---|---|---|
| **B** | 多服务器支持 + 轮转器 + 剿灭任务（代码） | 本地单测：能生成任一账号的任务文件并按槽位起停 |
| **C** | 试点 2-3 台（官服/B服/日服各一） | 真结算：日常跑完 + 状态回中心 + 模拟器释放 |
| **D** | 分批全量（49 台，8 槽轮转） | 一天内 49 台日常全绿；中心可查每台状态 |

## 5. 待拍板

1. **槽位数**：先 8（稳）还是直接 12（快，负载高）？
2. **剿灭作战**：一并做进日常流程？（AUTO-MAS 里多数账号是开的）
3. **账号凭据**（手机号/邮箱）：只留主机本地、不进库 ✓（默认）；如需归档到别处请指定位置。
4. 轮转器实现语言：**PowerShell**（主机无 Python 依赖，推荐）/ Python（需在主机装环境）。
