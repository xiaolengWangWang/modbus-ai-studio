package ui

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/slavescan"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

// 搜索站号：连着内置模拟器搜 1–3。模拟器对哪个站号都应答，三个都找到，都是先读给的地址找到的；
// 读取窗口用的是 Slave 1，可以改用 2、3。
func TestSlaveScanFindsDemo(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, true)
	waitFor(t, 5*time.Second, "读到数据", func() bool { return hasData(ws.windows[0]) })
	locked(func() {
		clearOverlays(ws)
		ws.runSlaveScan(ws.session, 1, 3, 50*time.Millisecond, slavescan.Probes(slavescan.Probe{Function: modbus.FuncReadHoldingRegisters, Address: 346}))
		for _, w := range ws.windows {
			if w.stateLbl.Text != "搜索站号中" {
				t.Errorf("搜索期间读取窗口应显示搜索站号中：%q", w.stateLbl.Text)
			}
		}
	})
	var text string
	waitFor(t, 10*time.Second, "搜索结果", func() bool { // waitFor 已经在锁里调用
		text = overlayText(ws)
		return strings.Contains(text, "搜索结果")
	})
	for _, want := range []string{"Slave 1：读 40347（FC03） 正常响应", "Slave 3：读 40347（FC03） 正常响应", "改用所选站号"} {
		if !strings.Contains(text, want) {
			t.Errorf("结果里没有 %s：%s", want, text)
		}
	}
	locked(func() {
		if ws.scanRunning || ws.windows[0].stop == nil {
			t.Error("搜完读取窗口应接着轮询")
		}
	})
}

// 找到几个别的站号时，选一个，全部读取窗口改用它。
func TestSlaveScanResultAppliesPick(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, true)
	hit := func(id int) slavescan.Hit {
		return slavescan.Hit{ID: id, Probe: slavescan.Probe{Function: modbus.FuncReadHoldingRegisters}, RTT: 5 * time.Millisecond}
	}
	locked(func() {
		clearOverlays(ws)
		ws.showSlaveScanResult([]slavescan.Hit{hit(1), hit(3), hit(7)}, nil, false)
		top := ws.win.Canvas().Overlays().Top()
		sel := findSelect(top)
		if sel == nil || len(sel.Options) != 2 || sel.Options[1] != "Slave 7" {
			t.Fatalf("应可选 Slave 3、Slave 7（不含读取窗口现在用的 1）：%v", sel)
		}
		sel.SetSelected("Slave 7")
		buttons := findButtons(top, "改用所选站号")
		if len(buttons) != 1 {
			t.Fatalf("没有“改用所选站号”：%s", overlayText(ws))
		}
		test.Tap(buttons[0])
		for _, w := range ws.windows {
			if w.def.Slave != 7 {
				t.Errorf("%s 仍是 Slave %d", w.def.Name, w.def.Slave)
			}
		}
	})
}

// findSelect 找出界面里的第一个下拉框。
func findSelect(o fyne.CanvasObject) *widget.Select {
	switch x := o.(type) {
	case *widget.Select:
		return x
	case *fyne.Container:
		for _, c := range x.Objects {
			if s := findSelect(c); s != nil {
				return s
			}
		}
	case fyne.Widget:
		for _, c := range test.WidgetRenderer(x).Objects() {
			if s := findSelect(c); s != nil {
				return s
			}
		}
	}
	return nil
}

// 串口参数扫描：设备是 19200 8E1，前面 5 种组合没有应答，第 6 种找到。
func TestScanSerial(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	live, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(live)
	silent, _ := net.Listen("tcp", "127.0.0.1:0") // 接受连接但从不应答，相当于参数不对
	t.Cleanup(func() { srv.Close(); silent.Close() })
	go func() {
		for {
			if _, err := silent.Accept(); err != nil {
				return
			}
		}
	}()
	var tried []string
	open := func(cfg transport.SerialConfig) (modbus.Transport, error) {
		addr := silent.Addr().String()
		if cfg.BaudRate == 19200 && cfg.Parity == "E" && cfg.StopBits == 1 && cfg.DataBits == 8 {
			addr = live.Addr().String()
		}
		return net.Dial("tcp", addr)
	}
	mode, baud, format, err := scanSerial(context.Background(), modbus.ModeRTU, open, "COM9", 1, 80*time.Millisecond, nil,
		func(_, _ int, try string) { tried = append(tried, try) })
	if err != nil || mode != modbus.ModeRTU || baud != 19200 || format != "8E1" || len(tried) != 6 {
		t.Fatalf("找到 %s %d %s（%v），试了 %v", mode, baud, format, err, tried)
	}
}

func TestDiagCounters(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Quantity: 1})
	}
	counts, err := readDiagCounters(context.Background(), c, 1)
	if err != nil || len(counts) != 8 || counts[0].sub != 0x0B || counts[0].n != 4 || counts[3].sub != 0x0E || counts[3].n != 7 {
		t.Fatalf("诊断计数器 %+v %v", counts, err)
	}
}
