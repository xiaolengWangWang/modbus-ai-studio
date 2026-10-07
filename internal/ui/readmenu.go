package ui

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// 读取窗口数据表的键盘和右键操作，按 Modbus Poll 的习惯：方向键直接移动选中（32 / 64 位值和多寄存器点整体移动），
// Enter 写入，Ctrl+C 复制选中的一行；右键单元格弹出菜单，改显示格式、字节序，写入、复制、打开调试窗口。

// grid 是读取窗口的数据表：在 Fyne 表格上改了键盘操作。Fyne 原本的方向键只移动焦点框，要再按空格才选中。
type grid struct {
	widget.Table
	w *readWindow
}

func newGrid(w *readWindow, length func() (int, int), create func() fyne.CanvasObject, update func(widget.TableCellID, fyne.CanvasObject)) *grid {
	g := &grid{w: w}
	g.Length, g.CreateCell, g.UpdateCell = length, create, update
	g.ShowHeaderRow = true
	g.ExtendBaseWidget(g)
	return g
}

func (g *grid) TypedKey(e *fyne.KeyEvent) {
	w := g.w
	switch e.Name {
	case fyne.KeyReturn, fyne.KeyEnter:
		w.writeSelected()
	case fyne.KeyUp:
		w.moveSel(-1, 0)
	case fyne.KeyDown:
		w.moveSel(1, 0)
	case fyne.KeyLeft:
		w.moveSel(0, -1)
	case fyne.KeyRight:
		w.moveSel(0, 1)
	default:
		g.Table.TypedKey(e)
	}
}

// TypedShortcut 只处理复制；“读取”菜单里的快捷键由主菜单先处理，到不了这里。
func (g *grid) TypedShortcut(s fyne.Shortcut) {
	if c, ok := s.(*fyne.ShortcutCopy); ok {
		if text := g.w.selectedText(); text != "" {
			c.Clipboard.SetContent(text)
		}
	}
}

// moveSel 按值移动选中：上下移动一个值，左右移到相邻一列的同一行。没有选中时选第一个值。
func (w *readWindow) moveSel(dRow, dGroup int) {
	if w.def.Qty == 0 || len(w.cols) == 0 {
		return
	}
	col := w.selCol()
	i := w.sel
	switch {
	case i < 0:
		i = 0
	case dRow != 0:
		i = w.step(i, dRow)
	case dGroup != 0:
		j := i + dGroup*w.rows
		if j < 0 || j >= w.def.Qty {
			return
		}
		i = w.align(j)
	}
	w.tapCell(w.cellOf(i, col))
}

// selCol 是选中单元格所在的列（在一组列里的序号），没有选中时取值列。
func (w *readWindow) selCol() int {
	if w.sel >= 0 {
		return w.selCell.Col % len(w.cols)
	}
	for c, k := range w.cols {
		if k == colValue {
			return c
		}
	}
	return 0
}

// step 从第 i 个寄存器（一个值的起始寄存器）往后或往前移动 n 个值，返回新值的起始寄存器；到头了停在边上。
func (w *readWindow) step(i, n int) int {
	for ; n > 0; n-- {
		j := i + 1
		for j < w.def.Qty && w.align(j) == i {
			j++
		}
		if j >= w.def.Qty {
			return i
		}
		i = j
	}
	for ; n < 0 && i > 0; n++ {
		i = w.align(i - 1)
	}
	return i
}

// selectedText 是选中的值所在一行的显示内容（地址、名称、值、单位、原始值），用制表符分隔，可以直接粘贴进 Excel。
func (w *readWindow) selectedText() string {
	if w.sel < 0 {
		return ""
	}
	start, count := w.selectionRange()
	var rows []string
	for i := start; i < start+count; i += w.valueFormat(i).width {
		rows = append(rows, w.rowText(i))
	}
	return strings.Join(rows, "\n")
}

func (w *readWindow) rowText(i int) string {
	var parts []string
	for c := range w.cols {
		l := newCell(func(widget.TableCellID) {}, func(widget.TableCellID) {}, nil)
		w.updateCell(w.cellOf(i, c), l)
		parts = append(parts, l.Text)
	}
	return strings.TrimRight(strings.Join(parts, "\t"), "\t")
}

// showCellMenu 弹出单元格的右键菜单。先选中右键的单元格，菜单里的操作都作用于它。
func (w *readWindow) showCellMenu(id widget.TableCellID, pos fyne.Position) {
	if w.ws.dialogOpen() {
		return
	}
	i := w.indexOf(id)
	if !w.selectedRegister(i) {
		w.selectCell(id, 0)
	}
	widget.ShowPopUpMenuAtPosition(w.cellMenu(), w.ws.win.Canvas(), pos)
}

// cellMenu 是右键菜单：写入、复制；显示格式、字节序、原始值（当前的打勾）；读取定义、调试窗口、探测可读地址。
func (w *readWindow) cellMenu() *fyne.Menu {
	ws := w.ws
	d := w.def
	write := fyne.NewMenuItem("写入…", w.writeSelected)
	write.Disabled = !w.canWrite()
	copyItem := fyne.NewMenuItem("复制", func() { ws.app.Clipboard().SetContent(w.selectedText()) })
	copyItem.Disabled = w.sel < 0

	var kinds []*fyne.MenuItem
	for _, k := range valueKinds {
		if k == kindPoint {
			continue
		}
		it := fyne.NewMenuItem(string(k), func() {
			start, count := w.selectionRange()
			if w.selAnchor == w.sel {
				count = k.width()
			}
			if err := w.setRegisterFormat(start, count, k, w.valueFormat(start).order); err != nil {
				dialog.ShowError(err, ws.win)
			}
		})
		it.Checked = w.sel >= 0 && w.valueFormat(w.sel).kind == k
		kinds = append(kinds, it)
	}
	kindItem := fyne.NewMenuItem("显示格式", nil)
	kindItem.ChildMenu = fyne.NewMenu("", kinds...)
	kindItem.Disabled = d.bits() || w.sel < 0
	typeItem := fyne.NewMenuItem("设置寄存器数据类型…", w.showRegisterFormat)
	typeItem.Disabled = d.bits() || w.sel < 0
	resetItem := fyne.NewMenuItem("恢复选中寄存器默认格式", w.clearRegisterFormats)
	resetItem.Disabled = d.bits() || w.sel < 0 || len(d.Formats) == 0

	orderItem := fyne.NewMenuItem("整窗字节序", nil)
	opts, cur, ok := w.orderChoice()
	var orders []*fyne.MenuItem
	for _, o := range opts {
		it := fyne.NewMenuItem(o, func() { ws.setWindowOrder(w, modbus.ByteOrder(o)) })
		it.Checked = ok && string(cur) == o
		orders = append(orders, it)
	}
	orderItem.ChildMenu = fyne.NewMenu("", orders...)
	orderItem.Disabled = !ok

	raw := fyne.NewMenuItem("显示原始值", func() { ws.redefine(w, func(d *readDef) { d.Raw = !d.Raw }) })
	raw.Checked = d.Raw
	raw.Disabled = d.bits()

	debug := fyne.NewMenuItem("调试功能码 / 数据类型 / 字节序…", func() { ws.openTypeTool(w) })
	probeItem := fyne.NewMenuItem("逐个探测可读地址…", func() { ws.probeRange(w) })
	probeItem.Disabled = ws.session == nil
	return fyne.NewMenu("", write, copyItem, fyne.NewMenuItemSeparator(),
		typeItem, kindItem, resetItem, orderItem, raw, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("读取定义…", func() { ws.showDefinition(w) }), debug, probeItem)
}
