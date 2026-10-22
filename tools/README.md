# tools/ —— 辅助脚本

| 文件 | 作用 | 备注 |
|---|---|---|
| `gen_roster.py` | 从 `runner/state.json` + `accounts.json` 生成 `runner/roster.json` | Go 班表的配套工具 |
| `mas_inventory.py` | MAS / fleet 只读盘点 + 接管 Plan（不写 MAS、不 dispatch） | 见 `docs/MAS-INVENTORY.md` |
| `mas_daily.py` | MAS 单账号 Plan/诊断（direct dispatch 已弃用；执行应走队列 AutoProxy） | 见 `docs/MAS-DAILY-CANARY.md` |
| `mas_split.py` | 多用户脚本拆分 Plan（只读；列出保留/克隆/队列/风险） | 不写 MAS；Apply 需另做 |
| `fleet_mcp.py` | MCP 网关（stdio），把旧 PS 栈的 11 个操作暴露给 Agent | **Legacy**：驱动 `ops/rogue_cli_ops.ps1`，不是 Go runner |
| `gen_accounts.ps1` | 从 AUTO-MAS 配置生成账号表 / 任务 TOML | **Legacy**：已被 `fleet gen` 取代 |
| `shot_annotate.py` | 给大厅截图叠信息面板（Pillow） | 对外分享请加 `--mask-phone` |
| `shots_pipeline.py` | 大厅截图归档 / 看门 | 需要 AUTO-MAS 在线接口；路径可用环境变量覆盖 |
| `shots_contact_sheet.py` | 把成品截图拼成一张大图 | 同上 |
| `switch_audit.py` | 切号日志 vs 配置 vs 大厅图对账 | 需要 `MAS_API` / `MAS_BASE` |
| `patch_screenshot_plugin.py` | 给 MAA `tasks.json` 打 ScreenshotTaskPlugin 补丁 | 会留 `.bak-pi-*` 备份 |

## 环境变量（截图工具）

| 变量 | 默认 | 说明 |
|---|---|---|
| `MAS_API` | `http://127.0.0.1:36163` | AUTO-MAS / MAS 接口地址 |
| `MAS_BASE` | 项目上级目录下的 `MAA-Multi` | MAA 槽位根目录（如 `E:\MAA-Multi`） |
| `MAS_HISTORY` | 项目上级目录下的 `AUTO-MAS\history` | AUTO-MAS 历史日志根目录 |
| `SHOTS_OUT` | `<项目>\shots` | 成品归档目录 |
| `SHOTS_DESK` | `%USERPROFILE%\Desktop\maa-shots` | 桌面副本目录 |
| `PYTHON` | 当前解释器 | 调 `shot_annotate.py` 用的 Python |

这些脚本是可选工作流，不属于 `fleet.exe runner` 的核心；路径按自己的机器调整。
