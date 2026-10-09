package ui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
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
	fault   string     // 正在出的错误种类（faultKey），为空表示正常；同一种错误连续出现只记一条日志
	faultAt time.Time
	faultN  int
	regs    []uint16 // 位读取时每个元素是 0 / 1
	changed []bool   // 与上一次相比变化了的寄存器，界面高亮
	lastOK  time.Time
	tx      int
	errN    int
	err     error

	inner      *container.InnerWindow // 多文档区域里的子窗口，标题栏显示窗口编号、地址和格式（mdi.go）
	pos        fyne.Position          // 子窗口不最大化时的位置和大小
	size       fyne.Size
	statusLbl  *widget.Label
	stateLbl   *widget.Label
	errLbl     *widget.Label
	hintLbl    *widget.Label
	actionBtn  *widget.Button
	aiBtn      *widget.Button
	diagBox    *fyne.Container // 错误行：错误说明和一键处理
	writeBtn   *widget.Button
	typeBtn    *widget.Button
	pauseBtn   *widget.Button
	table      *grid                    // 数据表，加了 Modbus Poll 的键盘操作（readmenu.go）
	tableBox   *container.ThemeOverride // 数据表套上紧凑主题，readLayout 排的是它
	sel        int                      // 选中的寄存器序号（相对 Start），-1 表示未选中
	selCell    widget.TableCellID       // 选中的单元格，sel >= 0 时有效
	selAnchor  int                      // Shift 连选和拖选的起始寄存器
	extending  bool                     // 选中回调中保留选择起点
	head       *fyne.Container
	headExtent *canvas.Rectangle // invisible measured extent for the scrollable wrapping header
	buttons    *fyne.Container   // 定义、写入、暂停
	bar        *readBar          // 功能码、格式、字节序、原始值（readbar.go）
	body       *fyne.Container   // 标题区 + 表格，readLayout 排列
	headScroll *container.Scroll // 标题区放不下时在这里滚动
	root       *activator        // 点窗口任何地方都设为当前窗口，当前窗口有高亮边框
}

func newReadWindow(ws *Workspace, no int, d readDef) *readWindow {
	w := &readWindow{ws: ws, no: no, sel: -1, selAnchor: -1}
	w.table = newGrid(w,
		func() (int, int) { return w.rows, w.groups * len(w.cols) },
		func() fyne.CanvasObject {
			c := newCell(w.tapCell, w.doubleTapCell, w.showCellMenu)
			c.onDrag = w.dragCell
			return denseCell(c)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) { w.updateCell(id, unwrap(o).(*cell)) },
	)
	w.table.CreateHeader = newGridHeader
	w.table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		l := headerLabel(o)
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
		ws.setCurrent(w)
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
		w.sel, w.selCell = i, id
		if !w.extending || w.selAnchor < 0 {
			w.selAnchor = i
		}
		w.updateWriteBtn()
		ws.inspect.showRegister(w)
		w.table.Refresh()
	}

	w.writeBtn = widget.NewButtonWithIcon("写入", theme.DocumentCreateIcon(), func() { ws.setCurrent(w); ws.showWrite(w) })
	w.pauseBtn = widget.NewButtonWithIcon("暂停", theme.MediaPauseIcon(), func() { ws.setCurrent(w); w.setPaused(!w.paused) })
	defBtn := widget.NewButtonWithIcon("定义", theme.SettingsIcon(), func() { ws.setCurrent(w); ws.showDefinition(w) })
	for _, b := range []*widget.Button{w.writeBtn, w.pauseBtn, defBtn} {
		b.Importance = widget.LowImportance
	}
	w.statusLbl = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	w.stateLbl = widget.NewLabelWithStyle("未连接", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
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
	w.aiBtn = widget.NewButton("AI分析", func() { ws.openAITarget(aiTarget{read: w}) })
	w.aiBtn.Importance = widget.LowImportance
	// Error text uses the full width; manual actions wrap on the following row.
	w.diagBox = container.New(toolbarLayout{}, w.errLbl, container.New(flowLayout{}, w.actionBtn, w.aiBtn))
	w.diagBox.Hide()
	w.hintLbl.Hide()
	w.typeBtn = widget.NewButton("类型", func() { ws.setCurrent(w); w.showRegisterFormat() })
	w.typeBtn.Importance = widget.LowImportance
	w.buttons = container.NewHBox(defBtn, w.typeBtn, w.writeBtn, w.pauseBtn)
	w.bar = newReadBar(w)
	// 标题在子窗口的标题栏上；第一行是控制条和按钮。高度不够时从下往上收起：先收状态行，错误和一键处理尽量留在可见范围
	controls := append([]fyne.CanvasObject{w.stateLbl}, w.bar.controls...)
	controls = append(controls, w.buttons.Objects...)
	w.head = container.New(toolbarLayout{}, container.New(flowLayout{}, controls...), w.diagBox, w.hintLbl, w.statusLbl)
	w.statusLbl.Wrapping = fyne.TextWrapWord
	// 平铺给的高度不够时（例如错误说明占了好几行），标题区在自己的范围内滚动，表格至少留出表头和两行
	w.headExtent = canvas.NewRectangle(color.Transparent)
	w.headScroll = container.NewVScroll(container.NewStack(w.headExtent, w.head))
	w.tableBox = dense(w.table)
	w.body = container.New(readLayout{w}, w.headScroll, w.tableBox)
	w.root = newActivator(w.body, func() { ws.setCurrent(w) })
	w.inner = container.NewInnerWindow("", w.root)
	w.inner.SetPadded(false)
	w.setDef(d)
	return w
}

// title 是子窗口标题栏上的文字。
func (w *readWindow) title() string { return w.inner.Title }

// setDef 应用新的读取定义并清空数据，调用方负责停止和重新开始轮询。
func (w *readWindow) setDef(d readDef) {
	w.def = d
	w.cols = d.columns(w.ws.points)
	w.rows = min(d.Rows, d.Qty)
	w.groups = (d.Qty + w.rows - 1) / w.rows
	for c := 0; c < w.groups*len(w.cols); c++ {
		w.table.SetColumnWidth(c, d.colWidth(w.cols[c%len(w.cols)], w.ws.points))
	}
	w.updateTitle()
	w.bar.sync()
	w.sel = -1
	w.selAnchor = -1
	w.table.UnselectAll()
	if w.ws.inspect.src == w {
		w.ws.inspect.clear()
	}
	w.reset()
}

// readLayout 排列读取窗口：标题区在上、表格在下。寄存器数据是主体：控制条总是显示，表格先拿到能
// 显示全部行的高度；错误说明在表格还能留出表头和 3 行时显示，提示和状态行只用剩下的空间。
// 放不下的部分在标题区里滚动查看，表格至少留出表头和一行，不会画到窗口外面。
type readLayout struct{ w *readWindow }

// rowHeight 是表格一行的高度（含分隔线）。
func rowHeight() float32 { return gridRowHeight() + theme.SeparatorThicknessSize() }

// headHeight 按顺序（控制条、错误说明、提示、状态行）收进标题区，直到放不下为止；后面的在标题区里滚动查看。
func (l readLayout) headHeight(size fyne.Size) float32 {
	table := float32(l.w.rows+1)*rowHeight() + theme.ScrollBarSize() // 表头和全部行
	var h float32
	for i, o := range l.w.head.Objects {
		if !o.Visible() {
			continue
		}
		next := h + toolbarRowHeight(o, size.Width) + theme.Padding()
		room := size.Height - table
		if o == l.w.diagBox && l.w.errLbl.Text != "" {
			room = max(room, size.Height-4*rowHeight())
		}
		if i > 0 && next > room {
			break
		}
		h = next
	}
	return h
}

func (l readLayout) Layout(_ []fyne.CanvasObject, size fyne.Size) {
	l.w.headExtent.SetMinSize(fyne.NewSize(0, toolbarHeight(l.w.head.Objects, size.Width)))
	head := min(l.headHeight(size), max(0, size.Height-2*rowHeight()))
	l.w.headScroll.Move(fyne.NewPos(0, 0))
	l.w.headScroll.Resize(fyne.NewSize(size.Width, head))
	l.w.tableBox.Move(fyne.NewPos(0, head))
	l.w.tableBox.Resize(fyne.NewSize(size.Width, max(size.Height-head, 0)))
}

func (l readLayout) MinSize([]fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(max(l.w.head.MinSize().Width, l.w.tableBox.MinSize().Width), l.w.head.Objects[0].MinSize().Height+2*rowHeight())
}

// prefSize 是子窗口按内容的大小：所有列、整条控制条、标题区和最多 20 行，加上子窗口的标题栏、边框和滚动条。
func (w *readWindow) prefSize() fyne.Size {
	var g float32
	for _, k := range w.cols {
		g += w.def.colWidth(k, w.ws.points) + theme.SeparatorThicknessSize()
	}
	pad := theme.Padding()
	var bar float32
	for _, o := range w.head.Objects[0].(*fyne.Container).Objects {
		if o.Visible() {
			bar += o.MinSize().Width + pad
		}
	}
	width := max(g*float32(w.groups)+theme.ScrollBarSize(), min(bar, 960), 880)
	height := toolbarHeight(w.head.Objects, width) + float32(min(w.rows, 20)+1)*rowHeight() + theme.ScrollBarSize()
	return fyne.NewSize(width+2*pad, max(340, height+theme.Size(theme.SizeNameWindowTitleBarHeight)+pad))
}

// indexOf 把表格单元换算成寄存器序号（相对 Start）；超出读取范围返回 -1。
func (w *readWindow) indexOf(id widget.TableCellID) int {
	if len(w.cols) == 0 || id.Row < 0 || id.Col < 0 || id.Row >= w.rows || id.Col >= w.groups*len(w.cols) {
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
	return w.valueFormat(i).start
}

func (w *readWindow) tapCell(id widget.TableCellID) {
	var modifiers fyne.KeyModifier
	if driver, ok := fyne.CurrentApp().Driver().(desktop.Driver); ok {
		modifiers = driver.CurrentKeyModifiers()
	}
	w.selectCell(id, modifiers)
}

func (w *readWindow) selectCell(id widget.TableCellID, modifiers fyne.KeyModifier) {
	w.ws.setCurrent(w)
	if c := fyne.CurrentApp().Driver().CanvasForObject(w.table); c != nil {
		c.Focus(w.table)
	}
	w.extending = modifiers&fyne.KeyModifierShift != 0 && w.sel >= 0
	defer func() { w.extending = false }()
	if w.sel >= 0 && w.selCell == id {
		w.table.OnSelected(id)
	} else {
		w.table.Select(id)
	}
}

func (w *readWindow) doubleTapCell(id widget.TableCellID) {
	w.tapCell(id)
	w.writeSelected()
}

// writeSelected 写入选中的值（双击、Enter）。不能写时说明原因，免得以为坏了。
func (w *readWindow) writeSelected() {
	if w.sel < 0 || w.ws.dialogOpen() {
		return
	}
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
	c.setSelected(i >= 0 && w.selectedRegister(i))
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
	case colRaw:
		c.TextStyle.Monospace, c.Alignment, c.Importance = true, fyne.TextAlignTrailing, widget.LowImportance
		c.SetText(w.rawText(i))
	case colType:
		f := w.valueFormat(i)
		if f.start != i {
			c.SetText("—")
			return
		}
		if f.kind == kindPoint {
			if p, ok := w.ws.points.get(d.area(), off); ok {
				c.SetText(string(p.Type) + " " + string(p.Order))
			} else {
				c.SetText("Unsigned AB")
			}
			return
		}
		c.SetText(string(f.kind) + " " + string(f.order.For(f.kind.dataType())))
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
	d := w.displayDef(i)
	if w.valueFormat(i).start != i {
		return "—", widget.LowImportance
	}
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
	case d.bits():
		return strconv.Itoa(int(regs[i])), imp(1)
	case d.Kind.width() > 1:
		n := d.Kind.width()
		if i+n > len(regs) {
			return "—", widget.LowImportance
		}
		text, plausible := formatWide(d.Kind, d.Order, regs[i:i+n])
		if !plausible {
			return text, widget.LowImportance
		}
		return text, imp(n)
	}
	return formatReg(d.Kind, swap16(d.Order, regs[i])), imp(1)
}

// rawText 是第 i 个寄存器线上的原始值（十六进制，不按字节序换算），调大小端时对照着看。
func (w *readWindow) rawText(i int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i >= len(w.regs) {
		return ""
	}
	return fmt.Sprintf("%04X", w.regs[i])
}

// canWrite：线圈和保持寄存器可写；点表模式下点表标为只读的点不可写。
func (w *readWindow) canWrite() bool {
	d := w.def
	if w.ws.session == nil || w.sel < 0 || w.ws.readOnly {
		return false
	}
	f := w.valueFormat(w.sel)
	_, count := w.selectionRange()
	if count > f.width || w.sel+f.width > d.Qty {
		return false
	}
	switch d.Function {
	case modbus.FuncReadCoils:
		if p, ok := w.ws.points.get(d.area(), d.Start+uint16(w.sel)); ok && d.usesPoints() {
			return p.RW
		}
		return true
	case modbus.FuncReadHoldingRegisters:
		if d.Kind == kindPoint && len(d.Formats) > 0 {
			for i := w.sel; i < w.sel+f.width; i++ {
				a := d.Start + uint16(i)
				for a > d.Start && w.ws.points.occupied(d.area(), a) {
					a--
				}
				if p, ok := w.ws.points.get(d.area(), a); ok && (!p.RW || p.Type == typeString) {
					return false
				}
			}
		}
		if p, ok := w.ws.points.get(d.area(), d.Start+uint16(w.sel)); ok && d.Kind == kindPoint {
			return p.RW && p.Type != typeString // 字符串点只读，要写用自定义请求 FC16
		}
		return true
	}
	return false
}

func (w *readWindow) updateWriteBtn() {
	if w.sel >= 0 && !w.def.bits() {
		w.typeBtn.Enable()
	} else {
		w.typeBtn.Disable()
	}
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
	w.fault, w.faultN = "", 0
	w.mu.Unlock()
	w.refresh()
}

func (w *readWindow) setPaused(p bool) {
	w.paused = p
	if p {
		w.halt()
		w.pauseBtn.Importance = widget.HighImportance
		w.pauseBtn.SetText("继续")
		w.pauseBtn.SetIcon(theme.MediaPlayIcon())
	} else {
		w.pauseBtn.Importance = widget.LowImportance
		w.pauseBtn.SetText("暂停")
		w.pauseBtn.SetIcon(theme.MediaPauseIcon())
		w.start()
	}
	w.refresh()
	w.ws.refreshReadActions()
}

// start 在已连接且未暂停时开始轮询，只在 UI 线程调用。
func (w *readWindow) start() {
	s := w.ws.session
	if s == nil || s.lost != nil || w.ws.probeRunning || w.paused || w.stop != nil || !slices.Contains(w.ws.windows, w) {
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

// refreshState 同步连接与检测状态，不重做表格和诊断，只在 UI 线程调用。
func (w *readWindow) refreshState() {
	w.mu.Lock()
	err, hasData := w.err, len(w.regs) > 0 && !w.lastOK.IsZero()
	w.mu.Unlock()
	w.updateState(err, hasData)
}

func (w *readWindow) updateState(err error, hasData bool) {
	state, importance := "读取正常", widget.SuccessImportance
	switch {
	case w.ws.connecting:
		state, importance = "连接中", widget.MediumImportance
	case w.ws.session == nil:
		state, importance = "未连接", widget.MediumImportance
	case w.ws.session.lost != nil:
		state, importance = "等待重连", widget.DangerImportance
	case w.ws.probeRunning:
		state, importance = "检测中", widget.WarningImportance
	case w.paused:
		state, importance = "已暂停", widget.WarningImportance
	case err != nil:
		state, importance = "读取失败", widget.DangerImportance
	case !hasData:
		state, importance = "等待数据", widget.MediumImportance
	}
	if w.stateLbl.Text != state || w.stateLbl.Importance != importance {
		w.stateLbl.Importance = importance
		w.stateLbl.SetText(state)
		w.head.Refresh()
		w.body.Refresh()
	}
}

// refresh 刷新状态行、错误分析和表格，只在 UI 线程调用。
func (w *readWindow) refresh() {
	w.mu.Lock()
	tx, errN, err, lastOK, regs := w.tx, w.errN, w.err, w.lastOK, w.regs
	w.mu.Unlock()
	w.updateState(err, len(regs) > 0 && !lastOK.IsZero())
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
	case regs != nil && d.Kind.dataType().Float() && !d.bits() && len(d.Formats) == 0:
		dt := d.Kind.dataType()
		if o, ok := suggestFloatOrder(dt, regs, d.Order.For(dt)); ok {
			dg.Hint = fmt.Sprintf("按 %s 解出的 %s 多数不合理（灰色），按 %s 全部合理：字节序可能是 %s。", d.Order.For(dt), dt, o, o)
			dg.Action, dg.Do = "改用 "+string(o), func() { w.ws.setWindowOrder(w, o) }
		}
	case regs != nil && d.usesPoints() && len(d.Formats) == 0:
		if cur, o, ok := suggestPointOrder(w.ws.points, d, regs); ok {
			dg.Hint = fmt.Sprintf("点表里的浮点数按 %s 解出多数不合理（灰色），按 %s 全部合理：设备的字节序可能是 %s。", cur, o, o)
			dg.Action, dg.Do = "点表改用 "+string(o), func() { w.ws.setPointOrder(cur, o) }
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
	rowVisible := dg.Text != "" || dg.Action != "" || dg.Hint != ""
	relayout := rowVisible != w.diagBox.Visible() || (dg.Hint != "") != w.hintLbl.Visible() || dg.Hint != w.hintLbl.Text || dg.Text != w.errLbl.Text || dg.Action != w.actionBtn.Text
	w.errLbl.SetText(dg.Text)
	w.hintLbl.SetText(dg.Hint)
	w.diagDo = dg.Do
	if dg.Action == "" {
		w.actionBtn.Hide()
	} else {
		w.actionBtn.SetText(dg.Action)
		w.actionBtn.Show()
	}
	for _, x := range []struct {
		obj fyne.CanvasObject
		on  bool
	}{{w.diagBox, rowVisible}, {w.errLbl, rowVisible}, {w.hintLbl, dg.Hint != ""}} {
		if x.on {
			x.obj.Show()
		} else {
			x.obj.Hide()
		}
	}
	if relayout {
		// 错误区出现、消失或说明变长后标题区高度变化，让窗口重新排列，否则会与表格重叠
		w.head.Refresh()
		w.body.Refresh()
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
		// 日志：开始出某种错误时记一条（带这次请求的原始报文），恢复正常时再记一条
		var logFail, logOK bool
		since, fails := w.faultAt, w.faultN
		if err == nil {
			w.changed = diffRegs(w.regs, vals)
			w.regs, w.lastOK, w.err = vals, time.Now(), nil
			logOK = w.fault != ""
			w.fault = ""
		} else {
			w.errN++
			w.err, w.changed = err, nil
			switch k := faultKey(err); {
			case errors.Is(err, modbus.ErrConnection): // 连接断开由连接保持记日志
			case k != w.fault:
				w.fault, w.faultAt, w.faultN, logFail = k, time.Now(), 1, true
			default:
				w.faultN++
			}
		}
		w.mu.Unlock()
		w.ws.stats.poll(err == nil)
		uiDo(w.refresh)
		if logFail {
			tx, res, _ := w.ws.ring.exchange(sameRequest(req))
			uiDo(func() { w.ws.logReadFail(w, d, err, tx, res) })
		}
		if logOK {
			uiDo(func() { w.ws.logRecovered(w, d, since, fails) })
		}
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
