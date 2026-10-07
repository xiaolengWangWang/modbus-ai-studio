package ui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// registerFormat 是当前读取窗口内一个值的格式，Offset 是协议地址。
// 32 / 64 位值占用后续 2 / 4 个寄存器；其他窗口和从站不受影响。
type registerFormat struct {
	Offset uint16
	Kind   valueKind
	Order  modbus.ByteOrder
}

type registerValue struct {
	start, width int
	kind         valueKind
	order        modbus.ByteOrder
}

func (d *readDef) retainFormats(previous readDef) {
	if d.Slave != previous.Slave || d.Function != previous.Function || d.Kind != previous.Kind {
		d.Formats = nil
		return
	}
	d.Formats = slices.DeleteFunc(slices.Clone(d.Formats), func(f registerFormat) bool {
		return int(f.Offset) < int(d.Start) || int(f.Offset)+f.Kind.width() > int(d.Start)+d.Qty
	})
}

func (w *readWindow) valueFormat(i int) registerValue {
	d := w.def
	for _, f := range d.Formats {
		start := int(f.Offset) - int(d.Start)
		if i >= start && i < start+f.Kind.width() {
			return registerValue{start, f.Kind.width(), f.Kind, f.Order}
		}
	}
	v := registerValue{i, 1, d.Kind, d.Order}
	if d.bits() {
		return v
	}
	if d.usesPoints() {
		for v.start > 0 && w.ws.points.occupied(d.area(), d.Start+uint16(v.start)) {
			v.start--
		}
		if p, ok := w.ws.points.get(d.area(), d.Start+uint16(v.start)); ok {
			v.width = p.regs()
		}
	} else {
		v.width = d.Kind.width()
		v.start = i - i%v.width
	}
	// 局部格式覆盖默认组合值的一部分时，剩余寄存器按原始 UINT16 显示，
	// 避免两种类型重复使用同一个寄存器。
	for _, f := range d.Formats {
		start := int(f.Offset) - int(d.Start)
		if start < v.start+v.width && start+f.Kind.width() > v.start {
			return registerValue{i, 1, kindUnsigned, modbus.OrderAB}
		}
	}
	return v
}

func (w *readWindow) displayDef(i int) readDef {
	d := w.def
	f := w.valueFormat(i)
	d.Kind, d.Order = f.kind, f.order
	return d
}

func (d readDef) validateFormats() error {
	used := map[int]bool{}
	for _, f := range d.Formats {
		if d.bits() || f.Kind == kindPoint || !slices.Contains(valueKinds, f.Kind) {
			return fmt.Errorf("寄存器数据类型 %q 不适用于当前读取窗口", f.Kind)
		}
		if int(f.Offset) < int(d.Start) || int(f.Offset)+f.Kind.width() > int(d.Start)+d.Qty {
			return fmt.Errorf("寄存器 %d 的类型超出读取范围", f.Offset)
		}
		if !slices.Contains(f.Kind.dataType().Orders(), f.Order) {
			return fmt.Errorf("%s 不支持字节序 %s", f.Kind, f.Order)
		}
		for i := int(f.Offset); i < int(f.Offset)+f.Kind.width(); i++ {
			if used[i] {
				return fmt.Errorf("寄存器 %d 的数据类型重叠", i)
			}
			used[i] = true
		}
	}
	return nil
}

// setRegisterFormat 按寄存器数量批量设置格式，不改变读取请求或清空已读数据。
func (w *readWindow) setRegisterFormat(start, count int, kind valueKind, order modbus.ByteOrder) error {
	if w.def.bits() || kind == kindPoint || !slices.Contains(valueKinds, kind) {
		return fmt.Errorf("请选择寄存器的数据类型")
	}
	width := kind.width()
	if start < 0 || count < 1 || start+count > w.def.Qty {
		return fmt.Errorf("设置范围必须位于 %s 内", refSpan(w.def.area(), w.def.Start, w.def.Qty))
	}
	if count%width != 0 {
		return fmt.Errorf("%s 每个值需要 %d 个寄存器，数量必须是 %d 的倍数", kind, width, width)
	}
	order = order.For(kind.dataType())
	d := w.def
	d.Formats = slices.Clone(d.Formats)
	lo, hi := int(d.Start)+start, int(d.Start)+start+count
	d.Formats = slices.DeleteFunc(d.Formats, func(f registerFormat) bool {
		return int(f.Offset) < hi && int(f.Offset)+f.Kind.width() > lo
	})
	for off := lo; off < hi; off += width {
		d.Formats = append(d.Formats, registerFormat{uint16(off), kind, order})
	}
	slices.SortFunc(d.Formats, func(a, b registerFormat) int { return int(a.Offset) - int(b.Offset) })
	if err := d.validateFormats(); err != nil {
		return err
	}
	w.def = d
	w.refreshFormats()
	return nil
}

func (w *readWindow) refreshFormats() {
	col := colValue
	if w.sel >= 0 {
		col = w.cols[w.selCol()]
	}
	w.cols = w.def.columns(w.ws.points)
	for c := 0; c < w.groups*len(w.cols); c++ {
		w.table.SetColumnWidth(c, w.def.colWidth(w.cols[c%len(w.cols)], w.ws.points))
	}
	w.updateTitle()
	if w.sel >= 0 {
		w.sel = w.align(w.sel)
		w.selAnchor = w.align(w.selAnchor)
		column := max(slices.Index(w.cols, col), 0)
		w.selCell = w.cellOf(w.sel, column)
		w.table.UnselectAll()
		w.extending = true
		w.table.Select(w.selCell)
		w.extending = false
	}
	w.table.Refresh()
	w.updateWriteBtn()
	if w.sel >= 0 {
		w.ws.inspect.showRegister(w)
	}
}

func (w *readWindow) updateTitle() {
	d := w.def
	name := ""
	if d.Name != "" {
		name = " " + d.Name
	}
	w.inner.SetTitle(fmt.Sprintf("窗口 %d%s · %s · %s", w.no, name, refSpan(d.area(), d.Start, d.Qty), d.format()))
	w.ws.refreshWindowMenu()
}

func (w *readWindow) selectionRange() (start, count int) {
	if w.sel < 0 {
		return 0, 0
	}
	anchor := w.selAnchor
	if anchor < 0 {
		anchor = w.sel
	}
	start, end := min(anchor, w.sel), max(anchor, w.sel)
	return start, end + w.valueFormat(end).width - start
}

func (w *readWindow) selectedRegister(i int) bool {
	start, count := w.selectionRange()
	return count > 0 && i >= start && i < start+count
}

func (w *readWindow) clearRegisterFormats() {
	start, count := w.selectionRange()
	if count == 0 {
		return
	}
	lo, hi := int(w.def.Start)+start, int(w.def.Start)+start+count
	w.def.Formats = slices.DeleteFunc(slices.Clone(w.def.Formats), func(f registerFormat) bool {
		return int(f.Offset) < hi && int(f.Offset)+f.Kind.width() > lo
	})
	w.refreshFormats()
}

func (w *readWindow) showRegisterFormat() {
	if w.ws.dialogOpen() || w.sel < 0 || w.def.bits() {
		return
	}
	start, count := w.selectionRange()
	f := w.valueFormat(start)
	addr := widget.NewEntry()
	addr.SetText(strconv.Itoa(int(w.def.Start) + start))
	qty := widget.NewEntry()
	qty.SetText(strconv.Itoa(count))
	var names []string
	for _, k := range valueKinds {
		if k != kindPoint {
			names = append(names, string(k))
		}
	}
	kind := widget.NewSelect(names, nil)
	order := widget.NewSelect(nil, nil)
	note := widget.NewLabel("")
	note.Wrapping = fyne.TextWrapWord
	update := func() {
		k := valueKind(kind.Selected)
		var orders []string
		for _, o := range k.dataType().Orders() {
			orders = append(orders, string(o))
		}
		order.SetOptions(orders)
		order.SetSelected(string(modbus.ByteOrder(order.Selected).For(k.dataType())))
		// 只选中一个值时，换成宽类型自动补齐所需寄存器。
		if w.selAnchor == w.sel {
			if n, _ := strconv.Atoi(qty.Text); n < k.width() {
				qty.SetText(strconv.Itoa(k.width()))
			}
		}
		note.SetText(fmt.Sprintf("只设置此范围，其他寄存器保持原格式。%s 每个值占 %d 个寄存器。", k, k.width()))
	}
	kind.OnChanged = func(string) { update() }
	if f.kind == kindPoint {
		f.kind, f.order = kindUnsigned, modbus.OrderAB
		if p, ok := w.ws.points.get(w.def.area(), w.def.Start+uint16(start)); ok && p.Type != typeString {
			f.kind, f.order = toolKind(p.Type), p.Order
		}
	}
	kind.SetSelected(string(f.kind))
	order.SetSelected(string(f.order.For(f.kind.dataType())))
	fields := []*widget.FormItem{
		widget.NewFormItem("起始 Offset（0 起始）", addr),
		widget.NewFormItem("寄存器数量", qty),
		widget.NewFormItem("数据类型", kind),
		widget.NewFormItem("字节序", order),
		widget.NewFormItem("", note),
	}
	dlg := dialog.NewForm("设置寄存器数据类型", "确定", "取消", fields, func(ok bool) {
		if !ok {
			return
		}
		off, err := strconv.Atoi(strings.TrimSpace(addr.Text))
		n, err2 := strconv.Atoi(strings.TrimSpace(qty.Text))
		if err != nil || err2 != nil {
			dialog.ShowError(fmt.Errorf("起始 Offset 和数量必须是整数"), w.ws.win)
			return
		}
		if err := w.setRegisterFormat(off-int(w.def.Start), n, valueKind(kind.Selected), modbus.ByteOrder(order.Selected)); err != nil {
			dialog.ShowError(err, w.ws.win)
		}
	}, w.ws.win)
	dlg.Resize(fyne.NewSize(520, 0))
	dlg.Show()
}

// dragCell 把拖动位置换算为当前可见表格的行列，按连续地址选择。
func (w *readWindow) dragCell(from widget.TableCellID, pos fyne.Position) {
	if w.indexOf(from) < 0 {
		return
	}
	row := min(max(from.Row+int(math.Floor(float64(pos.Y/rowHeight()))), 0), w.rows-1)
	x := pos.X
	for c := 0; c < from.Col; c++ {
		x += w.def.colWidth(w.cols[c%len(w.cols)], w.ws.points) + 1
	}
	col := 0
	for col < w.groups*len(w.cols)-1 {
		width := w.def.colWidth(w.cols[col%len(w.cols)], w.ws.points) + 1
		if x < width {
			break
		}
		x -= width
		col++
	}
	w.ws.setCurrent(w)
	if w.sel < 0 {
		w.selectCell(from, 0)
	}
	w.selAnchor = w.align(w.indexOf(from))
	w.selectCell(widget.TableCellID{Row: row, Col: col}, fyne.KeyModifierShift)
}
