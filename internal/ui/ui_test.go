package ui

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// locked 在 uiMu 下执行，和后台 goroutine 发起的界面更新互斥。
func locked(fn func()) {
	uiMu.Lock()
	defer uiMu.Unlock()
	fn()
}

func tap(b *widget.Button) { locked(func() { test.Tap(b) }) }

func closeWS(ws *Workspace) {
	locked(func() {
		if !ws.closed {
			ws.win.Close()
		}
	})
}

// openWS 在锁内打开主窗口，测试结束时关闭，停止它的后台刷新。
func openWS(t *testing.T, a fyne.App, demo bool) *Workspace {
	var ws *Workspace
	locked(func() {
		ws = open(a, "test", int(winSeq.Add(1)))
		if demo {
			ws.loadDemo()
		}
	})
	t.Cleanup(func() { closeWS(ws) })
	return ws
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		var done bool
		locked(func() { done = ok() })
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v 内没有等到：%s", d, what)
		}
	}
}

func hasData(w *readWindow) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.regs != nil && w.err == nil
}

func snapshotPNG(t *testing.T, w fyne.Window, name string) {
	dir := os.Getenv("MODBUS_AI_SNAPSHOT")
	if dir == "" {
		return
	}
	time.Sleep(300 * time.Millisecond)
	var img image.Image
	locked(func() { img = w.Canvas().Capture() })
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// 用 Fyne 软件渲染在无界面环境跑一遍：连接内置模拟器，等数据到达，检查表格内容；
// 窗口 3 的地址段应答慢于超时，应自动分析出“晚到响应”，一键把超时调大后恢复正常。
// 设置 MODBUS_AI_SNAPSHOT=<目录> 时保存截图，供布局检查（设计文档 13.10）。
func TestUIWithBuiltinSimulator(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, true) // 打开换热站示例时自动连接内置模拟器，不用点“连接”
	w1, w2, w3 := ws.windows[0], ws.windows[1], ws.windows[2]
	waitFor(t, 6*time.Second, "窗口 1 收到数据、窗口 3 超时", func() bool {
		w3.mu.Lock()
		slow := w3.errN > 0
		w3.mu.Unlock()
		return hasData(w1) && hasData(w2) && slow
	})

	checks := []struct {
		w    *readWindow
		i    int
		want string
	}{
		{w1, 0, "45.2"},   // 二次供水温度 FLOAT32 CDAB
		{w1, 1, "—"},      // 32 位点的第二个寄存器
		{w1, 4, "13.2"},   // 二次温差 INT16 × 0.1
		{w1, 5, "0x0001"}, // 状态字
		{w1, 14, "42.50"}, // 循环泵频率 × 0.01
		{w1, 18, "0 停止"},  // 补水泵状态（枚举含义）
		{w2, 0, "15.0"},   // 温差设定 40347
		{w2, 6, "1 自动"},   // 运行模式 40353
	}
	for _, c := range checks {
		var got string
		locked(func() { got, _ = c.w.valueText(c.i) })
		if got != c.want {
			t.Errorf("窗口 %d 第 %d 个寄存器：显示 %q，期望 %q", c.w.no, c.i, got, c.want)
		}
	}

	// 晚到响应在保护间隔内到达，诊断应识别出“设备有应答但慢”，并给出一键调大超时
	waitFor(t, 8*time.Second, "窗口 3 的诊断给出调大超时", func() bool {
		return w3.errLbl.Visible() && strings.HasPrefix(w3.actionBtn.Text, "超时改为") && w3.actionBtn.Visible()
	})
	var hint string
	locked(func() { hint = w3.hintLbl.Text })
	if !strings.Contains(hint, "设备有应答") {
		t.Errorf("诊断说明：%q", hint)
	}
	// 主界面截图（README 用）：照常用的摆法，窗口 1 在左，窗口 2、3 在右边层叠，窗口 3 在最上面露出错误分析
	locked(func() {
		area := ws.mdi.size
		right := area.Width - w1.size.Width - 6
		w1.pos = fyne.Position{}
		w2.pos, w2.size.Width = fyne.NewPos(area.Width-right, 0), right
		w3.size.Width = right - cascadeStep()
		w3.pos = fyne.NewPos(area.Width-w3.size.Width, area.Height-w3.size.Height)
		ws.setCurrent(w3)
		ws.mdi.box.Refresh()
	})
	snapshotPNG(t, ws.win, "modbus-ai-ui.png")
	tap(w3.actionBtn)
	locked(func() {
		if ws.timeout != 2*time.Second || ws.session.client.Timeout() != 2*time.Second {
			t.Errorf("超时应改为 2000 ms，界面 %v，连接 %v", ws.timeout, ws.session.client.Timeout())
		}
		// 窗口 3 扫描周期 5 s，缩短后立即重新轮询，不必等下一轮
		ws.redefine(w3, func(d *readDef) { d.Scan = 300 * time.Millisecond })
	})
	waitFor(t, 5*time.Second, "窗口 3 恢复正常", func() bool { return hasData(w3) && !w3.errLbl.Visible() })

	// 单击选中值：解析面板给出多解释视图
	locked(func() { w2.table.Select(widget.TableCellID{Row: 1, Col: 2}) })
	var text string
	locked(func() { text = ws.inspect.text })
	snapshotPNG(t, ws.win, "modbus-ai-inspect.png")
	if w2.sel != 0 || !strings.Contains(text, "40347") || !strings.Contains(text, "FLOAT32 15.0（合理）") || !strings.Contains(text, "温差设定") {
		t.Errorf("选中第二个寄存器应对齐到 40347，解析：\n%s", text)
	}

	// 通信报文：选中一行自动暂停并解析
	locked(func() {
		ws.traffic.flush()
		ws.traffic.list.Select(0)
	})
	locked(func() { text = ws.inspect.text })
	if !ws.traffic.paused || !strings.Contains(text, "报文解析") || !strings.Contains(text, "Slave ID") {
		t.Errorf("选中报文后应暂停并解析，paused=%v：\n%s", ws.traffic.paused, text)
	}
	snapshotPNG(t, ws.win, "modbus-ai-packet.png")
	locked(func() { ws.traffic.setPaused(false) })
	tap(ws.connBtn) // 断开，停止轮询和内置模拟器
}

// 多开：两个主窗口各自连接内置模拟器，互不影响；断开一个，另一个继续轮询。
func TestMultipleWindows(t *testing.T) {
	a := test.NewTempApp(t)
	one := openWS(t, a, true)
	var two *Workspace
	locked(func() {
		one.proto.SetSelected(protoTCP)
		two = one.openNew()
	})
	t.Cleanup(func() { closeWS(two) })
	if two.proto.Selected != protoTCP || !two.useSim.Checked {
		t.Errorf("新窗口应沿用协议和内置模拟器设置：%s %v", two.proto.Selected, two.useSim.Checked)
	}
	if one.no == two.no || !strings.Contains(two.win.Title(), "窗口") {
		t.Fatalf("窗口编号 %d / %d，标题 %q", one.no, two.no, two.win.Title())
	}
	if len(two.windows) != 0 || two.session != nil {
		t.Errorf("新主窗口应为空且未连接：%d 个读取窗口，session=%v", len(two.windows), two.session)
	}
	locked(func() { two.addWindow(defaultDef()) })
	tap(two.connBtn) // 窗口 1 是示例窗口，已自动连接
	waitFor(t, 5*time.Second, "两个窗口都有数据", func() bool { return hasData(one.windows[0]) && hasData(two.windows[0]) })
	tap(one.connBtn)
	var before int
	two.windows[0].mu.Lock()
	before = two.windows[0].tx
	two.windows[0].mu.Unlock()
	waitFor(t, 3*time.Second, "窗口 2 继续轮询", func() bool {
		two.windows[0].mu.Lock()
		defer two.windows[0].mu.Unlock()
		return two.windows[0].tx > before+1 && one.session == nil
	})
	locked(func() { two.win.Close() })
	if !two.closed || two.session != nil {
		t.Error("关闭主窗口应断开它的连接")
	}
}

// 主窗口初始为空：没有读取窗口、不连接、报文为空，只显示新建提示。
func TestInitiallyEmpty(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	time.Sleep(300 * time.Millisecond)
	locked(func() {
		if len(ws.windows) != 0 || ws.session != nil || ws.connecting || ws.traffic.total != 0 {
			t.Errorf("初始应为空：%d 个读取窗口，session=%v，connecting=%v，报文 %d 条", len(ws.windows), ws.session, ws.connecting, ws.traffic.total)
		}
		if len(ws.tiles.Objects) != 1 {
			t.Error("应显示新建读取窗口的提示")
		}
	})
}

// 读取窗口是多文档区域里的子窗口（Modbus Poll 的 MDI）：新窗口层叠在上面，不并列；可以最大化（切换窗口保持最大化）、
// 平铺、拖动（不会拖出区域）；标题栏的关闭按钮关掉读取窗口；“窗口”菜单列出全部窗口，当前的打勾。
func TestMDI(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var wins []*readWindow
	locked(func() {
		ws.win.Resize(fyne.NewSize(1280, 820))
		for i := 0; i < 3; i++ {
			w := ws.addWindow(defaultDef())
			ws.setCurrent(w) // 和“新建读取窗口”一样，新窗口成为当前窗口
			wins = append(wins, w)
		}
	})
	locked(func() {
		m := ws.mdi
		step := cascadeStep()
		if m.top() != wins[2].inner || ws.current() != wins[2] {
			t.Fatal("新建的窗口应在最上面并成为当前窗口")
		}
		if wins[1].pos != wins[0].pos.AddXY(step, step) || wins[2].pos != wins[1].pos.AddXY(step, step) {
			t.Errorf("新窗口应层叠：%v %v %v", wins[0].pos, wins[1].pos, wins[2].pos)
		}
		if wins[0].inner.Size().Width >= m.size.Width {
			t.Errorf("子窗口按内容的大小，不铺满：%v / %v", wins[0].inner.Size(), m.size)
		}
		labels := func() (out []string) {
			for _, it := range ws.winMenu.Items {
				l := it.Label
				if it.Checked {
					l += "✓"
				}
				out = append(out, l)
			}
			return out
		}
		if got := labels(); len(got) != 7 || got[6] != wins[2].title()+"✓" || got[0] != "层叠" || got[1] != "平铺" {
			t.Errorf("“窗口”菜单：%q", got)
		}
		ws.winMenu.Items[4].Action() // 列表里的窗口 1
		if ws.current() != wins[0] || m.top() != wins[0].inner {
			t.Error("从“窗口”菜单切到窗口 1")
		}

		wins[0].inner.OnMaximized()
		if !m.maxed || wins[0].inner.Position() != (fyne.Position{}) || wins[0].inner.Size() != m.size || !ws.winMenu.Items[2].Checked {
			t.Errorf("最大化应铺满区域：%v %v / %v", wins[0].inner.Position(), wins[0].inner.Size(), m.size)
		}
		ws.setCurrent(wins[1])
		if wins[1].inner.Size() != m.size {
			t.Error("最大化时切换窗口也铺满")
		}
		wins[0].inner.OnDragged(&fyne.DragEvent{Dragged: fyne.Delta{DX: 50}})
		if wins[0].pos != (fyne.Position{}) {
			t.Error("最大化时不能拖动")
		}
		ws.winMenu.Items[2].Action()
		if m.maxed || wins[0].inner.Size() == m.size {
			t.Error("再点最大化还原")
		}

		m.tile()
		for i, a := range wins {
			for _, b := range wins[i+1:] {
				ra := fyne.NewPos(a.pos.X+a.size.Width, a.pos.Y+a.size.Height)
				if a.pos.X < b.pos.X+b.size.Width-1 && b.pos.X < ra.X-1 && a.pos.Y < b.pos.Y+b.size.Height-1 && b.pos.Y < ra.Y-1 {
					t.Errorf("平铺后窗口 %d 和窗口 %d 重叠：%v %v / %v %v", a.no, b.no, a.pos, a.size, b.pos, b.size)
				}
			}
		}
		m.cascade()
		wins[1].inner.OnDragged(&fyne.DragEvent{Dragged: fyne.Delta{DX: 5000, DY: 5000}})
		if p, s := wins[1].inner.Position(), wins[1].inner.Size(); p.X+s.Width > m.size.Width+0.5 || p.Y+s.Height > m.size.Height+0.5 {
			t.Errorf("拖到区域外应收回来：%v %v / %v", p, s, m.size)
		}
		wins[2].inner.CloseIntercept()
		if len(ws.windows) != 2 || len(m.box.Objects) != 2 || len(labels()) != 6 {
			t.Errorf("标题栏的关闭按钮应关掉读取窗口：%d 个窗口，菜单 %q", len(ws.windows), labels())
		}
	})
}

func TestTile(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	n := len(ws.windows)
	locked(func() {
		for i := 0; i < 4; i++ {
			ws.addWindow(defaultDef())
		}
	})
	if len(ws.windows) != n+4 {
		t.Fatalf("应有 %d 个读取窗口，得到 %d", n+4, len(ws.windows))
	}
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
	})
	if len(ws.tiles.Objects) != 1 {
		t.Fatal("没有读取窗口时应显示提示")
	}
	// 已关闭的窗口不能被异步回调重新启动轮询
	var closedWin *readWindow
	locked(func() {
		closedWin = ws.addWindow(defaultDef())
		ws.removeWindow(closedWin)
		ws.session = &session{}
		closedWin.start()
		ws.session = nil
	})
	if closedWin.stop != nil {
		t.Error("已关闭的读取窗口不应开始轮询")
	}
}
