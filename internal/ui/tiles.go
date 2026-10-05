package ui

import (
	"math"
	"path/filepath"
	"slices"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// loadDemo 按换热站示例点表打开三个读取窗口；勾选了内置模拟器且未连接时顺便连接，马上能看到数据。
// 窗口 3（40601–40604）的模拟器应答慢于默认超时，用来演示错误自动分析。
func (ws *Workspace) loadDemo() {
	ws.setPoints(demoPoints())
	for _, d := range []readDef{
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 20, Scan: time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 346, Qty: 8, Scan: time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
		{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 600, Qty: 4, Scan: 5 * time.Second, Kind: kindPoint, Order: modbus.OrderCDAB, Rows: 10},
	} {
		ws.addWindow(d)
	}
	if ws.session == nil && ws.useSim.Checked && !ws.serialMode() {
		ws.connect()
	}
}

func (ws *Workspace) addWindow(d readDef) *readWindow {
	ws.nextWin++
	w := newReadWindow(ws, ws.nextWin, d)
	ws.windows = append(ws.windows, w)
	ws.relayout()
	ws.setCurrent(ws.cur) // 一次建多个窗口（示例、导入点表、打开工作区）时当前窗口仍是第一个
	w.start()
	return w
}

// current 返回当前读取窗口：最近点过、新建或改过定义的那个；它被关掉后取第一个。没有读取窗口时为 nil。
func (ws *Workspace) current() *readWindow {
	if slices.Contains(ws.windows, ws.cur) {
		return ws.cur
	}
	if len(ws.windows) > 0 {
		return ws.windows[0]
	}
	return nil
}

// setCurrent 设为当前读取窗口。有多个读取窗口时当前窗口的标题高亮、加一圈边框，看得出快捷键和“写入”作用于哪个；
// 换到另一个窗口时取消其他窗口里的选中，同一时间只有一个选中的值。
func (ws *Workspace) setCurrent(w *readWindow) {
	switched := w != nil && w != ws.cur
	ws.cur = w
	cur := ws.current()
	for _, x := range ws.windows {
		active := x == cur && len(ws.windows) > 1
		imp := widget.MediumImportance
		if active {
			imp = widget.HighImportance
		}
		if x.title.Importance != imp {
			x.title.Importance = imp
			x.title.Refresh()
		}
		x.root.setActive(active)
		if switched && x != cur && x.sel >= 0 {
			x.sel = -1
			x.table.UnselectAll()
			x.updateWriteBtn()
			if ws.inspect.src == x {
				ws.inspect.clear() // 解析面板不再显示已取消选中的窗口
			}
		}
	}
}

// addReadWindow 按最后一个窗口的 Slave 和功能码新建读取窗口，并直接打开读取定义。
func (ws *Workspace) addReadWindow() {
	d := defaultDef()
	if n := len(ws.windows); n > 0 {
		last := ws.windows[n-1].def
		d.Slave, d.Function = last.Slave, last.Function
	}
	w := ws.addWindow(d)
	ws.setCurrent(w)
	ws.showDefinition(w)
}

func (ws *Workspace) removeWindow(w *readWindow) {
	w.halt()
	for i, x := range ws.windows {
		if x == w {
			ws.windows = append(ws.windows[:i], ws.windows[i+1:]...)
			break
		}
	}
	if ws.inspect.src == w {
		ws.inspect.clear()
	}
	ws.relayout()
	ws.setCurrent(ws.cur)
}

// redefine 修改读取定义后重新开始轮询，诊断里的一键处理也走这里。
func (ws *Workspace) redefine(w *readWindow, change func(*readDef)) {
	d := w.def
	change(&d)
	if err := d.validate(); err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.applyDef(w, d)
}

func (ws *Workspace) applyDef(w *readWindow, d readDef) {
	w.halt()
	w.setDef(d)
	ws.relayout()
	w.start()
}

func (ws *Workspace) pauseAll(pause bool) {
	for _, w := range ws.windows {
		w.setPaused(pause)
	}
}

// relayout 把读取窗口平铺：列数取 ⌈√n⌉，靠后的列多放一个；分隔条初始位置按各窗口的内容大小分配。
func (ws *Workspace) relayout() {
	var obj fyne.CanvasObject
	if len(ws.windows) == 0 {
		box := container.NewVBox(widget.NewLabel("没有读取窗口。点右上角“读取窗口”新建，或用“读取”菜单里的“打开换热站示例”。"))
		if recent := ws.recent(); len(recent) > 0 {
			box.Add(widget.NewLabelWithStyle("最近打开的工作区", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
			for _, p := range recent {
				b := widget.NewButtonWithIcon(filepath.Base(p), theme.FolderOpenIcon(), func() { ws.openWorkspaceFile(p) })
				b.Alignment = widget.ButtonAlignLeading
				b.Importance = widget.LowImportance
				box.Add(b)
			}
		}
		obj = container.NewCenter(box)
	} else {
		obj = tile(ws.windows)
		if len(ws.windows) > 8 {
			obj = container.NewScroll(obj)
		}
	}
	ws.tiles.Objects = []fyne.CanvasObject{obj}
	ws.tiles.Refresh()
}

func tile(wins []*readWindow) fyne.CanvasObject {
	n := len(wins)
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	base, extra := n/cols, n%cols
	var columns []fyne.CanvasObject
	var widths []float32
	i := 0
	for c := 0; c < cols; c++ {
		k := base
		if c >= cols-extra {
			k++
		}
		var objs []fyne.CanvasObject
		var heights []float32
		var width float32
		for _, w := range wins[i : i+k] {
			objs = append(objs, w.root)
			heights = append(heights, w.prefHeight())
			width = max(width, w.prefWidth())
		}
		i += k
		columns = append(columns, chain(false, objs, heights))
		widths = append(widths, width)
	}
	return chain(true, columns, widths)
}

func chain(horizontal bool, objs []fyne.CanvasObject, weights []float32) fyne.CanvasObject {
	if len(objs) == 1 {
		return objs[0]
	}
	var total float32
	for _, w := range weights {
		total += w
	}
	rest := chain(horizontal, objs[1:], weights[1:])
	var s *container.Split
	if horizontal {
		s = container.NewHSplit(objs[0], rest)
	} else {
		s = container.NewVSplit(objs[0], rest)
	}
	s.Offset = float64(weights[0] / total)
	return s
}
