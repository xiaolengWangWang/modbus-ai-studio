package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
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
	ws.saveConnPrefs()
	ws.connecting = true
	ws.connErr = ""
	ws.setInputsEnabled(false)
	ws.timeoutE.Disable()
	ws.connBtn.Disable()
	ws.connBtn.SetText("连接中…")
	ws.refreshStatus()
	go func() {
		s, err := ws.open(cfg)
		uiDo(func() {
			ws.connecting = false
			ws.connBtn.Enable()
			ws.timeoutE.Enable()
			if ws.closed {
				if s != nil {
					s.close()
				}
				return
			}
			if err != nil {
				ws.connErr = dialErrText(err)
				ws.connBtn.SetText("连接")
				ws.setInputsEnabled(true)
				ws.logConnectFail(cfg, err)
				ws.refreshStatus()
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
			if l := s.link.Load(); l != nil && l.addrs != "" {
				// 连接报文：记下本机端口，断开时能和设备侧、防火墙的日志或抓包对上
				ws.addLog(logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventConnect,
					Detail: modeName[s.mode] + " · " + l.addrs}, mode: s.mode}, s.recID)
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
	s.link.Store(linkFor(t))
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
	if !ws.connecting && ws.session == nil && !ws.serialMode() && !ws.useSim.Checked {
		ws.detectBn.Enable()
	} else {
		ws.detectBn.Disable()
	}
}

// connectionState 统一顶部连接状态和底部状态栏的状态文字。
func (ws *Workspace) connectionState() (string, widget.Importance) {
	switch {
	case ws.connecting:
		return "连接中…", widget.WarningImportance
	case ws.session == nil && ws.connErr != "":
		return "连接失败", widget.DangerImportance
	case ws.session == nil:
		return "未连接", widget.MediumImportance
	case ws.session.lost != nil && ws.session.mode.Serial():
		return "串口断开", widget.DangerImportance
	case ws.session.lost != nil:
		return "重连中…", widget.WarningImportance
	default:
		return "已连接", widget.SuccessImportance
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

// 连接参数记在偏好设置里：连接时和关闭主窗口时保存，下次启动沿用。内置模拟器不记，启动时总是不勾选。
const (
	prefConnMode    = "conn.mode"
	prefConnTarget  = "conn.target"
	prefConnPort    = "conn.port"
	prefConnBaud    = "conn.baud"
	prefConnFormat  = "conn.format"
	prefConnTimeout = "conn.timeoutMs"
)

func (ws *Workspace) saveConnPrefs() {
	p := ws.app.Preferences()
	p.SetString(prefConnMode, string(protoModes[ws.proto.Selected]))
	p.SetString(prefConnTarget, strings.TrimSpace(ws.target.Text))
	p.SetString(prefConnPort, ws.port.Selected)
	p.SetString(prefConnBaud, strings.TrimSpace(ws.baud.Text))
	p.SetString(prefConnFormat, ws.frameFmt.Selected)
	p.SetInt(prefConnTimeout, int(ws.timeout.Milliseconds()))
}

func (ws *Workspace) restoreConnPrefs() {
	p := ws.app.Preferences()
	if name := protoName(modbus.Mode(p.String(prefConnMode))); name != "" {
		ws.proto.SetSelected(name)
	}
	if s := p.String(prefConnTarget); s != "" {
		ws.target.SetText(s)
	}
	if s := p.String(prefConnPort); s != "" {
		ws.port.SetSelected(s)
	}
	if s := p.String(prefConnBaud); s != "" {
		ws.baud.SetText(s)
	}
	if s := p.String(prefConnFormat); slices.Contains(ws.frameFmt.Options, s) {
		ws.frameFmt.SetSelected(s)
	}
	if t, err := parseTimeout(strconv.Itoa(p.Int(prefConnTimeout))); err == nil {
		ws.timeout = t
		ws.timeoutE.SetText(strconv.FormatInt(t.Milliseconds(), 10))
	}
}

// detectProtocol 自动识别 Modbus TCP / RTU over TCP / ASCII over TCP。识别要新建连接，已连接时先断开，识别完按结果重新连接。
func (ws *Workspace) detectProtocol() {
	if ws.connecting || ws.closed || ws.dialogOpen() {
		return
	}
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
