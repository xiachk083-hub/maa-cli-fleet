# MAS 多用户脚本拆分 Plan

> 2026-10-23。目标：一个账号一个脚本、一个用户；脚本名稳定，`scriptId + userId` 做身份。

## 只读 Plan

```powershell
E:\AUTO-MAS\environment\python\python.exe tools\mas_split.py `
  --mas-api http://127.0.0.1:36163 `
  --out dist\mas_split_plan.json `
  --keep-out dist\mas_split_keep.template.json
```

输出：

- 每个多用户脚本的 `keep`（留在原脚本的用户）和 `clones`（计划拆出的单用户脚本）；
- 新脚本名、目标队列、模拟器/MAA；
- 禁用用户记录（只列出，不自动删除）；
- 风险提示（默认保留、禁用记录、队列名含“并行”等）。

默认保留每个脚本的**第一个启用用户**；确认主号后用 `--keep` 指定：

```powershell
python tools\mas_split.py --keep "官-3:汐影月k" --keep "官-1:hbznn119-官"
```

也可以编辑 `dist\mas_split_keep.template.json` 后传 `--keep-file`。

## Apply（尚未实现）

建议的落地顺序：

1. 备份 MAS `ScriptConfig` / `QueueConfig` 和 `data/<scriptId>`；
2. 暂停相关队列，确认脚本未锁、未在跑；
3. 用 `POST /api/scripts/add {"type":"MAA","scriptId":"<原脚本>"}` 克隆；
   MAS 会重新生成 scriptId/userId，并复制 `data/<scriptId>`；
4. 在克隆里按 `Info.Id`/`Info.Name` 找到目标用户，删除其余用户；
5. 改脚本 `Info.Name` 为计划里的稳定名字；
6. 加入原 slot 队列（先串行、暂不并行）；
7. Plan/NOOP + 单账号 canary；
8. 再回原脚本删除/禁用已迁走的用户；
9. 原脚本只留主号，历史锚点不变。

## 红线

- 不手工改 JSON；
- 不在脚本锁定时拆；
- 不删除脚本/用户前不备份；
- 不把多个单用户脚本同时塞进同一个 MAA/模拟器的并行队列。
