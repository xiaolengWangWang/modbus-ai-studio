package ui

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/assets/icon"
	"modbus-ai-studio/internal/modbus"
)

// loadDemo 按换热站示例点表打开三个读取窗口；未连接时勾选内置模拟器并连接，马上能看到数据。
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
	ws.mdi.setMaxed(true)
	if ws.session == nil && !ws.connecting && !ws.serialMode() {
		ws.useSim.SetChecked(true)
		ws.connect()
	}
}

// addWindow 新建读取窗口，层叠放在已有窗口上面。一次建多个窗口（示例、导入点表、打开工作区）时当前窗口
// 仍是第一个，调用方建完后层叠一次，各窗口的标题栏都露出来。
func (ws *Workspace) addWindow(d readDef) *readWindow {
	ws.nextWin++
	w := newReadWindow(ws, ws.nextWin, d)
	ws.windows = append(ws.windows, w)
	ws.refreshReadActions()
	ws.mdi.attach(w)
	ws.relayout()
	ws.setCurrent(ws.cur)
	w.start()
	return w
}

// windowOf 返回子窗口 o 对应的读取窗口。
func (ws *Workspace) windowOf(o fyne.CanvasObject) *readWindow {
	for _, w := range ws.windows {
		if w.inner == o {
			return w
		}
	}
	return nil
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

// setCurrent 设为当前读取窗口：提到最上面、标题栏高亮，快捷键和“写入”作用于它；
// 换到另一个窗口时取消其他窗口里的选中，同一时间只有一个选中的值。
func (ws *Workspace) setCurrent(w *readWindow) {
	switched := w != nil && w != ws.cur
	ws.cur = w
	cur := ws.current()
	if cur != nil && ws.mdi.top() != cur.inner {
		ws.mdi.raise(cur)
	}
	for _, x := range ws.windows {
		if switched && x != cur && x.sel >= 0 {
			x.sel = -1
			x.table.UnselectAll()
			x.updateWriteBtn()
			if ws.inspect.src == x {
				ws.inspect.clear() // 解析面板不再显示已取消选中的窗口
			}
		}
	}
	ws.refreshWindowMenu()
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
	ws.mdi.detach(w)
	ws.refreshReadActions()
	if ws.inspect.src == w {
		ws.inspect.clear()
	}
	if ws.cur == w { // 关掉当前窗口后，下面一层的窗口成为当前窗口
		ws.cur = ws.windowOf(ws.mdi.top())
	}
	ws.relayout()
	ws.setCurrent(ws.cur)
}

// redefine 修改读取定义后重新开始轮询，诊断里的一键处理也走这里。
func (ws *Workspace) redefine(w *readWindow, change func(*readDef)) {
	d := w.def
	change(&d)
	d.retainFormats(w.def)
	if err := d.validate(); err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.applyDef(w, d)
}

func (ws *Workspace) applyDef(w *readWindow, d readDef) {
	d.retainFormats(w.def)
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

func (ws *Workspace) allReadsPaused() bool {
	if len(ws.windows) == 0 {
		return false
	}
	for _, w := range ws.windows {
		if !w.paused {
			return false
		}
	}
	return true
}

// 单个窗口和菜单修改暂停状态时，同步工具栏的全部暂停 / 继续按钮。
func (ws *Workspace) refreshReadActions() {
	if ws.pauseAllBtn == nil {
		return
	}
	if ws.allReadsPaused() {
		ws.pauseAllBtn.SetText("全部继续")
		ws.pauseAllBtn.SetIcon(theme.MediaPlayIcon())
	} else {
		ws.pauseAllBtn.SetText("全部暂停")
		ws.pauseAllBtn.SetIcon(theme.MediaPauseIcon())
	}
	if len(ws.windows) == 0 {
		ws.pauseAllBtn.Disable()
	} else {
		ws.pauseAllBtn.Enable()
	}
}

// relayout 没有读取窗口时显示新建提示和最近的工作区，有读取窗口时显示多文档区域。
func (ws *Workspace) relayout() {
	var obj fyne.CanvasObject
	if len(ws.windows) == 0 {
		title := widget.NewLabelWithStyle("开始调试 Modbus 设备", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
		brand := canvas.NewImageFromResource(icon.Application())
		brand.FillMode = canvas.ImageFillContain
		brand.SetMinSize(fyne.NewSize(48, 48))
		hint := widget.NewLabel("先设置上方连接参数，再新建读取窗口。\n没有设备时，可以打开换热站示例。")
		hint.Alignment = fyne.TextAlignCenter
		box := container.NewVBox(container.NewCenter(brand), title, hint, container.NewHBox(
			widget.NewButtonWithIcon("新建读取窗口", theme.ContentAddIcon(), func() {
				if !ws.dialogOpen() {
					ws.addReadWindow()
				}
			}),
			widget.NewButtonWithIcon("打开换热站示例", theme.MediaPlayIcon(), func() {
				if !ws.dialogOpen() {
					ws.loadDemo()
				}
			})))
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
		obj = ws.mdi.root
		ws.mdi.box.Refresh()
	}
	if len(ws.tiles.Objects) != 1 || ws.tiles.Objects[0] != obj {
		ws.tiles.Objects = []fyne.CanvasObject{obj}
		ws.tiles.Refresh()
	}
}

// windowMenuItems 是“窗口”菜单：层叠、平铺、最大化，下面列出全部读取窗口，当前的打勾（Modbus Poll 的 Window 菜单）。
func (ws *Workspace) windowMenuItems() []*fyne.MenuItem {
	none := len(ws.windows) == 0
	cascade := fyne.NewMenuItem("层叠", ws.mdi.cascade)
	tile := fyne.NewMenuItem("平铺", ws.mdi.tile)
	maxed := fyne.NewMenuItem("最大化", func() { ws.mdi.setMaxed(!ws.mdi.maxed) })
	maxed.Checked = ws.mdi.maxed
	for _, it := range []*fyne.MenuItem{cascade, tile, maxed} {
		it.Disabled = none
	}
	items := []*fyne.MenuItem{cascade, tile, maxed}
	if !none {
		items = append(items, fyne.NewMenuItemSeparator())
	}
	cur := ws.current()
	for _, w := range ws.windows {
		it := fyne.NewMenuItem(w.title(), func() { ws.setCurrent(w) })
		it.Checked = w == cur
		items = append(items, it)
	}
	return items
}

// refreshWindowMenu 在读取窗口增减、切换、改定义后更新“窗口”菜单和最大化时的标签；内容没变时不重建。
func (ws *Workspace) refreshWindowMenu() {
	if ws.mdi != nil {
		ws.mdi.refreshTabs()
	}
	if ws.winMenu == nil {
		return
	}
	items := ws.windowMenuItems()
	sig := func(its []*fyne.MenuItem) string {
		var b strings.Builder
		for _, it := range its {
			b.WriteString(it.Label)
			if it.Checked {
				b.WriteString("✓")
			}
			if it.Disabled {
				b.WriteString("×")
			}
			b.WriteByte('\n')
		}
		return b.String()
	}
	if sig(items) == sig(ws.winMenu.Items) {
		return
	}
	ws.winMenu.Items = items
	if m := ws.win.MainMenu(); m != nil {
		m.Refresh()
	}
}
