# bin/ — 可执行文件（不入库，自行获取）

- `maa.exe`：从 [maa-cli Releases](https://github.com/MaaAssistantArknights/maa-cli/releases) 下载
  `maa_cli-v<version>-x86_64-pc-windows-msvc.zip` 解压得到（当前实测 v0.7.5）。
- `adb/adb.exe`（+ `AdbWinApi.dll` / `AdbWinUsbApi.dll`）：任一模拟器自带的 adb，或
  [platform-tools](https://developer.android.com/tools/releases/platform-tools)。

放置完成后运行 `..\ops\rogue_cli_ops.ps1 status` 自检。
