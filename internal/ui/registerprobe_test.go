package ui

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

func registerProbeClient(t *testing.T, mode modbus.Mode, faults simulator.Faults, observer modbus.Observer, storeSize ...int) *modbus.Client {
	t.Helper()
	size := 4
	if len(storeSize) > 0 {
		size = storeSize[0]
	}
	srv := simulator.NewServer(mode, 1, simulator.NewStore(size))
	srv.SetFaults(faults)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Correctness cases allow race instrumentation overhead; timeout cases set
	// their shorter response budget explicitly below.
	c := modbus.NewClient(conn, modbus.Options{Mode: mode, Timeout: 250 * time.Millisecond, Guard: 100 * time.Millisecond, Observer: observer})
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRegisterProbeFourAreasAcrossProtocols(t *testing.T) {
	for _, mode := range []modbus.Mode{modbus.ModeTCP, modbus.ModeRTUOverTCP, modbus.ModeASCIIOverTCP} {
		for _, fn := range readFuncs {
			t.Run(fmt.Sprintf("%s/%s", mode, fn), func(t *testing.T) {
				var sent []modbus.Packet
				c := registerProbeClient(t, mode, simulator.Faults{}, modbus.ObserverFunc(func(p modbus.Packet) {
					if p.Dir == modbus.DirTX {
						sent = append(sent, p)
					}
				}))
				d := readDef{Slave: 1, Function: fn, Start: 2, Qty: 4}
				res, err := probe(context.Background(), c, d, func(int) {})
				if err != nil || fmt.Sprint(res) != "[1 1 -1 -1]" || len(sent) > 7 {
					t.Fatalf("Must identify the readable range and continue past illegal addresses: %v, sent %d, %v", res, len(sent), err)
				}
				for _, p := range sent {
					if p.Function != fn || p.Count < 1 || int(p.Count) > d.maxQty() {
						t.Fatalf("Probe must only read the selected area within protocol quantity limits: %+v", p)
					}
				}
			})
		}
	}
}

func TestRegisterProbeCancellationKeepsPartialResults(t *testing.T) {
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, nil, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := probe(ctx, c, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 130}, func(int) { cancel() })
	if err != nil || len(res) != 130 {
		t.Fatalf("Stopping must preserve completed results and leave the rest untested: %v, %v", res, err)
	}
	for i, state := range res {
		want := int8(0)
		if i < 125 {
			want = 1
		}
		if state != want {
			t.Fatalf("Stopping after the first batch must retain its 125 results: offset %d, state %d", i, state)
		}
	}
}

func TestRegisterProbeReadsValidRangeInOneRequest(t *testing.T) {
	var sent []modbus.Packet
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			sent = append(sent, p)
		}
	}), 128)
	res, err := probe(context.Background(), c, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 100}, func(int) {})
	if err != nil || len(res) != 100 || len(sent) != 1 || sent[0].Count != 100 {
		t.Fatalf("A valid 100-address range should need one batch request: count %d, states %v, err %v", len(sent), res, err)
	}
	for _, state := range res {
		if state != 1 {
			t.Fatalf("Valid batch should mark every address readable: %v", res)
		}
	}
}

func TestRegisterProbeDoesNotTreatOtherExceptionsAsMissing(t *testing.T) {
	for _, code := range []modbus.ExceptionCode{modbus.ExceptionIllegalFunction, modbus.ExceptionIllegalDataValue, modbus.ExceptionSlaveDeviceBusy} {
		t.Run(code.Name(), func(t *testing.T) {
			c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 1, Count: 1}, Code: code}}}, nil)
			d := defaultDef()
			d.Qty = 3
			res, err := probe(context.Background(), c, d, func(int) {})
			if err != nil || fmt.Sprint(res) != "[1 -2 1]" {
				t.Fatalf("Non-address exception must remain inconclusive while later addresses are tested: %v, %v", res, err)
			}
		})
	}
}

func TestRegisterProbeTimeoutIsInconclusive(t *testing.T) {
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 1, Count: 1}, Delay: 100 * time.Millisecond}}}, nil)
	c.SetTimeout(50 * time.Millisecond)
	d := defaultDef()
	d.Qty = 3
	var rechecked []int
	res, err := probe(context.Background(), c, d, func(int) {}, func(offset int) { rechecked = append(rechecked, offset) })
	if err != nil || fmt.Sprint(res) != "[1 -3 1]" {
		t.Fatalf("A timed-out address must stay inconclusive while later addresses are tested: %v, %v", res, err)
	}
	if fmt.Sprint(rechecked) != "[1]" {
		t.Fatalf("Recheck progress must identify the unresolved address: %v", rechecked)
	}
}

func TestRegisterProbeStopsAfterTimeoutWithoutTransactionIDs(t *testing.T) {
	for _, mode := range []modbus.Mode{modbus.ModeRTUOverTCP, modbus.ModeASCIIOverTCP} {
		t.Run(string(mode), func(t *testing.T) {
			sent := 0
			c := registerProbeClient(t, mode, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 2, Count: 4}, Delay: 250 * time.Millisecond}}}, modbus.ObserverFunc(func(p modbus.Packet) {
				if p.Dir == modbus.DirTX {
					sent++
				}
			}))
			c.SetTimeout(50 * time.Millisecond)
			res, err := probe(context.Background(), c, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 2, Qty: 4}, func(int) {})
			if err == nil || fmt.Sprint(res) != "[-2 -2 -2 -2]" || sent != 1 {
				t.Fatalf("Late batch exception must not be assigned to healthy addresses: states %v, requests %d, err %v", res, sent, err)
			}
		})
	}
}

func TestRegisterProbeLocatesSparseForwardingHoles(t *testing.T) {
	for _, failure := range []string{"illegal-address", "gateway-no-response", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			var faults simulator.Faults
			wantState := int8(-1)
			for _, offset := range []uint16{9, 14, 19} { // 40010, 40015, 40020.
				r := simulator.AddrRange{Start: offset, Count: 1}
				switch failure {
				case "timeout":
					faults.Slow = append(faults.Slow, simulator.SlowRange{AddrRange: r, Delay: 100 * time.Millisecond})
					wantState = -3
				case "gateway-no-response":
					faults.Exceptions = append(faults.Exceptions, simulator.ExceptionRange{AddrRange: r, Code: modbus.ExceptionGatewayTargetFailed})
					wantState = -3
				default:
					faults.Exceptions = append(faults.Exceptions, simulator.ExceptionRange{AddrRange: r, Code: modbus.ExceptionIllegalDataAddress})
				}
			}
			requests := 0
			c := registerProbeClient(t, modbus.ModeTCP, faults, modbus.ObserverFunc(func(p modbus.Packet) {
				if p.Dir == modbus.DirTX {
					requests++
				}
			}), 128)
			if failure == "timeout" {
				c.SetTimeout(50 * time.Millisecond)
			}
			d := defaultDef()
			d.Qty = 100
			res, err := probe(context.Background(), c, d, func(int) {})
			if err != nil || len(res) != 100 {
				t.Fatalf("Must finish the complete 40001–40100 range: %v, %v", res, err)
			}
			good, holes := 0, 0
			for i, state := range res {
				if i == 9 || i == 14 || i == 19 {
					if state != wantState {
						t.Errorf("Hole at offset %d must have state %d, got %d", i, wantState, state)
					}
					holes++
				} else if state == 1 {
					good++
				}
			}
			if good != 97 || holes != 3 {
				t.Fatalf("Expected exactly 97 readable addresses and 3 holes, got %d and %d: %v", good, holes, res)
			}
			if failure == "illegal-address" && requests >= 50 {
				t.Errorf("Sparse illegal addresses should be isolated by splitting failed batches, not 100 separate reads: %d requests", requests)
			}
			a := test.NewTempApp(t)
			ws := openWS(t, a, false)
			locked(func() {
				ws.showProbeResult(nil, d, res, err)
				test.Tap(findButtons(ws.win.Canvas().Overlays().Top(), "复制结果")[0])
				copied := ws.app.Clipboard().Content()
				for _, expected := range []string{"40001–40100", "40010、40015、40020", "可读（97）", "未检测（0）"} {
					if !strings.Contains(copied, expected) {
						t.Errorf("Result must list individual holes; missing %q: %s", expected, copied)
					}
				}
				if failure != "illegal-address" && (!strings.Contains(copied, "疑似未转发") || !strings.Contains(copied, "非法地址（0")) {
					t.Errorf("No response must be labelled as suspected forwarding failure, not confirmed illegal address: %s", copied)
				}
			})
			if failure == "timeout" {
				snapshotPNG(t, ws.win, "register-probe-sparse-holes.png")
			}
		})
	}
}

func TestRegisterProbeRejectsAddressWrapBeforeSending(t *testing.T) {
	sent := 0
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			sent++
		}
	}))
	d := defaultDef()
	d.Start, d.Qty = 65535, 2
	_, err := probe(context.Background(), c, d, func(int) {})
	if err == nil || sent != 0 {
		t.Fatalf("Address wrap must fail before sending: sent %d, %v", sent, err)
	}
}

func TestRegisterProbeMenuAvailableWithoutReadWindow(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var action func()
	locked(func() { action = menuAction(t, ws, "检测寄存器…") })
	locked(action)
	locked(func() {
		if !strings.Contains(overlayText(ws), "先连接") {
			t.Errorf("Disconnected scan should explain that a connection is required: %s", overlayText(ws))
		}
	})
}

func TestRegisterProbeResultsIncludeUnknownAndUntested(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		d := defaultDef()
		d.Qty = 6
		w := ws.addWindow(d)
		ws.showProbeResult(w, d, []int8{1, 1, -1, -2, 0, 0}, fmt.Errorf("40004: timeout"))
		text := overlayText(ws)
		for _, expected := range []string{"可读（2）", "非法地址（1", "40003", "无法判断（1）", "40004", "未检测（2）", "40005–40006", "复制结果"} {
			if !strings.Contains(text, expected) {
				t.Errorf("Scan result omits %q: %s", expected, text)
			}
		}
	})
}

func probeEntries(o fyne.CanvasObject) []*widget.Entry {
	switch x := o.(type) {
	case *widget.Entry:
		return []*widget.Entry{x}
	case *fyne.Container:
		var out []*widget.Entry
		for _, c := range x.Objects {
			out = append(out, probeEntries(c)...)
		}
		return out
	case fyne.Widget:
		var out []*widget.Entry
		for _, c := range test.WidgetRenderer(x).Objects() {
			out = append(out, probeEntries(c)...)
		}
		return out
	}
	return nil
}

func TestRegisterProbeDialogDefaultsRangeAndCopy(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var paused, running *readWindow
	locked(func() {
		running = ws.addWindow(defaultDef())
		d := defaultDef()
		d.Function, d.Start, d.Qty = modbus.FuncReadInputRegisters, 995, 10
		paused = ws.addWindow(d)
		paused.setPaused(true)
		ws.setCurrent(paused)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
	locked(func() {
		menuAction(t, ws, "检测寄存器…")()
		top := ws.win.Canvas().Overlays().Top()
		entries := probeEntries(top)
		if len(entries) != 3 || entries[0].Text != "1" || entries[1].Text != "995" || entries[2].Text != "10" {
			t.Fatalf("Scan form must use the current window definition: %v", entries)
		}
		selects := findSelects(top)
		if len(selects) != 2 || selects[0].Selected != modbus.FuncReadInputRegisters.String() || selects[1].Selected != probeModeNames[probeAuto] {
			t.Fatalf("Scan form must use the current function and default to automatic locating")
		}
		start := findButtons(top, "开始检测")[0]
		entries[1].SetText("65535")
		entries[2].SetText("2")
		if !start.Disabled() {
			t.Error("Address overflow must disable Start")
		}
		entries[2].SetText("1")
		if start.Disabled() {
			t.Error("Last address is valid for one register")
		}
		entries[1].SetText("995")
		entries[2].SetText("130") // More than the limit of a single FC04 request.
		if start.Disabled() {
			t.Fatal("Per-address scan must support a range exceeding 125 registers")
		}
		test.Tap(start)
		if running.stateLbl.Text != "检测中" || paused.stateLbl.Text != "检测中" {
			t.Errorf("Active detection must be visible in both polling and manually paused windows: %q / %q", running.stateLbl.Text, paused.stateLbl.Text)
		}
	})
	waitFor(t, 15*time.Second, "scan result", func() bool { return strings.Contains(overlayText(ws), "非法地址（125") })
	waitFor(t, 5*time.Second, "original polling connection restored", func() bool {
		return ws.session != nil && ws.session.lost == nil && paused.stateLbl.Text == "已暂停"
	})
	locked(func() {
		if !paused.paused || running.paused {
			t.Error("Detection must restore each window's original pause state")
		}
		if paused.stateLbl.Text != "已暂停" {
			t.Errorf("Completed detection must restore the manually paused window's visible state: %q", paused.stateLbl.Text)
		}
		test.Tap(findButtons(ws.win.Canvas().Overlays().Top(), "复制结果")[0])
		copied := ws.app.Clipboard().Content()
		for _, expected := range []string{"读输入寄存器", "可读（5）", "非法地址（125", "无法判断（0）", "未检测（0）", "Offset 995–1124"} {
			if !strings.Contains(copied, expected) {
				t.Errorf("Copied scan result missing %q: %s", expected, copied)
			}
		}
	})
	snapshotPNG(t, ws.win, "register-probe-result.png")
}

func TestRegisterProbeStopAndDisconnectRestorePolling(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			a := test.NewTempApp(t)
			ws := openWS(t, a, false)
			var running, paused *readWindow
			locked(func() {
				running = ws.addWindow(defaultDef())
				paused = ws.addWindow(defaultDef())
				paused.setPaused(true)
			})
			tap(ws.connBtn)
			waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
			locked(func() {
				ws.session.sim.SetFaults(simulator.Faults{Delay: 50 * time.Millisecond})
				ws.runRegisterProbe(nil, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 1000})
			})
			waitFor(t, 5*time.Second, "first probe result", func() bool {
				text := overlayText(ws)
				return strings.Contains(text, "已检测") && !strings.Contains(text, "已检测 0 / 1000")
			})
			locked(func() {
				before := ws.win.Canvas().Overlays().Top()
				ws.runRegisterProbe(nil, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 15, Qty: 2})
				if ws.win.Canvas().Overlays().Top() != before {
					t.Error("A second probe must not start while the first one owns the connection")
				}
				ws.pauseAll(false)
				if running.stop != nil || paused.stop != nil {
					t.Error("Continue-all must not restart polling while detection is running")
				}
				ws.pauseAll(true)
				if disconnect {
					ws.disconnect()
				} else {
					test.Tap(findButtons(ws.win.Canvas().Overlays().Top(), "停止")[0])
					if !strings.Contains(overlayText(ws), "正在停止") {
						t.Error("Stop must keep the dialog blocking another scan until cleanup finishes")
					}
				}
			})
			waitFor(t, 5*time.Second, "partial results", func() bool {
				text := overlayText(ws)
				return strings.Contains(text, "探测结果") && strings.Contains(text, "保留已完成")
			})
			locked(func() {
				if running.paused || !paused.paused {
					t.Errorf("Stop/disconnect changed original pause state: running=%v paused=%v", running.paused, paused.paused)
				}
			})
		})
	}
}

func TestRegisterProbeDoesNotApplyToChangedWindow(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		d := defaultDef()
		d.Start = 995
		w := ws.addWindow(d)
		ws.showProbeResult(w, d, []int8{1, 1, 1, 1, 1, -1, -1, -1, -1, -1}, nil)
		apply := findButtons(ws.win.Canvas().Overlays().Top(), "改为读取 40996–41000")[0]
		ws.redefine(w, func(nd *readDef) { nd.Start = 346 })
		test.Tap(apply)
		if w.def.Start != 346 || w.def.Qty != 10 {
			t.Fatalf("A stale probe must not overwrite a changed window: start=%d qty=%d", w.def.Start, w.def.Qty)
		}
	})
}

func TestRegisterProbeDoesNotApplyAfterConnectionOrWindowChanges(t *testing.T) {
	for _, change := range []string{"disconnect", "reconnect", "close-window"} {
		t.Run(change, func(t *testing.T) {
			a := test.NewTempApp(t)
			ws := openWS(t, a, false)
			var w *readWindow
			d := defaultDef()
			d.Start = 995
			locked(func() { w = ws.addWindow(d); w.setPaused(true) })
			tap(ws.connBtn)
			waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
			var apply *widget.Button
			var original *session
			locked(func() {
				original = ws.session
				ws.showProbeResult(w, d, []int8{1, 1, 1, 1, 1, -1, -1, -1, -1, -1}, nil)
				apply = findButtons(ws.win.Canvas().Overlays().Top(), "改为读取 40996–41000")[0]
				if change == "close-window" {
					ws.removeWindow(w)
				} else {
					ws.disconnect()
					if change == "reconnect" {
						ws.connect()
					}
				}
			})
			if change == "reconnect" {
				waitFor(t, 5*time.Second, "new connection", func() bool { return ws.session != nil && ws.session != original })
			}
			locked(func() {
				test.Tap(apply)
				if w.def.Qty != 10 || w.def.Start != 995 {
					t.Fatalf("Old results applied after %s: %+v", change, w.def)
				}
			})
		})
	}
}

// 逐个寄存器：每个地址单独读一次，非法地址和其他异常码都不打断，读完全部地址。
func TestProbeEachRegister(t *testing.T) {
	sent := 0
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Exceptions: []simulator.ExceptionRange{
		{AddrRange: simulator.AddrRange{Start: 1, Count: 1}, Code: modbus.ExceptionSlaveDeviceFailure},
	}}, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			sent++
		}
	}), 4)
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 6}
	res, err := probeUnits(context.Background(), c, d, probeSingle, nil, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 -2 1 1 -1 -1]" || sent != 6 {
		t.Fatalf("逐个寄存器应各读一次、异常码不中断：%v，发了 %d 条，%v", res, sent, err)
	}
}

// 按点表分段：地址首尾相接的点合成一段一次读完，中间有空档就另起一段；读不通的段逐点再读，指出是哪个点。
func TestProbeByPointSegments(t *testing.T) {
	pts := newPointTable([]point{
		{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "供水温度", Type: modbus.TypeFloat32},
		{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "状态", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 5, Name: "回水温度", Type: modbus.TypeFloat32},
		{Area: modbus.AreaHoldingRegisters, Offset: 7, Name: "频率", Type: modbus.TypeUint16},
		{Area: modbus.AreaInputRegisters, Offset: 0, Name: "别的数据区", Type: modbus.TypeUint16},
	})
	d, segs, ok := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	if !ok || d.Start != 0 || d.Qty != 8 || len(segs) != 2 || segs[0].n != 3 || segs[1].start != 5 || segs[1].n != 3 {
		t.Fatalf("分段：%+v %+v", d, segs)
	}
	if _, _, ok := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadCoils}); ok {
		t.Error("点表里没有线圈，不能按点表检测线圈")
	}
	sent := 0
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			sent++
		}
	}), 6) // 设备只有 0–5：第二段（5–7）读不通，“回水温度”跨到 6、“频率”在 7，都无效
	var progress []int
	res, err := probeUnits(context.Background(), c, d, probePoints, segs, func(n int) { progress = append(progress, n) })
	if err != nil || fmt.Sprint(res) != "[1 1 1 0 0 -1 -1 -1]" || sent != 4 || segs[0].state != 1 || segs[1].state != -1 {
		t.Fatalf("按段检测：%v，发了 %d 条，段 %d %d，%v", res, sent, segs[0].state, segs[1].state, err)
	}
	if fmt.Sprint(progress) != "[3 5 6]" {
		t.Errorf("失败段的进度应随逐点复核推进，到全部确认才完成：%v", progress)
	}
	text, summary, bad := pointProbeText(d, res, segs)
	if bad != 2 || !strings.Contains(text, "40006 回水温度（FLOAT32）：非法地址") || !strings.Contains(text, "40008 频率（UINT16）：非法地址") ||
		!strings.Contains(text, "40001–40003（2 个点）：整段可读") || !strings.Contains(summary, "4 个点：可读 2") {
		t.Errorf("结果：%s\n%s", summary, text)
	}
}

// 整段异常后中途停止：未逐点复核的点必须保持未检测。
func TestProbeByPointSegmentsCancellationKeepsUnverifiedPointsUntested(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stopAfter    int
		wantStates   string
		wantProgress string
		wantBad      int
		wantUntested string
	}{
		{"after-readable-point", 2, "[1 1 0 0 0]", "[2]", 0, "未检测 2"},
		{"after-illegal-point", 4, "[1 1 -1 -1 0]", "[2 4]", 1, "未检测 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pts := newPointTable([]point{
				{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "温度", Type: modbus.TypeFloat32},
				{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "压力", Type: modbus.TypeFloat32},
				{Area: modbus.AreaHoldingRegisters, Offset: 4, Name: "状态", Type: modbus.TypeUint16},
			})
			d, segs, _ := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
			c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, nil, 2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var progress []int
			res, err := probeUnits(ctx, c, d, probePoints, segs, func(n int) {
				progress = append(progress, n)
				if n >= tc.stopAfter {
					cancel()
				}
			})
			if err != nil || fmt.Sprint(res) != tc.wantStates {
				t.Fatalf("停止应保留已确认可读的点，其余点未检测，不能沿用整段异常：%v, %v", res, err)
			}
			if fmt.Sprint(progress) != tc.wantProgress {
				t.Errorf("进度应只统计已完成确认的寄存器：%v", progress)
			}
			text, summary, bad := pointProbeText(d, res, segs)
			if bad != tc.wantBad || !strings.Contains(summary, "可读 1") || !strings.Contains(summary, tc.wantUntested) || strings.Contains(text, "状态（UINT16）：非法地址") {
				t.Fatalf("停止后的点位报告不应把未复核点算作非法地址：%s\n%s", summary, text)
			}
		})
	}
}

// 检测对话框：选“按点表分段”时范围按点表算，起始地址和数量不用填；没有点表时提示先导入。
func TestRegisterProbeDialogModes(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), true)
	waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
	locked(func() {
		ws.registerProbeDialog()
		form := ws.win.Canvas().Overlays().Top()
		var mode *widget.Select
		var preview *widget.Label
		var entries []*widget.Entry
		walk(form, func(o fyne.CanvasObject) {
			switch x := o.(type) {
			case *widget.Select:
				if len(x.Options) == len(probeModeNames) && x.Options[0] == probeModeNames[0] {
					mode = x
				}
			case *widget.Label:
				if strings.Contains(x.Text, "Offset 从 0 起始") || strings.Contains(x.Text, "按点表分段：") {
					preview = x
				}
			case *widget.Entry:
				entries = append(entries, x)
			}
		})
		if mode == nil || preview == nil || len(entries) != 3 {
			t.Fatalf("检测对话框缺少检测方式或说明：%v %v %d", mode, preview, len(entries))
		}
		if mode.Selected != probeModeNames[probePoints] || !entries[1].Disabled() || !entries[2].Disabled() {
			t.Error("已有点表时应默认检测全部点表分段")
		}
		mode.SetSelected(probeModeNames[probePoints])
		if !entries[1].Disabled() || !entries[2].Disabled() || !strings.Contains(preview.Text, "按点表分段") || !strings.Contains(preview.Text, "个点") {
			t.Errorf("按点表分段：地址和数量应禁用，说明 %q", preview.Text)
		}
		ws.points = pointTable{}
		mode.SetSelected(probeModeNames[probeSingle])
		mode.SetSelected(probeModeNames[probePoints])
		if !strings.Contains(preview.Text, "先导入点表") {
			t.Errorf("没有点表时应提示先导入：%q", preview.Text)
		}
		mode.SetSelected(probeModeNames[probeSingle])
		if entries[1].Disabled() || !strings.Contains(preview.Text, "逐个寄存器") {
			t.Errorf("逐个寄存器：地址应可编辑，说明 %q", preview.Text)
		}
		clearOverlays(ws)
	})
}

// walk 按控件树访问界面里的每个对象（包括隐藏的）。
func walk(o fyne.CanvasObject, fn func(fyne.CanvasObject)) {
	fn(o)
	switch x := o.(type) {
	case *fyne.Container:
		for _, c := range x.Objects {
			walk(c, fn)
		}
	case fyne.Widget:
		for _, c := range test.WidgetRenderer(x).Objects() {
			walk(c, fn)
		}
	}
}
