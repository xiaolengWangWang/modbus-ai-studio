package ui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

var readFuncs = []modbus.FunctionCode{modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs, modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters}

func funcByName(s string) modbus.FunctionCode {
	for _, f := range readFuncs {
		if f.String() == s {
			return f
		}
	}
	return modbus.FuncReadHoldingRegisters
}

// startText 是起始地址输入框的初始内容：寄存器和离散输入用 40001 这类写法，线圈用 Offset。
func startText(d readDef) string {
	if d.Function == modbus.FuncReadCoils {
		return strconv.Itoa(int(d.Start))
	}
	return modbus.Reference(d.area(), d.Start)
}

// showDefinition 打开读取定义（Modbus Poll 的 Read/Write Definition）。地址支持 346、0x015A、40347、4x0347；
// 有歧义时列出全部解释让用户选，不做猜测（设计文档 6.3）。
func (ws *Workspace) showDefinition(w *readWindow) {
	d := w.def
	name := widget.NewEntry()
	name.SetText(d.Name)
	name.SetPlaceHolder("可选，例如 1# 换热机组")
	slave := widget.NewEntry()
	slave.SetText(strconv.Itoa(int(d.Slave)))
	var fnNames []string
	for _, f := range readFuncs {
		fnNames = append(fnNames, f.String())
	}
	fn := widget.NewSelect(fnNames, nil)
	fn.SetSelected(d.Function.String())
	addr := widget.NewEntry()
	addr.SetText(startText(d))
	addrInfo := widget.NewLabel("")
	addrInfo.Wrapping = fyne.TextWrapWord
	cand := widget.NewRadioGroup(nil, nil)
	cand.Hide()
	qty := widget.NewEntry()
	qty.SetText(strconv.Itoa(d.Qty))
	scan := widget.NewEntry()
	scan.SetText(strconv.FormatInt(d.Scan.Milliseconds(), 10))
	kind := widget.NewSelect(kindNames(), nil)
	kind.SetSelected(string(d.Kind))
	// 字节序的选项随显示格式变：32 位格式列 ABCD 等四种，64 位格式列 ABCDEFGH 等四种，换格式时换成同一种写法
	order := widget.NewSelect(nil, nil)
	order.Selected = string(d.Order)
	if order.Selected == "" {
		order.Selected = string(modbus.OrderABCD)
	}
	var rowNames []string
	for _, r := range rowOptions {
		rowNames = append(rowNames, strconv.Itoa(r))
	}
	rows := widget.NewSelect(rowNames, nil)
	rows.SetSelected(strconv.Itoa(d.Rows))

	var cands []modbus.AddressCandidate
	candLabel := func(c modbus.AddressCandidate) string {
		return fmt.Sprintf("Offset %d（%s）", c.Offset, c.Label)
	}
	chosen := func() (modbus.AddressCandidate, error) {
		if len(cands) == 0 {
			_, err := modbus.ParseAddress(addr.Text)
			return modbus.AddressCandidate{}, err
		}
		for _, c := range cands {
			if candLabel(c) == cand.Selected {
				return c, nil
			}
		}
		return cands[0], nil
	}
	showInfo := func() {
		c, err := chosen()
		if err != nil {
			addrInfo.SetText(err.Error())
			addrInfo.Importance = widget.DangerImportance
			addrInfo.Refresh()
			return
		}
		f := funcByName(fn.Selected)
		text := modbus.DescribeAddress(modbus.AreaOf(f), c.Offset)
		if c.Area != modbus.AreaNone && c.Area != modbus.AreaOf(f) {
			text += fmt.Sprintf("\n注意：地址写法是 %s，但功能码是 %s", c.Area.Prefix(), f)
		}
		addrInfo.SetText(text)
		addrInfo.Importance = widget.MediumImportance
		addrInfo.Refresh()
	}
	// 选中带数据区的解释（例如 30001 → 3x）时，把功能码切到对应的读功能码
	cand.OnChanged = func(string) {
		if c, err := chosen(); err == nil && c.Area != modbus.AreaNone && c.Area != modbus.AreaOf(funcByName(fn.Selected)) {
			fn.SetSelected(c.Area.ReadFunction().String())
		}
		showInfo()
	}
	parseAddr := func() {
		var err error
		cands, err = modbus.ParseAddress(addr.Text)
		if err != nil || len(cands) < 2 {
			cand.Hide()
			cand.Options = nil
			if err == nil && cands[0].Area != modbus.AreaNone && cands[0].Area != modbus.AreaOf(funcByName(fn.Selected)) {
				fn.SetSelected(cands[0].Area.ReadFunction().String())
			}
			showInfo()
			return
		}
		var opts []string
		pick := ""
		for _, c := range cands {
			opts = append(opts, candLabel(c))
			if pick == "" && c.Area == modbus.AreaOf(funcByName(fn.Selected)) {
				pick = candLabel(c)
			}
		}
		if pick == "" {
			pick = opts[0]
		}
		cand.Options = opts
		cand.Selected = ""
		cand.SetSelected(pick)
		cand.Show()
		showInfo()
	}
	addr.OnChanged = func(string) { parseAddr() }
	addr.Validator = func(s string) error {
		_, err := modbus.ParseAddress(s)
		return err
	}
	slave.Validator = func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < 1 || v > 255 {
			return errors.New("Slave ID 应为 1–255")
		}
		return nil
	}
	qty.Validator = func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		limit := readDef{Function: funcByName(fn.Selected)}.maxQty()
		if err != nil || v < 1 || v > limit {
			return fmt.Errorf("数量应为 1–%d", limit)
		}
		return nil
	}
	scan.Validator = func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < 20 || v > 3600000 {
			return errors.New("扫描周期应为 20–3600000 ms")
		}
		return nil
	}
	updateKind := func() {
		f := funcByName(fn.Selected)
		bits := f == modbus.FuncReadCoils || f == modbus.FuncReadDiscreteInputs
		if bits {
			kind.Disable()
		} else {
			kind.Enable()
		}
		k := valueKind(kind.Selected)
		dt := k.dataType() // 16 位格式 AB / BA（字节交换），32、64 位格式各四种
		if k == kindPoint {
			dt = modbus.TypeUint32 // 点表窗口的字节序按点，在控制条或“调整点表字节序”里改
		}
		var names []string
		for _, o := range dt.Orders() {
			names = append(names, string(o))
		}
		order.Options = names
		order.SetSelected(string(modbus.ByteOrder(order.Selected).For(dt)))
		if !bits && k != kindPoint {
			order.Enable()
		} else {
			order.Disable()
		}
	}
	fn.OnChanged = func(string) {
		qty.Validate()
		updateKind()
		showInfo()
	}
	kind.OnChanged = func(string) { updateKind() }
	parseAddr()
	updateKind()

	items := []*widget.FormItem{
		widget.NewFormItem("名称", name),
		widget.NewFormItem("Slave ID", slave),
		widget.NewFormItem("功能码", fn),
		widget.NewFormItem("起始地址", addr),
		widget.NewFormItem("", cand),
		widget.NewFormItem("", addrInfo),
		widget.NewFormItem("数量", qty),
		widget.NewFormItem("扫描周期（ms）", scan),
		widget.NewFormItem("显示格式", kind),
		widget.NewFormItem("字节序", order),
		widget.NewFormItem("每列行数", rows),
	}
	dlg := dialog.NewForm(fmt.Sprintf("读取定义 · 窗口 %d", w.no), "确定", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		c, err := chosen()
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		sv, _ := strconv.Atoi(strings.TrimSpace(slave.Text))
		n, _ := strconv.Atoi(strings.TrimSpace(qty.Text))
		ms, _ := strconv.Atoi(strings.TrimSpace(scan.Text))
		r, _ := strconv.Atoi(rows.Selected)
		nd := readDef{Name: strings.TrimSpace(name.Text), Slave: byte(sv), Function: funcByName(fn.Selected), Start: c.Offset, Qty: n,
			Scan: time.Duration(ms) * time.Millisecond, Kind: valueKind(kind.Selected), Order: modbus.ByteOrder(order.Selected), Rows: r, Raw: d.Raw}
		if err := nd.validate(); err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.applyDef(w, nd)
	}, ws.win)
	dlg.Resize(fyne.NewSize(520, 0))
	dlg.Show()
}

// readDef 是读取窗口的读取定义（Modbus Poll 的 Read/Write Definition）。
type readDef struct {
	Name     string // 窗口别名，可空
	Slave    byte
	Function modbus.FunctionCode // 01–04
	Start    uint16              // 协议地址（0 起始）
	Qty      int
	Scan     time.Duration
	Kind     valueKind
	Order    modbus.ByteOrder // 显示格式的字节序，用 For 换成格式对应宽度的写法；16 位格式 BA 表示字节交换
	Rows     int              // 每列行数
	Raw      bool             `json:",omitempty"` // 多显示一列寄存器的十六进制原始值
}

var rowOptions = []int{10, 20, 50, 100}

func (d readDef) area() modbus.Area { return modbus.AreaOf(d.Function) }

func (d readDef) bits() bool {
	return d.Function == modbus.FuncReadCoils || d.Function == modbus.FuncReadDiscreteInputs
}

// usesPoints 表示按点表解码，四个数据区都可以有点。
func (d readDef) usesPoints() bool { return d.Kind == kindPoint }

// names 表示显示名称和单位列：按点表解码，且读取范围内有点。
func (d readDef) names(pts pointTable) bool {
	if !d.usesPoints() {
		return false
	}
	for k := range pts {
		if k.area == d.area() && int(k.off) >= int(d.Start) && int(k.off) < int(d.Start)+d.Qty {
			return true
		}
	}
	return false
}

func (d readDef) maxQty() int {
	if d.bits() {
		return modbus.MaxReadBits
	}
	return modbus.MaxReadRegisters
}

func (d readDef) validate() error {
	switch {
	case d.Slave == 0:
		return errors.New("Slave ID 应为 1–255（0 是广播，读请求没有响应）")
	case d.Qty < 1 || d.Qty > d.maxQty():
		return fmt.Errorf("%s 的数量应为 1–%d", d.Function, d.maxQty())
	case int(d.Start)+d.Qty > 0x10000:
		return fmt.Errorf("起始地址 %d 加数量 %d 超出 65535", d.Start, d.Qty)
	case d.Scan < 20*time.Millisecond || d.Scan > time.Hour:
		return errors.New("扫描周期应为 20 ms–1 小时")
	case d.Rows < 1:
		return errors.New("每列行数至少为 1")
	}
	return nil
}

// format 是标题里的显示格式说明。
func (d readDef) format() string {
	switch {
	case d.bits() && d.usesPoints():
		return "点表"
	case d.bits():
		return "位"
	case d.Kind.width() > 1:
		return string(d.Kind) + " " + string(d.Order.For(d.Kind.dataType()))
	case d.Kind != kindPoint && d.Order.For(modbus.TypeUint16) == modbus.OrderBA:
		return string(d.Kind) + " BA"
	}
	return string(d.Kind)
}

type colKind int

const (
	colAddr colKind = iota
	colName
	colValue
	colUnit
	colRaw
)

var colTitle = map[colKind]string{colAddr: "地址", colName: "名称", colValue: "值", colUnit: "单位", colRaw: "原始"}

func (d readDef) columns(pts pointTable) []colKind {
	cols := []colKind{colAddr, colValue}
	if d.names(pts) {
		cols = []colKind{colAddr, colName, colValue, colUnit}
	}
	if d.Raw && !d.bits() {
		cols = append(cols, colRaw)
	}
	return cols
}

// textWidth 是读取范围内点的某段文字（名称、单位）需要的列宽，限制在 lo–hi 之间。
func (d readDef) textWidth(pts pointTable, text func(point) string, lo, hi float32) float32 {
	width := lo
	pad := 2*theme.InnerPadding() + 4
	for k, p := range pts {
		if k.area == d.area() && int(k.off) >= int(d.Start) && int(k.off) < int(d.Start)+d.Qty {
			width = max(width, fyne.MeasureText(text(p), theme.TextSize(), fyne.TextStyle{}).Width+pad)
		}
	}
	return min(width, hi)
}

// longInts 表示读取范围内有 64 位整型点：数值最长 20 位，值列要放宽。
func (d readDef) longInts(pts pointTable) bool {
	if !d.usesPoints() {
		return false
	}
	for k, p := range pts {
		if k.area == d.area() && int(k.off) >= int(d.Start) && int(k.off) < int(d.Start)+d.Qty &&
			(p.Type == modbus.TypeInt64 || p.Type == modbus.TypeUint64) {
			return true
		}
	}
	return false
}

func (d readDef) colWidth(k colKind, pts pointTable) float32 {
	switch k {
	case colAddr:
		return 72
	case colName: // 按点名称的长度，短名称不占地方，长名称不被截断
		return d.textWidth(pts, func(p point) string { return p.Name }, 72, 220)
	case colUnit:
		return d.textWidth(pts, func(p point) string { return p.Unit }, 44, 96)
	case colRaw:
		return 64
	}
	switch {
	case d.bits():
		return 56
	case d.Kind == kindBinary:
		return 176
	case d.Kind == kindInt64 || d.Kind == kindUint64 || d.longInts(pts):
		return 180
	case d.Kind == kindHex:
		return 84
	case d.Kind == kindASCII:
		return 64
	}
	return 108
}

func defaultDef() readDef {
	return readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 10, Scan: time.Second,
		Kind: kindSigned, Order: modbus.OrderABCD, Rows: 10}
}
