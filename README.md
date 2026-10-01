# Modbus AI Studio

面向工业现场的跨平台 Modbus 调试与诊断工具。界面参考 Modbus Poll，出错时自动分析原因并给出一键处理。

- 协议：Modbus TCP、RTU over TCP、ASCII over TCP、RTU 串口、ASCII 串口；协议识别、从站地址扫描、串口参数扫描
- 读取窗口可多开，多种显示格式，CSV 点表，工作区保存
- 错误自动分析：超时与晚到响应、异常码、校验错误、字节序、连接断开（自动重连并分析原因）
- 报文逐字段解析、自定义请求、FC08 诊断计数器；写入后回读验证，只读模式防止误写
- 报文记录存进本机 SQLite，可查看历史；内置换热站模拟器

详细说明见 [功能说明](docs/features.md)，开发、打包和发版见 [开发与发布](docs/development.md)。

## 下载

[GitHub Releases](https://github.com/xiaolengWangWang/modbus-ai-studio/releases) 提供：

- Windows x64 绿色版 zip：解压后双击 `ModbusAIStudio.exe`，不用安装。需要 Windows 10 / 11 64 位（不支持 Windows 7 / 8）和支持 OpenGL 2.1 的显卡；显卡不支持时程序会弹窗说明（远程桌面、虚拟机、老工控机常见），把 [Mesa3D](https://github.com/pal1000/mesa-dist-win) 软件渲染版的 `opengl32.dll` 放到 exe 同一目录即可；日志在 `%AppData%\ModbusAIStudio\app.log`。附带命令行工具 `modbus-sim.exe`、`modbus-cli.exe`。
- macOS DMG：Intel 和 Apple Silicon 各一个，最低 macOS 12。ad-hoc 签名，第一次打开要在“系统设置 → 隐私与安全性”里允许。

## 快速开始

```sh
go run ./cmd/modbus-ai   # 启动后菜单“读取 → 打开换热站示例”，自动连接内置模拟器
go test ./...            # 全部测试
```

## 目录

| 类别 | 目录 |
| --- | --- |
| 资源 | `assets/`：图标、点表模板 |
| 核心代码 | `cmd/`、`internal/`、`tests/` |
| 平台代码 | `platform/`：Windows 专用代码，macOS / Windows 打包配置 |
| 文档 | `README.md`、`docs/` |
| 打包编译 | `build/`：macOS、Windows 打包脚本和图标生成器；`.github/workflows/`：持续集成 |

各目录的详细说明见 [开发与发布](docs/development.md#目录结构)。

## 许可证

[MIT](LICENSE)
