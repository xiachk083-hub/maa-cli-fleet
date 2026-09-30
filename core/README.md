# core/ — MaaCore 运行库 + 资源（不入库，自行获取）

本目录需包含 MaaCore 的 DLL 与 `resource/`（肉鸽/日常识别资源、JP 全局资源）。

获取方式（任选）：
1. 从既有 MAA 安装拷贝（本项目当前做法）：把 MAA 安装目录内容（含 `MaaCore.dll`、`resource/`）复制到本目录；
2. 由 maa-cli 安装：`bin\maa.exe install`（会写入 `data/lib` 与 `data/resource`，然后可改为指向本目录）。

完成后确保：
- `data\lib`     → 指向本目录（目录联接）
- `data\resource`→ 指向本目录下的 `resource`（目录联接）
