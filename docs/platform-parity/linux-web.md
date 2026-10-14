# 与 Windows 桌面版的功能差异

目标版本：{{VERSION}}
Windows 基准版本：{{VERSION}}
对应安装包：{{PACKAGE}}
检查日期：2026-10-15

Windows 基准包括标准版和兼容版。Linux Web 是在网关或服务器运行、由浏览器访问的产品形态。

| 功能 | Windows 标准版 | Windows 兼容版 | 本版状态 | 原因 | 处理决定 | 验证依据 |
| --- | --- | --- | --- | --- | --- | --- |
| Modbus 连接、读取、写入回读、协议识别与寄存器检测 | 支持 | 支持 | 支持 | Web 接口调用共用协议与检测逻辑 | 不适用 | `internal/web/server.go`、`internal/web/probe.go` |
| DeepSeek AI 桌面诊断助手 | 支持 | 支持 | 未提供 | Web 服务没有桌面 AI 助手页面或对应接口 | 暂缓 | `internal/ui/ai.go`、`internal/web/server.go` |
| 多个桌面主窗口、系统托盘与工作区文件 | 支持 | 支持 | 未提供 | 浏览器服务没有桌面窗口、托盘和工作区文件 | 不适用 | `internal/ui/app.go`、`internal/web/server.go` |
| 应用内自动更新 | 支持 | 支持 | 未提供；更换服务程序后重启 | Web 包不参加桌面安装包的更新选择 | 暂缓 | `internal/update/update.go`、`build/linux-web.sh` |
| Windows API Key 的 DPAPI 持久加密 | 支持 | 支持 | 未提供 | 当前 Web 版没有 AI 凭据功能 | 暂缓 | `internal/ai/credentials_windows.go`、`internal/web/server.go` |
| Windows 7/8/8.1 兼容和 Mesa 软件渲染 | 不提供 | 支持 | 不适用 | Linux Web 不运行 Windows 桌面图形界面 | 不适用 | `build/windows.sh`、`build/linux-web.sh` |

## 本版独有或平台特有

- 多个浏览器可查看同一个服务状态，变化通过 Server-Sent Events 推送。依据：`internal/web/server.go`。
- 首次启动生成访问口令，可配置 HTTPS；收发报文按天保存并可从页面下载。依据：`internal/web/server.go`、`build/linux-web.sh`。
- 提供 Linux x64 和 ARM64 的单程序包，方便网关与服务器部署。依据：`build/linux-web.sh`。

## 后续优化依据

- 评估在 Web 界面补齐 AI 诊断的需求、凭据存储与权限边界。
- 评估安全的服务更新和回滚流程；目前由管理员更换程序并重启。
