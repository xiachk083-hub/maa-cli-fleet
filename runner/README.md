# runner/ —— 机端运行时目录

这个目录放 **本机运行态**，默认整个目录不入库（见根目录 `.gitignore`）：

| 文件 | 作用 |
|---|---|
| `runner.json` | 机端配置（路径 / center 地址 / nodeId / 并发位） |
| `accounts.json` | 账号表（账号 → 模拟器实例 / 日常 / 剿灭 / 肉鸽任务） |
| `state.json` | 任务台账（runner 自动维护） |
| `roster.json` | 班表（可选；不存在 = 理智自循环） |
| `center.token` | 中心共享 token（接 center 时用） |
| `queue.in` | CLI 热改队列（runner 每 30 秒合并后删除）；`-set-rogue` 也走这里 |
| `heartbeat.txt` | 主循环心跳（看门狗用） |
| `hb_rogue_*.txt` | 各肉鸽车道心跳 |
| `runner.log` / `*.out` | 运行日志与 maa 输出 |

## 首次配置

```powershell
copy runner.example.json   runner.json
copy accounts.example.json accounts.json
copy roster.example.json   roster.json   # 不用班表可删
```

然后按本机情况改 `runner.json` 与 `accounts.json`。真实账号表包含手机号/昵称，
**不要提交**；需要分享时请先脱敏。

> 已经有 AUTO-MAS 配置的机器可以用 `fleet gen -automas <AUTO-MAS 目录>` 一次性导入。
> 这是临时 ETL，导入后仍建议手工核对账号表。
