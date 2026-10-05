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
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/control"
	"modbus-ai-studio/internal/modbus"
)

// writeSpec 是一次写入验证：目标、要写入的寄存器，以及结果里怎么显示数值。
type writeSpec struct {
	title   string
	target  control.Target
	regs    []uint16 // 按写入字节序编码好的寄存器
	value   float64  // 输入的目标值
	rounded bool     // 整型取整改变了输入的值
	format  func(float64) string
	unit    string
}

func (s writeSpec) withUnit(text string) string {
	if s.unit == "" {
		return text
	}
	return text + " " + s.unit
}

func (s writeSpec) show(v float64) string { return s.withUnit(s.format(v)) }

// showRegs 显示寄存器对应的值：64 位整型按寄存器精确显示，其他按工程值 v 显示。
func (s writeSpec) showRegs(regs []uint16, o modbus.ByteOrder, v float64) string {
	if exactInt(s.target.Type, s.target.Scaling) {
		if text, err := modbus.FormatInt(s.target.Type, o, regs); err == nil {
			return s.withUnit(text)
		}
	}
	return s.show(v)
}

// showWrite 打开选中单元的写入对话框：点表里的可写点按工程值写入；其他保持寄存器按窗口的显示格式写入；
// 线圈写 ON / OFF。确认后都会多次回读验证（设计文档 13.4、第 11 章）。
func (ws *Workspace) showWrite(w *readWindow) {
	if ws.readOnly {
		ws.showReadOnlyInfo()
		return
	}
	if !w.canWrite() {
		return
	}
	d := w.def
	off := d.Start + uint16(w.sel)
	switch {
	case d.Function == modbus.FuncReadCoils:
		ws.showCoilWrite(w, off)
	case d.Kind == kindPoint:
		if p, ok := ws.points.get(modbus.AreaHoldingRegisters, off); ok {
			ws.showPointWrite(w, p)
			return
		}
		ws.showRegisterWrite(w, off)
	default:
		ws.showRegisterWrite(w, off)
	}
}

// writeForm 是三种写入对话框共用的部分：编码预览、检查、确认后写入并回读验证。
// build 根据当前输入返回写入规格；返回错误时显示在“检查”里，确认按钮仍可点，点了只提示错误。
// 返回的 update 由调用方挂到输入框的 OnChanged 上。
func (ws *Workspace) writeForm(title string, fields []*widget.FormItem, build func() (writeSpec, []string, error)) (update func()) {
	preview := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	preview.Wrapping = fyne.TextWrapBreak
	checks := widget.NewLabel("")
	checks.Wrapping = fyne.TextWrapWord
	update = func() {
		spec, notes, err := build()
		var lines []string
		if err == nil {
			req := control.WriteRequest(spec.target, spec.regs)
			if pdu, perr := req.PDU(); perr == nil && ws.session != nil {
				if spec.target.Area != modbus.AreaCoils {
					var rs []string
					for i, r := range spec.regs {
						rs = append(rs, fmt.Sprintf("%s = 0x%04X", modbus.Reference(modbus.AreaHoldingRegisters, spec.target.Address+uint16(i)), r))
					}
					lines = append(lines, "寄存器  "+strings.Join(rs, "   "))
				}
				lines = append(lines, fmt.Sprintf("请求    %s（%s）", frameText(ws.session.mode, modbus.EncodeADU(ws.session.mode, spec.target.Slave, 1, pdu)), req.Function))
			}
			if spec.rounded {
				notes = append(notes, "注意：取整后实际写入的值与输入不同")
			}
		}
		if err != nil {
			notes = append([]string{"错误：" + err.Error()}, notes...)
		}
		preview.SetText(strings.Join(lines, "\n"))
		checks.SetText(strings.Join(notes, "\n"))
	}
	update()
	content := container.NewVBox(widget.NewForm(fields...), widget.NewSeparator(),
		widget.NewLabelWithStyle("编码预览", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), preview,
		widget.NewLabelWithStyle("检查", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), checks,
		widget.NewLabel("确认后写入，并在 0 / 200 / 1000 ms 回读验证。"))
	d := dialog.NewCustomConfirm(title, "确认写入", "取消", content, func(ok bool) {
		if !ok {
			return
		}
		spec, _, err := build()
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.runVerify(spec)
	}, ws.win)
	d.Resize(fyne.NewSize(640, 0))
	d.Show()
	return update
}

// showPointWrite 按点表写入工程值：范围检查、字节序检查（设计文档 13.4）。
func (ws *Workspace) showPointWrite(w *readWindow, p point) {
	curRegs, hasCur := w.pointRegs(p)
	curText := "—"
	lo, hi, ranged := p.limits()
	value := widget.NewEntry()
	if hasCur {
		text, _, err := pointText(p, curRegs)
		cur, _, err2 := decodePoint(p, curRegs)
		hasCur = err == nil && err2 == nil
		if hasCur {
			curText = text + " " + p.Unit
			// 预填一个与当前值不同的值；64 位整型按原样预填，避免经过 float64 丢位
			step := 1.0
			if p.Scale < 1 {
				step = p.Scale * 10
			}
			v := cur + step
			if p.Type == modbus.TypeBool {
				v = 1 - cur
			}
			if ranged && v > hi {
				v = cur - step
			}
			if exactInt(p.Type, scaling(p)) {
				value.SetText(text)
			} else {
				value.SetText(formatEng(p, v))
			}
		}
	}
	orders := []string{string(p.Order)}
	if p.Type.Registers() > 1 {
		orders = nil
		for _, o := range p.Type.Orders() {
			orders = append(orders, string(o))
		}
	}
	order := widget.NewSelect(orders, nil)
	order.SetSelected(string(p.Order))
	build := func() (writeSpec, []string, error) {
		t := control.Target{Slave: w.def.Slave, Address: p.Offset, Type: p.Type, ReadOrder: p.Order,
			WriteOrder: modbus.ByteOrder(order.Selected), Scaling: scaling(p)}
		spec := writeSpec{title: p.Name, target: t, format: func(v float64) string { return formatEng(p, v) }, unit: p.Unit}
		regs, v, rounded, err := control.EncodeText(t, value.Text)
		var notes []string
		if t.WriteOrder != p.Order {
			if err == nil {
				if seen, _, e2 := pointText(p, regs); e2 == nil {
					notes = append(notes, fmt.Sprintf("警告：写入 %s，采集 %s。设备按 %s 理解会得到 %s", t.WriteOrder, p.Order, p.Order, seen))
				}
			}
		} else {
			notes = append(notes, fmt.Sprintf("通过：写入字节序与采集一致（%s）", p.Order))
		}
		if err != nil {
			return spec, notes, err
		}
		spec.regs, spec.value, spec.rounded = regs, v, rounded
		if ranged && (v < lo || v > hi) {
			return spec, notes, fmt.Errorf("超出点表允许范围 %v–%v", lo, hi)
		}
		if ranged {
			notes = append([]string{fmt.Sprintf("通过：在允许范围 %v–%v 内", lo, hi)}, notes...)
		}
		return spec, notes, nil
	}
	fields := []*widget.FormItem{
		widget.NewFormItem("当前值", widget.NewLabel(curText)),
		widget.NewFormItem("目标值"+unitSuffix(p.Unit), value),
		widget.NewFormItem("类型", widget.NewLabel(fmt.Sprintf("%s · Scale %v · Slave %d", p.Type, p.Scale, w.def.Slave))),
		widget.NewFormItem("写入字节序", order),
	}
	update := ws.writeForm(fmt.Sprintf("写入 %s %s", addrSpan(p.Offset, p.Type.Registers()), p.Name), fields, build)
	value.OnChanged = func(string) { update() }
	order.OnChanged = func(string) { update() }
}

// showRegisterWrite 按窗口的显示格式写原始值：Signed / Unsigned / Hex / Binary 写 1 个寄存器，32 位格式写 2 个，64 位格式写 4 个。
func (ws *Workspace) showRegisterWrite(w *readWindow, off uint16) {
	d := w.def
	kind := d.Kind
	if kind == kindPoint {
		kind = kindUnsigned
	}
	dt := kind.dataType()
	order := modbus.OrderAB
	n := dt.Registers()
	if n > 1 {
		order = d.Order.For(dt)
	}
	regs, _, _ := w.snapshot()
	i := int(off - d.Start)
	value := widget.NewEntry()
	curText := "—"
	if i+n <= len(regs) {
		if n > 1 {
			curText, _ = formatWide(kind, order, regs[i:i+n])
		} else {
			curText = formatReg(kind, regs[i])
		}
		value.SetText(curText)
	}
	fcs := []string{modbus.FuncWriteSingleRegister.String(), modbus.FuncWriteMultipleRegisters.String()}
	if n > 1 {
		fcs = fcs[1:]
	}
	fc := widget.NewSelect(fcs, nil)
	fc.SetSelected(fcs[0])
	format := func(v float64) string {
		if r, err := modbus.EncodeRaw(dt, order, v); err == nil && n == 1 {
			return formatReg(kind, r[0])
		}
		if dt.Float() {
			return formatFloat(v)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	build := func() (writeSpec, []string, error) {
		f := modbus.FuncWriteMultipleRegisters
		if fc.Selected == modbus.FuncWriteSingleRegister.String() {
			f = modbus.FuncWriteSingleRegister
		}
		t := control.Target{Slave: d.Slave, Address: off, Type: dt, ReadOrder: order, Scaling: modbus.Scaling{Scale: 1}, Function: f}
		spec := writeSpec{title: modbus.Reference(modbus.AreaHoldingRegisters, off), target: t, format: format}
		var notes []string
		if p, ok := ws.points.get(modbus.AreaHoldingRegisters, off); ok && d.Kind != kindPoint {
			notes = append(notes, fmt.Sprintf("提示：点表里这是“%s”（%s %s），按原始值写入", p.Name, p.Type, p.Order))
		}
		var err error
		if exactInt(dt, t.Scaling) {
			spec.regs, spec.value, spec.rounded, err = control.EncodeText(t, value.Text)
			return spec, notes, err
		}
		if spec.value, err = parseValue(kind, value.Text); err == nil {
			spec.regs, spec.rounded, err = control.Encode(t, spec.value)
		}
		return spec, notes, err
	}
	fields := []*widget.FormItem{
		widget.NewFormItem("当前值", widget.NewLabel(curText)),
		widget.NewFormItem("写入值", value),
		widget.NewFormItem("格式", widget.NewLabel(fmt.Sprintf("%s（%s %s）· Slave %d", kind, dt, order, d.Slave))),
		widget.NewFormItem("功能码", fc),
	}
	update := ws.writeForm(fmt.Sprintf("写入 %s", addrSpan(off, n)), fields, build)
	value.OnChanged = func(string) { update() }
	fc.OnChanged = func(string) { update() }
}

// showCoilWrite 写一个线圈：FC05 或 FC15，回读 FC01 验证。
func (ws *Workspace) showCoilWrite(w *readWindow, off uint16) {
	d := w.def
	regs, _, _ := w.snapshot()
	i := int(off - d.Start)
	cur := -1
	if i < len(regs) {
		cur = int(regs[i])
	}
	state := widget.NewRadioGroup([]string{"ON", "OFF"}, nil)
	state.Horizontal = true
	state.SetSelected("ON")
	if cur == 1 {
		state.SetSelected("OFF")
	}
	fcs := []string{modbus.FuncWriteSingleCoil.String(), modbus.FuncWriteMultipleCoils.String()}
	fc := widget.NewSelect(fcs, nil)
	fc.SetSelected(fcs[0])
	onOff := func(v float64) string {
		if v != 0 {
			return "ON"
		}
		return "OFF"
	}
	build := func() (writeSpec, []string, error) {
		f := modbus.FuncWriteSingleCoil
		if fc.Selected == modbus.FuncWriteMultipleCoils.String() {
			f = modbus.FuncWriteMultipleCoils
		}
		t := control.Target{Slave: d.Slave, Area: modbus.AreaCoils, Address: off, Function: f}
		v := 0.0
		if state.Selected == "ON" {
			v = 1
		}
		regs, _, err := control.Encode(t, v)
		return writeSpec{title: "线圈 " + strconv.Itoa(int(off)), target: t, regs: regs, value: v, format: onOff}, nil, err
	}
	curText := "—"
	if cur >= 0 {
		curText = onOff(float64(cur))
	}
	fields := []*widget.FormItem{
		widget.NewFormItem("当前状态", widget.NewLabel(curText)),
		widget.NewFormItem("写入", state),
		widget.NewFormItem("功能码", fc),
	}
	update := ws.writeForm(fmt.Sprintf("写线圈 Offset %d · Slave %d", off, d.Slave), fields, build)
	state.OnChanged = func(string) { update() }
	fc.OnChanged = func(string) { update() }
}

// pointRegs 返回点在窗口最近一次读到的寄存器。
func (w *readWindow) pointRegs(p point) ([]uint16, bool) {
	regs, _, _ := w.snapshot()
	i := int(p.Offset) - int(w.def.Start)
	n := p.Type.Registers()
	if i < 0 || i+n > len(regs) {
		return nil, false
	}
	return regs[i : i+n], true
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return "（" + unit + "）"
}

var resultTitle = map[control.Result]string{
	control.ResultPass:          "控制验证 PASS",
	control.ResultNotApplied:    "控制验证 FAILED：未生效",
	control.ResultOverwritten:   "控制验证 FAILED：被覆盖",
	control.ResultMismatch:      "控制验证 FAILED：值不符",
	control.ResultUnknown:       "控制验证：无法判断",
	control.ResultProtocolError: "控制验证 FAILED：协议拒绝",
}

func (ws *Workspace) runVerify(spec writeSpec) {
	s := ws.session
	if s == nil {
		return
	}
	progress := dialog.NewCustomWithoutButtons("控制验证", container.NewVBox(widget.NewLabel("写入并回读中…"), widget.NewProgressBarInfinite()), ws.win)
	progress.Show()
	go func() {
		rep := control.Verify(s.ctx, s.client, spec.target, spec.regs, control.DefaultSchedule)
		uiDo(func() {
			progress.Hide()
			if !ws.closed {
				ws.showReport(spec, rep)
			}
		})
	}()
}

func (ws *Workspace) showReport(spec writeSpec, rep control.Report) {
	var lines []string
	proto := "SUCCESS"
	switch {
	case errors.Is(rep.WriteErr, modbus.ErrTimeout):
		proto = "超时（可能已执行，以回读为准）"
	case rep.WriteErr != nil:
		proto = rep.WriteErr.Error()
	}
	t := spec.target
	original := spec.showRegs(rep.OriginalRegs, t.ReadOrder, rep.Original)
	if rep.OriginalRegs != nil {
		lines = append(lines, "原值："+original, "目标："+spec.showRegs(spec.regs, t.EncodeOrder(), spec.value), "协议写入："+proto)
	}
	for _, rb := range rep.Readbacks {
		if rb.Err != nil {
			lines = append(lines, fmt.Sprintf("回读 %d ms：%v", rb.At.Milliseconds(), rb.Err))
			continue
		}
		lines = append(lines, fmt.Sprintf("回读 %d ms：%s", rb.At.Milliseconds(), spec.showRegs(rb.Registers, t.ReadOrder, rb.Value)))
	}
	if rep.Hint != "" {
		lines = append(lines, "说明："+rep.Hint)
	}
	title := widget.NewLabelWithStyle(resultTitle[rep.Result], fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	title.Importance = widget.SuccessImportance
	if rep.Result != control.ResultPass {
		title.Importance = widget.DangerImportance
	}
	body := widget.NewLabel(strings.Join(lines, "\n"))
	body.Wrapping = fyne.TextWrapWord
	content := container.NewVBox(title, body)

	changed := len(rep.Readbacks) > 0 && rep.OriginalRegs != nil
	if changed {
		last := rep.Readbacks[len(rep.Readbacks)-1]
		changed = last.Err != nil || spec.showRegs(last.Registers, t.ReadOrder, last.Value) != original
	}
	if rep.Result == control.ResultPass || !changed {
		d := dialog.NewCustom("控制验证 · "+spec.title, "关闭", content, ws.win)
		d.Resize(fyne.NewSize(520, 0))
		d.Show()
		return
	}
	d := dialog.NewCustomConfirm("控制验证 · "+spec.title, "恢复原值", "关闭", content, func(restore bool) {
		if !restore || ws.session == nil {
			return
		}
		c := ws.session.client
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := control.Restore(ctx, c, spec.target, rep.OriginalRegs)
			uiDo(func() {
				if err != nil {
					dialog.ShowError(fmt.Errorf("恢复原值失败，请人工处理：%w", err), ws.win)
					return
				}
				dialog.ShowInformation("控制验证", "已恢复原值 "+original, ws.win)
			})
		}()
	}, ws.win)
	d.Resize(fyne.NewSize(520, 0))
	d.Show()
}
