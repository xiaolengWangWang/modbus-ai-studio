package ui

import (
	"encoding/binary"
	"fmt"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// inspector 是解析面板：选中读取窗口的单元时显示寄存器的多种解读（多解释视图，设计文档 7.3），
// 选中通信报文时逐字段解析报文。
type inspector struct {
	ws    *Workspace
	src   *readWindow // 正在显示寄存器解析的读取窗口，随轮询实时刷新；nil 表示显示的是报文或为空
	title *widget.Label
	body  *fyne.Container
	wrap  *container.ThemeOverride
	rows  []rowView
	text  string
	root  fyne.CanvasObject
}

type rowView struct {
	full               bool
	name, hex, meaning *widget.Label
	obj                fyne.CanvasObject
}

func newInspector(ws *Workspace) *inspector {
	in := &inspector{ws: ws}
	in.title = widget.NewLabelWithStyle("解析", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	in.body = container.NewVBox()
	copyBtn := widget.NewButtonWithIcon("复制", theme.ContentCopyIcon(), func() { ws.app.Clipboard().SetContent(in.text) })
	copyBtn.Importance = widget.LowImportance
	in.wrap = compact(in.body)
	in.root = container.NewBorder(container.NewBorder(nil, nil, nil, copyBtn, in.title), nil, nil, nil, container.NewVScroll(in.wrap))
	in.clear()
	return in
}

func (in *inspector) clear() {
	in.src = nil
	in.show("解析", []decodeRow{{Meaning: "单击读取窗口里的值，查看它在各种数据类型和字节序下的解读；单击通信报文，逐字段解析报文。双击值可以写入。"}})
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

// registerInsight 给出选中寄存器的全部解读：地址的各种写法、16 位的有符号 / 无符号 / 二进制 / 字节交换，
// 以及与下一个寄存器组成 32 位时四种字节序下的 FLOAT32 / INT32 / UINT32，并标出哪些结果合理。
func registerInsight(w *readWindow) []decodeRow {
	d := w.def
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
			m := fmt.Sprintf("FLOAT32 %s（%s）· INT32 %d · UINT32 %d", formatFloat(it.Float32), ok, it.Int32, it.Uint32)
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
			m := fmt.Sprintf("FLOAT64 %s（%s）· INT64 %d · UINT64 %d", formatFloat(f), ok, int64(u), u)
			if width == 4 && o == cur {
				m += " · 当前"
			}
			rows = append(rows, decodeRow{Name: string(o), Hex: hexs(binary.BigEndian.AppendUint64(nil, u)), Meaning: m})
		}
	}
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
