# Modbus AI Studio

面向工业现场的 Modbus 调试与诊断工具，Windows、macOS 可用。界面参考 Modbus Poll；读不到数据时自动分析原因并给出一键处理，写入后回读确认设备真的执行了。

![主界面](assets/screenshots/main.png)

## 能做什么

- **连接**：Modbus TCP、RTU over TCP、ASCII over TCP、RTU 串口、ASCII 串口。不知道协议、站号、波特率时可以自动识别和扫描；断线自动重连。
- **读取**：读取窗口可开多个，各自设置站号、地址和周期。显示格式有 16 / 32 / 64 位整型、FLOAT32 / FLOAT64、Hex、Binary、ASCII，四种字节序。导入点表（xlsx、CSV，物联网平台导出的设备属性表也能直接用）后按名称、单位和工程值显示。
- **出错自动分析**：超时与晚到响应、异常码、校验错误、字节序不对、连接断开，都会说明原因，大多能一键处理（调超时、识别协议、探测可读地址、改字节序……）。
- **写入验证**：写入后多次回读，判断是生效、未生效、被 PLC 改回，还是值不符；可以一键恢复原值。只读模式防止在生产设备上误写。
- **报文与日志**：逐字段解析每条报文，自定义请求可以发任意功能码，FC08 诊断计数器。日志页记录连接失败、读取失败与恢复、断开和重连，附原因分析及出错时的原始报文；收发和日志存进本机 SQLite，可查看历史。
- **多开**：多个主窗口各自连接不同设备；连接、读取窗口和点表可以存成工作区文件。
- 内置换热站模拟器，没有设备也能上手。
- **软件更新**：“帮助 → 检查更新”下载新版本，校验 SHA-256 后替换程序并重启。

## 下载

[GitHub Releases](https://github.com/xiaolengWangWang/modbus-ai-studio/releases) 提供：

| 文件 | 适用 |
| --- | --- |
| `ModbusAIStudio-x.y.z-Windows-x64.zip` | Windows 10 / 11 64 位。绿色版，解压后双击 `ModbusAIStudio.exe`；附带命令行工具 `modbus-cli.exe`、`modbus-sim.exe` |
| `ModbusAIStudio-x.y.z-macOS-AppleSilicon.dmg` | M 系列芯片的 Mac，macOS 12 及以上 |
| `ModbusAIStudio-x.y.z-macOS-Intel.dmg` | Intel 芯片的 Mac，macOS 12 及以上 |

Linux 暂时没有安装包，可以 [从源码编译](docs/development.md#从源码编译)。Windows 远程桌面、虚拟机里打不开，或 macOS 提示无法打开，见 [常见问题](docs/faq.md#安装与运行)。

## 快速上手

1. **先看示例**：打开程序，选菜单“读取 → 打开换热站示例”。会打开三个读取窗口并连接内置模拟器；窗口 3 故意应答慢，演示自动分析和一键处理。
2. **连接你的设备**：在顶部选协议，填 `IP:端口` 或选串口参数，点“连接”。不确定协议就点“识别”。
3. **读数据**：点右上角“读取窗口”，在“定义”里填 Slave ID、功能码、起始地址（手册上的 40001 写法即可）和数量。
4. **按点表显示**：有设备地址表时，用“读取 → 导入点表”导入 xlsx 或 CSV，读取窗口自动按点表显示名称、单位和工程值。

## 文档

- [使用说明](docs/guide.md)：连接、读取、点表、写入验证、自动分析、报文与历史、工作区、只读模式、命令行工具、快捷键
- [点表格式](docs/points.md)：点表的列、数据类型、字节序、平台导出的设备属性表
- [常见问题](docs/faq.md)：连不上、超时、断开、异常码、值不对、写入失败、报文记录在哪里、安装问题
- [开发与发布](docs/development.md)：从源码编译、目录结构、测试、打包发版

## 目录

| 类别 | 目录 |
| --- | --- |
| 资源 | `assets/`：图标、点表模板、文档截图 |
| 核心代码 | `cmd/`、`internal/`、`tests/` |
| 平台代码 | `platform/`：Windows 专用代码，macOS / Windows 打包配置 |
| 文档 | `README.md`、`docs/` |
| 打包编译 | `build/`：macOS、Windows 打包脚本和图标生成器；`.github/workflows/`：持续集成 |

## 许可证

[MIT](LICENSE)
