# config/tasks —— maa-cli 任务模板

## 命名约定

- **肉鸽**：`rogue_<theme>_<account>.toml`，例如 `rogue_jiegarden_l2.toml`。
- **日常**：`daily_<account>.toml`。
- **剿灭**：`ann_<account>.toml`。
- `daily_a*.toml` / `ann_a*.toml` 由生成器产生，属于本机运行态，默认不入库。

> ⚠️ **以文件里的 `theme` 字段为准，不要按文件名猜主题。**
> 历史上为了“立刻生效”，曾把新主题内容写进旧文件（例如 `rogue_mizuki_l7.toml`
> 里实际是 Sami），这些旧文件已经归档到 `legacy/`，不要再引用。

## 当前启用映射

| 账号 | 启用 | 日常 | 肉鸽 |
|---|---|---|---|
| `l2` | ✅ | `daily_l2` | `rogue_jiegarden_l2`（界园） |
| `l7` | ✅ | `daily_l7` | `rogue_jiegarden_l7`（界园） |
| `l1` | ⏸ | `daily_l1` | `rogue_sami_l1`（萨米） |
| `l4` | ⏸ | `daily_l4` | `rogue_sarkaz_l4`（萨卡兹） |
| `l5` | ⏸ | — | `rogue_sarkaz_l5`（萨卡兹） |

停用账号没有常驻车道；`fleet runner -set-rogue` 会拒绝停用账号。

## 热切换

```powershell
fleet.exe runner -conf runner\runner.json -set-rogue l2:rogue_jiegarden_l2
```

切换前确认：

1. 目标文件存在且非空；
2. 文件里的 `theme` 是你要的主题；
3. 当前主题没有进行中的探索（日服会锁其他主题卡，MAA 不会自动结算/放弃）。

## legacy/

`legacy/` 是旧别名归档：文件名和内容可能不一致，仅用于查历史/回滚，**不要**再写进
`accounts.json` 的 `rogueTask`，也不要直接拿来做热切换。
