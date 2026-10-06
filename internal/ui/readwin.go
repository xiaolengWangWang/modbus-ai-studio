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
	errLbl     *widget.Label
	hintLbl    *widget.Label
	actionBtn  *widget.Button
	diagBox    *fyne.Container // 错误行：错误说明和一键处理
	writeBtn   *widget.Button
	pauseBtn   *widget.Button
	table      *grid                    // 数据表，加了 Modbus Poll 的键盘操作（readmenu.go）
	tableBox   *container.ThemeOverride // 数据表套上紧凑主题，readLayout 排的是它
	sel        int                      // 选中的寄存器序号（相对 Start），-1 表示未选中
	selCell    widget.TableCellID       // 选中的单元格，sel >= 0 时有效
	head       *fyne.Container
	buttons    *fyne.Container   // 定义、写入、暂停
	bar        *readBar          // 功能码、格式、字节序、原始值（readbar.go）
	body       *fyne.Container   // 标题区 + 表格，readLayout 排列
	headScroll *container.Scroll // 标题区放不下时在这里滚动
	root       *activator        // 点窗口任何地方都设为当前窗口，当前窗口有高亮边框
}

func newReadWindow(ws *Workspace, no int, d readDef) *readWindow {
	w := &readWindow{ws: ws, no: no, sel: -1}
	w.table = newGrid(w,
		func() (int, int) { return w.rows, w.groups * len(w.cols) },
		func() fyne.CanvasObject { return denseCell(newCell(w.tapCell, w.doubleTapCell, w.showCellMenu)) },
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
		w.updateWriteBtn()
		ws.inspect.showRegister(w)
	}

	w.writeBtn = widget.NewButtonWithIcon("写入", theme.DocumentCreateIcon(), func() { ws.setCurrent(w); ws.showWrite(w) })
	w.pauseBtn = widget.NewButtonWithIcon("", theme.MediaPauseIcon(), func() { ws.setCurrent(w); w.setPaused(!w.paused) })
	defBtn := widget.NewButtonWithIcon("定义", theme.SettingsIcon(), func() { ws.setCurrent(w); ws.showDefinition(w) })
	for _, b := range []*widget.Button{w.writeBtn, w.pauseBtn, defBtn} {
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
	// 一键处理放在错误行右侧，说明另起一行占满宽度，窄窗口里也不会被挤成好几行
	w.diagBox = container.NewBorder(nil, nil, nil, w.actionBtn, w.errLbl)
	w.diagBox.Hide()
	w.hintLbl.Hide()
	w.buttons = container.NewHBox(defBtn, w.writeBtn, w.pauseBtn)
	w.bar = newReadBar(w)
	// 标题在子窗口的标题栏上；第一行是控制条和按钮。高度不够时从下往上收起：先收状态行，错误和一键处理尽量留在可见范围
	w.head = container.NewVBox(container.NewBorder(nil, nil, nil, w.buttons, w.bar.root), w.diagBox, w.hintLbl, w.statusLbl)
	// 平铺给的高度不够时（例如错误说明占了好几行），标题区在自己的范围内滚动，表格至少留出表头和两行
	w.headScroll = container.NewVScroll(w.head)
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
	name := ""
	if d.Name != "" {
		name = " " + d.Name
	}
	w.inner.SetTitle(fmt.Sprintf("窗口 %d%s · %s · %s", w.no, name, refSpan(d.area(), d.Start, d.Qty), d.format()))
	w.ws.refreshWindowMenu()
	w.bar.sync()
	w.sel = -1
	w.table.UnselectAll()
	if w.ws.inspect.src == w {
		w.ws.inspect.clear()
	}
	w.reset()
}

// readLayout 排列读取窗口：标题区在上、表格在下。高度够时标题区完整显示；不够时标题区按整行收起
// （从下往上：状态行、说明，内部可滚动查看），表格至少留出表头和一行，不会画到窗口外面。
type readLayout struct{ w *readWindow }

// rowHeight 是表格一行的高度（含分隔线）。
func rowHeight() float32 { return gridRowHeight() + theme.SeparatorThicknessSize() }

// headHeight 是标题区在 room 高度内能完整显示的行数对应的高度，至少保留第一、二行（标题、控制条）。
func (l readLayout) headHeight(room float32) float32 {
	var h float32
	n := 0
	for _, o := range l.w.head.Objects {
		if !o.Visible() {
			continue
		}
		next := h + o.MinSize().Height
		if n > 0 {
			next += theme.Padding()
		}
		if next > room && n >= 2 {
			break
		}
		h, n = next, n+1
	}
	return h
}

func (l readLayout) Layout(_ []fyne.CanvasObject, size fyne.Size) {
	head := l.w.head.MinSize().Height
	reserve := 2 * rowHeight() // 表头加一行
	if l.w.diagBox.Visible() {
		reserve = rowHeight() // 出错时数据是旧的，先保证错误和一键处理看得见
	}
	if room := size.Height - reserve; head > room {
		head = l.headHeight(room)
	}
	l.w.headScroll.Move(fyne.NewPos(0, 0))
	l.w.headScroll.Resize(fyne.NewSize(size.Width, head))
	l.w.tableBox.Move(fyne.NewPos(0, head))
	l.w.tableBox.Resize(fyne.NewSize(size.Width, max(size.Height-head, 0)))
}

func (l readLayout) MinSize([]fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(max(l.w.head.MinSize().Width, l.w.tableBox.MinSize().Width), l.headHeight(0)+2*rowHeight())
}

// prefSize 是子窗口按内容的大小：所有列、整条控制条、标题区和最多 20 行，加上子窗口的标题栏、边框和滚动条。
func (w *readWindow) prefSize() fyne.Size {
	var g float32
	for _, k := range w.cols {
		g += w.def.colWidth(k, w.ws.points) + theme.SeparatorThicknessSize()
	}
	pad := theme.Padding()
	bar := w.bar.box.MinSize().Width + w.buttons.MinSize().Width + pad
	width := max(g*float32(w.groups)+theme.ScrollBarSize(), bar, 380)
	height := w.head.MinSize().Height + float32(min(w.rows, 20)+1)*rowHeight() + theme.ScrollBarSize()
	return fyne.NewSize(width+2*pad, height+theme.Size(theme.SizeNameWindowTitleBarHeight)+pad)
}

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
	w.ws.setCurrent(w)
	if c := fyne.CurrentApp().Driver().CanvasForObject(w.table); c != nil {
		c.Focus(w.table)
	}
	w.table.Select(id)
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
		if i%n != 0 || i+n > len(regs) {
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
	switch d.Function {
	case modbus.FuncReadCoils:
		if p, ok := w.ws.points.get(d.area(), d.Start+uint16(w.sel)); ok && d.usesPoints() {
			return p.RW
		}
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
	w.fault, w.faultN = "", 0
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
	case regs != nil && d.usesPoints():
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
	rowVisible := dg.Text != "" || dg.Action != ""
	relayout := rowVisible != w.diagBox.Visible() || (dg.Hint != "") != w.hintLbl.Visible() || dg.Hint != w.hintLbl.Text
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
