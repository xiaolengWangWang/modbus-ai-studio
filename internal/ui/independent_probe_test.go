package ui

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

func independentProbeWorkspace(t *testing.T, faults simulator.Faults) *Workspace {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return independentProbeWorkspaceAt(t, faults, ln)
}

func probePollTx(w *readWindow) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tx
}

func independentProbeWorkspaceAt(t *testing.T, faults simulator.Faults, ln net.Listener) *Workspace {
	t.Helper()
	ws, _ := independentProbeWorkspaceServer(t, faults, ln)
	return ws
}

func independentProbeWorkspaceServer(t *testing.T, faults simulator.Faults, ln net.Listener) (*Workspace, *simulator.Server) {
	t.Helper()
	srv := simulator.NewServer(modbus.ModeTCP, 1, simulator.NewStore(32))
	srv.SetFaults(faults)
	go srv.Serve(ln)
	t.Cleanup(func() {
		_ = ln.Close()
		_ = srv.Close()
	})
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.proto.SetSelected(protoTCP)
		ws.target.SetText(ln.Addr().String())
		ws.timeoutE.SetText("100")
	})
	return ws, srv
}

// 限制真实模拟器同时只接受一个 TCP 客户端，模拟常见单连接设备。
type singleProbeListener struct {
	net.Listener
	active, accepted, rejected atomic.Int32
	acceptedNotify             chan int32
}

func (l *singleProbeListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.active.CompareAndSwap(0, 1) {
			l.rejected.Add(1)
			_ = conn.Close()
			continue
		}
		n := l.accepted.Add(1)
		if l.acceptedNotify != nil {
			select {
			case l.acceptedNotify <- n:
			default:
			}
		}
		return &singleProbeConn{Conn: conn, listener: l}, nil
	}
}

type singleProbeConn struct {
	net.Conn
	listener *singleProbeListener
	closed   sync.Once
}

func (c *singleProbeConn) Close() error {
	err := c.Conn.Close()
	c.closed.Do(func() { c.listener.active.Add(-1) })
	return err
}

// 检测遇到两个会断线的点时，整段失败后逐点定位，后续正常段仍应完成。
// 检测独占 TCP 连接，断线不能进入主会话的 connLost；结束后恢复正常轮询。
func TestIndependentPointProbeKeepsMainSessionAndContinuesPastDisconnects(t *testing.T) {
	ws := independentProbeWorkspace(t, simulator.Faults{DisconnectOn: []simulator.AddrRange{
		{Start: 1, Count: 1}, {Start: 3, Count: 1},
	}})
	var mainWindow *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Scan = 20, 1, 50*time.Millisecond
		mainWindow = ws.addWindow(d)
		ws.connect()
	})
	waitFor(t, 3*time.Second, "主连接正常读取", func() bool { return ws.session != nil && hasData(mainWindow) })
	var primary *session
	var txBefore int
	locked(func() {
		primary, txBefore = ws.session, probePollTx(mainWindow)
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "正常前点"},
			{Area: modbus.AreaHoldingRegisters, Offset: 1, Type: modbus.TypeUint16, Name: "断线点一"},
			{Area: modbus.AreaHoldingRegisters, Offset: 2, Type: modbus.TypeUint16, Name: "正常中点"},
			{Area: modbus.AreaHoldingRegisters, Offset: 3, Type: modbus.TypeUint16, Name: "断线点二"},
			{Area: modbus.AreaHoldingRegisters, Offset: 4, Type: modbus.TypeUint16, Name: "正常后点"},
			{Area: modbus.AreaHoldingRegisters, Offset: 8, Type: modbus.TypeUint16, Name: "后续段一"},
			{Area: modbus.AreaHoldingRegisters, Offset: 9, Type: modbus.TypeUint16, Name: "后续段二"},
		}))
		d, segs, ok := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
		if !ok || len(segs) != 2 || segs[0].n != 5 || segs[1].start != 8 || segs[1].n != 2 {
			t.Fatalf("连续点应形成两段：%+v", segs)
		}
		ws.runRegisterProbeMode(nil, d, probePoints, segs)
	})
	waitFor(t, 10*time.Second, "断线点后继续完成全部点位检测", func() bool { return !ws.probeRunning })
	waitFor(t, 2*time.Second, "原主会话恢复正常轮询", func() bool {
		return ws.session == primary && primary.lost == nil && probePollTx(mainWindow) > txBefore && !mainWindow.paused && hasData(mainWindow)
	})
	locked(func() {
		if ws.session != primary {
			t.Error("独立点位检测不能替换主会话对象")
		}
		if primary.lost != nil || len(primary.losses) != 0 || primary.reconnects != 0 {
			t.Errorf("检测断线不能触发主会话 connLost：losses=%d, reconnects=%d, lost=%v", len(primary.losses), primary.reconnects, primary.lost != nil)
		}
		report := overlayText(ws)
		for _, want := range []string{
			"7 个点：可读 5", "读取出错 2", "未检测 0",
			"40002 断线点一（UINT16）：读取出错", "40004 断线点二（UINT16）：读取出错",
			"40001–40005（5 个点）：整段读不通", "40009–40010（2 个点）：整段可读",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("逐点定位后应完成后续好点，报告缺少 %q：%s", want, report)
			}
		}
	})
}

// 只有连接参数和点表时，也能从实际检测对话框启动独立 TCP 检测。
func TestIndependentPointProbeStartsWithoutMainConnection(t *testing.T) {
	ws := independentProbeWorkspace(t, simulator.Faults{})
	locked(func() {
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "温度"},
			{Area: modbus.AreaHoldingRegisters, Offset: 1, Type: modbus.TypeUint16, Name: "压力"},
		}))
		ws.registerProbeDialog()
		var mode *widget.Select
		var entries []*widget.Entry
		walk(ws.win.Canvas().Overlays().Top(), func(o fyne.CanvasObject) {
			if s, ok := o.(*widget.Select); ok && len(s.Options) == len(probeModeNames) && s.Options[0] == probeModeNames[0] {
				mode = s
			}
			if e, ok := o.(*widget.Entry); ok {
				entries = append(entries, e)
			}
		})
		if mode == nil {
			t.Fatalf("没有主连接时仍应打开检测对话框：%s", overlayText(ws))
		}
		if mode.Selected != probeModeNames[probePoints] || len(entries) != 3 || !entries[1].Disabled() || !entries[2].Disabled() {
			t.Fatalf("已有点表时应默认按点表分段并禁用地址/数量：mode=%q, entries=%d", mode.Selected, len(entries))
		}
		buttons := findButtons(ws.win.Canvas().Overlays().Top(), "开始检测")
		if len(buttons) != 1 {
			t.Fatal("点表检测必须提供开始检测按钮")
		}
		test.Tap(buttons[0])
		if !ws.probeRunning {
			t.Fatalf("未连接主会话时应启动独立点位检测：%s", overlayText(ws))
		}
	})
	waitFor(t, 3*time.Second, "未连接主会话的独立点位检测完成", func() bool { return !ws.probeRunning })
	locked(func() {
		if ws.session != nil {
			t.Error("独立点位检测不应创建主会话")
		}
		report := overlayText(ws)
		if !strings.Contains(report, "2 个点：可读 2") || !strings.Contains(report, "40001–40002（2 个点）：整段可读") {
			t.Errorf("独立连接应读完连续点位：%s", report)
		}
	})
}

func TestIndependentPointProbeReleasesMainSocketForSingleClientDevice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := &singleProbeListener{Listener: ln}
	ws := independentProbeWorkspaceAt(t, simulator.Faults{}, limited)
	var mainWindow *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Scan = 20, 1, 50*time.Millisecond
		mainWindow = ws.addWindow(d)
		ws.connect()
	})
	waitFor(t, 3*time.Second, "单客户端设备主连接正常读取", func() bool { return ws.session != nil && hasData(mainWindow) })
	var primary *session
	var txBefore int
	locked(func() {
		primary, txBefore = ws.session, probePollTx(mainWindow)
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "温度"},
			{Area: modbus.AreaHoldingRegisters, Offset: 1, Type: modbus.TypeUint16, Name: "压力"},
		}))
		d, segs, _ := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
		ws.runRegisterProbeMode(nil, d, probePoints, segs)
	})
	waitFor(t, 5*time.Second, "单客户端设备点位检测完成", func() bool { return !ws.probeRunning })
	waitFor(t, 2*time.Second, "单客户端检测后主连接继续正常读取", func() bool {
		return ws.session == primary && primary.lost == nil && probePollTx(mainWindow) > txBefore && !mainWindow.paused && hasData(mainWindow)
	})
	locked(func() {
		if ws.session != primary || primary.lost != nil || len(primary.losses) != 0 {
			t.Error("单客户端检测不能丢失或触发主会话断线")
		}
		if report := overlayText(ws); !strings.Contains(report, "2 个点：可读 2") {
			t.Errorf("释放主 socket 后应完成独立检测：%s", report)
		}
	})
	if accepted := limited.accepted.Load(); accepted < 2 {
		t.Errorf("检测应释放主 socket 并建立自己的 TCP 连接，设备仅接受了 %d 个连接", accepted)
	}
	if rejected := limited.rejected.Load(); rejected != 0 {
		t.Errorf("检测不能同时占用单客户端设备：设备拒绝了 %d 个并发连接", rejected)
	}
}

func TestIndependentPointProbeCancellationWhileWaitingForConnection(t *testing.T) {
	for _, action := range []string{"stop", "close-window"} {
		t.Run(action, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			limited := &singleProbeListener{Listener: ln}
			ws := independentProbeWorkspaceAt(t, simulator.Faults{DisconnectOnAccept: true}, limited)
			locked(func() {
				ws.setPoints(newPointTable([]point{{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "温度"}}))
				d, segs, _ := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
				ws.runRegisterProbeMode(nil, d, probePoints, segs)
			})
			waitFor(t, 2*time.Second, "无主连接的检测正在自动重连", func() bool {
				return ws.probeRunning && limited.accepted.Load() >= 2 && strings.Contains(overlayText(ws), "等待自动重连")
			})
			locked(func() {
				if action == "close-window" {
					ws.win.Close()
					return
				}
				buttons := findButtons(ws.win.Canvas().Overlays().Top(), "停止")
				if len(buttons) != 1 {
					t.Fatal("等待检测重连时必须可以停止")
				}
				test.Tap(buttons[0])
			})
			waitFor(t, time.Second, "检测取消清理并释放 socket", func() bool { return !ws.probeRunning && limited.active.Load() == 0 })
			locked(func() {
				if ws.session != nil {
					t.Error("取消无主连接的检测不能留下主会话")
				}
				if action == "stop" {
					if ws.connBtn.Disabled() || ws.timeoutE.Disabled() || !strings.Contains(overlayText(ws), "检测已停止") {
						t.Errorf("停止后应恢复连接操作并保留结果：%s", overlayText(ws))
					}
				} else if !ws.closed {
					t.Error("关闭主窗口应结束工作区")
				}
			})
			accepted := limited.accepted.Load()
			time.Sleep(200 * time.Millisecond)
			if limited.accepted.Load() != accepted || limited.active.Load() != 0 {
				t.Error("取消后不能继续建立检测连接或保留 socket")
			}
		})
	}
}

func TestIndependentPointProbeTakesOverInFlightMainReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := &singleProbeListener{Listener: ln, acceptedNotify: make(chan int32, 8)}
	ws, srv := independentProbeWorkspaceServer(t, simulator.Faults{}, limited)
	var mainWindow *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Scan = 20, 1, 50*time.Millisecond
		mainWindow = ws.addWindow(d)
		ws.connect()
	})
	waitFor(t, 3*time.Second, "主连接正常读取", func() bool { return ws.session != nil && hasData(mainWindow) })
	srv.SetFaults(simulator.Faults{DisconnectOn: []simulator.AddrRange{{Start: 20, Count: 1}}})
	waitFor(t, 2*time.Second, "主连接等待重连", func() bool { return lost(ws) })
	srv.SetFaults(simulator.Faults{})
	for {
		select {
		case n := <-limited.acceptedNotify:
			if n < 2 {
				continue
			}
		case <-time.After(2 * time.Second):
			t.Fatal("主连接未开始 TCP 重连")
		}
		break
	}
	var primary *session
	var lossesBefore, reconnectsBefore, txBefore int
	locked(func() {
		if !lost(ws) {
			t.Fatal("应在主重连 socket 尚未移交会话时启动检测")
		}
		primary = ws.session
		lossesBefore, reconnectsBefore, txBefore = len(primary.losses), primary.reconnects, probePollTx(mainWindow)
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "温度"},
			{Area: modbus.AreaHoldingRegisters, Offset: 1, Type: modbus.TypeUint16, Name: "压力"},
		}))
		d, segs, _ := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
		ws.runRegisterProbeMode(nil, d, probePoints, segs)
	})
	waitFor(t, 3*time.Second, "主连接重连中的独立检测完成", func() bool { return !ws.probeRunning })
	waitFor(t, 2*time.Second, "接管重连后原主会话恢复读取", func() bool {
		return ws.session == primary && primary.lost == nil && probePollTx(mainWindow) > txBefore && !mainWindow.paused && hasData(mainWindow)
	})
	locked(func() {
		if ws.session != primary || primary.lost != nil || len(primary.losses) != lossesBefore || primary.reconnects != reconnectsBefore {
			t.Error("检测应接管重连并恢复原主会话，不能增加主会话损失或常规重连")
		}
		if report := overlayText(ws); !strings.Contains(report, "2 个点：可读 2") {
			t.Errorf("独占连接后应读完正常点位：%s", report)
		}
	})
	if rejected := limited.rejected.Load(); rejected != 0 {
		t.Errorf("主重连和独立检测不能并发占用 TCP：设备拒绝了 %d 个连接", rejected)
	}
}
