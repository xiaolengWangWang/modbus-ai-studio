package ui

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
)

// 测试里把退避和阈值调小，几秒内跑完。在任何 goroutine 启动前设置，不会有数据竞争。
func init() {
	reconnectDelays = []time.Duration{200 * time.Millisecond, 300 * time.Millisecond}
	idleMin = 300 * time.Millisecond
	acceptProbe = 100 * time.Millisecond
}

func startSim(t *testing.T, f simulator.Faults) (*simulator.Server, string) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	srv.SetFaults(f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

// linkWS 打开主窗口连到外部模拟器，按 defs 建读取窗口（扫描周期都为 scan）。
func linkWS(t *testing.T, addr string, scan time.Duration, starts ...uint16) (*Workspace, []*readWindow) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var wins []*readWindow
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.target.SetText(addr)
		for _, st := range starts {
			d := defaultDef()
			d.Start, d.Qty, d.Scan = st, 2, scan
			wins = append(wins, ws.addWindow(d))
		}
	})
	tap(ws.connBtn)
	t.Cleanup(func() { locked(ws.disconnect) })
	return ws, wins
}

func lost(ws *Workspace) bool { return ws.session != nil && ws.session.lost != nil }

// 连接用了一阵后被断开：自动重连，恢复轮询；断开和重连都记进报文库。
func TestReconnectAfterDrop(t *testing.T) {
	r, err := recorder.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, "p.db", nil)
	t.Cleanup(func() { SetRecorder(nil, "", nil); r.Close() })
	srv, addr := startSim(t, simulator.Faults{})
	ws, wins := linkWS(t, addr, 100*time.Millisecond, 0)
	waitFor(t, 5*time.Second, "收到数据", func() bool { return hasData(wins[0]) })

	srv.SetFaults(simulator.Faults{DisconnectOn: []simulator.AddrRange{{Start: 0, Count: 10}}})
	waitFor(t, 3*time.Second, "发现断开", func() bool { return lost(ws) })
	srv.SetFaults(simulator.Faults{})
	var tx int
	locked(func() {
		e := ws.session.losses[0]
		if e.kind != lossRandom || !strings.Contains(e.describe(), "发送 Slave 1 · 03 读保持寄存器 · 40001–40002 后连接断开") {
			t.Errorf("断开分析：%d %s", e.kind, e.describe())
		}
		wins[0].mu.Lock()
		tx = wins[0].tx
		wins[0].mu.Unlock()
	})
	waitFor(t, 5*time.Second, "重连后恢复轮询", func() bool {
		wins[0].mu.Lock()
		defer wins[0].mu.Unlock()
		return !lost(ws) && ws.session.reconnects == 1 && wins[0].tx > tx+2 && wins[0].err == nil
	})
	locked(func() {
		ws.refreshStatus()
		if !strings.Contains(ws.status.Text, "已重连 1 次") {
			t.Errorf("状态栏：%s", ws.status.Text)
		}
	})
	id := ws.session.recID
	ev, err := r.Events(id)
	if err != nil || len(ev) != 2 || ev[0].Kind != recorder.EventDisconnect || ev[1].Kind != recorder.EventReconnect {
		t.Errorf("报文库里的断开和重连：%+v %v", ev, err)
	}
}

// 设备接受连接后马上关闭（连接数已满、IP 白名单）：重连时探测出来，给出对应分析。
func TestDisconnectOnAccept(t *testing.T) {
	_, addr := startSim(t, simulator.Faults{DisconnectOnAccept: true})
	ws, wins := linkWS(t, addr, 100*time.Millisecond, 0)
	waitFor(t, 5*time.Second, "连上就断的分析", func() bool {
		return lost(ws) && ws.session.lost.kind == lossOnConnect && strings.Contains(wins[0].hintLbl.Text, "连接数可能已满")
	})
	locked(func() {
		ws.refreshStatus()
		if !strings.Contains(ws.status.Text, "连接断开") || !strings.Contains(ws.status.Text, "重连") {
			t.Errorf("状态栏：%s", ws.status.Text)
		}
	})
	// 断开期间不再轮询，不会刷满连接错误
	var n1, n2 int
	locked(func() { ws.traffic.flush(); n1 = ws.traffic.total })
	time.Sleep(400 * time.Millisecond)
	locked(func() { ws.traffic.flush(); n2 = ws.traffic.total })
	if n2-n1 > 6 {
		t.Errorf("断开期间报文从 %d 涨到 %d 条，应停止轮询", n1, n2)
	}
}

// 某条请求每次发出设备都断开、其他请求正常：指出是哪条请求，一键暂停它，其他窗口照常。
func TestRequestTriggersDisconnect(t *testing.T) {
	_, addr := startSim(t, simulator.Faults{DisconnectOn: []simulator.AddrRange{{Start: 346, Count: 2}}})
	ws, wins := linkWS(t, addr, 100*time.Millisecond, 0, 346)
	waitFor(t, 8*time.Second, "指出是哪条请求", func() bool {
		return lost(ws) && strings.Contains(wins[1].hintLbl.Text, "其他请求正常") && strings.Contains(wins[1].hintLbl.Text, "40347–40348") &&
			wins[1].actionBtn.Text == "暂停这条请求"
	})
	tap(wins[1].actionBtn)
	var n int
	waitFor(t, 5*time.Second, "暂停后重连恢复", func() bool {
		n = len(ws.session.losses)
		return !lost(ws) && wins[1].paused && hasData(wins[0])
	})
	time.Sleep(600 * time.Millisecond)
	locked(func() {
		if len(ws.session.losses) != n || lost(ws) {
			t.Errorf("暂停后不应再断开：%d → %d 次", n, len(ws.session.losses))
		}
	})
}

// 每条请求设备都断开、一次正常响应都没有：多半是协议格式不对，给“识别协议”。
func TestAllRequestsDisconnect(t *testing.T) {
	_, addr := startSim(t, simulator.Faults{DisconnectOn: []simulator.AddrRange{{Start: 0, Count: 65535}}})
	ws, wins := linkWS(t, addr, 100*time.Millisecond, 0)
	waitFor(t, 8*time.Second, "协议格式的分析", func() bool {
		return lost(ws) && strings.Contains(wins[0].hintLbl.Text, "协议格式不对") && wins[0].actionBtn.Text == "识别协议"
	})
}

// 空闲一段时间后设备断开：建议缩短扫描周期，一键改完后不再断开。
func TestIdleDisconnect(t *testing.T) {
	_, addr := startSim(t, simulator.Faults{IdleTimeout: 300 * time.Millisecond})
	ws, wins := linkWS(t, addr, time.Second, 0)
	waitFor(t, 6*time.Second, "空闲超时的分析", func() bool {
		return lost(ws) && ws.session.lost.kind == lossIdle && strings.Contains(wins[0].hintLbl.Text, "空闲超时") &&
			wins[0].actionBtn.Text == "扫描周期改为 200 ms"
	})
	tap(wins[0].actionBtn)
	var n int
	waitFor(t, 5*time.Second, "改周期后重连恢复", func() bool {
		n = len(ws.session.losses)
		return !lost(ws) && wins[0].def.Scan == 200*time.Millisecond && hasData(wins[0])
	})
	time.Sleep(time.Second)
	locked(func() {
		if len(ws.session.losses) != n {
			t.Errorf("扫描周期缩短后不应再断开：%d → %d 次", n, len(ws.session.losses))
		}
	})
}

// 串口断开不自动重连，提示检查 USB 转 485。
func TestSerialLoss(t *testing.T) {
	l := newLink()
	l.observe(modbus.Packet{Dir: modbus.DirTX, Status: modbus.StatusSent, Slave: 1, Function: 3, Time: time.Now()})
	e := classifyLoss(modbus.ModeRTU, l, "read: device not configured")
	if e.kind != lossSerial || !strings.Contains(e.describe(), "串口") {
		t.Fatalf("串口断开：%d %s", e.kind, e.describe())
	}
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.session = &session{mode: modbus.ModeRTU, lost: &e}
		dg := ws.lossDiagnosis()
		ws.session = nil
		if dg.Action != "重新连接" || !strings.Contains(dg.Hint, "USB 转 485") {
			t.Errorf("串口断开的分析：%+v", dg)
		}
	})
}
