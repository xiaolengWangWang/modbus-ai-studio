// Package ui 是 Fyne 桌面界面：连接栏、Modbus Poll 式读取窗口、通信报文、报文 / 寄存器解析、
// 自定义请求和写入验证。每个主窗口是一个独立的工作区（连接、读取窗口、报文互不影响），
// 可以同时开多个主窗口调试多台设备。界面只负责输入、展示和交互，协议全部交给 internal/modbus。
//
// 文件分工：
//
//	app.go        主窗口：布局、菜单、状态栏、后台刷新
//	conn.go       连接：协议、会话、连接 / 断开 / 重连、协议识别、串口占用
//	link.go       连接保持：断开后自动重连，按断开时机分析原因
//	tiles.go      读取窗口的增删、换热站示例、平铺
//	readdef.go    读取定义：类型、列规则、定义对话框
//	readwin.go    读取窗口：表格、轮询、显示值、错误行
//	readbar.go    读取窗口的控制条（功能码、格式、字节序、原始值）和当前窗口边框
//	diagnose.go   错误自动分析和一键处理
//	write.go      写入对话框和写入验证报告
//	readonly.go   只读模式
//	traffic.go    通信报文列表
//	faultlog.go   故障日志列表和原始报文关联
//	inspector.go  解析面板（寄存器多种解读、报文逐字段）
//	decode*.go    报文逐字段解析
//	reqtool.go    自定义请求窗口
//	scan.go       总线工具：地址探测、从站扫描、串口参数扫描、诊断计数器
//	points.go     点表，解析 CSV / xlsx 点表和平台导出的设备属性表
//	xlsx.go       读取 xlsx 单元格（导入点表用）
//	workspace.go  工作区文件、最近打开、导入点表
//	history.go    报文记录（SQLite）和历史报文窗口
//	update.go     检查更新、下载安装（“帮助”菜单和启动时自动检查）
//	format.go     数值、地址、报文的显示格式
//	widgets.go    通用界面部件
package ui

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
	"modbus-ai-studio/platform"
)

// uiMu 串行化后台 goroutine 发起的界面更新。正式驱动本来就在主线程依次执行 fyne.Do；
// 测试驱动会在调用方 goroutine 直接执行，不加锁时无界面测试和截图会偶发数据竞争。
var uiMu sync.Mutex

func uiDo(fn func()) {
	fyne.Do(func() {
		uiMu.Lock()
		defer uiMu.Unlock()
		fn()
	})
}

// Workspace 是一个主窗口：一条连接、若干读取窗口、通信报文和解析面板。
type Workspace struct {
	Version string

	app fyne.App
	win fyne.Window
	no  int

	proto    *widget.Select
	target   *widget.Entry
	useSim   *widget.Check
	port     *widget.Select
	baud     *widget.SelectEntry
	frameFmt *widget.Select // 数据位、校验位、停止位，例如 8N1
	timeoutE *widget.Entry
	connBtn  *widget.Button
	detectBn *widget.Button
	tcpBox   *fyne.Container
	serBox   *fyne.Container
	bar      *fyne.Container

	session     *session
	connecting  bool
	recID       atomic.Int64 // 正在记录的会话 ID，收发回调里读；0 表示不记录
	points      pointTable   // 本窗口的点表，初始为空
	readOnly    bool         // 只读模式，禁止一切写入（readonly.go）
	roItem      *fyne.MenuItem
	autoUpdItem *fyne.MenuItem // “帮助 → 自动检查更新”，勾选状态随设置变化
	path        string         // 工作区文件，未保存时为空
	timeout     time.Duration
	windows     []*readWindow
	cur         *readWindow // 当前读取窗口（tiles.go 的 current），读取窗口的快捷键作用于它
	nextWin     int
	tiles       *fyne.Container
	traffic     *trafficPanel
	inspect     *inspector
	log         *faultLog
	tabs        *container.AppTabs // 通信报文 / 日志
	logTab      *container.TabItem
	ring        packetRing // 最近的收发，出错时取出原始报文记进日志
	status      *widget.Label
	stats       stats
	evidence    evidence
	tools       []fyne.Window // 自定义请求等工具窗口，主窗口关闭时一起关闭
	historyWin  fyne.Window   // 打开着的历史报文窗口，只开一个
	done        chan struct{}
	closed      bool
}

// Open 新建一个主窗口并显示。主窗口初始为空：没有读取窗口，也不自动连接。
// 换热站示例在“读取 → 打开换热站示例”里。
func Open(app fyne.App, version string) *Workspace {
	return open(app, version, int(winSeq.Add(1)))
}

func open(app fyne.App, version string, no int) *Workspace {
	w := app.NewWindow("Modbus AI Studio " + version)
	want := fyne.NewSize(1280, 820)
	workW, workH := platform.WorkArea()
	if workW > 0 {
		w.Resize(fyne.NewSize(960, 620)) // 首帧先放进小屏幕；Show 后才能取得 Fyne 的真实缩放比例
	} else {
		w.Resize(want)
	}
	ws := newWorkspace(app, w, version, no)
	w.Show()
	if workW > 0 {
		w.Resize(platform.FitSize(want, workW, workH, w.Canvas().Scale()))
	}
	return ws
}

func newWorkspace(app fyne.App, win fyne.Window, version string, no int) *Workspace {
	ws := &Workspace{Version: version, app: app, win: win, no: no, timeout: time.Second, done: make(chan struct{}), points: pointTable{}}
	ws.traffic = newTrafficPanel(ws)
	ws.traffic.title.Hide() // 页签已经写了“通信报文”
	ws.inspect = newInspector(ws)
	ws.log = newFaultLog(app)
	ws.log.onSelect = func(e logEntry) { ws.inspect.show("日志", e.detail()) }
	ws.status = widget.NewLabel("")
	ws.tiles = container.NewStack()
	win.SetContent(ws.layout())
	ws.setMenu()
	ws.refreshTitle()
	ws.relayout()
	win.SetOnClosed(ws.shutdown)
	go ws.tick()
	ws.refreshStatus()
	return ws
}

// tick 每 100 ms 把收发记录刷到界面，每 500 ms 刷新状态栏。报文多时合并刷新，避免界面卡顿。
func (ws *Workspace) tick() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-ws.done:
			return
		case <-t.C:
		}
		if n%20 == 0 {
			uiDo(func() {
				if ws.closed || ws.session != nil || !ws.serialMode() {
					return
				}
				go func() { // 枚举串口可能要几十毫秒，不放在界面线程
					if list, err := transport.ListSerialPorts(); err == nil {
						uiDo(func() {
							if !ws.closed && ws.session == nil {
								ws.setPorts(list)
							}
						})
					}
				}()
			})
		}
		status := n%5 == 0
		if ws.traffic.hasPending() || status {
			uiDo(func() {
				if ws.closed {
					return
				}
				ws.traffic.flush()
				if status {
					ws.refreshStatus()
				}
			})
		}
	}
}

func (ws *Workspace) shutdown() {
	if ws.closed {
		return
	}
	ws.closed = true
	close(ws.done)
	for _, w := range ws.windows {
		w.halt()
	}
	if ws.session != nil {
		ws.recID.Store(0)
		ws.session.close()
		ws.session = nil
	}
	for _, t := range slices.Clone(ws.tools) { // 工具窗口关闭时会把自己从 ws.tools 里删掉
		t.Close()
	}
}

func (ws *Workspace) layout() fyne.CanvasObject {
	ws.target = widget.NewEntry()
	ws.target.SetText("127.0.0.1:502")
	ws.useSim = widget.NewCheck("内置模拟器", func(on bool) {
		if on {
			ws.target.Disable()
		} else {
			ws.target.Enable()
		}
		ws.updateDetectBtn()
	})
	ws.useSim.SetChecked(true)

	// 串口列表在切到 RTU 串口时和未连接期间每 2 s 自动刷新，插拔 USB 转 485 不用手动刷新
	ws.port = widget.NewSelect(nil, nil)
	ws.port.PlaceHolder = "选择串口"
	ws.baud = widget.NewSelectEntry([]string{"1200", "2400", "4800", "9600", "19200", "38400", "57600", "115200"})
	ws.baud.SetText("9600")
	ws.frameFmt = widget.NewSelect(rtuFormats, nil)
	ws.frameFmt.SetSelected("8N1")
	ws.detectBn = widget.NewButtonWithIcon("识别", theme.SearchIcon(), ws.detectProtocol)

	// 连接栏按 1024 宽的工控机屏幕设计，两种协议下都不超宽
	ws.tcpBox = container.NewHBox(widget.NewLabel("目标"), fixed(140, ws.target), ws.useSim, ws.detectBn)
	ws.serBox = container.NewHBox(widget.NewLabel("串口"), fixed(140, ws.port), fixed(100, ws.baud), fixed(72, ws.frameFmt))
	ws.serBox.Hide()
	ws.proto = widget.NewSelect(protoNames, func(s string) {
		if ws.serialMode() {
			formats := rtuFormats
			if protoModes[s].IsASCII() {
				formats = asciiFormats
			}
			ws.frameFmt.SetOptions(formats)
			if !slices.Contains(formats, ws.frameFmt.Selected) {
				ws.frameFmt.SetSelected(formats[0])
			}
			ws.tcpBox.Hide()
			ws.serBox.Show()
			list, _ := transport.ListSerialPorts()
			ws.setPorts(list)
		} else {
			ws.serBox.Hide()
			ws.tcpBox.Show()
		}
		ws.updateDetectBtn()
		if ws.bar != nil {
			ws.bar.Refresh()
		}
	})
	ws.proto.SetSelected(protoRTUTCP)

	ws.timeoutE = widget.NewEntry()
	ws.timeoutE.SetText("1000")
	ws.timeoutE.OnSubmitted = func(string) { ws.applyTimeoutEntry() }
	ws.connBtn = widget.NewButtonWithIcon("连接", theme.LoginIcon(), ws.toggleConnect)
	ws.connBtn.Importance = widget.HighImportance
	ws.updateDetectBtn()

	ws.bar = container.NewHBox(fixed(140, ws.proto), ws.tcpBox, ws.serBox,
		widget.NewLabel("超时(ms)"), fixed(64, ws.timeoutE), ws.connBtn, layout.NewSpacer(),
		widget.NewButtonWithIcon("读取窗口", theme.ContentAddIcon(), ws.addReadWindow),
		widget.NewButtonWithIcon("自定义请求", theme.MailSendIcon(), ws.openRequestTool),
		widget.NewButtonWithIcon("", theme.ContentCopyIcon(), func() { ws.openNew() }))
	bar := ws.bar

	ws.logTab = container.NewTabItem("日志", ws.log.root)
	ws.tabs = container.NewAppTabs(container.NewTabItem("通信报文", ws.traffic.root), ws.logTab)
	ws.log.onChange = func(n int) {
		ws.logTab.Text = "日志"
		if n > 0 {
			ws.logTab.Text = fmt.Sprintf("日志 (%d)", n)
		}
		ws.tabs.Refresh()
	}
	bottom := container.NewHSplit(ws.tabs, ws.inspect.root)
	bottom.Offset = 0.56
	main := container.NewVSplit(ws.tiles, bottom)
	main.Offset = 0.7
	return container.NewBorder(
		container.NewVBox(bar, widget.NewSeparator()),
		container.NewVBox(widget.NewSeparator(), ws.status),
		nil, nil, main)
}

// setMenu 设置菜单和快捷键（macOS 用 ⌘，Windows、Linux 用 Ctrl）。Fyne 先按主菜单匹配快捷键，
// 焦点在输入框里也能用；“读取”菜单里定义、写入、暂停这几项作用于当前读取窗口。
// 不能用的组合：Ctrl+A / C / V / X / Z / Y 在 Windows、Linux 上被 Fyne 当成编辑快捷键，到不了菜单；
// ⌘H、⌘M、⌘Q 是 macOS 系统的。
func (ws *Workspace) setMenu() {
	key := func(it *fyne.MenuItem, k fyne.KeyName, shift bool) *fyne.MenuItem {
		mod := fyne.KeyModifierShortcutDefault
		if shift {
			mod |= fyne.KeyModifierShift
		}
		it.Shortcut = &desktop.CustomShortcut{KeyName: k, Modifier: mod}
		return it
	}
	item := fyne.NewMenuItem
	cur := func(fn func(*readWindow)) func() {
		return func() {
			if w := ws.current(); w != nil {
				fn(w)
			}
		}
	}
	// 会弹对话框的菜单项：已有对话框打开时不再弹。对话框只挡住窗口里的点击，macOS 的原生菜单和
	// 各平台的快捷键照样能用，不拦着就会一层层叠下去
	modal := func(fn func()) func() {
		return func() {
			if ws.win.Canvas().Overlays().Top() != nil {
				return
			}
			fn()
		}
	}
	ws.roItem = item("只读模式（禁止写入）", func() { ws.setReadOnly(!ws.readOnly) })
	ws.autoUpdItem = item("自动检查更新", ws.toggleAutoUpdate)
	ws.autoUpdItem.Checked = autoUpdate(ws.app)
	ws.win.SetMainMenu(fyne.NewMainMenu(
		fyne.NewMenu("文件",
			key(item("新建窗口", func() { ws.openNew() }), fyne.KeyN, false),
			key(item("打开工作区…", modal(ws.openWorkspace)), fyne.KeyO, false),
			key(item("保存工作区", modal(ws.saveWorkspace)), fyne.KeyS, false),
			key(item("工作区另存为…", modal(ws.saveWorkspaceAs)), fyne.KeyS, true),
			fyne.NewMenuItemSeparator(),
			key(item("关闭窗口", ws.win.Close), fyne.KeyW, false)),
		fyne.NewMenu("连接",
			key(item("连接 / 断开", ws.toggleConnect), fyne.KeyK, false),
			key(item("识别协议", modal(ws.detectProtocol)), fyne.KeyD, false),
			item("扫描串口参数…", modal(ws.scanSerialDialog)),
			fyne.NewMenuItemSeparator(), ws.roItem),
		fyne.NewMenu("读取",
			key(item("新建读取窗口", modal(ws.addReadWindow)), fyne.KeyT, false),
			key(item("读取定义…", modal(cur(ws.showDefinition))), fyne.KeyE, false),
			key(item("写入选中的值…", modal(cur(ws.showWrite))), fyne.KeyReturn, false),
			key(item("暂停 / 继续", cur(func(w *readWindow) { w.setPaused(!w.paused) })), fyne.KeyP, false),
			item("关闭读取窗口", cur(ws.removeWindow)),
			fyne.NewMenuItemSeparator(),
			key(item("导入点表…", modal(ws.importPoints)), fyne.KeyI, false),
			item("调整点表字节序…", modal(func() { ws.showPointOrderDialog(ws.current()) })),
			item("打开换热站示例", ws.loadDemo),
			fyne.NewMenuItemSeparator(),
			key(item("全部暂停", func() { ws.pauseAll(true) }), fyne.KeyP, true),
			key(item("全部继续", func() { ws.pauseAll(false) }), fyne.KeyR, true)),
		fyne.NewMenu("调试",
			key(item("自定义请求…", ws.openRequestTool), fyne.KeyR, false),
			item("扫描从站地址…", modal(ws.scanSlavesDialog)), item("读取诊断计数器…", modal(ws.diagCountersDialog)),
			fyne.NewMenuItemSeparator(),
			key(item("历史报文…", modal(ws.openHistory)), fyne.KeyH, true),
			item("打开报文数据库", modal(func() { ws.openDatabase(ws.win, false) })),
			item("报文数据库所在文件夹", modal(func() { ws.openDatabase(ws.win, true) })),
			key(item("清空通信报文", ws.traffic.clear), fyne.KeyL, false)),
		fyne.NewMenu("帮助",
			item("检查更新…", modal(func() { ws.checkUpdate(true) })),
			ws.autoUpdItem,
			fyne.NewMenuItemSeparator(),
			item("下载页面", ws.openReleasePage)),
	))
}

// openNew 新建一个主窗口，协议和目标沿用当前窗口，方便连同一网段的另一台设备。
func (ws *Workspace) openNew() *Workspace {
	n := Open(ws.app, ws.Version)
	n.proto.SetSelected(ws.proto.Selected)
	n.useSim.SetChecked(ws.useSim.Checked)
	n.target.SetText(ws.target.Text)
	n.timeoutE.SetText(ws.timeoutE.Text)
	n.baud.SetText(ws.baud.Text)
	n.frameFmt.SetSelected(ws.frameFmt.Selected)
	n.setReadOnly(ws.readOnly)
	return n
}

// ---------- 读取窗口管理 ----------

// ---------- 连接 ----------

// OnPacket 接收全部收发记录，分发给通信报文、状态栏和诊断证据（Packet Recorder 的界面一侧）。
func (ws *Workspace) OnPacket(p modbus.Packet) {
	if p.Dir == modbus.DirRX && p.Status == modbus.StatusSuccess {
		ws.stats.rtt.Store(int64(p.RTT))
	}
	ws.evidence.observe(p)
	ws.ring.push(p)
	ws.traffic.push(p)
	if id := ws.recID.Load(); id != 0 {
		if r := currentRecorder(); r != nil {
			r.Record(id, p)
		}
	}
}

func (ws *Workspace) refreshTitle() {
	title := "Modbus AI Studio " + ws.Version
	if ws.no > 1 {
		title += fmt.Sprintf(" · 窗口 %d", ws.no)
	}
	if ws.path != "" {
		title += " · " + filepath.Base(ws.path)
	}
	if s := ws.session; s != nil {
		title += " · " + modeName[s.mode] + " " + s.desc
	}
	ws.win.SetTitle(title)
}

func (ws *Workspace) refreshStatus() {
	pts := ""
	if n := len(ws.points); n > 0 {
		pts = fmt.Sprintf(" · 点表 %d 点", n)
	}
	if ws.readOnly {
		pts += " · 只读模式"
	}
	if ws.session == nil {
		ws.status.SetText(fmt.Sprintf("○ 未连接 · Modbus AI Studio %s · 窗口 %d%s%s", ws.Version, ws.no, pts, updateStatus()))
		return
	}
	rtt := "—"
	if v := ws.stats.rtt.Load(); v > 0 {
		rtt = formatRTT(time.Duration(v))
	}
	state := "● 已连接"
	if ws.session.lost != nil {
		state = "◐ 连接断开"
	}
	text := fmt.Sprintf("%s · %s %s%s · 超时 %d ms · 轮询 %d · 有效响应 %d · 错误 %d", state,
		modeName[ws.session.mode], ws.session.desc, ws.session.linkStatus(), ws.timeout.Milliseconds(), ws.stats.polls.Load(), ws.stats.ok.Load(), ws.stats.errs.Load())
	if n := ws.evidence.late.Load(); n > 0 {
		text += fmt.Sprintf(" · 晚到 %d", n)
	}
	if r := currentRecorder(); r != nil && r.Dropped.Load() > 0 {
		text += fmt.Sprintf(" · 报文记录丢了 %d 条", r.Dropped.Load())
	}
	ws.status.SetText(text + " · RTT " + rtt + pts + updateStatus())
}
