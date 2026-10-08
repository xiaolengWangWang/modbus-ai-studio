package ui

import (
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/modbus"
)

// A growing count must not push actions offscreen, and filter reset must be
// reachable without scrolling the toolbar horizontally.
func TestTrafficControlsStayVisibleInCompactPanels(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		p.filter.SetSelected(filterRX)
		p.search.SetText("01 03")
		p.total = 1000000
		p.setPaused(true)
		for _, width := range []float32{600, 420, 320} {
			p.root.Resize(fyne.NewSize(width, 340))
			for _, text := range []string{"继续", "清空", "复制", "保存", "清除筛选"} {
				buttons := findButtons(p.root, text)
				if len(buttons) != 1 {
					t.Fatalf("missing button %s", text)
				}
				b := buttons[0]
				pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(b)
				base := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.root)
				if pos.X < base.X || pos.X+b.Size().Width > base.X+width+1 {
					t.Errorf("width %.0f: %s outside visible panel", width, text)
				}
				listPos := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.list)
				if pos.Y+b.Size().Height > listPos.Y+1 {
					t.Errorf("width %.0f: %s overlaps packet list", width, text)
				}
			}
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.search)
			base := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.root)
			if p.search.Size().Width < 140 || pos.X+p.search.Size().Width > base.X+width+1 {
				t.Errorf("width %.0f: search input clipped", width)
			}
			if p.count.Size().Width > width+1 || p.count.Size().Height < p.count.MinSize().Height {
				t.Errorf("width %.0f: counter clipped", width)
			}
		}
		p.resetBtn.OnTapped()
		if p.filter.Selected != filterAll || p.search.Text != "" {
			t.Error("visible reset did not clear filters")
		}
	})
}

func TestTrafficExplainsEmptyFilteredAndPausedStates(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		visibleText := func(want string) bool {
			var walk func(fyne.CanvasObject) bool
			walk = func(o fyne.CanvasObject) bool {
				if !o.Visible() {
					return false
				}
				if label, ok := o.(*widget.Label); ok {
					return strings.Contains(label.Text, want)
				}
				if c, ok := o.(*fyne.Container); ok {
					for _, child := range c.Objects {
						if walk(child) {
							return true
						}
					}
				}
				return false
			}
			return walk(p.root)
		}
		if !visibleText("暂无报文") {
			t.Error("empty traffic panel offers no explanation")
		}
		p.push(modbus.Packet{Dir: modbus.DirTX, Raw: []byte{1, 3}})
		p.flush()
		if visibleText("暂无报文") {
			t.Error("empty message masks arriving packets")
		}
		p.search.SetText("FF")
		if !visibleText("没有匹配") {
			t.Error("filtering to zero looks like no traffic")
		}
		p.clear()
		p.setPaused(true)
		p.push(modbus.Packet{Dir: modbus.DirRX, Raw: []byte{0xFF}})
		p.flush()
		if !visibleText("显示已暂停") {
			t.Error("paused buffering looks like missing communication")
		}
		p.setPaused(false)
		if visibleText("显示已暂停") || len(p.view) != 1 {
			t.Error("resume did not reveal buffered match")
		}
	})
}

func TestAIHeaderActionsFitNarrowWindow(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		tool.win.Resize(fyne.NewSize(400, 600))
		root := tool.win.Content()
		if root.Size().Width > 401 {
			t.Errorf("assistant minimum width prevents compact sizing: %.0f", root.Size().Width)
		}
		for _, text := range []string{"使用当前选择", "查看发送内容", "模型设置", "复制报告", "导出报告"} {
			buttons := findButtons(root, text)
			if len(buttons) != 1 {
				t.Fatalf("missing AI button %s", text)
			}
			b := buttons[0]
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(b)
			if pos.X+b.Size().Width > 401 {
				t.Errorf("%s exceeds narrow assistant", text)
			}
			if pos.Y+b.Size().Height > tool.tabs.Position().Y+1 {
				t.Errorf("%s overlaps assistant tabs", text)
			}
		}
	})
}

func TestHistoryFilteringNeverOffersHiddenResumeAction(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := newTrafficPanel(ws)
		p.pauseBtn.Hide()
		p.setPackets([]modbus.Packet{{Dir: modbus.DirTX, Raw: []byte{1, 3}}})
		p.list.Select(0)
		p.search.SetText("FF")
		if p.paused || strings.Contains(p.count.Text, "已暂停") || !strings.Contains(p.empty.Text, "没有匹配") {
			t.Error("history filter offers an unavailable resume action")
		}
		p.resetBtn.OnTapped()
		p.clear()
		if !strings.Contains(p.empty.Text, "暂无报文") {
			t.Error("empty history still looks paused")
		}
	})
}

func TestTrafficEmptyHintUsesAvailableWidth(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := ws.traffic
		p.root.Resize(fyne.NewSize(320, 340))
		if p.empty.Size().Width < 280 || p.empty.Size().Width > 320 {
			t.Error("wrapped hint is measured at an unusable width")
		}
	})
}

func TestReadControlsAndDiagnosisFitNarrowWindow(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		w.setDiagnosis(diagnosis{Text: "读取失败：地址或响应内容不符合请求，请核对设备地址范围。", Hint: "核对起始地址和寄存器数量。", Action: "检查读取范围"})
		for _, width := range []float32{620, 380, 300} {
			w.body.Resize(fyne.NewSize(width, 400))
			base := fyne.CurrentApp().Driver().AbsolutePositionForObject(w.body)
			for _, o := range []fyne.CanvasObject{w.bar.fn, w.bar.kind, w.bar.order, w.bar.raw, w.writeBtn, w.pauseBtn, w.actionBtn, w.aiBtn} {
				pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(o)
				if pos.X < base.X || pos.X+o.Size().Width > base.X+width+1 {
					t.Errorf("read width %.0f: control clipped", width)
				}
			}
			if w.errLbl.Size().Width < width-24 {
				t.Errorf("read width %.0f: error text squeezed by action buttons", width)
			}
			bottom := fyne.CurrentApp().Driver().AbsolutePositionForObject(w.tableBox).Y
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(w.aiBtn)
			if pos.Y+w.aiBtn.Size().Height > bottom+1 {
				t.Errorf("read width %.0f: diagnosis actions overlap registers", width)
			}
		}
	})
}

func TestReadHeaderNamesConnectionAndPollingState(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		assertState := func(want string) {
			w.refresh()
			var walk func(fyne.CanvasObject) bool
			walk = func(o fyne.CanvasObject) bool {
				if !o.Visible() {
					return false
				}
				if label, ok := o.(*widget.Label); ok {
					return label.Text == want
				}
				if c, ok := o.(*fyne.Container); ok {
					for _, child := range c.Objects {
						if walk(child) {
							return true
						}
					}
				}
				return false
			}
			if !walk(w.head) {
				t.Errorf("read header does not explain state %s", want)
			}
		}
		assertState("未连接")
		ws.session = &session{}
		assertState("等待数据")
		w.paused = true
		assertState("已暂停")
		w.paused = false
		w.mu.Lock()
		w.err = modbus.ErrTimeout
		w.mu.Unlock()
		assertState("读取失败")
		w.mu.Lock()
		w.err = nil
		w.lastOK = time.Now()
		w.regs = []uint16{1}
		w.mu.Unlock()
		assertState("读取正常")
		ws.session = nil // synthetic UI fixture has no transport to close
	})
}

func TestShortReadWindowCanScrollThroughEntireHeader(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		w.setDiagnosis(diagnosis{Text: "读取失败，需要核对完整请求范围。", Hint: "核对设备手册中的起始地址、寄存器数量和响应格式。", Action: "检查读取范围"})
		w.body.Resize(fyne.NewSize(300, 160))
		w.headScroll.ScrollToBottom()
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(w.statusLbl)
		viewport := fyne.CurrentApp().Driver().AbsolutePositionForObject(w.headScroll)
		if pos.Y+w.statusLbl.Size().Height > viewport.Y+w.headScroll.Size().Height+1 {
			t.Error("bottom of wrapped header cannot be reached by scrolling")
		}
		if w.tableBox.Size().Height < 20 {
			t.Error("header leaves no space for register table")
		}
	})
}

func TestReadWindowsPreferMoreSpaceForRegisters(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Qty = 2
		w := ws.addWindow(d)
		if size := w.prefSize(); size.Width < 880 || size.Height < 340 {
			t.Errorf("reading window remains too small: %v", size)
		}
		if ws.mainSplit.Offset < .70 {
			t.Error("reading area retains too little of the workspace height")
		}
		// The packet panel must not hold the split at its old three-row header height.
		for _, c := range []struct {
			size  fyne.Size
			share float32
		}{{fyne.NewSize(1040, 680), .68}, {fyne.NewSize(960, 620), .6}} {
			ws.win.Resize(c.size)
			ws.win.Resize(c.size) // the header measures its rows at the width of the previous pass
			if share := ws.tiles.Size().Height / ws.mainSplit.Size().Height; share < c.share {
				t.Errorf("window %v: reading area gets only %.0f%% of the workspace height", c.size, share*100)
			}
			list := ws.traffic.list
			head := ws.traffic.root.(*fyne.Container).Objects[0]
			at := fyne.CurrentApp().Driver().AbsolutePositionForObject
			if list.Size().Height < 28 || at(list).Y < at(head).Y+head.Size().Height {
				t.Errorf("window %v: packet header %v covers the list %v", c.size, head.Size(), list.Size())
			}
		}
		ws.win.Resize(fyne.NewSize(960, 620))
		ws.mdi.setMaxed(true)
		if w.tableBox.Size().Height < 100 {
			t.Errorf("enlarged reading area leaves too few table rows: %.0f", w.tableBox.Size().Height)
		}
	})
}
