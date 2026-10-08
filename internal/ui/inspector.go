package ui

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// inspector 是解析面板：选中读取窗口的单元时显示寄存器的多种解读（多解释视图，设计文档 7.3），
// 选中通信报文时逐字段解析报文。
type inspector struct {
	ws                    *Workspace
	src                   *readWindow     // 正在显示寄存器解析的读取窗口，随轮询实时刷新；nil 表示显示的是报文或为空
	packet                *modbus.Packet  // immutable copy of a selected packet for explicit AI analysis
	packetContext         []modbus.Packet // only the selected historical session
	event                 *logEntry
	closed, selectionOnly bool
	placeholder           string // 没有内容时的提示
	title                 *widget.Label
	orderBtn              *widget.Button
	body                  *fyne.Container
	wrap                  *container.ThemeOverride
	rows                  []rowView
	text                  string
	root                  fyne.CanvasObject
}

type rowView struct {
	full               bool
	name, hex, meaning *widget.Label
	obj                fyne.CanvasObject
}

func newInspector(ws *Workspace) *inspector {
	in := &inspector{ws: ws, placeholder: "单击读取窗口里的值，查看它在各种数据类型和字节序下的解读；单击通信报文，逐字段解析报文。双击值可以写入。"}
	in.title = widget.NewLabelWithStyle("解析", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	in.title.Truncation = fyne.TextTruncateEllipsis
	in.body = container.NewVBox()
	copyBtn := widget.NewButtonWithIcon("复制", theme.ContentCopyIcon(), func() { ws.app.Clipboard().SetContent(in.text) })
	copyBtn.Importance = widget.LowImportance
	in.orderBtn = widget.NewButton("字节序…", func() { ws.showPointOrderDialog(in.src) })
	in.orderBtn.Importance = widget.LowImportance
	in.wrap = compact(in.body)
	aiBtn := widget.NewButton("AI解释", in.openAI)
	aiBtn.Importance = widget.LowImportance
	in.root = container.NewBorder(container.NewBorder(nil, nil, nil, container.NewHBox(in.orderBtn, copyBtn, aiBtn), in.title), nil, nil, nil, container.NewVScroll(in.wrap))
	in.clear()
	return in
}

func (in *inspector) clear() {
	in.src = nil
	in.packet = nil
	in.show("解析", []decodeRow{{Meaning: in.placeholder}})
}

func newRowView(full bool) rowView {
	r := rowView{full: full, meaning: widget.NewLabel("")}
	r.meaning.Wrapping = fyne.TextWrapWord
	if full {
		r.obj = r.meaning
		return r
	}
	r.name = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	r.name.Truncation = fyne.TextTruncateEllipsis
	r.hex = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	r.hex.Truncation = fyne.TextTruncateEllipsis
	r.obj = container.New(rowLayout{96, 150}, r.name, r.hex, r.meaning)
	return r
}

// rowLayout 把一行排成三列：字段名和字节固定宽度，含义占满剩余宽度并自动换行。
type rowLayout struct{ name, hex float32 }

func (l rowLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var h float32
	for _, o := range objs {
		h = max(h, o.MinSize().Height)
	}
	return fyne.NewSize(l.name+l.hex+80, h)
}

func (l rowLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	objs[0].Move(fyne.NewPos(0, 0))
	objs[0].Resize(fyne.NewSize(l.name, size.Height))
	objs[1].Move(fyne.NewPos(l.name, 0))
	objs[1].Resize(fyne.NewSize(l.hex, size.Height))
	objs[2].Move(fyne.NewPos(l.name+l.hex, 0))
	objs[2].Resize(fyne.NewSize(max(size.Width-l.name-l.hex, 0), size.Height))
}

// show 显示解析结果。行结构不变时只更新文字，轮询刷新时不会闪烁，也不会丢失滚动位置。
func (in *inspector) show(title string, rows []decodeRow) {
	if title != "日志" {
		in.event = nil
	}
	if title != "报文解析" {
		in.packet = nil
	}
	in.title.SetText(title)
	in.text = title + "\n" + rowsText(rows)
	same := len(rows) == len(in.rows)
	for i := 0; same && i < len(rows); i++ {
		same = in.rows[i].full == (rows[i].Name == "" && rows[i].Hex == "")
	}
	if !same {
		in.rows = in.rows[:0]
		objs := make([]fyne.CanvasObject, 0, len(rows))
		for _, r := range rows {
			v := newRowView(r.Name == "" && r.Hex == "")
			in.rows = append(in.rows, v)
			objs = append(objs, v.obj)
		}
		in.body.Objects = objs
		defer in.wrap.Refresh() // 新建的行要重新套用紧凑主题
	}
	for i, r := range rows {
		v := in.rows[i]
		v.meaning.SetText(r.Meaning)
		if !v.full {
			v.name.SetText(r.Name)
			v.hex.SetText(r.Hex)
		}
	}
	in.body.Refresh()
}

func (in *inspector) showPacket(p modbus.Packet) {
	in.src = nil
	in.packet = cloneAIPacket(&p)
	rows := append([]decodeRow{{Name: "原始报文", Hex: "", Meaning: frameText(p.Mode, p.Raw)}}, describePacket(p, in.ws.points)...)
	in.show("报文解析", rows)
}

func (in *inspector) showRegister(w *readWindow) {
	if w.sel < 0 {
		return
	}
	in.src = w
	in.show(fmt.Sprintf("寄存器解析 · 窗口 %d", w.no), registerInsight(w))
}

func (in *inspector) showLog(e logEntry) {
	in.src = nil
	in.packet = nil
	in.event = cloneAILog(&e)
	in.show("日志", e.detail())
}

func (in *inspector) aiTarget() aiTarget {
	if in.event != nil {
		return aiTarget{event: in.event}
	}
	if in.packet != nil {
		return aiTarget{packet: in.packet}
	}
	if in.src != nil {
		return aiTarget{read: in.src}
	}
	if in.selectionOnly {
		return aiTarget{}
	}
	return aiTarget{read: in.ws.current()}
}

func (in *inspector) openAI() { in.ws.openAIFrom(in.aiTarget(), in) }

// registerInsight 给出选中寄存器的全部解读：地址的各种写法、16 位的有符号 / 无符号 / 二进制 / 字节交换，
// 以及与下一个寄存器组成 32 位时四种字节序下的 FLOAT32 / INT32 / UINT32，并标出哪些结果合理。
func registerInsight(w *readWindow) []decodeRow {
	d := w.displayDef(w.sel)
	i := w.sel
	off := d.Start + uint16(i)
	area := d.area()
	rows := []decodeRow{
		{Name: "地址", Meaning: modbus.DescribeAddress(area, off)},
		{Name: "读取", Meaning: fmt.Sprintf("Slave %d · %s · 窗口 %d", d.Slave, d.Function, w.no)},
	}
	regs, lastOK, err := w.snapshot()
	if i >= len(regs) {
		return append(rows, decodeRow{Meaning: "还没有读到数据。"})
	}
	if err != nil {
		rows = append(rows, decodeRow{Name: "注意", Meaning: fmt.Sprintf("最近一次读取失败，下面是 %s 的值", lastOK.Format("15:04:05"))})
	}
	if d.bits() {
		state := "OFF"
		if regs[i] != 0 {
			state = "ON"
		}
		rows = append(rows, decodeRow{Name: "值", Meaning: fmt.Sprintf("%d（%s）", regs[i], state)})
		if p, ok := w.ws.points.get(area, off); ok && d.usesPoints() {
			rows = append(rows, decodeRow{Name: "点表", Meaning: fmt.Sprintf("%s · %s", p.Name, p.Type)})
			if text, _, err := pointText(p, regs[i:i+1]); err == nil {
				rows = append(rows, decodeRow{Name: "工程值", Meaning: text + " " + p.Unit})
			}
		}
		return rows
	}
	r := regs[i]
	b := []byte{byte(r >> 8), byte(r)}
	rows = append(rows,
		decodeRow{Name: "寄存器", Hex: hexs(b), Meaning: fmt.Sprintf("Unsigned %d · Signed %d · Hex 0x%04X", r, int16(r), r)},
		decodeRow{Name: "Binary", Meaning: formatReg(kindBinary, r)},
	)
	sw := r>>8 | r<<8
	rows = append(rows, decodeRow{Name: "BA 字节交换", Hex: hexs([]byte{b[1], b[0]}), Meaning: fmt.Sprintf("Unsigned %d · Signed %d", sw, int16(sw))})
	rows = append(rows, decodeRow{Name: "ASCII", Hex: hexs(b), Meaning: asciiText(b)})
	// 设备型号、序列号常按字符串存：从这里读到窗口末尾或遇到 0 为止，两种字节序都给出
	if str := regs[i:min(len(regs), i+(maxStringLen+1)/2)]; len(str) > 1 && asciiText(b) == string(b) {
		rows = append(rows, decodeRow{Name: "字符串", Meaning: fmt.Sprintf("“%s”（AB）· “%s”（BA 字节交换）",
			decodeString(modbus.OrderAB, str), decodeString(modbus.OrderBA, str))})
	}

	// 当前窗口按哪种宽度和字节序解读这几个寄存器，下面对应的行标“当前”
	cur, width := modbus.ByteOrder(""), 0
	p, isPoint := w.ws.points.get(area, off)
	switch {
	case d.Kind.width() > 1:
		width, cur = d.Kind.width(), d.Order.For(d.Kind.dataType())
	case isPoint && d.Kind == kindPoint && p.Type != typeString && p.regs() > 1:
		width, cur = p.regs(), p.Order
	}
	if i+1 < len(regs) {
		pair := regs[i : i+2]
		rows = append(rows, decodeRow{Meaning: fmt.Sprintf("与 %s 组成 32 位，线上字节 %s：", modbus.Reference(area, off+1), hexs(modbus.RegistersToBytes(pair)))})
		for _, it := range modbus.Interpret32(pair) {
			be := binary.BigEndian.AppendUint32(nil, it.Uint32)
			ok := "合理"
			if !it.Plausible {
				ok = "不合理"
			}
			m := fmt.Sprintf("FLOAT32 %s（%s）· INT32 %d · UINT32 %d", formatFloatExact(it.Float32, 32), ok, it.Int32, it.Uint32)
			if width == 2 && it.Order == cur {
				m += " · 当前"
			}
			rows = append(rows, decodeRow{Name: string(it.Order), Hex: hexs(be), Meaning: m})
		}
		if width == 2 {
			if o, ok := modbus.SuggestByteOrder(cur, pair); ok {
				rows = append(rows, decodeRow{Name: "建议", Meaning: fmt.Sprintf("按 %s 解出的 FLOAT32 不合理，只有 %s 合理，字节序可能是 %s", cur, o, o)})
			}
		}
	}
	if i+3 < len(regs) {
		quad := regs[i : i+4]
		rows = append(rows, decodeRow{Meaning: fmt.Sprintf("与 %s 组成 64 位，线上字节 %s：", refSpan(area, off+1, 3), hexs(modbus.RegistersToBytes(quad)))})
		for _, o := range modbus.Orders64 {
			u, _ := modbus.Bits(modbus.TypeUint64, o, quad)
			f := math.Float64frombits(u)
			ok := "合理"
			if !modbus.PlausibleFloat64(f) {
				ok = "不合理"
			}
			m := fmt.Sprintf("FLOAT64 %s（%s）· INT64 %d · UINT64 %d", formatFloatExact(f, 64), ok, int64(u), u)
			if width == 4 && o == cur {
				m += " · 当前"
			}
			rows = append(rows, decodeRow{Name: string(o), Hex: hexs(binary.BigEndian.AppendUint64(nil, u)), Meaning: m})
		}
	}
	floatType := modbus.TypeFloat32 // 普通寄存器窗口也能在导入点表前诊断 FLOAT32 邻址
	if d.Kind.dataType().Float() {
		floatType = d.Kind.dataType()
	}
	if isPoint && d.Kind == kindPoint && p.Type.Float() {
		floatType = p.Type
	}
	rows = append(rows, adjacentFloatRows(d, i, regs, floatType)...)
	if isPoint {
		access := "只读"
		if p.RW {
			access = "可写"
			if lo, hi, ok := p.limits(); ok {
				access += fmt.Sprintf("，允许 %v–%v", lo, hi)
			}
		}
		if p.Type == typeString {
			rows = append(rows, decodeRow{Name: "点表", Meaning: fmt.Sprintf("%s · STRING %d 字符 · %s · 只读", p.Name, p.Len, p.Order)})
			if n := p.regs(); i+n <= len(regs) {
				rows = append(rows, decodeRow{Name: "内容", Meaning: "“" + decodeString(p.Order, regs[i:i+n]) + "”"})
			}
			return rows
		}
		rows = append(rows, decodeRow{Name: "点表", Meaning: fmt.Sprintf("%s · %s %s · Scale %v · %s", p.Name, p.Type, p.Order, p.Scale, access)})
		if n := p.regs(); i+n <= len(regs) {
			if text, _, err := pointText(p, regs[i:i+n]); err == nil {
				rows = append(rows, decodeRow{Name: "工程值", Meaning: text + " " + p.Unit})
			}
		}
	}
	return rows
}

// formatFloatExact 保留设备原始浮点精度，让两个低位不同的候选值能直接比较。
func formatFloatExact(v float64, bits int) string {
	s := strconv.FormatFloat(v, 'g', -1, bits)
	if !strings.ContainsAny(s, ".eE") && !math.IsNaN(v) && !math.IsInf(v, 0) {
		s += ".0"
	}
	return s
}

// adjacentFloatRows 展示起始地址前后差一位时的候选解读，不据此自动改配置。
func adjacentFloatRows(d readDef, i int, regs []uint16, t modbus.DataType) []decodeRow {
	var rows []decodeRow
	for _, shift := range []int{-1, 1} {
		start := i + shift
		if start < 0 || start+t.Registers() > len(regs) {
			continue
		}
		if len(rows) == 0 {
			rows = append(rows, decodeRow{Meaning: "起始地址差一位时的候选读法："})
		}
		pair := regs[start : start+t.Registers()]
		for _, order := range t.Orders() {
			v, err := modbus.DecodeRaw(t, order, pair)
			if err != nil {
				continue
			}
			quality := "不合理"
			if isPlausible(t, v) {
				quality = "合理"
			}
			rows = append(rows, decodeRow{Name: fmt.Sprintf("%+d %s", shift, order), Hex: hexs(modbus.RegistersToBytes(pair)),
				Meaning: fmt.Sprintf("从 %s 读取 %s %s（%s）", modbus.Reference(d.area(), d.Start+uint16(start)), t, formatFloatExact(v, t.Registers()*16), quality)})
		}
	}
	if len(rows) > 0 {
		rows = append(rows, decodeRow{Meaning: "字节序和地址差一位的结果可能很接近；请对照设备面板确认，程序不会自动选择。"})
	}
	return rows
}
