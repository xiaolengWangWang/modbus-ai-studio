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
|  | `cmd/modbus-web/`、`internal/web/` | Linux Web 版：入口和服务端（页面在 `internal/web/static/`，编进程序），见 [Linux Web 版](#linux-web-版) |
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
|  | `platform/windows/` | Windows 绿色版说明模板 `README.txt`，打包时替换版本号、加 UTF-8 BOM 和 CRLF |
| 文档 | `README.md`、`docs/` | 首页；`guide.md` 使用说明、`points.md` 点表格式、`faq.md` 常见问题、`development.md` 开发与发布 |
| 打包编译 | `build/` | `macos.sh`（.app 和 DMG）、`windows.sh`（交叉编译绿色版 zip）、`linux-web.sh`（Linux Web 版 x64 / ARM64 的 tar.gz）、`release/`（本机发版工具，见 [打包与发版](#打包与发版)）、`icon/`（图标生成器） |
|  | `.github/workflows/` | `test.yml` 在 Windows、macOS、Linux 上跑 vet 和全部测试；`package.yml` 打桌面版三个安装包和 Linux Web 版两个包（GitHub 规定的位置） |

桌面版没有 Linux 安装包：同一份代码可以在 Linux 上编译运行（见 [从源码编译](#从源码编译)），持续集成里每次都测；Linux 网关和服务器上用 Web 版。编译输出在 `dist/`、`bin/`，不进仓库。

寄存器检测集中在 `internal/ui/`：`registerprobe.go` 管理参数表单、进度和轮询暂停恢复；`registerprobe_connection.go` 管理独立检测连接、断线重连与请求重试；`registerprobe_scan.go` 执行批量检测、异常地址定位和超时复核；`registerprobe_result.go` 展示统计、具体地址并处理复制和应用范围。`registerprobe_test.go` 和 `robustness_test.go` 覆盖协议、点表分段、断线重连与检测状态恢复；`import_load_test.go` 覆盖 1000 点 CSV / XLSX 的后台导入与建窗数量。`scan.go` 负责从站、串口参数扫描和通信诊断计数器。

AI 界面由 `ai.go` 管理设置、来源、异步请求与取消；`ai_context.go` 在界面线程复制冻结证据并校验日志帧，`ai_report.go` 用纯文本显示报告并导出冻结证据。`ai_test.go` 和 `ai_workflow_test.go` 覆盖隐私默认值、快照不可变、最小连接测试、来源切换、无效帧、未知历史字段、报告导出、取消 / 关闭、重试保留上次报告、范围数量匹配和亚毫秒延迟。`internal/ai/client_test.go` 使用本地 HTTP 测试服务验证请求、证据引用、错误、重定向、超时及大小边界；CI 不需要实际 API Key，不请求外部模型。调试截图的 AI 报告使用本地合成响应。

`toolbar.go` 的分组换行与面板布局根据当前宽度预留工具栏高度，供主窗口、读取窗口、报文区和 AI 助手使用。读取窗口嵌套诊断区按实际宽度计算高度，顶部滚动区域显式保留完整内容范围，表格独立布局。`gui_test.go` 验证窄面板按钮、查找输入框、计数、AI 顶部操作以及空记录 / 无匹配 / 暂停提示；回归同时覆盖窄读取窗口、完整错误宽度、矮窗口滚动、实际连接过程和检测期间的状态恢复；原生截图另覆盖紧凑 AI 窗口、报文无匹配与读取诊断滚动。

`dist/` 只留最新版本的安装包、校验值和发布记录：发版最后自动删掉旧版本的文件，也可单独运行 `go run ./build/release clean`。发版不用 `bin/` 里的脚本，`bin/` 只放编译输出和 `go-winres` 这类工具；源码、资源和测试按上表存放。

## 常用命令

```sh
go run ./cmd/modbus-ai              # 运行桌面应用（启动时为空；“读取 → 打开换热站示例”加载示例并连接内置模拟器）
build/macos.sh                      # 打包 dist/ 下的 Intel 与 Apple Silicon .app 和 DMG（版本号取自 cmd/modbus-ai/main.go）
build/windows.sh                    # 交叉编译 Windows x64 绿色版 zip（需要 brew install mingw-w64）
build/linux-web.sh                  # 编 Linux Web 版 x64 / ARM64 的 tar.gz（不需要 cgo；装了加密软件的电脑上会因说明被加密而停止）
go run ./cmd/modbus-web -listen 127.0.0.1:8502 -data /tmp/web   # 本机运行 Web 版，终端打印地址和访问口令

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
- 持续集成（`.github/workflows/test.yml`）在 Windows、macOS、Linux 上跑 `go vet` 和全部测试，Windows 以外加 `-race`；只在推分支和 Pull Request 时运行，发版推 tag 不重跑。

## 打包与发版

`build/macos.sh` 在 macOS 上打 Intel 和 Apple Silicon 两个 .app 和 DMG（ad-hoc 签名，最低 macOS 12）；`build/windows.sh` 在 Linux / macOS 上用 mingw-w64 交叉编译 Windows x64 绿色版 zip（静态链接，只依赖系统 DLL）；`build/linux-web.sh` 用 `CGO_ENABLED=0` 编 Linux Web 版 x64 和 ARM64，各打一个 tar.gz（`modbus-web` 加 `README.txt`，打包时写上可执行位）。Actions 分别使用 Ubuntu 和 macOS runner，Web 版和 Windows 版都在 Ubuntu 上打。

Windows 启动阶段耗时和字体预扫描结果追加写入 `%AppData%\ModbusAIStudio\app.log`，每行带进程号，多个实例不会相互覆盖日志；图标资源可用 `go-winres extract` 检查 `GLFW_ICON` 和高 DPI 清单。主窗口默认 1040 × 680 逻辑尺寸，并按屏幕可用区域和 DPI 缩放适配；关闭按钮默认隐藏到托盘，采集继续，托盘菜单退出时关闭本实例全部连接并写完数据库缓冲。界面测试覆盖后台轮询、独立窗口关闭和退出清理，Windows 本机已验证两个进程同时运行及隐藏后保留进程。150% 缩放、16 像素图标和启动速度仍需在不同设备上验证；遇到问题时请附 `app.log`。

版本号只写在 `cmd/modbus-ai/main.go` 的 `version` 一处，打包脚本、`package` 工作流和发版工具都从这里取。发版用 Go 写的 `build/release`，不用按版本复制脚本：

1. 改 `version`，在 `platform/windows/README.txt`（打进 zip 的说明）的更新记录里加这一版，提交并推送。
2. 写发布说明 `dist/release-x.y.z-summary.md`，第一行 `# Modbus AI Studio x.y.z`；不写 SHA-256 段落。
3. `go run ./build/release prepare`：
   - 确认 GitHub 上的 main 就是本地 HEAD、版本号比已发布的新，`platform/windows/README.txt` 里有这一版的更新记录（`### x.y.z …` 标题）；
   - 等 `test` 工作流在三个平台上通过；这个提交还没打过包就触发 `package` 工作流，它在 GitHub Actions 上调用 `build/windows.sh`、`build/linux-web.sh` 和 `build/macos.sh`，本机不需要 macOS 编译环境；macOS runner 排不上、任务被取消时自动重跑；
   - 下载两个产物，按 GitHub 记录的摘要核对，再逐个检查安装包：DMG 结尾有 koly 块；Windows zip 只有 `ModbusAIStudio.exe` 和 `README.txt`、路径用 `/`，README 是没被加密的 UTF-8 BOM + CRLF 文本、带这一版的更新记录，exe 的文件版本、`GLFW_ICON` 图标和 per monitor v2 高 DPI 清单都在；Web 版 tar.gz 只有可执行的 `modbus-web` 和明文 `README.txt`，程序是对应架构（x86-64 / ARM64）的 Linux ELF，说明里有这一版的版本号；
   - 生成 `dist/release-x.y.z.md`（说明末尾加 `## SHA-256` 段落，每行 `校验值  文件名`，自动更新按它校验下载的安装包）、各安装包的 `.sha256` 和记录 `release-x.y.z-assets.json`；Windows 版另解压到 `dist/ModbusAIStudio-x.y.z-Windows-x64/`，可以直接试用。
4. 看一遍 `dist/release-x.y.z.md`，运行 `go run ./build/release publish`：
   - 在打包的提交上打 tag，推到 GitHub；建草稿，逐个上传五个安装包（网络慢时一次传完会超时），按 GitHub 算出的摘要核对后发布为最新版；
   - 把这个提交和 tag 推到 Gitee 镜像，建发行版，上传五个安装包；Gitee 没有草稿，传完之前检查更新会改用 GitHub 上的同一版本；
   - 核对结果（也可单独运行 `verify`）：两边的说明都与本地一致；不带令牌从 Gitee 下载全部安装包，SHA-256 与 CI 产物一致；用程序自己的检查更新代码确认优先查到 Gitee 上的新版本，三个平台的安装包和校验值都对，GitHub 可作备用下载源；
   - 删掉 `dist/` 里旧版本的文件。

安装包文件名不要改：程序检查更新时同时查 Gitee 和 GitHub，按结尾（`-Windows-x64.zip`、`-macOS-Intel.dmg`、`-macOS-AppleSilicon.dmg`）找本机的安装包。Web 版的包（`ModbusAIStudio-Web-x.y.z-Linux-x64.tar.gz`、`-Linux-arm64.tar.gz`）只随发布上传，检查更新不认它们，Web 版没有自动更新，换新版本时替换程序后重启。

凭据：GitHub 用 git 凭据管理器里推送用的令牌（或环境变量 `GH_TOKEN`）；Gitee 用 `~/.gitee_token` 里的私人令牌（勾选 projects，或环境变量 `GITEE_TOKEN`），推送走 https，由工具回答 git 的用户名和密码提问，令牌不出现在命令行里。

每一步先查已有状态再动手，网络出错自动重试；仍失败时重跑同一条命令，已完成的步骤会跳过。过程记在 `dist/release-x.y.z.log`。

发布前会先验证两侧凭据、本地安装包名称与 SHA-256、发布说明中的校验值，再执行推 tag 和远程发布。可以编辑发布正文，校验段必须与核对过的安装包一致。

## Linux Web 版

`modbus-web` 在网关或服务器上运行，用浏览器调试设备。连接、读取表、写入回读、协议识别和报文监视都在服务端执行，页面只负责显示和操作，多个浏览器看到的是同一份状态；状态变化和新报文通过 Server-Sent Events 推给页面，页面重连时带上最后一条报文的序号，服务端补发中间漏掉的。页面、样式和脚本在 `internal/web/static/`，用 `embed` 编进程序，不依赖外部资源。

- 安全：访问口令首次启动时随机生成，存在数据目录的 `web-password`；同一来源 5 分钟内输错 5 次暂时拒绝；写操作只接受 JSON 且 Origin 必须是本站，页面带 CSP。跨网访问用 `-cert` / `-key` 开 HTTPS。
- 记录：收发报文按天写入数据目录的 `records/packets-YYYYMMDD-NNN.sqlite3`，表结构与桌面版相同，页面上可下载，桌面版的历史记录能直接打开。
- 编译：不需要 cgo，SQLite 用纯 Go 的 `modernc.org/sqlite`（`internal/recorder/driver_purego.go`），一份静态程序可在任何 Linux 发行版上运行；CI 在 Linux 上用 `CGO_ENABLED=0` 跑 `internal/web` 的测试，与发出去的程序一致。
- 断线原因判断与桌面版共用 `internal/transport/netcause.go`。

## SQLite3 存储

应用直接读写标准 SQLite3 文件，不编译或附带 `sqlite3.exe`。会话、报文、日志的字段、压缩标记、索引及兼容规则见 [SQLite3 数据文件与表结构](sqlite-storage.md)。大字段采用无损 zlib 压缩，查询条件保留原样；历史列表先选择最近的会话，再只统计这些会话。数据库迁移和压缩的公共逻辑集中在 `internal/recorder/`，建表定义只保留在 `schema` 一处。

CI 在原有 cgo 测试之外单独验证纯 Go SQLite 驱动：`CGO_ENABLED=0 go test ./internal/recorder ./internal/web`。本机缺少 C 编译器时可用 `go test -tags ci ./...` 运行 Fyne 软件渲染回归；桌面安装包的原生构建仍由三个平台的 CI 检查。

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
