# 与 Windows 桌面版的功能差异

目标版本：{{VERSION}}
Windows 基准版本：{{VERSION}}
对应安装包：{{PACKAGE}}
检查日期：2026-10-15

Windows 基准同时包括标准版和兼容版；两者使用同一套核心功能。兼容版另外面向旧版 Windows 和无法使用显卡 OpenGL 的设备。

| 功能 | Windows 标准版 | Windows 兼容版 | 本版状态 | 原因 | 处理决定 | 验证依据 |
| --- | --- | --- | --- | --- | --- | --- |
| Modbus 连接、读取、写入回读、协议识别与寄存器检测 | 支持 | 支持 | 支持 | 桌面界面与协议代码共用 | 不适用 | `internal/ui/app.go`、`internal/ui/registerprobe.go` |
| DeepSeek AI 诊断、证据预览与报告导出 | 支持 | 支持 | 支持 | 桌面界面与 AI 逻辑共用 | 不适用 | `internal/ui/ai.go`、`internal/ui/ai_report.go` |
| API Key 使用系统加密后持久保存 | 支持，使用 Windows DPAPI | 支持，使用 Windows DPAPI | 未提供；可用会话密钥或环境变量 `DEEPSEEK_API_KEY` | 当前非 Windows 凭据实现没有持久加密存储 | 暂缓 | `internal/ai/credentials_windows.go`、`internal/ai/credentials_other.go` |
| Windows 7/8/8.1 兼容运行 | 不适用 | 支持 | 不适用 | 操作系统不同；macOS 包要求 macOS 12 及以上 | 不适用 | `build/windows.sh`、`build/macos.sh` |
| 自带 Mesa 软件 OpenGL 渲染 | 不提供 | 支持 | 未提供 | Mesa DLL 为 Windows 兼容版专用 | 不适用 | `build/windows.sh`、`platform/windows.go` |
| 程序目录 `data` 便携数据模式 | 不提供 | 支持 | 未提供 | 由 Windows 兼容版启动逻辑设置 | 暂缓 | `platform/windows.go`、`platform/other.go` |

## 本版独有或平台特有

- 提供 Intel 与 Apple Silicon 原生 DMG 和 `.app`，使用 macOS ad-hoc 签名；Windows 两版是 x64 绿色版 zip。依据：`build/macos.sh`、`build/windows.sh`。

## 后续优化依据

- 优先评估 macOS API Key 的安全持久存储方式，再决定是否补齐。
- 如现场确需便携数据目录，再设计 macOS 平台行为；目前保持“暂缓”。
