# MAS 队列清理

> 2026-10-23。目标：正式队列一个 MAA 路径一个；临时补跑/验收队列归档，不删除配置。

## 只读 Plan

```powershell
E:\AUTO-MAS\environment\python\python.exe tools\mas_queue_audit.py `
  --mas-api http://127.0.0.1:36163 `
  --out dist\mas_queue_plan.json `
  --md dist\mas_queue_plan.md
```

检查：

- `并行*` 正式队列；
- `临时*` 临时队列；
- 同一 MAA 路径被哪些队列占用；
- 同一脚本是否出现在多个队列；
- 队列内是否多脚本共用同一 MAA 路径。

## 归档 Apply

```powershell
python tools\mas_queue_apply.py --stage archive
python tools\mas_queue_apply.py --stage archive --apply
```

只做：

- 名字含“临时”的队列改名为 `归档-*`；
- 强制 `TimeEnabled=false`、`StartUpEnabled=false`；
- 备份 `QueueConfig.json` 和 API dump；
- **不删除队列、不改队列项、不动 `并行*`**。

恢复：

```powershell
python tools\mas_queue_apply.py --stage restore `
  --mapping dist\mas_queue_cleanup.json --apply
```

## 清理后仍要注意

- `并行3/4` 等正式队列内仍可能有多个脚本共用同一个 MAA 路径；
- 同一队列必须串行，MAA 退出后留 5-10 秒冷却；
- 需要 MAA 路径锁，避免两个队列同时占同一个目录；
- 归档队列不要再启用。
