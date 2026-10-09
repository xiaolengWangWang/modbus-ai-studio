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
| 资源 | `assets/icon/` | macOS 图标 `AppIcon.png`；Windows 用 `windows/icon16.png` 至 `icon256.png` 分别绘制，并以 `GLFW_ICON` 资源名嵌入 exe；`resources.go` 嵌入应用、窗口和托盘图标，直接运行源码也有图标 |
|  | `assets/examples/` | CSV 点表模板（换热站示例，测试保证与内置点表一致） |
|  | `assets/screenshots/` | 文档里的截图，由界面测试生成（见 [测试](#测试)） |
| 核心代码 | `cmd/modbus-ai/` | 桌面应用入口 |
|  | `cmd/modbus-cli/`、`cmd/modbus-sim/` | 命令行主站、命令行模拟从站 |
|  | `internal/modbus/` | 协议核心：PDU、MBAP / RTU / ASCII 分帧、客户端、数据类型与字节序、地址解析 |
|  | `internal/transport/` | TCP 与串口 |
|  | `internal/detect/` | 协议自动识别 |
|  | `internal/ai/` | DeepSeek JSON 诊断客户端、证据 / 结果校验、Windows DPAPI 密钥存储；不依赖设备和界面 |
|  | `internal/control/` | 写入与控制验证：多次回读，判定 PASS / 未生效 / 被覆盖 / 值不符 |
|  | `internal/simulator/` | 模拟从站与故障注入 |
|  | `internal/recorder/` | 报文记录：会话、收发、断开重连事件存进本机 SQLite |
|  | `internal/update/` | 软件更新：查询 GitHub Releases、下载并校验 SHA-256、替换 Windows 程序文件或 macOS .app |
|  | `internal/ui/` | 桌面界面（各文件分工见 `internal/ui/app.go` 开头的说明） |
|  | `tests/` | 基于模拟器的集成测试 |
| 平台代码 | `platform/` | 各操作系统专用的 Go 代码：`windows.go`（按进程号追加启动日志、获取屏幕可用区域、显卡不支持 OpenGL 时弹窗），`other.go`（macOS、Linux）；`window.go` 按可用区域和 DPI 缩放限制窗口尺寸 |
|  | `platform/macos/` | `Info.plist` 模板 |
|  | `platform/windows/` | Windows 绿色版说明模板 `README.md`，打包时生成 UTF-8 的 `README.txt` |
| 文档 | `README.md`、`docs/` | 首页；`guide.md` 使用说明、`points.md` 点表格式、`faq.md` 常见问题、`development.md` 开发与发布 |
| 打包编译 | `build/` | `macos.sh`（.app 和 DMG）、`windows.sh`（交叉编译绿色版 zip）、`gitee.sh`（同步到 Gitee 镜像）、`icon/`（图标生成器） |
|  | `.github/workflows/` | 持续集成：Windows、macOS、Linux 上跑 vet 和全部测试（GitHub 规定的位置） |

Linux 版目前没有专用代码和安装包：同一份代码可以在 Linux 上编译运行（见 [从源码编译](#从源码编译)），持续集成里每次都测。编译输出在 `dist/`、`bin/`，不进仓库。

寄存器检测按职责集中在 `internal/ui/` 的四个文件：`registerprobe.go` 管理参数表单、进度和轮询暂停恢复；`registerprobe_scan.go` 执行批量检测、异常地址定位和超时复核；`registerprobe_result.go` 展示统计、具体地址并处理复制和应用范围；`registerprobe_test.go` 保留协议、地址缺口与界面的回归测试。`scan.go` 负责从站、串口参数扫描和通信诊断计数器。

AI 界面由 `ai.go` 管理设置、来源、异步请求与取消；`ai_context.go` 在界面线程复制冻结证据并校验日志帧，`ai_report.go` 用纯文本显示报告并导出冻结证据。`ai_test.go` 和 `ai_workflow_test.go` 覆盖隐私默认值、快照不可变、最小连接测试、来源切换、无效帧、未知历史字段、报告导出、取消 / 关闭、重试保留上次报告、范围数量匹配和亚毫秒延迟。`internal/ai/client_test.go` 使用本地 HTTP 测试服务验证请求、证据引用、错误、重定向、超时及大小边界；CI 不需要实际 API Key，不请求外部模型。调试截图的 AI 报告使用本地合成响应。

`toolbar.go` 的分组换行与面板布局根据当前宽度预留工具栏高度，供主窗口、读取窗口、报文区和 AI 助手使用。读取窗口嵌套诊断区按实际宽度计算高度，顶部滚动区域显式保留完整内容范围，表格独立布局。`gui_test.go` 验证窄面板按钮、查找输入框、计数、AI 顶部操作以及空记录 / 无匹配 / 暂停提示；回归同时覆盖窄读取窗口、完整错误宽度、矮窗口滚动、实际连接过程和检测期间的状态恢复；原生截图另覆盖紧凑 AI 窗口、报文无匹配与读取诊断滚动。

本地发版后仅保留最新版本的安装包、校验值和验证记录；旧安装包、重复解压目录、临时截图和旧版本发版脚本清理。必要工具保留在 `bin/`，源码、资源和测试按上表存放。

## 常用命令

```sh
go run ./cmd/modbus-ai              # 运行桌面应用（启动时为空；“读取 → 打开换热站示例”加载示例并连接内置模拟器）
VERSION=1.0.0 build/macos.sh       # 打包 dist/ 下的 Intel 与 Apple Silicon .app 和 DMG
VERSION=1.0.0 build/windows.sh     # 交叉编译 Windows x64 绿色版 zip（需要 brew install mingw-w64）

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
- `MODBUS_AI_SNAPSHOT=<目录> go test -run 'TestUIWithBuiltinSimulator|Test64BitPoints' ./internal/ui` 会把测试中的界面存成 PNG，用来检查布局；该命令生成 `modbus-ai-ui.png` 和 `modbus-ai-packet.png`；当前 `assets/screenshots/` 的主窗口、报文和 AI 图使用下面的原生调试构建生成。
- 要看真实 OpenGL 渲染，用调试构建：`go build -tags capture -o /tmp/cap ./cmd/modbus-ai && CAPTURE_DIR=/tmp /tmp/cap`。它按 `internal/ui/capture.go` 的剧本操作界面（打开示例、读取定义、写入、字节序、历史报文、报文筛选、960 / 1024 宽布局，以及 AI 报告、发送内容、设置、故障 / 日志来源、导出对话框、紧凑 AI 布局、报文无匹配提示、窄读取窗口、诊断滚动和 AI 重试失败保留报告、证据详情与完整 JSON、窄设置页、检测结果和暂停读数字节序调整 / 生效），每步存一张截图后退出，共 32 张；终端没有屏幕录制权限也能用。运行时应隔离用户数据目录；AI 使用本地合成响应，截图不是实际 DeepSeek 诊断结果。
- 真实接口回归仅在显式启用时运行：设置 `MODBUS_AI_LIVE_TEST=1` 后执行 `go test -tags deepseek_live ./internal/ui -run '^TestDeepSeekLiveDiagnosticWorkflow$' -v -count=1 -timeout 4m`。它读取环境变量或已有加密密钥，不修改设置，仅向官方接口发送三个合成场景，产生实际 API 用量。可设置 `MODBUS_AI_LIVE_REPORT_DIR` 保存合成诊断报告；常规测试不编译此文件。
- 持续集成（`.github/workflows/test.yml`）在 Windows、macOS、Linux 上跑 `go vet` 和全部测试，Windows 以外加 `-race`。

## 打包与发版

`build/macos.sh` 在 Intel Mac 上同时打 Intel 和 Apple Silicon 两个 .app 和 DMG（ad-hoc 签名，最低 macOS 12）；`build/windows.sh` 在 macOS 上用 mingw-w64 交叉编译 Windows x64 绿色版 zip（静态链接，只依赖系统 DLL）。

Windows 启动阶段耗时和字体预扫描结果追加写入 `%AppData%\ModbusAIStudio\app.log`，每行带进程号，多个实例不会相互覆盖日志；图标资源可用 `go-winres extract` 检查 `GLFW_ICON` 和高 DPI 清单。主窗口默认 1040 × 680 逻辑尺寸，并按屏幕可用区域和 DPI 缩放适配；关闭按钮默认隐藏到托盘，采集继续，托盘菜单退出时关闭本实例全部连接并写完数据库缓冲。界面测试覆盖后台轮询、独立窗口关闭和退出清理，Windows 本机已验证两个进程同时运行及隐藏后保留进程。150% 缩放、16 像素图标和启动速度仍需在不同设备上验证；遇到问题时请附 `app.log`。

1. 改代码时同步递增版本号：`cmd/modbus-ai/main.go` 的 `version`、`build/*.sh` 的默认 `VERSION`、本文里的命令示例。
2. `VERSION=x.y.z build/macos.sh`、`VERSION=x.y.z build/windows.sh` 打出两个 DMG 和 Windows zip。安装包文件名不要改：程序里的“检查更新”按结尾（`-Windows-x64.zip`、`-macOS-Intel.dmg`、`-macOS-AppleSilicon.dmg`）找本机的安装包。
   没有本机 macOS 编译环境时，推送后运行 GitHub Actions 的 `package` 工作流，输入版本号；它调用同一组打包脚本生成三个安装包。下载 `installers-Linux` 和 `installers-macOS` 产物，核验后再上传发行版。
3. 提交并推送，等持续集成在三个平台上都通过。
4. `git tag -a vx.y.z` 并推送；`gh release create --draft` 建草稿，`gh release upload` 逐个上传三个文件（网络慢时一次传完会超时），核对大小后 `gh release edit --draft=false --latest` 发布。发布说明末尾必须有 `## SHA-256` 段落，每行 `校验值  文件名`：自动更新按它校验下载的安装包，没有校验值的版本只能手动下载。
5. 同步到 Gitee 镜像：`VERSION=x.y.z NOTES=发布说明.md build/gitee.sh`，用 SSH 推 main 和 tag；有 `~/.gitee_token`（Gitee 私人令牌，勾选 projects）时自动建发行版并上传三个安装包，没有令牌时按提示在网页上手动传。程序检查更新时同时查 Gitee 和 GitHub。

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
