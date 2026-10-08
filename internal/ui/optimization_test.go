package ui

import (
	"net"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"modbus-ai-studio/internal/modbus"
)

// A user must not change the protocol or endpoint while a connection is opening.
func TestConnectionLocksConfigWhileDialing(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		ws.connect()
		if w.stateLbl.Text != "连接中" {
			t.Error("existing read window missed dialing state")
		}
		if !ws.proto.Disabled() || !ws.useSim.Disabled() || !ws.baud.Disabled() || !ws.detectBn.Disabled() {
			t.Error("connection parameters must be locked as soon as dialing starts")
		}
		if !strings.Contains(ws.status.Text, "连接中") {
			t.Errorf("dialing state missing from status: %s", ws.status.Text)
		}
	})
	waitFor(t, 5*time.Second, "connected", func() bool { return ws.session != nil })
	locked(func() {
		ws.disconnect()
		if ws.proto.Disabled() || ws.useSim.Disabled() || !strings.Contains(ws.status.Text, "未连接") {
			t.Error("disconnect must restore editable parameters and offline status")
		}
	})
}

func TestConnectionFailureRestoresInputs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	ws := openWS(t, test.NewTempApp(t), false)
	var w *readWindow
	locked(func() {
		w = ws.addWindow(defaultDef())
		ws.useSim.SetChecked(false)
		ws.target.SetText(address)
		ws.connect()
		if w.stateLbl.Text != "连接中" {
			t.Error("read window did not enter dialing state")
		}
		if !ws.target.Disabled() {
			t.Error("endpoint must be locked while dialing")
		}
	})
	waitFor(t, 5*time.Second, "failed connection", func() bool { return !ws.connecting })
	locked(func() {
		if w.stateLbl.Text != "未连接" {
			t.Error("failed dialing did not restore read window status")
		}
		if ws.session != nil || ws.target.Disabled() || ws.proto.Disabled() || ws.connBtn.Disabled() || ws.detectBn.Disabled() {
			t.Error("failed connection must restore all applicable inputs")
		}
		if !strings.Contains(ws.status.Text, "连接失败") {
			t.Errorf("failure missing from status: %s", ws.status.Text)
		}
		clearOverlays(ws)
		ws.useSim.SetChecked(true)
		ws.connect()
	})
	waitFor(t, 5*time.Second, "successful retry", func() bool { return ws.session != nil })
	locked(func() {
		if strings.Contains(ws.status.Text, "失败") || !strings.Contains(ws.status.Text, "已连接") {
			t.Errorf("successful retry must clear the previous failure: %s", ws.status.Text)
		}
	})
}

func TestTrafficDirectionAndSearch(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		p.setPackets([]modbus.Packet{
			{Dir: modbus.DirTX, RequestID: 1, Raw: []byte{1, 3, 0, 0}},
			{Dir: modbus.DirRX, RequestID: 1, Raw: []byte{1, 3, 2, 0, 42}, Status: modbus.StatusSuccess},
			{Dir: modbus.DirTX, RequestID: 2, Raw: []byte{1, 4, 0, 0}},
			{Dir: modbus.DirRX, RequestID: 2, Status: modbus.StatusTimeout},
		})
		for _, c := range []struct {
			filter, query string
			want          []int
		}{
			{"仅发送", "", []int{0, 2}},
			{"仅接收", "", []int{1, 3}},
			{"仅接收", "01 03", []int{1}},
			{"仅错误", "", []int{2, 3}},
			{"全部", "FF FF", nil},
		} {
			p.filter.SetSelected(c.filter)
			p.search.SetText(c.query)
			if len(p.view) != len(c.want) {
				t.Errorf("%s / %q: visible rows %v, want %v", c.filter, c.query, p.view, c.want)
				continue
			}
			for i, want := range c.want {
				if p.view[i] != want {
					t.Errorf("%s / %q: visible rows %v, want %v", c.filter, c.query, p.view, c.want)
				}
			}
		}
	})
}

func TestTrafficFilterClearsPacketInspector(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		p.setPackets([]modbus.Packet{
			{Dir: modbus.DirTX, Raw: []byte{1, 3, 0, 0}},
			{Dir: modbus.DirRX, Raw: []byte{1, 3, 2, 0, 42}},
		})
		p.list.Select(0)
		if !strings.Contains(ws.inspect.text, "报文解析") {
			t.Fatal("selection did not show packet details")
		}
		p.search.SetText("02 00 2A")
		if strings.Contains(ws.inspect.text, "报文解析") {
			t.Error("changed filter must clear details belonging to the old selection")
		}
		p.list.Select(0)
		if !strings.Contains(ws.inspect.text, "01 03 02 00 2A") {
			t.Error("the same list index must select the newly visible packet")
		}
		buttons := findButtons(p.root, "清除筛选")
		if len(buttons) != 1 {
			t.Error("one-click reset filter action is missing")
			return
		}
		test.Tap(buttons[0])
		if p.search.Text != "" || p.filter.Selected != "全部" || len(p.view) != 2 {
			t.Error("reset must restore all visible rows")
		}
	})
}

func TestTrafficCountsRetainedFilteredAndPausedPackets(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		for i := 0; i < 5002; i++ {
			raw := []byte{0}
			if i >= 5000 {
				raw = []byte{0xAA}
			}
			p.push(modbus.Packet{Dir: modbus.DirTX, Raw: raw})
		}
		p.flush()
		p.search.SetText("AA")
		if p.total != 5002 || len(p.all) != 5000 || len(p.view) != 2 {
			t.Errorf("cumulative/retained/visible counts: %d / %d / %d", p.total, len(p.all), len(p.view))
		}
		if !strings.Contains(p.count.Text, "2 / 5000") || !strings.Contains(p.count.Text, "累计 5002") {
			t.Errorf("filtered count is ambiguous: %s", p.count.Text)
		}
		p.setPaused(true)
		p.push(modbus.Packet{Dir: modbus.DirRX, Raw: []byte{0xAA}})
		p.flush()
		if len(p.view) != 2 || !strings.Contains(p.count.Text, "待显示 1") {
			t.Errorf("paused count: %s", p.count.Text)
		}
		p.setPaused(false)
		if len(p.view) != 3 || p.total != 5003 || len(p.all) != 5000 {
			t.Error("resuming must merge buffered packets without counting them twice")
		}
		p.clear()
		if p.total != 0 || len(p.all) != 0 || len(p.view) != 0 || len(p.held) != 0 {
			t.Error("clear must reset packet buffers and all counters")
		}
	})
}

func TestToolbarPauseTracksIndividualWindows(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		buttons := findButtons(ws.bar, "全部暂停")
		if len(buttons) != 1 {
			t.Error("toolbar pause action is missing")
			return
		}
		button := buttons[0]
		if !button.Disabled() {
			t.Error("pause must be disabled without read windows")
		}
		one, two := ws.addWindow(defaultDef()), ws.addWindow(defaultDef())
		if button.Disabled() {
			t.Error("pause must enable when read windows exist")
		}
		test.Tap(button)
		if !one.paused || !two.paused {
			t.Error("all windows must pause")
		}
		one.setPaused(false)
		test.Tap(button)
		if !one.paused || !two.paused {
			t.Error("mixed states must pause the remaining active windows")
		}
		test.Tap(button)
		if one.paused || two.paused {
			t.Error("all paused windows must resume together")
		}
		ws.removeWindow(one)
		ws.removeWindow(two)
		if !button.Disabled() {
			t.Error("removing the last window must disable pause")
		}
	})
}

// A truncated label in Border's right slot can otherwise collapse to an ellipsis.
func TestTrafficCountRemainsReadable(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		for _, size := range []fyne.Size{fyne.NewSize(1240, 760), fyne.NewSize(960, 620)} {
			ws.win.Resize(size)
			want := fyne.MeasureText("显示 0 / 0 · 累计 0", theme.TextSize(), fyne.TextStyle{}).Width
			if ws.traffic.count.Size().Width < want {
				t.Errorf("window %.0f: packet count width %.0f hides the counts (needs %.0f)", size.Width, ws.traffic.count.Size().Width, want)
			}
		}
	})
}

func TestTrafficCountReflowsWhenPaused(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		p.setPackets([]modbus.Packet{{Dir: modbus.DirTX}, {Dir: modbus.DirRX}})
		p.setPaused(true)
		p.push(modbus.Packet{Dir: modbus.DirRX})
		p.flush()
		want := fyne.MeasureText("显示 2 / 2 · 累计 3 · 已暂停，待显示 1", theme.TextSize(), fyne.TextStyle{}).Width
		if p.count.Size().Width < want || p.count.Position().X+p.count.Size().Width > p.root.Size().Width {
			t.Errorf("growing packet count must remain inside its panel: position %v, size %v, text width %.0f", p.count.Position(), p.count.Size(), want)
		}
	})
}
