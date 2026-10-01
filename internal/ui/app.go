// Package ui 是 Fyne 桌面界面：连接栏、Modbus Poll 式读取窗口、通信报文、报文 / 寄存器解析、
// 自定义请求和写入验证。每个主窗口是一个独立的工作区（连接、读取窗口、报文互不影响），
// 可以同时开多个主窗口调试多台设备。界面只负责输入、展示和交互，协议全部交给 internal/modbus。
package ui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

const (
	protoTCP      = "Modbus TCP"
	protoRTUTCP   = "RTU over TCP"
	protoASCIITCP = "ASCII over TCP"
	protoRTU      = "RTU 串口"
	protoASCII    = "ASCII 串口"
)

var (
	protoNames = []string{protoTCP, protoRTUTCP, protoASCIITCP, protoRTU, protoASCII}
	protoModes = map[string]modbus.Mode{protoTCP: modbus.ModeTCP, protoRTUTCP: modbus.ModeRTUOverTCP,
		protoASCIITCP: modbus.ModeASCIIOverTCP, protoRTU: modbus.ModeRTU, protoASCII: modbus.ModeASCII}
	// RTU 规定 8 位数据位；ASCII 常用 7E1，也有设备用 8 位
	rtuFormats   = []string{"8N1", "8E1", "8O1", "8N2"}
	asciiFormats = []string{"7E1", "7O1", "7N2", "8N1", "8E1", "8O1", "8N2"}
)

// protoName 返回模式在协议下拉框里的名称。
func protoName(m modbus.Mode) string {
	for name, pm := range protoModes {
		if pm == m {
			return name
		}
	}
	return ""
}

// serialMode 表示当前选的是串口协议（RTU 串口或 ASCII 串口）。
func (ws *Workspace) serialMode() bool { return protoModes[ws.proto.Selected].Serial() }

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

// 进程内的主窗口编号和串口占用。一个串口同一时刻只能被一个连接打开。
var (
	winSeq  atomic.Int32
	portsMu sync.Mutex
	ports   = map[string]int{} // 串口 → 占用它的主窗口编号
)

func claimPort(port string, no int) error {
	portsMu.Lock()
	defer portsMu.Unlock()
	if owner, ok := ports[port]; ok && owner != no {
		return fmt.Errorf("%s 已在窗口 %d 中打开。一个串口同一时刻只能被一个连接使用；"+
			"要调试同一条总线上的多台设备，在同一个窗口里新建读取窗口，分别设置 Slave ID", port, owner)
	}
	ports[port] = no
	return nil
}

func releasePort(port string) {
	portsMu.Lock()
	delete(ports, port)
	portsMu.Unlock()
}

type stats struct{ polls, ok, errs, rtt atomic.Int64 }

func (s *stats) poll(ok bool) {
	s.polls.Add(1)
	if ok {
		s.ok.Add(1)
	} else {
		s.errs.Add(1)
	}
}

func (s *stats) reset() {
	for _, v := range []*atomic.Int64{&s.polls, &s.ok, &s.errs, &s.rtt} {
		v.Store(0)
	}
}

// session 是一次连接。只在 UI 线程读写 Workspace.session。
type session struct {
	client *modbus.Client
	mode   modbus.Mode
	desc   string
	port   string // 串口名，关闭时释放占用
	ctx    context.Context
	cancel context.CancelFunc
	sim    *simulator.Server
	simEnd context.CancelFunc
	recID  int64 // 报文数据库里的会话 ID，0 表示没有记录

	// 连接保持（link.go）。link 由收发回调读，其余只在界面线程读写
	target     string // TCP 类的重连地址
	link       atomic.Pointer[linkState]
	lost       *lossEvent // 断开还没恢复时不为 nil
	losses     []lossEvent
	reconnects int
	tries      int // 本轮重连已尝试次数，仅显示
	backoff    int // 下次重连从第几档退避开始
	retryAt    time.Time
	dialErr    string
}

func (s *session) close() {
	s.cancel()
	s.client.Close()
	if r := currentRecorder(); r != nil && s.recID != 0 {
		r.EndSession(s.recID)
	}
	if s.sim != nil {
		s.simEnd()
		s.sim.Close()
	}
	if s.port != "" {
		releasePort(s.port)
	}
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

	session    *session
	connecting bool
	recID      atomic.Int64 // 正在记录的会话 ID，收发回调里读；0 表示不记录
	points     pointTable   // 本窗口的点表，初始为空
	readOnly   bool         // 只读模式，禁止一切写入（readonly.go）
	roItem     *fyne.MenuItem
	path       string // 工作区文件，未保存时为空
	timeout    time.Duration
	windows    []*readWindow
	nextWin    int
	tiles      *fyne.Container
	traffic    *trafficPanel
	inspect    *inspector
	status     *widget.Label
	stats      stats
	evidence   evidence
	tools      []fyne.Window // 自定义请求等工具窗口，主窗口关闭时一起关闭
	done       chan struct{}
	closed     bool
}

// Open 新建一个主窗口并显示。主窗口初始为空：没有读取窗口，也不自动连接。
// 换热站示例在“读取 → 打开换热站示例”里。
func Open(app fyne.App, version string) *Workspace {
	return open(app, version, int(winSeq.Add(1)))
}

func open(app fyne.App, version string, no int) *Workspace {
	w := app.NewWindow("Modbus AI Studio " + version)
	w.Resize(fyne.NewSize(1280, 820))
	ws := newWorkspace(app, w, version, no)
	w.Show()
	return ws
}

func newWorkspace(app fyne.App, win fyne.Window, version string, no int) *Workspace {
	ws := &Workspace{Version: version, app: app, win: win, no: no, timeout: time.Second, done: make(chan struct{}), points: pointTable{}}
	ws.traffic = newTrafficPanel(ws)
	ws.inspect = newInspector(ws)
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

// loadDemo 按换热站示例点表打开三个读取窗口；勾选了内置模拟器且未连接时顺便连接，马上能看到数据。
// 窗口 3（40601–40604）的模拟器应答慢于默认超时，用来演示错误自动分析。
func (ws *Workspace) loadDemo() {
	ws.setPoints(demoPoints())
	for _, d := range []readDef{
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 20, Scan: time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 346, Qty: 8, Scan: time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 600, Qty: 4, Scan: 5 * time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
	} {
		ws.addWindow(d)
	}
	if ws.session == nil && ws.useSim.Checked && !ws.serialMode() {
		ws.connect()
	}
}

func defaultDef() readDef {
	return readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 10, Scan: time.Second,
		Kind: kindSigned, Order: modbus.OrderABCD, Rows: 10}
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

	bottom := container.NewHSplit(ws.traffic.root, ws.inspect.root)
	bottom.Offset = 0.56
	main := container.NewVSplit(ws.tiles, bottom)
	main.Offset = 0.7
	return container.NewBorder(
		container.NewVBox(bar, widget.NewSeparator()),
		container.NewVBox(widget.NewSeparator(), ws.status),
		nil, nil, main)
}

func (ws *Workspace) setMenu() {
	short := func(k fyne.KeyName) fyne.Shortcut {
		return &desktop.CustomShortcut{KeyName: k, Modifier: fyne.KeyModifierShortcutDefault}
	}
	ws.roItem = fyne.NewMenuItem("只读模式（禁止写入）", func() { ws.setReadOnly(!ws.readOnly) })
	newWin := fyne.NewMenuItem("新建窗口", func() { ws.openNew() })
	newWin.Shortcut = short(fyne.KeyN)
	newRead := fyne.NewMenuItem("新建读取窗口", ws.addReadWindow)
	newRead.Shortcut = short(fyne.KeyT)
	custom := fyne.NewMenuItem("自定义请求…", ws.openRequestTool)
	custom.Shortcut = short(fyne.KeyR)
	conn := fyne.NewMenuItem("连接 / 断开", ws.toggleConnect)
	conn.Shortcut = short(fyne.KeyK)
	closeWin := fyne.NewMenuItem("关闭窗口", ws.win.Close)
	closeWin.Shortcut = short(fyne.KeyW)
	openWs := fyne.NewMenuItem("打开工作区…", ws.openWorkspace)
	openWs.Shortcut = short(fyne.KeyO)
	saveWs := fyne.NewMenuItem("保存工作区", ws.saveWorkspace)
	saveWs.Shortcut = short(fyne.KeyS)
	items := []*fyne.MenuItem{newWin, closeWin, openWs, saveWs, newRead, custom, conn}
	ws.win.SetMainMenu(fyne.NewMainMenu(
		fyne.NewMenu("文件", newWin, openWs, saveWs, fyne.NewMenuItem("工作区另存为…", ws.saveWorkspaceAs), fyne.NewMenuItemSeparator(), closeWin),
		fyne.NewMenu("连接", conn, fyne.NewMenuItem("识别协议", ws.detectProtocol), fyne.NewMenuItem("扫描串口参数…", ws.scanSerialDialog),
			fyne.NewMenuItemSeparator(), ws.roItem),
		fyne.NewMenu("读取", newRead, fyne.NewMenuItem("导入点表 CSV…", ws.importPoints), fyne.NewMenuItem("打开换热站示例", ws.loadDemo), fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("全部暂停", func() { ws.pauseAll(true) }),
			fyne.NewMenuItem("全部继续", func() { ws.pauseAll(false) })),
		fyne.NewMenu("调试", custom, fyne.NewMenuItem("扫描从站地址…", ws.scanSlavesDialog), fyne.NewMenuItem("读取诊断计数器…", ws.diagCountersDialog),
			fyne.NewMenuItemSeparator(), fyne.NewMenuItem("历史报文…", ws.openHistory), fyne.NewMenuItem("清空通信报文", ws.traffic.clear)),
	))
	// macOS 的原生菜单自己处理快捷键；其他平台的菜单栏只显示快捷键，需要另外注册
	if runtime.GOOS != "darwin" {
		for _, it := range items {
			it := it
			ws.win.Canvas().AddShortcut(it.Shortcut, func(fyne.Shortcut) { it.Action() })
		}
	}
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

// fixed 给输入框一个固定宽度，HBox 里的 Entry 默认会缩成最小宽度。
func fixed(w float32, o fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(w, o.MinSize().Height), o)
}

// setPorts 更新串口列表，列表没变时不动，避免下拉框闪烁；选中的串口被拔掉时清空选择。
func (ws *Workspace) setPorts(list []string) {
	if strings.Join(list, "\n") != strings.Join(ws.port.Options, "\n") {
		ws.port.SetOptions(list)
	}
	if ws.port.Selected != "" && !slices.Contains(list, ws.port.Selected) {
		ws.port.ClearSelected()
	}
}

func (ws *Workspace) updateDetectBtn() {
	if ws.detectBn == nil {
		return
	}
	if !ws.serialMode() && !ws.useSim.Checked {
		ws.detectBn.Enable()
	} else {
		ws.detectBn.Disable()
	}
}

// ---------- 读取窗口管理 ----------

func (ws *Workspace) addWindow(d readDef) *readWindow {
	ws.nextWin++
	w := newReadWindow(ws, ws.nextWin, d)
	ws.windows = append(ws.windows, w)
	ws.relayout()
	w.start()
	return w
}

// addReadWindow 按最后一个窗口的 Slave 和功能码新建读取窗口，并直接打开读取定义。
func (ws *Workspace) addReadWindow() {
	d := defaultDef()
	if n := len(ws.windows); n > 0 {
		last := ws.windows[n-1].def
		d.Slave, d.Function = last.Slave, last.Function
	}
	w := ws.addWindow(d)
	ws.showDefinition(w)
}

func (ws *Workspace) removeWindow(w *readWindow) {
	w.halt()
	for i, x := range ws.windows {
		if x == w {
			ws.windows = append(ws.windows[:i], ws.windows[i+1:]...)
			break
		}
	}
	if ws.inspect.src == w {
		ws.inspect.clear()
	}
	ws.relayout()
}

// redefine 修改读取定义后重新开始轮询，诊断里的一键处理也走这里。
func (ws *Workspace) redefine(w *readWindow, change func(*readDef)) {
	d := w.def
	change(&d)
	if err := d.validate(); err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.applyDef(w, d)
}

func (ws *Workspace) applyDef(w *readWindow, d readDef) {
	w.halt()
	w.setDef(d)
	ws.relayout()
	w.start()
}

func (ws *Workspace) pauseAll(pause bool) {
	for _, w := range ws.windows {
		w.setPaused(pause)
	}
}

// relayout 把读取窗口平铺：列数取 ⌈√n⌉，靠后的列多放一个；分隔条初始位置按各窗口的内容大小分配。
func (ws *Workspace) relayout() {
	var obj fyne.CanvasObject
	if len(ws.windows) == 0 {
		box := container.NewVBox(widget.NewLabel("没有读取窗口。点右上角“读取窗口”新建，或用“读取”菜单里的“打开换热站示例”。"))
		if recent := ws.recent(); len(recent) > 0 {
			box.Add(widget.NewLabelWithStyle("最近打开的工作区", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
			for _, p := range recent {
				b := widget.NewButtonWithIcon(filepath.Base(p), theme.FolderOpenIcon(), func() { ws.openWorkspaceFile(p) })
				b.Alignment = widget.ButtonAlignLeading
				b.Importance = widget.LowImportance
				box.Add(b)
			}
		}
		obj = container.NewCenter(box)
	} else {
		obj = tile(ws.windows)
	}
	ws.tiles.Objects = []fyne.CanvasObject{obj}
	ws.tiles.Refresh()
}

func tile(wins []*readWindow) fyne.CanvasObject {
	n := len(wins)
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	base, extra := n/cols, n%cols
	var columns []fyne.CanvasObject
	var widths []float32
	i := 0
	for c := 0; c < cols; c++ {
		k := base
		if c >= cols-extra {
			k++
		}
		var objs []fyne.CanvasObject
		var heights []float32
		var width float32
		for _, w := range wins[i : i+k] {
			objs = append(objs, w.root)
			heights = append(heights, w.prefHeight())
			width = max(width, w.prefWidth())
		}
		i += k
		columns = append(columns, chain(false, objs, heights))
		widths = append(widths, width)
	}
	return chain(true, columns, widths)
}

func chain(horizontal bool, objs []fyne.CanvasObject, weights []float32) fyne.CanvasObject {
	if len(objs) == 1 {
		return objs[0]
	}
	var total float32
	for _, w := range weights {
		total += w
	}
	rest := chain(horizontal, objs[1:], weights[1:])
	var s *container.Split
	if horizontal {
		s = container.NewHSplit(objs[0], rest)
	} else {
		s = container.NewVSplit(objs[0], rest)
	}
	s.Offset = float64(weights[0] / total)
	return s
}

// ---------- 连接 ----------

// OnPacket 接收全部收发记录，分发给通信报文、状态栏和诊断证据（Packet Recorder 的界面一侧）。
func (ws *Workspace) OnPacket(p modbus.Packet) {
	if p.Dir == modbus.DirRX && p.Status == modbus.StatusSuccess {
		ws.stats.rtt.Store(int64(p.RTT))
	}
	ws.evidence.observe(p)
	ws.traffic.push(p)
	if id := ws.recID.Load(); id != 0 {
		if r := currentRecorder(); r != nil {
			r.Record(id, p)
		}
	}
}

func (ws *Workspace) toggleConnect() {
	if ws.session != nil {
		ws.disconnect()
		return
	}
	ws.connect()
}

func (ws *Workspace) reconnect() {
	if ws.session != nil {
		ws.disconnect()
	}
	ws.connect()
}

type connConfig struct {
	mode    modbus.Mode
	target  string
	useSim  bool
	serial  transport.SerialConfig
	timeout time.Duration
}

func (ws *Workspace) connConfig() (connConfig, error) {
	cfg := connConfig{mode: protoModes[ws.proto.Selected], target: strings.TrimSpace(ws.target.Text), useSim: ws.useSim.Checked}
	t, err := parseTimeout(ws.timeoutE.Text)
	if err != nil {
		return cfg, err
	}
	cfg.timeout = t
	if cfg.mode.Serial() {
		baud, err := strconv.Atoi(strings.TrimSpace(ws.baud.Text))
		if err != nil || baud < 300 || baud > 4000000 {
			return cfg, errors.New("波特率应为 300–4000000 的整数")
		}
		cfg.serial = serialConfig(ws.port.Selected, baud, ws.frameFmt.Selected)
		if cfg.serial.Port == "" {
			return cfg, errors.New("请选择串口。插上 USB 转 485 后几秒内会自动出现在列表里")
		}
	} else if !cfg.useSim {
		if _, _, err := net.SplitHostPort(cfg.target); err != nil {
			return cfg, errors.New("目标地址应为 IP:端口，例如 192.168.1.100:502")
		}
	}
	return cfg, nil
}

func parseTimeout(s string) (time.Duration, error) {
	ms, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || ms < 50 || ms > 60000 {
		return 0, errors.New("超时应为 50–60000 ms")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (ws *Workspace) connect() {
	if ws.connecting || ws.session != nil {
		return // 菜单快捷键在连接过程中也能触发，不能开出第二条连接
	}
	cfg, err := ws.connConfig()
	if err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.timeout = cfg.timeout
	ws.connecting = true
	ws.connBtn.Disable()
	ws.connBtn.SetText("连接中…")
	go func() {
		s, err := ws.open(cfg)
		uiDo(func() {
			ws.connecting = false
			ws.connBtn.Enable()
			if ws.closed {
				if s != nil {
					s.close()
				}
				return
			}
			if err != nil {
				ws.connBtn.SetText("连接")
				dialog.ShowError(err, ws.win)
				return
			}
			ws.session = s
			if r := currentRecorder(); r != nil {
				if id, err := r.StartSession(s.mode, s.desc, ws.no); err == nil {
					s.recID = id
					ws.recID.Store(id)
				}
			}
			ws.connBtn.SetText("断开")
			ws.connBtn.SetIcon(theme.LogoutIcon())
			ws.setInputsEnabled(false)
			ws.traffic.clear()
			ws.stats.reset()
			ws.evidence.reset()
			for _, w := range ws.windows {
				w.reset()
				w.start()
			}
			ws.refreshTitle()
			ws.refreshStatus()
		})
	}()
}

func (ws *Workspace) disconnect() {
	s := ws.session
	if s == nil {
		return
	}
	ws.session = nil
	ws.recID.Store(0)
	for _, w := range ws.windows {
		w.halt()
	}
	s.close()
	ws.connBtn.SetText("连接")
	ws.connBtn.SetIcon(theme.LoginIcon())
	ws.setInputsEnabled(true)
	for _, w := range ws.windows {
		w.refresh()
	}
	ws.refreshTitle()
	ws.refreshStatus()
}

// open 建立连接；勾选内置模拟器时先在本机随机端口启动换热站模拟器，
// 其中 40601–40604 约 1013 ms 才应答，用来演示超时、晚到响应和自动诊断。
func (ws *Workspace) open(cfg connConfig) (*session, error) {
	s := &session{mode: cfg.mode}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	fail := func(err error) (*session, error) {
		s.cancel()
		if s.sim != nil {
			s.simEnd()
			s.sim.Close()
		}
		if s.port != "" {
			releasePort(s.port)
		}
		return nil, fmt.Errorf("连接失败：%w", err)
	}
	target := cfg.target
	if cfg.useSim && !cfg.mode.Serial() {
		store := simulator.HeatStation()
		srv := simulator.NewServer(cfg.mode, 0, store)
		srv.SetFaults(simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 600, Count: 4}, Delay: 1013 * time.Millisecond}}})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fail(err)
		}
		simCtx, simEnd := context.WithCancel(context.Background())
		go simulator.RunHeatStation(simCtx, store)
		go srv.Serve(ln)
		s.sim, s.simEnd, target = srv, simEnd, ln.Addr().String()
	}
	var t modbus.Transport
	var err error
	if cfg.mode.Serial() {
		if err := claimPort(cfg.serial.Port, ws.no); err != nil {
			return nil, err
		}
		s.port = cfg.serial.Port
		t, err = transport.OpenSerial(cfg.serial)
		s.desc = fmt.Sprintf("%s %d %d%s%d", cfg.serial.Port, cfg.serial.BaudRate, cfg.serial.DataBits, cfg.serial.Parity, cfg.serial.StopBits)
	} else {
		t, err = transport.DialTCP(context.Background(), target, 3*time.Second)
		s.desc, s.target = target, target
		if s.sim != nil {
			s.desc += "（内置模拟器）"
		}
	}
	if err != nil {
		return fail(err)
	}
	s.link.Store(newLink())
	s.client = modbus.NewClient(t, ws.clientOptions(s, cfg.timeout))
	return s, nil
}

func (ws *Workspace) setInputsEnabled(on bool) {
	for _, w := range []fyne.Disableable{ws.proto, ws.port, ws.baud, ws.frameFmt, ws.useSim} {
		if on {
			w.Enable()
		} else {
			w.Disable()
		}
	}
	if on && !ws.useSim.Checked {
		ws.target.Enable()
	} else {
		ws.target.Disable()
	}
	if on {
		ws.updateDetectBtn()
	} else {
		ws.detectBn.Disable()
	}
}

// applyTimeoutEntry 在输入框里回车后修改超时；已连接时立即生效，不用重新连接。
func (ws *Workspace) applyTimeoutEntry() {
	t, err := parseTimeout(ws.timeoutE.Text)
	if err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.setTimeout(t)
}

func (ws *Workspace) setTimeout(t time.Duration) {
	ws.timeout = t
	ws.timeoutE.SetText(strconv.FormatInt(t.Milliseconds(), 10))
	if ws.session != nil {
		ws.session.client.SetTimeout(t)
	}
	ws.refreshStatus()
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
		ws.status.SetText(fmt.Sprintf("○ 未连接 · Modbus AI Studio %s · 窗口 %d%s", ws.Version, ws.no, pts))
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
	ws.status.SetText(text + " · RTT " + rtt + pts)
}

// detectProtocol 自动识别 Modbus TCP / RTU over TCP / ASCII over TCP。识别要新建连接，已连接时先断开，识别完按结果重新连接。
func (ws *Workspace) detectProtocol() {
	if ws.serialMode() || ws.useSim.Checked {
		dialog.ShowInformation("识别协议", "协议识别用于只知道 IP 和端口的网络设备。内置模拟器和串口不需要识别。", ws.win)
		return
	}
	target := strings.TrimSpace(ws.target.Text)
	if _, _, err := net.SplitHostPort(target); err != nil {
		dialog.ShowError(errors.New("目标地址应为 IP:端口，例如 192.168.1.100:502"), ws.win)
		return
	}
	slaves := []byte{1, 255}
	if len(ws.windows) > 0 && ws.windows[0].def.Slave != 1 {
		slaves = []byte{ws.windows[0].def.Slave, 1, 255}
	}
	reconnect := ws.session != nil
	ws.disconnect()
	timeout := ws.timeout
	progress := dialog.NewCustomWithoutButtons("识别协议", container.NewVBox(
		widget.NewLabel(fmt.Sprintf("正在向 %s 依次发送 Modbus TCP、RTU over TCP、ASCII over TCP 格式的请求…", target)),
		widget.NewProgressBarInfinite()), ws.win)
	progress.Show()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dial := func(ctx context.Context) (modbus.Transport, error) {
			return transport.DialTCP(ctx, target, 3*time.Second)
		}
		res, err := detect.Detect(ctx, dial, detect.Options{Slaves: slaves, Timeout: timeout, Observer: ws})
		uiDo(func() {
			progress.Hide()
			if ws.closed {
				return
			}
			var lines []string
			for _, a := range res.Attempts {
				r := "有响应"
				if a.Err != nil {
					r = errSummary(a.Err)
				}
				lines = append(lines, fmt.Sprintf("%s · Slave %d：%s", modeName[a.Mode], a.Slave, r))
			}
			if err != nil {
				dialog.ShowError(fmt.Errorf("没有识别出协议：\n%s\n\n检查 IP、端口和 Slave ID；串口服务器还要确认串口侧参数", strings.Join(lines, "\n")), ws.win)
				return
			}
			ws.proto.SetSelected(protoName(res.Mode))
			msg := fmt.Sprintf("识别为 %s（Slave %d 应答）", modeName[res.Mode], res.Slave)
			if res.ByException {
				msg += "\n设备返回的是异常响应，协议和 Slave ID 是对的，地址 40001 不可读。"
			}
			dialog.ShowInformation("识别协议", msg+"\n\n"+strings.Join(lines, "\n"), ws.win)
			if reconnect {
				ws.connect()
			}
		})
	}()
}

// errSummary 是错误的短说明，用在识别结果、探测结果等列表里。
func errSummary(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("异常 %02X %s", byte(ex.Code), ex.Code.Name())
	}
	switch {
	case errors.Is(err, modbus.ErrTimeout):
		return "超时"
	case errors.Is(err, modbus.ErrCRC):
		return "CRC 错误"
	case errors.Is(err, modbus.ErrConnection):
		return "连接错误"
	}
	return err.Error()
}
