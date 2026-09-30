# tools/ — 装配与同步脚本（预留）

计划：
- `bootstrap.ps1`：检查 bin/core 是否就位 → 建 data\lib 与 data\resource 联接 → 自检
- `sync_tasks.ps1`：把 `config\tasks\*.toml` 同步到 maa-cli 任务目录（本项目已用 MAA_CONFIG_DIR 直指 `config\`，一般无需同步）
