package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// typeTool 是“功能码 / 数据类型 / 字节序调试”窗口，对接新设备、点表不全时用：
// 按选定的功能码读一段地址，把同一批寄存器按所选数据类型的每种字节序并排解出来，浮点数不合理的灰显，
// 统计每种字节序有几个合理值并给出建议；换数据类型不用重新读。“探测功能码”依次用 03 / 04 / 01 / 02
// 读同一地址，看哪个有正常响应。确认后一键应用到读取窗口。读请求和轮询共用连接，收发也记在主窗口的通信报文里。
type typeTool struct {
	ws  *Workspace
	win fyne.Window

	slave, addr, qty, period *widget.Entry
	fn, dtype, order         *widget.Select
	addrInfo, status, advice *widget.Label
	readBtn, probeBtn        *widget.Button
	loop                     *widget.Check
	table                    *widget.Table

	// 最近一次成功读取的结果，换数据类型时直接重新解读
	req    modbus.Request
	regs   []uint16
	bits   []bool
	orders []modbus.ByteOrder // 表格里各字节序列，按数据类型
	best   modbus.ByteOrder   // 合理值最多的字节序，没有明显最好的为空

	lastFn  modbus.FunctionCode // 功能码下拉框上一次的值，换功能码时把地址换成新数据区的写法
	syncing bool                // 程序按地址写法切换功能码时，不再反过来改地址

	busy bool
	stop context.CancelFunc // 自动刷新进行中
}

var toolTypes = []modbus.DataType{modbus.TypeInt16, modbus.TypeUint16, modbus.TypeInt32, modbus.TypeUint32,
	modbus.TypeFloat32, modbus.TypeInt64, modbus.TypeUint64, modbus.TypeFloat64}

// probeFuncs 是“探测功能码”的顺序：寄存器在前，最常见的 03 最先。
var probeFuncs = []modbus.FunctionCode{modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters, modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs}

// openTypeTool 打开调试窗口，按读取窗口 w（可为 nil）的设置和选中的地址填好；已打开时换成 w 的设置并重新读取。
func (ws *Workspace) openTypeTool(w *readWindow) {
	if t := ws.typeTool; t != nil {
		t.fill(w)
		t.win.RequestFocus()
		t.read()
		return
	}
	win := ws.app.NewWindow(fmt.Sprintf("功能码 / 数据类型 / 字节序调试 · 窗口 %d", ws.no))
	win.Resize(fyne.NewSize(880, 620))
	t := &typeTool{ws: ws, win: win}
	win.SetContent(t.build())
	t.fill(w)
	ws.typeTool = t
	ws.addTool(win, func() {
		t.stopLoop()
		ws.typeTool = nil
	})
	win.Show()
	t.read()
}

func (t *typeTool) build() fyne.CanvasObject {
	t.slave = widget.NewEntry()
	t.addr = widget.NewEntry()
	t.qty = widget.NewEntry()
	t.period = widget.NewEntry()
	t.period.SetText("1000")
	var fns []string
	for _, f := range readFuncs {
		fns = append(fns, funcLabels[f])
	}
	// 换功能码时地址指同一个 Offset，换成新数据区的写法：40011 换到 04 是 30011，换到 01 是 10
	t.fn = widget.NewSelect(fns, func(string) {
		f := t.function()
		if !t.syncing && t.lastFn != 0 && t.lastFn != f {
			if off, err := t.parseFor(t.lastFn); err == nil {
				t.lastFn = f
				t.addr.SetText(startText(readDef{Function: f, Start: off}))
			}
		}
		t.lastFn = f
		t.updateType()
		t.showAddr()
	})
	var types []string
	for _, dt := range toolTypes {
		types = append(types, string(dt))
	}
	t.dtype = widget.NewSelect(types, func(string) { t.render() }) // 换类型只是重新解读，不用再读
	t.order = widget.NewSelect(nil, nil)
	t.order.PlaceHolder = string(modbus.OrderABCDEFGH)
	t.addrInfo = widget.NewLabel("")
	t.status = widget.NewLabel("")
	t.status.Wrapping = fyne.TextWrapWord
	t.advice = widget.NewLabel("")
	t.advice.Wrapping = fyne.TextWrapWord
	t.addr.OnChanged = func(string) { t.showAddr() }
	for _, e := range []*widget.Entry{t.slave, t.addr, t.qty} {
		e.OnSubmitted = func(string) { t.read() }
	}
	t.readBtn = widget.NewButtonWithIcon("读取", theme.ViewRefreshIcon(), t.read)
	t.readBtn.Importance = widget.HighImportance
	t.probeBtn = widget.NewButtonWithIcon("探测功能码", theme.SearchIcon(), t.probeFunctions)
	t.loop = widget.NewCheck("自动刷新", func(on bool) {
		if on {
			t.startLoop()
		} else {
			t.stopLoop()
		}
	})
	apply := widget.NewButtonWithIcon("应用到当前读取窗口", theme.ConfirmIcon(), func() { t.apply(false) })
	apply.Importance = widget.HighImportance
	newWin := widget.NewButtonWithIcon("新建读取窗口", theme.ContentAddIcon(), func() { t.apply(true) })
	copyBtn := widget.NewButtonWithIcon("复制表格", theme.ContentCopyIcon(), func() { t.ws.app.Clipboard().SetContent(t.text()) })
	copyBtn.Importance = widget.LowImportance

	t.table = widget.NewTableWithHeaders(t.size,
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.Truncation = fyne.TextTruncateEllipsis
			return denseCell(l)
		},
		func(id widget.TableCellID, o fyne.CanvasObject) { t.updateCell(id, unwrap(o).(*widget.Label)) })
	t.table.ShowHeaderColumn = false
	t.table.CreateHeader = newGridHeader
	t.table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		l := headerLabel(o)
		l.SetText("")
		if id.Row < 0 && id.Col >= 0 {
			l.SetText(t.header(id.Col))
		}
	}

	settings := container.NewHBox(
		widget.NewLabel("Slave"), fixed(52, t.slave),
		widget.NewLabel("功能码"), fixed(150, t.fn),
		widget.NewLabel("起始地址"), fixed(96, t.addr),
		widget.NewLabel("数量"), fixed(60, t.qty),
		widget.NewLabel("数据类型"), fixed(112, t.dtype))
	actions := container.NewHBox(t.readBtn, t.loop, widget.NewLabel("周期"), fixed(68, t.period), widget.NewLabel("ms"),
		widget.NewSeparator(), t.probeBtn, layout.NewSpacer(), copyBtn)
	top := container.NewVBox(container.NewHScroll(settings), t.addrInfo, actions, t.status, t.advice, widget.NewSeparator())
	bottom := container.NewVBox(widget.NewSeparator(),
		container.NewHBox(widget.NewLabel("字节序"), fixed(130, t.order), apply, newWin))
	return container.NewBorder(top, bottom, nil, nil, dense(t.table))
}

// fill 按读取窗口 w 填写设置：Slave、功能码；选中了值就从它开始读，数据类型取它的显示格式或点的类型，
// Hex、Binary、ASCII 这些不是数值类型的格式先按 FLOAT32 看。
func (t *typeTool) fill(w *readWindow) {
	d := defaultDef()
	start, qty := d.Start, 10
	dt := modbus.TypeFloat32
	if w != nil {
		d = w.def
		start, qty = d.Start, d.Qty
		if w.sel >= 0 {
			d = w.displayDef(w.sel)
			start, qty = d.Start+uint16(w.sel), max(d.Qty-w.sel, 4)
		}
		switch {
		case d.Kind == kindPoint:
			if p, ok := t.ws.points.get(d.area(), start); ok && p.Type != typeString && p.Type != modbus.TypeBool {
				dt = p.Type
			}
		case d.Kind != kindHex && d.Kind != kindBinary && d.Kind != kindASCII:
			dt = d.Kind.dataType()
		}
	}
	qty = max(1, min(qty, modbus.MaxReadRegisters, 0x10000-int(start)))
	t.slave.SetText(strconv.Itoa(int(d.Slave)))
	t.setFunction(d.Function)
	t.addr.SetText(startText(readDef{Function: d.Function, Start: start}))
	t.qty.SetText(strconv.Itoa(qty))
	t.dtype.SetSelected(string(dt))
}

func (t *typeTool) function() modbus.FunctionCode {
	for f, l := range funcLabels {
		if l == t.fn.Selected {
			return f
		}
	}
	return modbus.FuncReadHoldingRegisters
}

func (t *typeTool) dataType() modbus.DataType { return modbus.DataType(t.dtype.Selected) }

func (t *typeTool) isBits(f modbus.FunctionCode) bool {
	return f == modbus.FuncReadCoils || f == modbus.FuncReadDiscreteInputs
}

func (t *typeTool) updateType() {
	if t.isBits(t.function()) {
		t.dtype.Disable()
	} else {
		t.dtype.Enable()
	}
}

// setFunction 由程序切换功能码，地址框不跟着改写。
func (t *typeTool) setFunction(f modbus.FunctionCode) {
	t.syncing = true
	t.fn.SetSelected(funcLabels[f])
	t.syncing = false
	t.lastFn = f
}

// parseFor 按功能码 f 解释地址框：优先取 f 的数据区的写法，其次原始 Offset，都没有时取第一种解释。
func (t *typeTool) parseFor(f modbus.FunctionCode) (uint16, error) {
	cands, err := modbus.ParseAddress(t.addr.Text)
	if err != nil {
		return 0, err
	}
	for _, c := range cands {
		if c.Area == modbus.AreaOf(f) {
			return c.Offset, nil
		}
	}
	for _, c := range cands {
		if c.Area == modbus.AreaNone {
			return c.Offset, nil
		}
	}
	return cands[0].Offset, nil
}

// offset 解析起始地址。40001 这类带数据区的写法优先取和功能码一致的解释；只有别的数据区的解释时
// （例如功能码 03 时输入 30001），把功能码切到那个数据区的读功能码，和读取定义里的规则一样。
func (t *typeTool) offset() (uint16, error) {
	cands, err := modbus.ParseAddress(t.addr.Text)
	if err != nil {
		return 0, err
	}
	area := modbus.AreaOf(t.function())
	for _, c := range cands {
		if c.Area == area {
			return c.Offset, nil
		}
	}
	c := cands[0]
	if c.Area != modbus.AreaNone {
		t.setFunction(c.Area.ReadFunction())
	}
	return c.Offset, nil
}

func (t *typeTool) showAddr() {
	if t.addrInfo == nil || t.fn == nil {
		return
	}
	off, err := t.offset()
	if err != nil {
		t.addrInfo.SetText(err.Error())
		return
	}
	t.addrInfo.SetText(modbus.DescribeAddress(modbus.AreaOf(t.function()), off))
}

// request 按界面上的设置组请求。
func (t *typeTool) request() (modbus.Request, error) {
	slave, err := strconv.Atoi(strings.TrimSpace(t.slave.Text))
	if err != nil || slave < 1 || slave > 255 {
		return modbus.Request{}, errors.New("Slave ID 应为 1–255")
	}
	off, err := t.offset()
	if err != nil {
		return modbus.Request{}, err
	}
	f := t.function()
	limit := readDef{Function: f}.maxQty()
	n, err := strconv.Atoi(strings.TrimSpace(t.qty.Text))
	if err != nil || n < 1 || n > limit {
		return modbus.Request{}, fmt.Errorf("数量应为 1–%d", limit)
	}
	if int(off)+n > 0x10000 {
		return modbus.Request{}, fmt.Errorf("起始地址 %d 加数量 %d 超出 65535", off, n)
	}
	return modbus.Request{Slave: byte(slave), Function: f, Address: off, Quantity: uint16(n)}, nil
}

func (t *typeTool) setStatus(text string, imp widget.Importance) {
	t.status.SetText(text)
	t.status.Importance = imp
	t.status.Refresh()
}

func (t *typeTool) read() {
	if t.busy {
		return
	}
	s := t.ws.session
	if s == nil {
		t.stopLoop()
		t.setStatus("未连接：先在主窗口点“连接”", widget.DangerImportance)
		return
	}
	req, err := t.request()
	if err != nil {
		t.stopLoop()
		t.setStatus(err.Error(), widget.DangerImportance)
		return
	}
	t.busy = true
	timeout := t.ws.timeout
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 2*timeout+5*time.Second)
		start := time.Now()
		resp, err := s.client.Do(ctx, req)
		cost := time.Since(start)
		cancel()
		uiDo(func() {
			t.busy = false
			t.showResult(req, resp, err, cost)
		})
	}()
}

func (t *typeTool) showResult(req modbus.Request, resp *modbus.Response, err error, cost time.Duration) {
	stamp := time.Now().Format("15:04:05.000")
	what := fmt.Sprintf("Slave %d · %s · %s", req.Slave, req.Function, refSpan(modbus.AreaOf(req.Function), req.Address, int(req.Quantity)))
	if err != nil {
		text := fmt.Sprintf("%s  %s：%s · 耗时 %s", stamp, what, errSummary(err), formatRTT(cost))
		if ex, ok := modbus.AsException(err); ok {
			text += "\n" + ex.Code.Tip()
			if ex.Code == modbus.ExceptionIllegalFunction || ex.Code == modbus.ExceptionIllegalDataAddress {
				text += "\n点“探测功能码”看看这个地址用哪个功能码能读。"
			}
		}
		t.setStatus(text, widget.DangerImportance)
		return
	}
	t.req, t.regs, t.bits = req, resp.Registers, resp.Bits
	t.setStatus(fmt.Sprintf("%s  %s：正常响应 · 耗时 %s", stamp, what, formatRTT(cost)), widget.MediumImportance)
	t.render()
}

// render 按当前数据类型重新解读最近一次读到的数据，刷新表格、统计和建议。
func (t *typeTool) render() {
	if t.table == nil {
		return
	}
	dt := t.dataType()
	bits := t.isBits(t.req.Function)
	t.orders, t.best = nil, ""
	if !bits {
		t.orders = dt.Orders()
	}
	var opts []string
	for _, o := range t.orders {
		opts = append(opts, string(o))
	}
	t.order.Options = opts
	if bits {
		t.order.ClearSelected()
		t.order.Disable()
	} else {
		t.order.Enable()
		if sel := modbus.ByteOrder(t.order.Selected).For(dt); sel != "" {
			t.order.SetSelected(string(sel))
		} else {
			t.order.SetSelected(opts[0])
		}
	}
	t.advice.SetText(t.analyse(dt, bits))
	if t.best != "" {
		t.order.SetSelected(string(t.best))
	}
	widths := []float32{84, 46*float32(dt.Registers()) + 8}
	valueW := map[int]float32{1: 96, 2: 136, 4: 200}[dt.Registers()]
	if bits {
		widths = []float32{84, 96}
	}
	for c := 0; c < 2+len(t.orders); c++ {
		w := valueW
		if c < len(widths) {
			w = widths[c]
		}
		t.table.SetColumnWidth(c, w)
	}
	t.table.Refresh()
}

// analyse 统计每种字节序有几个合理的浮点值，给出建议；整型没有合理性可判断，提示对照设备面板。
func (t *typeTool) analyse(dt modbus.DataType, bits bool) string {
	switch {
	case t.regs == nil && t.bits == nil:
		return ""
	case bits:
		on := 0
		for _, b := range t.bits {
			if b {
				on++
			}
		}
		return fmt.Sprintf("%d 个位，%d 个 ON。", len(t.bits), on)
	}
	n := dt.Registers()
	values := len(t.regs) / n
	note := ""
	if rest := len(t.regs) % n; rest > 0 {
		note = fmt.Sprintf("末尾 %d 个寄存器凑不成一个 %s，没有显示。", rest, dt)
	}
	if values == 0 {
		return fmt.Sprintf("读到 %d 个寄存器，不够一个 %s（要 %d 个）：把数量改大。", len(t.regs), dt, n)
	}
	if !dt.Float() {
		return strings.TrimSpace("整型没有合理不合理之分：对照设备面板或说明书上的值，看哪一列对得上。" + note)
	}
	counts := map[modbus.ByteOrder]int{}
	zero := 0
	for i := 0; i < values; i++ {
		part := t.regs[i*n : (i+1)*n]
		if allZero(part) {
			zero++
		}
		for _, o := range t.orders {
			if v, err := modbus.DecodeRaw(dt, o, part); err == nil && isPlausible(dt, v) {
				counts[o]++
			}
		}
	}
	var parts []string
	top, tie := modbus.ByteOrder(""), false
	for _, o := range t.orders {
		parts = append(parts, fmt.Sprintf("%s %d", o, counts[o]))
		switch {
		case top == "" || counts[o] > counts[top]:
			top, tie = o, false
		case counts[o] == counts[top]:
			tie = true
		}
	}
	summary := fmt.Sprintf("%d 个值里合理的个数：%s。", values, strings.Join(parts, " · "))
	switch {
	case zero == values:
		return summary + "数据全是 0，四种字节序结果一样：换一段有非零数据的地址再看。" + note
	case counts[top] == 0:
		return summary + "四种字节序都不合理：可能不是 " + string(dt) + "，或起始地址差了一位（把起始地址加减 1 再读）。" + note
	case tie:
		return summary + "几种字节序结果一样合理，看数值大小和设备面板对照。" + note
	}
	t.best = top
	return summary + fmt.Sprintf("字节序很可能是 %s（已在下方选好）。", top) + note
}

func allZero(regs []uint16) bool {
	for _, r := range regs {
		if r != 0 {
			return false
		}
	}
	return true
}

func (t *typeTool) size() (int, int) {
	if t.isBits(t.req.Function) {
		return len(t.bits), 2
	}
	return len(t.regs) / t.dataType().Registers(), 2 + len(t.orders)
}

func (t *typeTool) header(col int) string {
	switch col {
	case 0:
		return "地址"
	case 1:
		if t.isBits(t.req.Function) {
			return "值"
		}
		return "原始寄存器"
	}
	if col-2 >= len(t.orders) {
		return ""
	}
	o := t.orders[col-2]
	if o == t.best {
		return string(o) + " ✓"
	}
	return string(o)
}

// cellText 返回表格单元的文字和样式；导出、复制也用它。
func (t *typeTool) cellText(row, col int) (string, widget.Importance) {
	area := modbus.AreaOf(t.req.Function)
	if t.isBits(t.req.Function) {
		if row >= len(t.bits) {
			return "", widget.MediumImportance
		}
		if col == 0 {
			return modbus.Reference(area, t.req.Address+uint16(row)), widget.LowImportance
		}
		if t.bits[row] {
			return "1 ON", widget.HighImportance
		}
		return "0 OFF", widget.MediumImportance
	}
	dt := t.dataType()
	n := dt.Registers()
	if (row+1)*n > len(t.regs) {
		return "", widget.MediumImportance
	}
	part := t.regs[row*n : (row+1)*n]
	switch col {
	case 0:
		return modbus.Reference(area, t.req.Address+uint16(row*n)), widget.LowImportance
	case 1:
		var words []string
		for _, r := range part {
			words = append(words, fmt.Sprintf("%04X", r))
		}
		return strings.Join(words, " "), widget.LowImportance
	}
	if col-2 >= len(t.orders) {
		return "", widget.MediumImportance
	}
	o := t.orders[col-2]
	if !dt.Float() {
		s, err := modbus.FormatInt(dt, o, part)
		if err != nil {
			return err.Error(), widget.DangerImportance
		}
		return s, widget.MediumImportance
	}
	v, err := modbus.DecodeRaw(dt, o, part)
	if err != nil {
		return err.Error(), widget.DangerImportance
	}
	if !isPlausible(dt, v) {
		return formatFloat(v), widget.LowImportance
	}
	return formatFloat(v), widget.MediumImportance
}

func (t *typeTool) updateCell(id widget.TableCellID, l *widget.Label) {
	text, imp := t.cellText(id.Row, id.Col)
	l.TextStyle = fyne.TextStyle{Monospace: true}
	l.Alignment = fyne.TextAlignTrailing
	if id.Col == 0 {
		l.Alignment = fyne.TextAlignLeading
	}
	l.Importance = imp
	l.SetText(text)
}

// text 是整张表（含表头），制表符分隔，可以直接粘贴进 Excel。
func (t *typeTool) text() string {
	rows, cols := t.size()
	var b strings.Builder
	for c := 0; c < cols; c++ {
		if c > 0 {
			b.WriteByte('\t')
		}
		b.WriteString(strings.TrimSuffix(t.header(c), " ✓"))
	}
	for r := 0; r < rows; r++ {
		b.WriteByte('\n')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte('\t')
			}
			s, _ := t.cellText(r, c)
			b.WriteString(s)
		}
	}
	return b.String()
}

// probeFunctions 依次用 03、04、01、02 读同一地址和数量（位读取数量相同），列出哪些功能码有正常响应。
// 当前功能码读不了、别的能读时，切到第一个能读的并重新读取。连接断开就停。
func (t *typeTool) probeFunctions() {
	s := t.ws.session
	if s == nil {
		t.setStatus("未连接：先在主窗口点“连接”", widget.DangerImportance)
		return
	}
	req, err := t.request()
	if err != nil {
		t.setStatus(err.Error(), widget.DangerImportance)
		return
	}
	cur := req.Function
	t.stopLoop()
	t.probeBtn.Disable()
	t.readBtn.Disable()
	t.setStatus(fmt.Sprintf("正在依次用 03 / 04 / 01 / 02 读 Offset %d × %d…", req.Address, req.Quantity), widget.MediumImportance)
	timeout := t.ws.timeout
	go func() {
		var lines []string
		var good []modbus.FunctionCode
		for _, f := range probeFuncs {
			r := req
			r.Function = f
			ctx, cancel := context.WithTimeout(s.ctx, 2*timeout+5*time.Second)
			_, err := s.client.Do(ctx, r)
			cancel()
			if s.ctx.Err() != nil || errors.Is(err, modbus.ErrConnection) {
				lines = append(lines, fmt.Sprintf("%s：连接断开，探测停止", funcLabels[f]))
				break
			}
			res := "正常响应"
			if err != nil {
				res = errSummary(err)
			} else {
				good = append(good, f)
			}
			lines = append(lines, fmt.Sprintf("%s（%s）：%s", funcLabels[f], modbus.Reference(modbus.AreaOf(f), req.Address), res))
		}
		uiDo(func() {
			t.probeBtn.Enable()
			t.readBtn.Enable()
			if t.ws.closed {
				return
			}
			text := strings.Join(lines, "\n")
			imp := widget.MediumImportance
			switch {
			case len(good) == 0:
				text += "\n四个功能码都读不了：异常 02 说明地址不对，超时说明 Slave ID、协议或接线不对。"
				imp = widget.DangerImportance
			case containsFunc(good, cur):
				text += fmt.Sprintf("\n当前功能码 %02X 能读。", byte(cur))
			default:
				text += fmt.Sprintf("\n当前功能码 %02X 读不了，已切到 %s 并重新读取。", byte(cur), funcLabels[good[0]])
				t.fn.SetSelected(funcLabels[good[0]]) // 地址框跟着换成新数据区的写法
			}
			t.setStatus(text, imp)
			if len(good) > 0 && !containsFunc(good, cur) {
				t.read()
			}
		})
	}()
}

func containsFunc(fs []modbus.FunctionCode, f modbus.FunctionCode) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

func (t *typeTool) startLoop() {
	if t.stop != nil {
		return
	}
	ms, err := strconv.Atoi(strings.TrimSpace(t.period.Text))
	if err != nil || ms < 100 {
		t.loop.SetChecked(false)
		t.setStatus("刷新周期应为不小于 100 的整数（ms）", widget.DangerImportance)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.stop = cancel
	go func() {
		tick := time.NewTicker(time.Duration(ms) * time.Millisecond)
		defer tick.Stop()
		for {
			uiDo(func() {
				if t.stop != nil {
					t.read()
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *typeTool) stopLoop() {
	if t.stop == nil {
		return
	}
	t.stop()
	t.stop = nil
	if t.loop.Checked {
		t.loop.SetChecked(false)
	}
}

// toolKind 把数据类型换成读取窗口的显示格式。
func toolKind(dt modbus.DataType) valueKind {
	switch dt {
	case modbus.TypeInt16:
		return kindSigned
	case modbus.TypeInt32:
		return kindInt32
	case modbus.TypeUint32:
		return kindUint32
	case modbus.TypeFloat32:
		return kindFloat32
	case modbus.TypeInt64:
		return kindInt64
	case modbus.TypeUint64:
		return kindUint64
	case modbus.TypeFloat64:
		return kindFloat64
	}
	return kindUnsigned
}

// apply 把功能码、地址、数量、数据类型和选中的字节序应用到当前读取窗口（没有时新建），或新建一个读取窗口。
// 应用到已有窗口时保留它的名称、扫描周期、每列行数和原始值开关。
func (t *typeTool) apply(newWin bool) {
	ws := t.ws
	req, err := t.request()
	if err != nil {
		t.setStatus(err.Error(), widget.DangerImportance)
		return
	}
	d := defaultDef()
	cur := ws.current()
	if cur != nil && !newWin {
		d = cur.def
	}
	d.Slave, d.Function, d.Start, d.Qty = req.Slave, req.Function, req.Address, int(req.Quantity)
	d.Formats = nil // 调试工具的应用操作替换整个窗口的显示定义。
	if !t.isBits(req.Function) {
		d.Kind = toolKind(t.dataType())
		if t.order.Selected != "" {
			d.Order = modbus.ByteOrder(t.order.Selected)
		}
	}
	if err := d.validate(); err != nil {
		t.setStatus(err.Error(), widget.DangerImportance)
		return
	}
	if cur == nil || newWin {
		cur = ws.addWindow(d)
	} else {
		ws.applyDef(cur, d)
	}
	ws.setCurrent(cur)
	t.setStatus(fmt.Sprintf("已应用到窗口 %d：%s · %s · %s", cur.no, d.Function, refSpan(d.area(), d.Start, d.Qty), d.format()), widget.MediumImportance)
}
