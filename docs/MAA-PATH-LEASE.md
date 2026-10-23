# MAA 路径租约与冷却

> 2026-10-23。解决同一 MAA 目录快速重启导致的 `InitializeLogger → MoveFile → IOException`。

## 原理

- 一个 MAA 目录同一时间只允许一个 owner；
- 启动前确认该路径没有运行中的 `MAA.exe`；
- 上一个任务释放后至少冷却 N 秒；
- 租约状态落在 `dist/maa_path_leases.json`，不写 MAS。

## 用法

```powershell
# 当前路径/进程
python tools\maa_path_lease.py status

# 调度器启动前等待
python tools\maa_path_lease.py wait `
  --path "E:\MAA-Multi\MAA_3" --cooldown 10 --timeout 300

# 或显式租约
python tools\maa_path_lease.py acquire `
  --path "E:\MAA-Multi\MAA_3" --owner a19 --cooldown 10
python tools\maa_path_lease.py release `
  --path "E:\MAA-Multi\MAA_3" --owner a19
```

## 边界

这个工具只能约束**我们自己的调度器/canary**。MAS 自动队列由 MAS 内部直接启动 MAA，
不会先调用本工具。

要彻底消除 MAS 队列的启动竞态，需要二选一：

1. 我们的 dispatcher 接管 MAA 启动，先 acquire 再发 AutoProxy；
2. 保留 MAS 自动队列，但接受偶发日志锁，靠重试恢复。
