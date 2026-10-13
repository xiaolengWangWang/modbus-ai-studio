package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
)

// 导入点表建了几个读取窗口：导入结果里点“修改站号”，一次改好全部窗口，窗口里设的数据类型保留。
func TestChangeSlaveForImportedPoints(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("属性标识,属性名称\n")
	for i := range 3 {
		fmt.Fprintf(&sb, "4x%04d:REAL,点%d\n", 1+i*200, i)
	}
	imp, err := parsePointsFile("a.csv", []byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		msg := ws.applyImport(imp)
		if len(ws.windows) != 3 || !strings.Contains(msg, "读取窗口读的是 Slave 1") {
			t.Fatalf("应建 3 个窗口并说明站号：%d 个，%s", len(ws.windows), msg)
		}
		w := ws.windows[0]
		w.def.Formats = []registerFormat{{Offset: w.def.Start, Kind: kindFloat32, Order: modbus.OrderCDAB}}
		ws.showImportResult(msg)
		pressButton(t, ws, "修改站号…")
		entries, selects, _ := overlayWidgets(ws)
		if len(entries) != 1 || len(selects) != 0 || !strings.Contains(overlayText(ws), "全部 3 个读取窗口，现在是 Slave 1") {
			t.Fatalf("窗口都是同一个站号时只填新站号：%d 个输入框、%d 个下拉框，%s", len(entries), len(selects), overlayText(ws))
		}
		entries[0].SetText("300")
		pressButton(t, ws, "应用")
		if ws.windows[1].def.Slave != 1 || overlayCount(ws) != 1 {
			t.Fatalf("站号超出 1–255 不应应用")
		}
		entries[0].SetText("5")
		pressButton(t, ws, "应用")
		for _, x := range ws.windows {
			if x.def.Slave != 5 {
				t.Errorf("窗口 %d 的站号 %d，应为 5", x.no, x.def.Slave)
			}
		}
		if len(w.def.Formats) != 1 {
			t.Error("改的是同一台设备的站号，窗口里设的数据类型应保留")
		}
	})
}

// 同一总线上几台设备、窗口用了几个站号：先选改哪一组，默认是当前窗口那组；也可以改全部窗口。
func TestChangeSlaveScope(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		for _, id := range []byte{1, 1, 2} {
			d := defaultDef()
			d.Slave = id
			ws.addWindow(d)
		}
		slaves := func() []byte {
			var out []byte
			for _, w := range ws.windows {
				out = append(out, w.def.Slave)
			}
			return out
		}
		ws.showSlaveDialog(ws.windows[2])
		entries, selects, _ := overlayWidgets(ws)
		if len(selects) != 1 || selects[0].Selected != "Slave 2 的读取窗口（1 个）" || entries[0].Text != "2" {
			t.Fatalf("应默认选当前窗口的站号：%v", selects)
		}
		selects[0].SetSelected("Slave 1 的读取窗口（2 个）")
		if entries[0].Text != "1" {
			t.Errorf("换范围后从那组的站号改起：%q", entries[0].Text)
		}
		entries[0].SetText("9")
		pressButton(t, ws, "应用")
		if got := fmt.Sprint(slaves()); got != "[9 9 2]" {
			t.Fatalf("只改 Slave 1 的窗口：%s", got)
		}
		ws.showSlaveDialog(nil)
		entries, selects, _ = overlayWidgets(ws)
		selects[0].SetSelected("全部读取窗口（3 个）")
		entries[0].SetText("4")
		pressButton(t, ws, "应用")
		if got := fmt.Sprint(slaves()); got != "[4 4 4]" {
			t.Fatalf("改全部窗口：%s", got)
		}
	})
}

// 晚到帧在通信报文里说明回的是哪条请求、比超时晚了多久；别的主站的响应单独标出来。历史报文里 Err 是存下的文字，照样显示。
func TestTrafficLineLateAndForeign(t *testing.T) {
	req := modbus.Packet{RequestID: 7, Mode: modbus.ModeTCP, Time: time.Date(2026, 10, 13, 13, 5, 48, 504e6, time.Local),
		Raw: []byte{0, 7, 0, 0, 0, 6, 3, 3, 0, 0x9E, 0, 0x0A}}
	le := &modbus.LateError{Request: req, Timeout: time.Second, Elapsed: 1013 * time.Millisecond, Max: 1013 * time.Millisecond}
	late := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeTCP, RequestID: 9, Status: modbus.StatusLate, Raw: []byte{0, 7, 0, 0, 0, 5, 3, 3, 2, 0, 1}, Err: le}
	const want = "← 晚到响应，已丢弃（比超时晚 13 ms：超时 1000 ms，响应用了 1013 ms；回的是 13:05:48.504 发出的请求 #000007：00 07 00 00 00 06 03 03 00 9E 00 0A）"
	for _, err := range []error{le, errors.New(le.Error())} {
		late.Err = err
		if text, _ := trafficLine(late, false); !strings.HasSuffix(text, want) {
			t.Errorf("晚到帧：%s", text)
		}
	}
	foreign := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeTCP, Status: modbus.StatusLate, Raw: []byte{0x1C, 0xA6, 0, 0, 0, 5, 0x0A, 3, 2, 0xD8, 0xDA}, Err: &modbus.ForeignError{TxID: 0x1CA6}}
	if text, _ := trafficLine(foreign, false); !strings.HasSuffix(text, "← 非本连接的响应，已丢弃（事务号 1CA6 不是本连接最近发出的请求，多半是别的主站的）") {
		t.Errorf("别的主站的响应：%s", text)
	}
	if rows := rowsText(describePacket(foreign, nil)); !strings.Contains(rows, "串口服务器 / DTU") {
		t.Errorf("报文解析应说明是转发来的：%s", rows)
	}
}

// 晚到帧的应答延迟按它回的那条请求记，不按收到它时正在等的请求；一键调大超时据此给值。别的主站的响应不算晚到。
func TestEvidenceUsesLateRequest(t *testing.T) {
	var ev evidence
	owner := modbus.Packet{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 100}
	rx := modbus.Packet{Dir: modbus.DirRX, Status: modbus.StatusLate, Slave: 9, Function: modbus.FuncReadHoldingRegisters, Address: 500}
	rx.Err = &modbus.LateError{Request: owner, Timeout: time.Second, Elapsed: 2500 * time.Millisecond, Max: 3 * time.Second}
	ev.observe(rx)
	rx.Err = &modbus.LateError{Request: modbus.Packet{Slave: 2, Function: modbus.FuncReadHoldingRegisters}, Cancelled: true, Timeout: time.Second, Elapsed: 9 * time.Second, Max: 9 * time.Second}
	ev.observe(rx)
	rx.Err = &modbus.ForeignError{TxID: 0x1CA6}
	ev.observe(rx)
	if d := ev.delayOf(reqKey{1, modbus.FuncReadHoldingRegisters, 100}); d != 2500 {
		t.Errorf("按回的那条请求记，只知道范围时取下限：%d ms", d)
	}
	if ev.delayOf(reqKey{9, modbus.FuncReadHoldingRegisters, 500}) != 0 || ev.delayOf(reqKey{2, modbus.FuncReadHoldingRegisters, 0}) != 0 {
		t.Error("不应记到正在等的请求上；取消的请求不算超时")
	}
	if ev.late.Load() != 2 || ev.foreign.Load() != 1 {
		t.Errorf("晚到 %d、别的主站的响应 %d", ev.late.Load(), ev.foreign.Load())
	}
}
