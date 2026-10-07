package ui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

func registerTypeAction(t *testing.T, w *readWindow) func() {
	t.Helper()
	for _, item := range w.cellMenu().Items {
		if item.Label == "设置寄存器数据类型…" {
			return item.Action
		}
	}
	t.Fatal("右键菜单应支持设置选中寄存器的数据类型")
	return nil
}

func TestRegisterFormats64BitAndInvalidWorkspace(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		w.regs = []uint16{0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff, 0xffff}
		if err := w.setRegisterFormat(1, 8, kindUint64, modbus.OrderGHEFCDAB); err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{1, 5} {
			if got, _ := w.valueText(i); got != "18446744073709551615" {
				t.Errorf("64 位整数必须精确显示：%s", got)
			}
		}
		w.selectCell(w.cellOf(8, 1), 0)
		if w.sel != 5 {
			t.Errorf("64 位组合值应对齐到地址 5，实际 %d", w.sel)
		}
		w.moveSel(-1, 0)
		if w.sel != 1 {
			t.Errorf("方向键应移动一个 64 位值，实际 %d", w.sel)
		}
		d := w.def
		d.Formats = append(slices.Clone(d.Formats), registerFormat{Offset: 2, Kind: kindSigned, Order: modbus.OrderAB})
		data, err := json.Marshal(workspaceFile{Mode: modbus.ModeTCP, Windows: []readDef{d}})
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.applyWorkspace(data); err == nil {
			t.Fatal("工作区里重叠的寄存器类型应被拒绝")
		}
		if ws.windows[0] != w {
			t.Error("无效工作区不能改变现有窗口")
		}
	})
}

func TestRegisterFormatsRangeSelectionAndBoundaries(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		other := ws.addWindow(defaultDef())
		w.regs = []uint16{0x4170, 0, 0x41a0, 0, 0xffff, 1, 2, 3, 4, 5}
		w.selectCell(w.cellOf(0, 1), 0)
		w.selectCell(w.cellOf(3, 1), fyne.KeyModifierShift)
		if start, count := w.selectionRange(); start != 0 || count != 4 {
			t.Fatalf("Shift 应选中四个寄存器：%d %d", start, count)
		}
		if err := w.setRegisterFormat(0, 4, kindFloat32, modbus.OrderABCD); err != nil {
			t.Fatal(err)
		}
		for i, want := range []string{"15.0", "—", "20.0", "—", "-1"} {
			if got, _ := w.valueText(i); got != want {
				t.Errorf("寄存器 %d：%s，期望 %s", i, got, want)
			}
		}
		if other.def.Kind != kindSigned || len(other.def.Formats) != 0 {
			t.Error("局部格式不能影响其他窗口")
		}
		if copied := w.selectedText(); len(strings.Split(copied, "\n")) != 2 || !strings.Contains(copied, "20.0") {
			t.Errorf("复制应包含两个组合值：%s", copied)
		}
		before := slices.Clone(w.def.Formats)
		for _, c := range []struct {
			start, count int
			kind         valueKind
		}{
			{9, 2, kindFloat32}, {0, 3, kindFloat32}, {-1, 2, kindFloat32}, {0, 4, kindPoint},
		} {
			if err := w.setRegisterFormat(c.start, c.count, c.kind, modbus.OrderABCD); err == nil {
				t.Errorf("非法范围应被拒绝：%+v", c)
			}
			if !slices.Equal(w.def.Formats, before) {
				t.Fatal("无效设置不能修改格式")
			}
		}
		w.selectCell(w.cellOf(0, 1), 0)
		w.clearRegisterFormats()
		if got, _ := w.valueText(0); got != "16752" {
			t.Errorf("恢复默认后应按 Signed 显示，实际 %s", got)
		}
		if got, _ := w.valueText(2); got != "20.0" {
			t.Errorf("清除一组格式不能影响其他组，实际 %s", got)
		}
		w.sel = -1
		w.table.UnselectAll()
		cell := unwrap(w.table.CreateCell()).(*cell)
		w.updateCell(w.cellOf(4, 1), cell)
		cell.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(2, 2*rowHeight())}})
		cell.DragEnd()
		if start, count := w.selectionRange(); start != 4 || count != 3 {
			t.Errorf("拖选应包含三个寄存器，实际 %d %d", start, count)
		}
		ws.redefine(w, func(d *readDef) { d.Raw = true })
		if len(w.def.Formats) != 1 {
			t.Error("切换原始值不能丢失局部格式")
		}
		ws.redefine(w, func(d *readDef) { d.Slave = 2 })
		if len(w.def.Formats) != 0 {
			t.Error("切换设备不能沿用上一个设备的局部格式")
		}
	})
}

func TestRegisterFormatsOverlappingDefaultsAndWriteType(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Kind = kindFloat32
		w := ws.addWindow(d)
		w.regs = []uint16{0x1234, 0xffff, 0, 0x4170, 0, 1, 2, 3, 4, 5}
		if err := w.setRegisterFormat(1, 1, kindSigned, modbus.OrderAB); err != nil {
			t.Fatal(err)
		}
		if got, _ := w.valueText(0); got != "4660" {
			t.Errorf("被局部覆盖的默认组合值剩余部分应按原始值显示，实际 %s", got)
		}
		if got, _ := w.valueText(1); got != "-1" {
			t.Errorf("单个寄存器应按 Signed 显示，实际 %s", got)
		}
		if err := w.setRegisterFormat(2, 2, kindFloat32, modbus.OrderCDAB); err != nil {
			t.Fatal(err)
		}
		w.selectCell(w.cellOf(3, 1), 0)
		ws.session = &session{mode: modbus.ModeTCP}
		defer func() { ws.session = nil }()
		if !w.canWrite() {
			t.Fatal("单个完整的 FLOAT32 值应可写")
		}
		ws.showWrite(w)
		if text := overlayText(ws); !strings.Contains(text, "FLOAT32") || !strings.Contains(text, "CDAB") || !strings.Contains(text, "15.0") {
			t.Errorf("写入应使用局部类型、字节序和当前值：%s", text)
		}
		clearOverlays(ws)
		ws.setReadOnly(true)
		if w.canWrite() {
			t.Error("局部类型不能绕过只读模式")
		}
		ws.setReadOnly(false)
		w.selectCell(w.cellOf(5, 1), fyne.KeyModifierShift)
		if w.canWrite() {
			t.Error("选中多个值时不能触发单值写入")
		}
		ws.points = newPointTable([]point{{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1}})
		d = defaultDef()
		d.Kind = kindPoint
		ws.session = nil // 建窗口时不启动轮询；只验证本地写入权限。
		ro := ws.addWindow(d)
		ws.session = &session{mode: modbus.ModeTCP}
		if err := ro.setRegisterFormat(0, 2, kindFloat32, modbus.OrderCDAB); err != nil {
			t.Fatal(err)
		}
		ro.selectCell(ro.cellOf(0, 2), 0)
		if ro.canWrite() {
			t.Error("局部类型不能绕过点表中的只读属性")
		}
	})
}

func TestRegisterTypeDialogChangesOnlySelectedRegisters(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		w.mu.Lock()
		w.regs = []uint16{0, 0, 0x4170, 0, 0xffff, 2, 3, 4, 5, 6}
		w.mu.Unlock()
		w.tapCell(widget.TableCellID{Row: 1, Col: 1})
		registerTypeAction(t, w)()
		entries, selects, _ := overlayWidgets(ws)
		if len(entries) != 2 || len(selects) != 2 {
			t.Fatalf("应能设置起始地址、数量、类型和字节序：%s", overlayText(ws))
		}
		entries[0].SetText("1")
		entries[1].SetText("2")
		selects[0].SetSelected("FLOAT32")
		selects[1].SetSelected("CDAB")
		pressButton(t, ws, "确定")
		for i, want := range []string{"0", "15.0", "—", "0", "-1"} {
			if got, _ := w.valueText(i); got != want {
				t.Errorf("寄存器 %d 应显示 %s，实际 %s", i, want, got)
			}
		}
		if w.def.Kind != kindSigned {
			t.Error("局部格式不能改变其他寄存器的窗口默认格式")
		}
		w.tapCell(widget.TableCellID{Row: 2, Col: 1})
		if w.sel != 1 {
			t.Errorf("组合值的第二个寄存器应定位到起始地址，实际 %d", w.sel)
		}
		if text := w.selectedText(); !strings.Contains(text, "15.0") {
			t.Errorf("复制应包含按局部类型解码的值：%s", text)
		}
		data, err := ws.encodeWorkspace()
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.applyWorkspace(data); err != nil {
			t.Fatal(err)
		}
		w = ws.windows[0]
		w.mu.Lock()
		w.regs = []uint16{0, 0, 0x4170, 0, 0xffff, 2, 3, 4, 5, 6}
		w.mu.Unlock()
		if got, _ := w.valueText(1); got != "15.0" {
			t.Errorf("重开工作区后应保留寄存器格式，实际 %s", got)
		}
		w.table.Refresh()
		w.selectCell(w.cellOf(1, 1), 0)
	})
	snapshotPNG(t, ws.win, "modbus-ai-mixed.png")
}
