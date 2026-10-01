# 开发与发布

## 从源码编译

需要 Go 1.26 以上和 C 编译器（界面库 Fyne 和 SQLite 驱动都要 cgo）：

| 系统 | 准备 |
| --- | --- |
| macOS | `xcode-select --install` |
| Windows | 装 [MSYS2](https://www.msys2.org/) 的 mingw-w64 gcc，并加到 PATH |
| Linux（Debian / Ubuntu） | `sudo apt install gcc libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev` |

```sh
git clone https://github.com/xiaolengWangWang/modbus-ai-studio.git
cd modbus-ai-studio
go build -o bin/ ./cmd/...    # 得到 modbus-ai（桌面应用）、modbus-cli、modbus-sim
```

国内下载依赖慢时先设 `go env -w GOPROXY=https://goproxy.cn,direct`。

## 目录结构

按文件用途分五类：

| 类别 | 目录 | 内容 |
| --- | --- | --- |
| 资源 | `assets/icon/` | 应用图标 `AppIcon.png`（1024×1024，由 `go run ./build/icon` 生成，打包时转成 macOS icns 和 Windows ico） |
|  | `assets/examples/` | CSV 点表模板（换热站示例，测试保证与内置点表一致） |
|  | `assets/screenshots/` | 文档里的截图，由界面测试生成（见 [测试](#测试)） |
| 核心代码 | `cmd/modbus-ai/` | 桌面应用入口 |
|  | `cmd/modbus-cli/`、`cmd/modbus-sim/` | 命令行主站、命令行模拟从站 |
|  | `internal/modbus/` | 协议核心：PDU、MBAP / RTU / ASCII 分帧、客户端、数据类型与字节序、地址解析 |
|  | `internal/transport/` | TCP 与串口 |
|  | `internal/detect/` | 协议自动识别 |
|  | `internal/control/` | 写入与控制验证：多次回读，判定 PASS / 未生效 / 被覆盖 / 值不符 |
|  | `internal/simulator/` | 模拟从站与故障注入 |
|  | `internal/recorder/` | 报文记录：会话、收发、断开重连事件存进本机 SQLite |
|  | `internal/ui/` | 桌面界面（各文件分工见 `internal/ui/app.go` 开头的说明） |
|  | `tests/` | 基于模拟器的集成测试 |
| 平台代码 | `platform/` | 各操作系统专用的 Go 代码：`windows.go`（启动日志、显卡不支持 OpenGL 时弹窗），`other.go`（macOS、Linux，目前没有专用代码） |
|  | `platform/macos/` | `Info.plist` 模板 |
|  | `platform/windows/` | Windows 绿色版里的 `README.txt` 模板 |
| 文档 | `README.md`、`docs/` | 首页；`guide.md` 使用说明、`points.md` 点表格式、`faq.md` 常见问题、`development.md` 开发与发布 |
| 打包编译 | `build/` | `macos.sh`（.app 和 DMG）、`windows.sh`（交叉编译绿色版 zip）、`icon/`（图标生成器） |
|  | `.github/workflows/` | 持续集成：Windows、macOS、Linux 上跑 vet 和全部测试（GitHub 规定的位置） |

Linux 版目前没有专用代码和安装包：同一份代码可以在 Linux 上编译运行（见 [从源码编译](#从源码编译)），持续集成里每次都测。编译输出在 `dist/`、`bin/`，不进仓库。

## 常用命令

```sh
go run ./cmd/modbus-ai              # 运行桌面应用（启动时为空；“读取 → 打开换热站示例”加载示例并连接内置模拟器）
VERSION=0.9.0 build/macos.sh        # 打包 dist/ 下的 Intel 与 Apple Silicon .app 和 DMG
VERSION=0.9.0 build/windows.sh      # 交叉编译 Windows x64 绿色版 zip（需要 brew install mingw-w64）

go build -o bin/ ./cmd/...

# 模拟器：TCP 或 RTU over TCP，可注入延迟、丢包、CRC 错误、慢应答
./bin/modbus-sim -listen 127.0.0.1:1502 -mode rtu-over-tcp -slow 600:4:1035ms

# 主站
./bin/modbus-cli -target 127.0.0.1:1502 -mode rtu-over-tcp -type float32 -order CDAB -trace read 40001 4
./bin/modbus-cli -target 127.0.0.1:1502 -mode rtu-over-tcp -type float32 -order CDAB write 40347 16
./bin/modbus-cli -target 127.0.0.1:1502 detect
./bin/modbus-cli -target 127.0.0.1:1502 -type uint64 -order CDAB write 40701 72623859790382857   # 64 位整型精确写入
./bin/modbus-sim -listen 127.0.0.1:1503 -mode ascii-over-tcp
./bin/modbus-cli -target 127.0.0.1:1503 -mode ascii-over-tcp -trace read 40001 2
./bin/modbus-cli -port /dev/cu.usbserial-110 -mode ascii -databits 7 -parity E read 40001 10
./bin/modbus-cli -port /dev/cu.usbserial-110 -baud 9600 read 40001 10
```

## 测试

```sh
go test ./...                       # 单元测试、集成测试、界面测试
go test -race -count=3 ./...        # 并发与稳定性
```

- 界面测试用 Fyne 的软件渲染驱动，不需要显示器和显卡。
- `MODBUS_AI_SNAPSHOT=<目录> go test -run 'TestUIWithBuiltinSimulator|Test64BitPoints' ./internal/ui` 会把测试中的界面存成 PNG，用来检查布局；`assets/screenshots/` 里的截图就是这样生成的（`modbus-ai-ui.png` 即 `main.png`，`modbus-ai-packet.png` 即 `packet.png`）。
- 要看真实 OpenGL 渲染，用调试构建：`go build -tags capture -o /tmp/cap ./cmd/modbus-ai && CAPTURE_DIR=/tmp /tmp/cap`，启动后第 2、8 秒各存一张截图后退出。
- 持续集成（`.github/workflows/test.yml`）在 Windows、macOS、Linux 上跑 `go vet` 和全部测试，Windows 以外加 `-race`。

## 打包与发版

`build/macos.sh` 在 Intel Mac 上同时打 Intel 和 Apple Silicon 两个 .app 和 DMG（ad-hoc 签名，最低 macOS 12）；`build/windows.sh` 在 macOS 上用 mingw-w64 交叉编译 Windows x64 绿色版 zip（静态链接，只依赖系统 DLL）。

1. 改代码时同步递增版本号：`cmd/modbus-ai/main.go` 的 `version`、`build/*.sh` 的默认 `VERSION`、本文里的命令示例。
2. `VERSION=x.y.z build/macos.sh`、`VERSION=x.y.z build/windows.sh` 打出两个 DMG 和 Windows zip。
3. 提交并推送，等持续集成在三个平台上都通过。
4. `git tag -a vx.y.z` 并推送；`gh release create --draft` 建草稿，`gh release upload` 逐个上传三个文件（网络慢时一次传完会超时），核对大小后 `gh release edit --draft=false --latest` 发布。

## 协议引擎的约定

- 所有模式都按长度分帧，超时只用于异常恢复。
- 同一连接同一时刻只有一个未完成请求；响应必须通过 Transaction ID（TCP）或 Slave / 功能码 / 字节数 / 回显（RTU）校验才被采用。
- 超时后等待保护间隔并清空接收缓冲，期间收到的字节记为 `LATE_RESPONSE`，不会错配到下一条请求。
- 读请求超时或 CRC 错误后重试（默认 1 次），写请求不重试。
- 每条收发都通过 `Observer` 输出，状态值与 SQLite `packets.status` 一致。

## 进度

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 | 协议核心 + 模拟器 + 自动化测试（无 GUI）；0.5.0 补上 Modbus ASCII 和 07/08/11/12/17/20–24/43 功能码，0.9.0 补上 64 位数据类型 | 已完成 |
| M2 | Fyne 桌面界面（连接栏、Modbus Poll 式读取窗口、通信报文、写入验证、多开）；待在 VM / 远程桌面 / 工控机上验证 | 进行中 |
| M3 | 调试页 + 报文监控 + Session（调试页、报文监控、工作区文件、SQLite 报文记录与历史查看） | 已完成 |
| M4 | 控制验证 + 诊断 + 安全层（写入验证、自动诊断、只读模式） | 已完成 |
| M5 | CSV 点表 + 打包发布（CSV 点表导入、macOS DMG、Windows x64 绿色版、GitHub Releases） | 已完成 |
