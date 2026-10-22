# legacy —— 旧任务模板别名（归档）

这里的文件是 2026-10-22 canonical 化时从 `config/tasks/` 根目录移出来的，
共同点是**文件名和文件里的 `theme` 不一致**：

| 文件名 | 实际 theme |
|---|---|
| `rogue_mizuki_l2.toml` | Phantom |
| `rogue_mizuki_l5.toml` | Sami |
| `rogue_mizuki_l7.toml` | Sami |
| `rogue_sami_l5.toml` | Sarkaz |
| `rogue_sami_l7.toml` | JieGarden |
| `rogue_sarkaz_l2.toml` | Mizuki |

这些内容已经以正确的 canonical 文件名补回根目录（例如 `rogue_phantom_l2.toml`、
`rogue_sami_l7.toml`），因此这里仅作历史/回滚参考：

- **不要**把这些文件名写进 `runner/accounts.json` 的 `rogueTask`；
- **不要**用 `-set-rogue` 指向这里；
- 需要回滚某个主题时，用根目录对应的 canonical 文件。
