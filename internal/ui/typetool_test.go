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

// writeFloats 往内置模拟器的保持寄存器 start 起按 CDAB 写几个 FLOAT32，后面补 extra 个 0。
func writeFloats(t *testing.T, ws *Workspace, start uint16, extra int, vals ...float64) {
	t.Helper()
	var regs []uint16
	for _, v := range vals {
		r, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, v)
		regs = append(regs, r...)
	}
	regs = append(regs, make([]uint16, extra)...)
	var c *modbus.Client
	locked(func() { c = ws.session.client })
	if _, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncWriteMultipleRegisters, Address: start, Values: regs}); err != nil {
		t.Fatal(err)
	}
}

func toolHasData(tt *typeTool) bool { return tt.regs != nil || tt.bits != nil }

// 调试窗口：按读取窗口选中的地址填好设置，四种字节序并排解读，找出合理值最多的 CDAB；换数据类型不用重读；
// 应用到读取窗口后按 CDAB 显示；探测功能码列出 03 / 04 / 01 / 02 的结果。
func TestTypeTool(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	tap(ws.connBtn)
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order = 700, 8, kindFloat32, modbus.OrderABCD // 设备是 CDAB，窗口按 ABCD 看是乱的
		w = ws.addWindow(d)
	})
	waitFor(t, 5*time.Second, "连接并读到数据", func() bool { return hasData(w) })
	writeFloats(t, ws, 700, 2, 85.5, -12.25, 1000)

	locked(func() {
		w.tapCell(widget.TableCellID{Row: 0, Col: 1})
		pressShortcut(ws, fyne.KeyB, false)
	})
	var tt *typeTool
	locked(func() { tt = ws.typeTool })
	if tt == nil {
		t.Fatal("⌘B 应打开调试窗口")
	}
	locked(func() {
		if tt.slave.Text != "1" || tt.fn.Selected != "03 保持寄存器" || tt.addr.Text != "40701" || tt.qty.Text != "8" || tt.dtype.Selected != "FLOAT32" {
			t.Errorf("应按读取窗口填好：%s %s %s %s %s", tt.slave.Text, tt.fn.Selected, tt.addr.Text, tt.qty.Text, tt.dtype.Selected)
		}
	})
	waitFor(t, 5*time.Second, "调试窗口读到数据", func() bool { return toolHasData(tt) })
	locked(func() {
		if rows, cols := tt.size(); rows != 4 || cols != 6 {
			t.Errorf("8 个寄存器按 FLOAT32 是 4 行，地址、原始和四种字节序 6 列：%d × %d", rows, cols)
		}
		if tt.best != modbus.OrderCDAB || tt.order.Selected != "CDAB" || tt.header(3) != "CDAB ✓" || !strings.Contains(tt.advice.Text, "很可能是 CDAB") {
			t.Errorf("应建议 CDAB：best=%s 选中 %s 表头 %s 建议 %s", tt.best, tt.order.Selected, tt.header(3), tt.advice.Text)
		}
		if v, imp := tt.cellText(1, 3); v != "-12.25" || imp != widget.MediumImportance {
			t.Errorf("40703 按 CDAB 应是 -12.25：%s %v", v, imp)
		}
		if v, _ := tt.cellText(0, 0); v != "40701" {
			t.Errorf("地址列：%s", v)
		}
		if !strings.HasPrefix(tt.text(), "地址\t原始寄存器\tABCD\tCDAB\tBADC\tDCBA\n40701\t0000 42AB\t") {
			t.Errorf("复制的表格：%q", tt.text())
		}

		// 换成整型：不用重读，两种字节序，没有合理性建议
		tt.dtype.SetSelected("INT16")
		if rows, cols := tt.size(); rows != 8 || cols != 4 || tt.best != "" || !strings.Contains(tt.advice.Text, "整型") ||
			!slices.Equal(tt.order.Options, []string{"AB", "BA"}) {
			t.Errorf("INT16：%d × %d best=%q %s %v", rows, cols, tt.best, tt.advice.Text, tt.order.Options)
		}
		tt.dtype.SetSelected("FLOAT32")
		tt.apply(false)
		if w.def.Kind != kindFloat32 || w.def.Order != modbus.OrderCDAB || w.def.Start != 700 || w.def.Qty != 8 {
			t.Errorf("应用到窗口：%+v", w.def)
		}
	})
	waitFor(t, 5*time.Second, "读取窗口按 CDAB 显示 85.5", func() bool { v, _ := w.valueText(0); return v == "85.5" })

	locked(func() { tt.probeFunctions() })
	waitFor(t, 10*time.Second, "探测功能码完成", func() bool { return strings.Contains(tt.status.Text, "当前功能码 03 能读") })
	locked(func() {
		for _, f := range []string{"03 保持寄存器", "04 输入寄存器", "01 线圈", "02 离散输入"} {
			if !strings.Contains(tt.status.Text, f) {
				t.Errorf("探测结果里应有 %s：%s", f, tt.status.Text)
			}
		}
		tt.addr.SetText("990")
		tt.qty.SetText("20")
		tt.probeFunctions()
	})
	waitFor(t, 10*time.Second, "越界地址探测完成", func() bool { return strings.Contains(tt.status.Text, "四个功能码都读不了") })
	locked(func() {
		if !strings.Contains(tt.status.Text, "异常 02") {
			t.Errorf("越界地址应是异常 02：%s", tt.status.Text)
		}
		tt.read()
	})
	waitFor(t, 5*time.Second, "读越界地址出错", func() bool { return strings.Contains(tt.status.Text, "探测功能码") })

	// 再按 ⌘B 不新开窗口，换成当前读取窗口的设置
	locked(func() {
		pressShortcut(ws, fyne.KeyB, false)
		if ws.typeTool != tt || tt.addr.Text != "40701" {
			t.Errorf("调试窗口只开一个，再打开时换成当前窗口的设置：%s", tt.addr.Text)
		}
		tt.win.Close()
		if ws.typeTool != nil || len(ws.tools) != 0 {
			t.Error("关掉调试窗口后应从工具窗口列表去掉")
		}
	})
	tap(ws.connBtn)
}

// 地址写法和功能码：30001 切到 04，线圈用 Offset；参数错误时不发请求。
func TestTypeToolAddress(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.openTypeTool(nil)
		tt := ws.typeTool
		if !strings.Contains(tt.status.Text, "未连接") {
			t.Errorf("未连接时应提示：%s", tt.status.Text)
		}
		tt.addr.SetText("30011")
		if tt.fn.Selected != "04 输入寄存器" || !strings.Contains(tt.addrInfo.Text, "Offset 10") {
			t.Errorf("30011 应切到 04、Offset 10：%s %s", tt.fn.Selected, tt.addrInfo.Text)
		}
		tt.fn.SetSelected("01 线圈")
		if !tt.dtype.Disabled() || tt.addr.Text != "10" {
			t.Errorf("换到线圈：没有数据类型，地址换成 Offset 写法 10，实际 %q", tt.addr.Text)
		}
		tt.fn.SetSelected("03 保持寄存器")
		if tt.addr.Text != "40011" {
			t.Errorf("换到 03 地址应是 40011：%q", tt.addr.Text)
		}
		tt.fn.SetSelected("01 线圈")
		tt.qty.SetText("3000")
		if _, err := tt.request(); err == nil || !strings.Contains(err.Error(), "1–2000") {
			t.Errorf("线圈数量上限 2000：%v", err)
		}
		tt.qty.SetText("8")
		tt.slave.SetText("0")
		if _, err := tt.request(); err == nil {
			t.Error("Slave 0 不能读")
		}
		if toolKind(modbus.TypeInt16) != kindSigned || toolKind(modbus.TypeUint16) != kindUnsigned || toolKind(modbus.TypeFloat64) != kindFloat64 {
			t.Error("数据类型换成显示格式不对")
		}
		tt.win.Close()
	})
}

// 读取窗口的键盘操作：方向键按值移动（FLOAT32 一次两个寄存器），左右换列，Ctrl+C 复制一行，Enter 写入；右键菜单改格式和字节序。
func TestReadWindowKeysAndMenu(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	tap(ws.connBtn)
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order, d.Rows = 700, 8, kindFloat32, modbus.OrderCDAB, 4
		w = ws.addWindow(d)
	})
	waitFor(t, 5*time.Second, "读到数据", func() bool { return hasData(w) })
	writeFloats(t, ws, 700, 2, 85.5, -12.25, 1000)
	waitFor(t, 5*time.Second, "显示 85.5", func() bool { v, _ := w.valueText(0); return v == "85.5" })

	locked(func() {
		key := func(k fyne.KeyName) { w.table.TypedKey(&fyne.KeyEvent{Name: k}) }
		key(fyne.KeyDown) // 没有选中时选第一个值
		if w.sel != 0 || w.selCell.Col != 1 {
			t.Fatalf("方向键应先选中第一个值：sel=%d col=%d", w.sel, w.selCell.Col)
		}
		key(fyne.KeyDown)
		if w.sel != 2 {
			t.Errorf("FLOAT32 向下应跳到下一个值 40703：sel=%d", w.sel)
		}
		key(fyne.KeyRight)
		if w.sel != 6 {
			t.Errorf("向右应到下一列同一行（每列 4 行）：sel=%d", w.sel)
		}
		key(fyne.KeyDown)
		if w.sel != 6 {
			t.Errorf("到最后一个值停住：sel=%d", w.sel)
		}
		key(fyne.KeyLeft)
		key(fyne.KeyUp)
		key(fyne.KeyUp)
		if w.sel != 0 {
			t.Errorf("回到第一个值：sel=%d", w.sel)
		}
		w.table.TypedShortcut(&fyne.ShortcutCopy{Clipboard: a.Clipboard()})
		if got := a.Clipboard().Content(); got != "40701\t85.5" {
			t.Errorf("Ctrl+C 应复制地址和值：%q", got)
		}
		key(fyne.KeyReturn)
		if len(ws.win.Canvas().Overlays().List()) != 1 {
			t.Error("Enter 应打开写入")
		}
		key(fyne.KeyReturn)
		if len(ws.win.Canvas().Overlays().List()) != 1 {
			t.Error("已有对话框时 Enter 不再叠一个")
		}
		clearOverlays(ws)

		m := w.cellMenu()
		item := func(menu *fyne.Menu, label string) *fyne.MenuItem {
			for _, it := range menu.Items {
				if it.Label == label {
					return it
				}
			}
			t.Fatalf("菜单里没有“%s”", label)
			return nil
		}
		if item(m, "写入…").Disabled || !item(item(m, "显示格式").ChildMenu, "FLOAT32").Checked || !item(item(m, "字节序").ChildMenu, "CDAB").Checked {
			t.Error("右键菜单应可写入，当前格式、字节序打勾")
		}
		item(item(m, "字节序").ChildMenu, "DCBA").Action()
		if w.def.Order != modbus.OrderDCBA {
			t.Errorf("右键菜单改字节序：%s", w.def.Order)
		}
		item(item(w.cellMenu(), "显示格式").ChildMenu, "Hex").Action()
		if w.def.Kind != kindHex {
			t.Errorf("右键菜单改格式：%s", w.def.Kind)
		}
		item(w.cellMenu(), "显示原始值").Action()
		if !w.def.Raw {
			t.Error("右键菜单打开原始值")
		}
		w.showCellMenu(widget.TableCellID{Row: 1, Col: 1}, fyne.NewPos(20, 20))
		if !ws.dialogOpen() || w.sel != 1 {
			t.Errorf("右键应选中单元格并弹出菜单：sel=%d", w.sel)
		}
		clearOverlays(ws)
		item(w.cellMenu(), "调试功能码 / 数据类型 / 字节序…").Action()
		if ws.typeTool == nil || ws.typeTool.addr.Text != "40702" {
			t.Error("右键菜单打开调试窗口，从选中的地址开始")
		}
	})
	tap(ws.connBtn)
}
