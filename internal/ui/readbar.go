package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// 读取窗口的控制条：功能码、显示格式、字节序和“原始值”开关放在一行，选了立即按新设置读取和显示，
// 调试功能码和大小端时不用反复打开读取定义。另有 activator：点读取窗口里任何地方都把它设为当前窗口。

var funcLabels = map[modbus.FunctionCode]string{
	modbus.FuncReadCoils:            "01 线圈",
	modbus.FuncReadDiscreteInputs:   "02 离散输入",
	modbus.FuncReadHoldingRegisters: "03 保持寄存器",
	modbus.FuncReadInputRegisters:   "04 输入寄存器",
}

// kindLabel 是显示格式在控制条里的写法，“工程值（点表）”太长，写成“点表”。
func kindLabel(k valueKind) string {
	if k == kindPoint {
		return "点表"
	}
	return string(k)
}

type readBar struct {
	w       *readWindow
	fn      *widget.Select
	kind    *widget.Select
	order   *widget.Select
	raw     *widget.Check
	syncing bool            // 程序同步选项时不当作用户修改
	box     *fyne.Container // 控制条的内容，按它的宽度定子窗口的初始宽度
	root    fyne.CanvasObject
}

func newReadBar(w *readWindow) *readBar {
	b := &readBar{w: w}
	ws := w.ws
	var fns []string
	for _, f := range readFuncs {
		fns = append(fns, funcLabels[f])
	}
	b.fn = widget.NewSelect(fns, func(s string) {
		if b.syncing {
			return
		}
		for f, l := range funcLabels {
			if l == s {
				ws.setCurrent(w)
				ws.redefine(w, func(d *readDef) {
					d.Function = f
					d.Qty = min(d.Qty, d.maxQty())
				})
			}
		}
	})
	var kinds []string
	for _, k := range valueKinds {
		kinds = append(kinds, kindLabel(k))
	}
	b.kind = widget.NewSelect(kinds, func(s string) {
		if b.syncing {
			return
		}
		for _, k := range valueKinds {
			if kindLabel(k) == s {
				ws.setCurrent(w)
				ws.redefine(w, func(d *readDef) { d.Kind = k })
			}
		}
	})
	b.order = widget.NewSelect(nil, func(s string) {
		if b.syncing || s == "" {
			return
		}
		ws.setCurrent(w)
		ws.setWindowOrder(w, modbus.ByteOrder(s))
	})
	b.raw = widget.NewCheck("原始值", func(on bool) {
		if b.syncing {
			return
		}
		ws.setCurrent(w)
		ws.redefine(w, func(d *readDef) { d.Raw = on })
	})
	// Fyne 的下拉框最小宽度取占位文字的宽度（默认“(Select one)”），设成最长的选项，宽度刚好够用
	b.fn.PlaceHolder = funcLabels[modbus.FuncReadInputRegisters]
	b.kind.PlaceHolder = string(kindUnsigned)
	b.order.PlaceHolder = string(modbus.OrderABCD)
	// 窄窗口里放不下时可以横向滚动，不把整个窗口撑宽
	b.box = container.NewHBox(b.fn, b.kind, b.order, b.raw)
	b.root = container.NewHScroll(b.box)
	return b
}

// sync 让控制条显示窗口当前的读取定义。
func (b *readBar) sync() {
	b.syncing = true
	defer func() { b.syncing = false }()
	d := b.w.def
	b.fn.SetSelected(funcLabels[d.Function])
	b.kind.SetSelected(kindLabel(d.Kind))
	b.raw.SetChecked(d.Raw)
	if d.bits() { // 线圈、离散输入只有 0 / 1，没有格式和字节序
		b.kind.Disable()
		b.raw.Disable()
	} else {
		b.kind.Enable()
		b.raw.Enable()
	}
	opts, cur, ok := b.w.orderChoice()
	b.order.Options = opts
	b.order.PlaceHolder = string(modbus.OrderABCD)
	for _, o := range opts {
		if len(o) > len(b.order.PlaceHolder) {
			b.order.PlaceHolder = o // 64 位格式的 8 字母写法
		}
	}
	if ok {
		b.order.Enable()
		b.order.SetSelected(string(cur))
	} else {
		b.order.ClearSelected()
		b.order.Disable()
	}
	b.order.Refresh()
}

// orderChoice 是字节序下拉框的选项和当前值：16 位格式 AB / BA（字节交换），32 位、64 位格式各四种；
// 点表窗口是本窗口里 32 / 64 位点的字节序（按 32 位写法，取最多的那种）。
// 线圈、离散输入和没有多寄存器点的点表窗口没有字节序。
func (w *readWindow) orderChoice() (opts []string, cur modbus.ByteOrder, ok bool) {
	d := w.def
	names := func(os []modbus.ByteOrder) []string {
		var out []string
		for _, o := range os {
			out = append(out, string(o))
		}
		return out
	}
	switch {
	case d.bits():
		return nil, "", false
	case d.Kind == kindPoint:
		count := map[modbus.ByteOrder]int{}
		for k, p := range w.ws.points {
			if k.area == d.area() && int(k.off) >= int(d.Start) && int(k.off)+p.regs() <= int(d.Start)+d.Qty && p.Type != typeString && p.regs() > 1 {
				count[p.Order.For(modbus.TypeFloat32)]++
			}
		}
		for _, o := range modbus.Orders32 {
			if count[o] > count[cur] {
				cur = o
			}
		}
		return names(modbus.Orders32), cur, cur != ""
	case d.Kind.width() == 4:
		return names(modbus.Orders64), d.Order.For(modbus.TypeFloat64), true
	case d.Kind.width() == 2:
		return names(modbus.Orders32), d.Order.For(modbus.TypeFloat32), true
	}
	return []string{string(modbus.OrderAB), string(modbus.OrderBA)}, d.Order.For(modbus.TypeUint16), true
}

// setWindowOrder 按控制条选的字节序重新显示：点表窗口改本窗口里的 32 / 64 位点，其他窗口改显示格式的字节序。
func (ws *Workspace) setWindowOrder(w *readWindow, o modbus.ByteOrder) {
	if w.def.Kind == kindPoint {
		ws.changePointOrder(orderWindow, w, 0, o)
		return
	}
	ws.redefine(w, func(d *readDef) { d.Order = o })
}

// activator 包住读取窗口的内容：点窗口里不响应点击的地方（状态行、空白）也把它设为当前窗口，
// 当前窗口提到最上面、标题栏高亮（mdi.go），看得出快捷键和“写入”作用于哪个窗口。
type activator struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onTap   func()
}

func newActivator(content fyne.CanvasObject, onTap func()) *activator {
	a := &activator{content: content, onTap: onTap}
	a.ExtendBaseWidget(a)
	return a
}

func (a *activator) Tapped(*fyne.PointEvent) { a.onTap() }

func (a *activator) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(a.content) }
