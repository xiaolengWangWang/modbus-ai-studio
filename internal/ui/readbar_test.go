package ui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// 点被压在下面的读取窗口露出来的标题栏，按真实的点击路由落到它，设为当前窗口并提到最上面。
func TestTapTitleActivatesWindow(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.win.Resize(fyne.NewSize(1280, 820))
		ws.loadDemo()
		w2 := ws.windows[1]
		if ws.mdi.top() == w2.inner {
			t.Fatal("打开示例后窗口 2 应在下面")
		}
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(w2.inner).AddXY(80, 8)
		test.TapCanvas(ws.win.Canvas(), pos)
		if ws.current() != w2 || ws.mdi.top() != w2.inner {
			t.Errorf("点窗口 2 的标题栏应设为当前窗口并提到最上面，当前是窗口 %d", ws.current().no)
		}
		pressShortcut(ws, fyne.KeyT, false) // 新建读取窗口成为当前窗口
		if n := len(ws.windows); ws.current() != ws.windows[n-1] {
			t.Error("新建的读取窗口应成为当前窗口")
		}
	})
}

// 控制条：功能码、格式、字节序、原始值，选了立即生效；线圈窗口没有格式和字节序。
func TestReadBar(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	tap(ws.connBtn)
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty = 700, 6
		w = ws.addWindow(d)
	})
	waitFor(t, 5*time.Second, "连接并读到数据", func() bool { return hasData(w) })
	// 设备上是 CDAB 的 FLOAT32 85.5 和 0x1234
	f, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
	var c *modbus.Client
	locked(func() { c = ws.session.client })
	if _, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncWriteMultipleRegisters, Address: 700, Values: append(f, 0x1234)}); err != nil {
		t.Fatal(err)
	}

	locked(func() {
		if w.bar.fn.Selected != "03 保持寄存器" || w.bar.kind.Selected != "Signed" || w.bar.order.Selected != "AB" || !slices.Equal(w.bar.order.Options, []string{"AB", "BA"}) {
			t.Errorf("控制条应显示当前定义：%s %s %s %v", w.bar.fn.Selected, w.bar.kind.Selected, w.bar.order.Selected, w.bar.order.Options)
		}
		w.bar.kind.SetSelected("FLOAT32")
		if w.def.Kind != kindFloat32 || !slices.Equal(w.bar.order.Options, []string{"ABCD", "CDAB", "BADC", "DCBA"}) || w.bar.order.Selected != "ABCD" {
			t.Errorf("换成 FLOAT32：%s，字节序选项 %v %s", w.def.Kind, w.bar.order.Options, w.bar.order.Selected)
		}
		w.bar.order.SetSelected("CDAB")
		if w.def.Order != modbus.OrderCDAB || !strings.Contains(w.title(), "FLOAT32 CDAB") {
			t.Errorf("字节序应改成 CDAB：%s，标题 %s", w.def.Order, w.title())
		}
	})
	waitFor(t, 5*time.Second, "按 CDAB 显示 85.5", func() bool { v, _ := w.valueText(0); return v == "85.5" })

	locked(func() {
		w.bar.kind.SetSelected("Hex")
		w.bar.order.SetSelected("BA")
		if w.def.Order.For(modbus.TypeUint16) != modbus.OrderBA || !strings.Contains(w.title(), "Hex BA") {
			t.Errorf("16 位格式应能字节交换：%s %s", w.def.Order, w.title())
		}
		w.bar.raw.SetChecked(true)
		if !slices.Contains(w.cols, colRaw) {
			t.Errorf("打开原始值后应多一列：%v", w.cols)
		}
	})
	waitFor(t, 5*time.Second, "字节交换后的 Hex 和原始值", func() bool {
		v, _ := w.valueText(2) // 40703 写的是 0x1234
		return v == "0x3412" && w.rawText(2) == "1234"
	})

	locked(func() {
		w.bar.fn.SetSelected("04 输入寄存器")
		if w.def.Function != modbus.FuncReadInputRegisters || !strings.Contains(w.title(), "30701") {
			t.Errorf("功能码应改成 04：%s %s", w.def.Function, w.title())
		}
		w.bar.fn.SetSelected("01 线圈")
		if w.def.Function != modbus.FuncReadCoils || !w.bar.kind.Disabled() || !w.bar.order.Disabled() || !w.bar.raw.Disabled() || slices.Contains(w.cols, colRaw) {
			t.Errorf("线圈窗口没有格式、字节序和原始值：%s kind=%v order=%v raw=%v cols=%v", w.def.Function, w.bar.kind.Disabled(), w.bar.order.Disabled(), w.bar.raw.Disabled(), w.cols)
		}
	})
	tap(ws.connBtn)
}

// 点表窗口里，控制条的字节序作用于本窗口的 32 / 64 位点，别的窗口的点不变。
func TestReadBarPointOrder(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "温度", Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "压力", Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 100, Name: "流量", Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1},
		}))
		for _, start := range []uint16{0, 100} {
			d := defaultDef()
			d.Start, d.Qty, d.Kind = start, 4, kindPoint
			ws.addWindow(d)
		}
		w1, w2 := ws.windows[0], ws.windows[1]
		if w1.bar.order.Disabled() || w1.bar.order.Selected != "ABCD" {
			t.Fatalf("点表窗口的字节序应可选，当前 ABCD：%v %s", w1.bar.order.Disabled(), w1.bar.order.Selected)
		}
		w1.bar.order.SetSelected("CDAB")
		for off, want := range map[uint16]modbus.ByteOrder{0: modbus.OrderCDAB, 2: modbus.OrderCDAB, 100: modbus.OrderABCD} {
			if p, _ := ws.points.get(modbus.AreaHoldingRegisters, off); p.Order != want {
				t.Errorf("40%03d 字节序 %s，期望 %s", off+1, p.Order, want)
			}
		}
		if w1.bar.order.Selected != "CDAB" || w2.bar.order.Selected != "ABCD" {
			t.Errorf("两个窗口的控制条应各自显示：%s %s", w1.bar.order.Selected, w2.bar.order.Selected)
		}
		w1.tapCell(widget.TableCellID{Row: 0, Col: 2})
		if ws.current() != w1 {
			t.Error("点了窗口 1 的值应成为当前窗口")
		}
	})
}

func TestByteOrderImmediatelyReinterpretsPausedAndOfflineData(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Kind, d.Order, d.Qty = kindFloat32, modbus.OrderABCD, 4
		w := ws.addWindow(d)
		w.setPaused(true)
		regs, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
		w.mu.Lock()
		w.regs = append(regs, regs...)
		gen := w.gen
		w.mu.Unlock()
		w.bar.order.SetSelected("CDAB")
		if v, _ := w.valueText(0); v != "85.5" {
			t.Errorf("byte-order change must reinterpret existing data immediately, got %q", v)
		}
		w.mu.Lock()
		if len(w.regs) != 4 || w.gen != gen {
			t.Error("display-only change cleared data or restarted polling")
		}
		w.mu.Unlock()
		if !w.paused || w.bar.order.Selected != "CDAB" {
			t.Error("pause or toolbar state was lost")
		}
	})
}

func TestByteOrderDialogAppliesToRawWindowAndLocalFormats(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Kind, d.Order, d.Qty = kindFloat32, modbus.OrderABCD, 4
		w := ws.addWindow(d)
		regs, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
		w.mu.Lock()
		w.regs = append(regs, regs...)
		w.mu.Unlock()
		w.tapCell(w.cellOf(0, 1))
		ws.showPointOrderDialog(w)
		var choice *widget.Select
		for _, s := range findSelects(ws.win.Canvas().Overlays().Top()) {
			if slices.Contains(s.Options, "CDAB") {
				choice = s
			}
		}
		if choice == nil {
			t.Fatal("byte-order choices unavailable")
		}
		choice.SetSelected("CDAB")
		pressButton(t, ws, "应用")
		if v, _ := w.valueText(0); v != "85.5" {
			t.Errorf("dialog did not change the selected raw value: %q", v)
		}
		clearOverlays(ws)
		d.Kind = kindPoint
		mixed := ws.addWindow(d)
		if err := mixed.setRegisterFormat(0, 2, kindFloat32, modbus.OrderABCD); err != nil {
			t.Fatal(err)
		}
		mixed.mu.Lock()
		mixed.regs = append(regs, regs...)
		mixed.mu.Unlock()
		if mixed.bar.order.Disabled() {
			t.Error("local FLOAT32 format has no usable order control")
		}
		mixed.bar.order.SetSelected("CDAB")
		if v, _ := mixed.valueText(0); v != "85.5" {
			t.Errorf("local format ignored the window order: %q", v)
		}
	})
}

func TestLocalByteOrderCorrectionRemovesObsoleteWholeWindowHint(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Kind, d.Qty, d.Order = kindFloat32, 2, modbus.OrderABCD
		w := ws.addWindow(d)
		w.setPaused(true)
		regs, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
		w.mu.Lock()
		w.regs = regs
		w.mu.Unlock()
		w.refresh()
		if !w.hintLbl.Visible() {
			t.Fatal("fixture lacks the original byte-order hint")
		}
		if err := w.setRegisterFormat(0, 2, kindFloat32, modbus.OrderCDAB); err != nil {
			t.Fatal(err)
		}
		if w.hintLbl.Visible() || w.actionBtn.Visible() {
			t.Error("corrected local format still shows obsolete ABCD warning")
		}
	})
}

// 边界情况：2000 个线圈切到保持寄存器时数量收到 125；64 位格式的字节序是八字母写法，切回 32 位按同一类换算；
// 点表窗口没有多寄存器点时字节序不可选；ASCII + BA + 原始值保存工作区再打开不丢。
func TestReadBarEdgeCases(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var data []byte
	locked(func() {
		d := defaultDef()
		d.Function, d.Qty = modbus.FuncReadCoils, 2000
		w := ws.addWindow(d)
		w.bar.fn.SetSelected("03 保持寄存器")
		if w.def.Qty != modbus.MaxReadRegisters {
			t.Errorf("2000 个线圈切到保持寄存器，数量应收到 %d，实际 %d", modbus.MaxReadRegisters, w.def.Qty)
		}
		w.bar.kind.SetSelected("FLOAT64")
		if !slices.Equal(w.bar.order.Options, []string{"ABCDEFGH", "GHEFCDAB", "BADCFEHG", "HGFEDCBA"}) || w.bar.order.Selected != "ABCDEFGH" {
			t.Errorf("64 位格式的字节序选项 %v，当前 %s", w.bar.order.Options, w.bar.order.Selected)
		}
		w.bar.order.SetSelected("GHEFCDAB")
		w.bar.kind.SetSelected("FLOAT32")
		if w.bar.order.Selected != "CDAB" {
			t.Errorf("GHEFCDAB 换到 32 位应是 CDAB，实际 %s", w.bar.order.Selected)
		}
		w.bar.kind.SetSelected("点表")
		if !w.bar.order.Disabled() {
			t.Error("点表窗口里没有多寄存器点时字节序不可选")
		}
		w.bar.kind.SetSelected("ASCII")
		w.bar.order.SetSelected("BA")
		w.bar.raw.SetChecked(true)
		var err error
		if data, err = ws.encodeWorkspace(); err != nil {
			t.Fatal(err)
		}
	})
	dst := openWS(t, a, false)
	locked(func() {
		if err := dst.applyWorkspace(data); err != nil {
			t.Fatal(err)
		}
		w := dst.windows[0]
		if w.def.Kind != kindASCII || w.def.Order.For(modbus.TypeUint16) != modbus.OrderBA || !w.def.Raw ||
			w.bar.order.Selected != "BA" || !w.bar.raw.Checked {
			t.Errorf("工作区应保留 ASCII、BA 和原始值：%+v，控制条 %s %v", w.def, w.bar.order.Selected, w.bar.raw.Checked)
		}
	})
}
