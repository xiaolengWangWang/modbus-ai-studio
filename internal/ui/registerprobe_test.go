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
	c := modbus.NewClient(conn, modbus.Options{Mode: mode, Timeout: 50 * time.Millisecond, Guard: 100 * time.Millisecond, Observer: observer})
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
			if err == nil || fmt.Sprint(res) != "[1 -2 0]" {
				t.Fatalf("Non-address exception must remain inconclusive and stop: %v, %v", res, err)
			}
		})
	}
}

func TestRegisterProbeTimeoutIsInconclusive(t *testing.T) {
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 1, Count: 1}, Delay: 100 * time.Millisecond}}}, nil)
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
		if len(selects) != 1 || selects[0].Selected != modbus.FuncReadInputRegisters.String() {
			t.Fatalf("Scan form must use the current function")
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
	})
	waitFor(t, 15*time.Second, "scan result", func() bool { return strings.Contains(overlayText(ws), "非法地址（125") })
	locked(func() {
		if !paused.paused || running.paused {
			t.Error("Detection must restore each window's original pause state")
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
