# MAS 多用户脚本拆分 Plan

> 2026-10-23。目标：一个账号一个脚本、一个用户；脚本名稳定，`scriptId + userId` 做身份。

## 只读 Plan

```powershell
E:\AUTO-MAS\environment\python\python.exe tools\mas_split.py `
  --mode full `
  --mas-api http://127.0.0.1:36163 `
  --out dist\mas_split_plan.json `
  --keep-out dist\mas_split_mapping.template.json
```

`--mode full`（默认）：**所有启用用户全部拆出**，旧脚本清空并归档；
`--mode keep`：保留一个用户在原脚本，其余拆出（可选 `--keep` 指定主号）。

输出：

- 每个多用户脚本的 `clones`（计划拆出的单用户脚本）；
- 新脚本名（默认 = `用户名-服务器`，如 `v-官服`；重名才加短 userId）、目标队列、模拟器/MAA；
- 脚本名只是容器标签，身份始终用 `scriptId + userId`；创建后不要改名；
- 禁用用户记录（留在旧脚本归档，不自动删除）；
- 风险提示（旧 history 不迁移、禁用记录、队列名含“并行”等）。
- full 模式额外输出 old → new 映射模板，Apply 时补 scriptId/userId。

## Apply（分阶段）

```powershell
# 1) clone：备份、暂停队列、创建单用户脚本；旧脚本/用户/队列项不动
python tools\mas_split_apply.py --stage clone --script "官-3" `
  --plan dist\mas_split_plan.json --apply

# 2) 单账号 canary：用 clone 输出的 mapping 里的新 scriptId
python tools\mas_daily.py --script-id <newScriptId> --apply --wait --timeout 1800

# 3) finalize：验证新脚本、删旧队列项、加新队列项、清旧启用用户
python tools\mas_split_apply.py --stage finalize `
  --mapping dist\mas_split_apply_<scriptId8>.json --apply

# 失败回滚：删除新脚本、恢复队列；旧脚本不动
python tools\mas_split_apply.py --stage rollback `
  --mapping dist\mas_split_apply_<scriptId8>.json --apply
```

finalize 默认仍暂停队列；确认并发/顺序后再加 `--enable-queue`。

建议的落地顺序：

1. 备份 MAS `ScriptConfig` / `QueueConfig` 和 `data/<scriptId>`；
2. 暂停相关队列，确认脚本未锁、未在跑；
3. 用 `POST /api/scripts/add {"type":"MAA","scriptId":"<原脚本>"}` 克隆；
   MAS 会重新生成 scriptId/userId，并复制 `data/<scriptId>`；
4. 在克隆里按 `Info.Id`/`Info.Name` 找到目标用户，删除其余用户；
5. 改脚本 `Info.Name` 为计划里的稳定名字；
6. 加入原 slot 队列（先串行、暂不并行）；
7. Plan/NOOP + 单账号 canary；
8. full 模式：全部新脚本验证后，清空旧脚本用户、移除旧队列项，旧脚本归档；
   keep 模式：只删除已迁走的用户，原脚本留主号；
9. 记录 old script/user → new script/user 映射；旧 history 不迁移。

## 红线

- 不手工改 JSON；
- 不在脚本锁定时拆；
- 不删除脚本/用户前不备份；
- 不把多个单用户脚本同时塞进同一个 MAA/模拟器的并行队列。
