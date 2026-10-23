# 模拟器 Session 画像采集

> 2026-10-23。只读采集，不写 MAS、不改队列、不杀进程。

## 用途

采集模拟器/MAA 的 CPU 曲线，用来回答：

- 一个模拟器开着游戏、空闲时 CPU 基线是多少；
- N 个模拟器同时在线时 CPU 曲线；
- MAA 任务阶段的增量峰值；
- 关闭模拟器后 CPU 回落到多少。

## 运行

```powershell
E:\AUTO-MAS\environment\python\python.exe tools\session_profiler.py `
  --minutes 10 --interval 5 `
  --label mixed_after_pause `
  --mumu "E:\MuMu Player 12\nx_main\MuMuManager.exe" `
  --out dist\session_profile_current.jsonl `
  --summary dist\session_profile_current_summary.json
```

输出：

- JSONL：每 5 秒一条，含总 CPU、内存、每个运行中 MuMu 实例的 pid/headless pid/CPU、MAA 进程列表；
- summary：按 `emu_count` 和 `maa_count` 分桶的 CPU mean/p95/max。

## 解读

先看 `by_emu_count`：

- 模拟器数量增加时，CPU 是否线性上升；
- 同样模拟器数量下，MAA 数量增加多少；
- 空闲/等待时 CPU 是否仍有高基线。

模拟器/游戏是常驻成本；调度单位应该是 **emulator session**，不是单个 MAA 任务。
