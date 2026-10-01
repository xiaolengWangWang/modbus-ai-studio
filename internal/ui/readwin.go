package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// readWindow 是 Modbus Poll 式读取窗口：读取定义、状态行、错误与自动分析、按列排布的数据表（设计文档 13.3）。
type readWindow struct {
	ws     *Workspace
	no     int
	def    readDef // 只在 UI 线程读写；轮询 goroutine 拿的是副本
	cols   []colKind
	rows   int
	groups int
	paused bool
	stop   context.CancelFunc
	diagDo func()

	mu      sync.Mutex // 保护下面由轮询 goroutine 写入的字段
	gen     int        // 每次开始轮询加 1，旧 goroutine 的结果直接丢弃
	regs    []uint16   // 位读取时每个元素是 0 / 1
	changed []bool     // 与上一次相比变化了的寄存器，界面高亮
	lastOK  time.Time
	tx      int
	errN    int
	err     error

	title     *widget.Label
	statusLbl *widget.Label
	errLbl    *widget.Label
	hintLbl   *widget.Label
	actionBtn *widget.Button
	diagBox   *fyne.Container
	writeBtn  *widget.Button
	pauseBtn  *widget.Button
	table     *widget.Table
	sel       int // 选中的寄存器序号（相对 Start），-1 表示未选中
	head      *fyne.Container
	root      *fyne.Container
}

func newReadWindow(ws *Workspace, no int, d readDef) *readWindow {
	w := &readWindow{ws: ws, no: no, sel: -1}
	w.table = widget.NewTableWithHeaders(
		func() (int, int) { return w.rows, w.groups * len(w.cols) },
		func() fyne.CanvasObject { return newCell(w.tapCell, w.doubleTapCell) },
		func(id widget.TableCellID, o fyne.CanvasObject) { w.updateCell(id, o.(*cell)) },
	)
	w.table.ShowHeaderColumn = false
	w.table.CreateHeader = func() fyne.CanvasObject { return widget.NewLabel("") }
	w.table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		l := o.(*widget.Label)
		l.TextStyle = fyne.TextStyle{Bold: true}
		l.Alignment = fyne.TextAlignLeading
		if id.Row < 0 && id.Col >= 0 && len(w.cols) > 0 {
			k := w.cols[id.Col%len(w.cols)]
			if k == colValue {
				l.Alignment = fyne.TextAlignTrailing
			}
			l.SetText(colTitle[k])
			return
		}
		l.SetText("")
	}
	w.table.OnSelected = func(id widget.TableCellID) {
		i := w.indexOf(id)
		if i < 0 {
			w.sel = -1
			w.updateWriteBtn()
			return
		}
		if a := w.align(i); a != i {
			// 32 位值的第二个寄存器：选中移到第一个寄存器上，高亮和写入目标一致
			w.table.Select(w.cellOf(a, id.Col%len(w.cols)))
			return
		}
		w.sel = i
		w.updateWriteBtn()
		ws.inspect.showRegister(w)
	}

	w.title = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	w.title.Truncation = fyne.TextTruncateEllipsis
	w.writeBtn = widget.NewButtonWithIcon("写入", theme.DocumentCreateIcon(), func() { ws.showWrite(w) })
	w.pauseBtn = widget.NewButtonWithIcon("", theme.MediaPauseIcon(), func() { w.setPaused(!w.paused) })
	closeBtn := widget.NewButtonWithIcon("", theme.WindowCloseIcon(), func() { ws.removeWindow(w) })
	defBtn := widget.NewButtonWithIcon("定义", theme.SettingsIcon(), func() { ws.showDefinition(w) })
	for _, b := range []*widget.Button{w.writeBtn, w.pauseBtn, closeBtn, defBtn} {
		b.Importance = widget.LowImportance
	}
	w.statusLbl = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	w.errLbl = widget.NewLabel("")
	w.errLbl.Importance = widget.DangerImportance
	w.errLbl.Wrapping = fyne.TextWrapWord
	w.hintLbl = widget.NewLabel("")
	w.hintLbl.Wrapping = fyne.TextWrapWord
	w.actionBtn = widget.NewButton("", func() {
		if w.diagDo != nil {
			w.diagDo()
		}
	})
	w.actionBtn.Importance = widget.HighImportance
	// 一键处理放在错误行右侧，说明占满整行，窄窗口里也不会被挤成好几行
	w.diagBox = container.NewVBox(container.NewBorder(nil, nil, nil, w.actionBtn, w.errLbl), w.hintLbl)
	w.diagBox.Hide()
	buttons := container.NewHBox(defBtn, w.writeBtn, w.pauseBtn, closeBtn)
	w.head = container.NewVBox(container.NewBorder(nil, nil, nil, buttons, w.title), w.statusLbl, w.diagBox)
	w.root = container.NewBorder(w.head, nil, nil, nil, w.table)
	w.setDef(d)
	return w
}

// setDef 应用新的读取定义并清空数据，调用方负责停止和重新开始轮询。
func (w *readWindow) setDef(d readDef) {
	w.def = d
	w.cols = d.columns(w.ws.points)
	w.rows = min(d.Rows, d.Qty)
	w.groups = (d.Qty + w.rows - 1) / w.rows
	for c := 0; c < w.groups*len(w.cols); c++ {
		w.table.SetColumnWidth(c, d.colWidth(w.cols[c%len(w.cols)], w.ws.points))
	}
	name := ""
	if d.Name != "" {
		name = " " + d.Name
	}
	w.title.SetText(fmt.Sprintf("窗口 %d%s · %s · %s", w.no, name, refSpan(d.area(), d.Start, d.Qty), d.format()))
	w.sel = -1
	w.table.UnselectAll()
	if w.ws.inspect.src == w {
		w.ws.inspect.clear()
	}
	w.reset()
}

func (w *readWindow) prefWidth() float32 {
	var g float32
	for _, k := range w.cols {
		g += w.def.colWidth(k, w.ws.points) + theme.Padding()
	}
	return max(g*float32(w.groups), 380)
}

func (w *readWindow) prefHeight() float32 { return float32(w.rows+1)*36 + 90 }

// indexOf 把表格单元换算成寄存器序号（相对 Start）；超出读取范围返回 -1。
func (w *readWindow) indexOf(id widget.TableCellID) int {
	if len(w.cols) == 0 {
		return -1
	}
	i := id.Col/len(w.cols)*w.rows + id.Row
	if i >= w.def.Qty {
		return -1
	}
	return i
}

// cellOf 返回第 i 个寄存器在第 col 类列上的单元格。
func (w *readWindow) cellOf(i, col int) widget.TableCellID {
	return widget.TableCellID{Row: i % w.rows, Col: i/w.rows*len(w.cols) + col}
}

// align 让选中落在一个值的第一个寄存器上：多寄存器点的后几个寄存器、32 / 64 位格式的非起始位置都往前退。
func (w *readWindow) align(i int) int {
	d := w.def
	switch {
	case d.bits():
	case d.usesPoints():
		for i > 0 && w.ws.points.occupied(d.area(), d.Start+uint16(i)) {
			i--
		}
	case d.Kind.width() > 1:
		return i - i%d.Kind.width()
	}
	return i
}

func (w *readWindow) tapCell(id widget.TableCellID) {
	if c := fyne.CurrentApp().Driver().CanvasForObject(w.table); c != nil {
		c.Focus(w.table)
	}
	w.table.Select(id)
}

func (w *readWindow) doubleTapCell(id widget.TableCellID) {
	w.tapCell(id)
	switch {
	case w.canWrite():
		w.ws.showWrite(w)
	case w.ws.readOnly && w.ws.session != nil && (w.def.Function == modbus.FuncReadCoils || w.def.Function == modbus.FuncReadHoldingRegisters):
		w.ws.showReadOnlyInfo() // 双击没反应会让人以为坏了，说清楚原因
	}
}

func (w *readWindow) updateCell(id widget.TableCellID, c *cell) {
	c.id = id
	c.TextStyle, c.Alignment, c.Importance = fyne.TextStyle{}, fyne.TextAlignLeading, widget.MediumImportance
	i := w.indexOf(id)
	if i < 0 {
		c.SetText("")
		return
	}
	d := w.def
	off := d.Start + uint16(i)
	p, isPoint := w.ws.points.get(d.area(), off)
	isPoint = isPoint && d.usesPoints()
	switch w.cols[id.Col%len(w.cols)] {
	case colAddr:
		c.TextStyle.Monospace = true
		c.Importance = widget.LowImportance
		c.SetText(modbus.Reference(d.area(), off))
	case colName:
		if isPoint {
			c.SetText(p.Name)
			return
		}
		c.SetText("")
	case colUnit:
		if isPoint {
			c.SetText(p.Unit)
			return
		}
		c.SetText("")
	case colValue:
		c.TextStyle.Monospace, c.Alignment = true, fyne.TextAlignTrailing
		text, imp := w.valueText(i)
		c.Importance = imp
		c.SetText(text)
	}
}

// valueText 返回第 i 个寄存器的显示值。刚变化的值高亮；读取失败时保留上次的值并灰显；
// FLOAT32 结果不合理时灰显，提示字节序可能不对。
func (w *readWindow) valueText(i int) (string, widget.Importance) {
	w.mu.Lock()
	regs, stale, changed := w.regs, w.err != nil || w.ws.session == nil, w.changed
	w.mu.Unlock()
	if regs == nil || i >= len(regs) {
		return "", widget.LowImportance
	}
	d := w.def
	imp := func(n int) widget.Importance {
		if stale {
			return widget.LowImportance
		}
		for k := i; k < i+n && k < len(changed); k++ {
			if changed[k] {
				return widget.HighImportance
			}
		}
		return widget.MediumImportance
	}
	off := d.Start + uint16(i)
	switch {
	case d.bits():
		return strconv.Itoa(int(regs[i])), imp(1)
	case d.Kind == kindPoint:
		pts := w.ws.points
		if pts.occupied(d.area(), off) && i > 0 {
			return "—", widget.LowImportance
		}
		p, ok := pts.get(d.area(), off)
		if !ok {
			return strconv.Itoa(int(regs[i])), imp(1)
		}
		n := p.regs()
		if i+n > len(regs) {
			return fmt.Sprintf("需 %d 个寄存器", n), widget.DangerImportance
		}
		if p.Type == typeString {
			return decodeString(p.Order, regs[i:i+n]), imp(n)
		}
		text, plausible, err := pointText(p, regs[i:i+n])
		if err != nil {
			return err.Error(), widget.DangerImportance
		}
		if p.Name == "状态字" {
			return fmt.Sprintf("0x%04X", regs[i]), imp(n)
		}
		if !plausible {
			return text, widget.LowImportance
		}
		return text, imp(n)
	case d.Kind.width() > 1:
		n := d.Kind.width()
		if i%n != 0 || i+n > len(regs) {
			return "—", widget.LowImportance
		}
		text, plausible := formatWide(d.Kind, d.Order, regs[i:i+n])
		if !plausible {
			return text, widget.LowImportance
		}
		return text, imp(n)
	}
	return formatReg(d.Kind, regs[i]), imp(1)
}

// canWrite：线圈和保持寄存器可写；点表模式下点表标为只读的点不可写。
func (w *readWindow) canWrite() bool {
	d := w.def
	if w.ws.session == nil || w.sel < 0 || w.ws.readOnly {
		return false
	}
	switch d.Function {
	case modbus.FuncReadCoils:
		return true
	case modbus.FuncReadHoldingRegisters:
		if p, ok := w.ws.points.get(d.area(), d.Start+uint16(w.sel)); ok && d.Kind == kindPoint {
			return p.RW && p.Type != typeString // 字符串点只读，要写用自定义请求 FC16
		}
		return true
	}
	return false
}

func (w *readWindow) updateWriteBtn() {
	if w.canWrite() {
		w.writeBtn.Enable()
	} else {
		w.writeBtn.Disable()
	}
}

// snapshot 返回寄存器副本、最近的错误和最后一次成功时间。
func (w *readWindow) snapshot() ([]uint16, time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]uint16(nil), w.regs...), w.lastOK, w.err
}

func (w *readWindow) reset() {
	w.mu.Lock()
	w.regs, w.changed, w.tx, w.errN, w.err = nil, nil, 0, 0, nil
	w.mu.Unlock()
	w.refresh()
}

func (w *readWindow) setPaused(p bool) {
	w.paused = p
	if p {
		w.halt()
		w.pauseBtn.SetIcon(theme.MediaPlayIcon())
	} else {
		w.pauseBtn.SetIcon(theme.MediaPauseIcon())
		w.start()
	}
	w.refresh()
}

// start 在已连接且未暂停时开始轮询，只在 UI 线程调用。
func (w *readWindow) start() {
	s := w.ws.session
	if s == nil || s.lost != nil || w.paused || w.stop != nil || !slices.Contains(w.ws.windows, w) {
		return // 已关闭的窗口可能被异步操作（探测、诊断）回调，不能再开始轮询
	}
	ctx, cancel := context.WithCancel(s.ctx)
	w.stop = cancel
	w.mu.Lock()
	w.gen++
	gen := w.gen
	w.mu.Unlock()
	go w.run(ctx, s.client, w.def, gen)
}

func (w *readWindow) halt() {
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.mu.Lock()
	w.gen++
	w.mu.Unlock()
}

// refresh 刷新状态行、错误分析和表格，只在 UI 线程调用。
func (w *readWindow) refresh() {
	w.mu.Lock()
	tx, errN, err, lastOK, regs := w.tx, w.errN, w.err, w.lastOK, w.regs
	w.mu.Unlock()
	d := w.def
	status := fmt.Sprintf("Tx = %d: Err = %d: ID = %d: F = %02X: SR = %dms", tx, errN, d.Slave, byte(d.Function), d.Scan.Milliseconds())
	if w.paused {
		status += "  · 已暂停"
	}
	w.statusLbl.SetText(status)

	var dg diagnosis
	switch {
	case w.ws.session == nil && tx > 0:
		dg.Text = "未连接"
	case w.ws.session != nil && w.ws.session.lost != nil:
		dg = w.ws.lossDiagnosis()
	case err != nil:
		dg = w.ws.diagnose(w, err)
		if regs != nil {
			dg.Text += " · 灰色为 " + lastOK.Format("15:04:05") + " 的值"
		}
	case regs != nil && d.Kind.dataType().Float() && !d.bits():
		dt := d.Kind.dataType()
		if o, ok := suggestFloatOrder(dt, regs, d.Order.For(dt)); ok {
			dg.Hint = fmt.Sprintf("按 %s 解出的 %s 多数不合理（灰色），按 %s 全部合理：字节序可能是 %s。", d.Order.For(dt), dt, o, o)
			dg.Action, dg.Do = "改用 "+string(o), func() { w.ws.redefine(w, func(d *readDef) { d.Order = o }) }
		}
	}
	w.setDiagnosis(dg)
	w.table.Refresh()
	w.updateWriteBtn()
	if w.ws.inspect.src == w {
		w.ws.inspect.showRegister(w)
	}
}

func (w *readWindow) setDiagnosis(dg diagnosis) {
	visible := dg.Text != "" || dg.Hint != ""
	w.errLbl.SetText(dg.Text)
	if dg.Text == "" && dg.Action == "" {
		w.errLbl.Hide()
	} else {
		w.errLbl.Show()
	}
	if dg.Hint == "" {
		w.hintLbl.Hide()
	} else {
		w.hintLbl.Show()
	}
	w.hintLbl.SetText(dg.Hint)
	w.diagDo = dg.Do
	if dg.Action == "" {
		w.actionBtn.Hide()
	} else {
		w.actionBtn.SetText(dg.Action)
		w.actionBtn.Show()
	}
	if visible != w.diagBox.Visible() {
		if visible {
			w.diagBox.Show()
		} else {
			w.diagBox.Hide()
		}
		// 错误区出现或消失后标题区高度变化，需要让外层重新布局，否则会与表格重叠
		w.head.Refresh()
		w.root.Refresh()
	}
}

// run 按扫描周期轮询。一轮耗时超过周期时自动跳过下一轮，不堆积请求。
func (w *readWindow) run(ctx context.Context, c *modbus.Client, d readDef, gen int) {
	t := time.NewTicker(d.Scan)
	defer t.Stop()
	req := modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start, Quantity: uint16(d.Qty)}
	for {
		resp, err := c.Do(ctx, req)
		if ctx.Err() != nil {
			return
		}
		var vals []uint16
		if err == nil {
			vals = resp.Registers
			if d.bits() {
				vals = make([]uint16, len(resp.Bits))
				for i, b := range resp.Bits {
					if b {
						vals[i] = 1
					}
				}
			}
		}
		w.mu.Lock()
		if w.gen != gen {
			w.mu.Unlock()
			return
		}
		w.tx++
		if err == nil {
			w.changed = diffRegs(w.regs, vals)
			w.regs, w.lastOK, w.err = vals, time.Now(), nil
		} else {
			w.errN++
			w.err, w.changed = err, nil
		}
		w.mu.Unlock()
		w.ws.stats.poll(err == nil)
		uiDo(w.refresh)
		if errors.Is(err, modbus.ErrConnection) {
			return // 连接断了，由连接保持负责停轮询、重连后再启动
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func diffRegs(old, cur []uint16) []bool {
	if len(old) != len(cur) {
		return nil
	}
	out := make([]bool, len(cur))
	for i := range cur {
		out[i] = old[i] != cur[i]
	}
	return out
}
