package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// fixed 给输入框一个固定宽度，HBox 里的 Entry 默认会缩成最小宽度。
func fixed(w float32, o fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(w, o.MinSize().Height), o)
}

// cell 是表格单元：单击选中，双击打开写入，右键弹出菜单（Modbus Poll 的习惯）。
type cell struct {
	widget.Label
	id       widget.TableCellID
	onTap    func(widget.TableCellID)
	onDouble func(widget.TableCellID)
	onMenu   func(widget.TableCellID, fyne.Position)
	onDrag   func(widget.TableCellID, fyne.Position)
	dragID   *widget.TableCellID
	bg       *canvas.Rectangle
}

func newCell(tap, double func(widget.TableCellID), menu func(widget.TableCellID, fyne.Position)) *cell {
	c := &cell{onTap: tap, onDouble: double, onMenu: menu, bg: canvas.NewRectangle(color.Transparent)}
	c.Truncation = fyne.TextTruncateEllipsis
	c.ExtendBaseWidget(c)
	return c
}

func (c *cell) Tapped(*fyne.PointEvent) { c.onTap(c.id) }

func (c *cell) DoubleTapped(*fyne.PointEvent) { c.onDouble(c.id) }

func (c *cell) TappedSecondary(e *fyne.PointEvent) {
	if c.onMenu != nil {
		c.onMenu(c.id, e.AbsolutePosition)
	}
}

func (c *cell) Dragged(e *fyne.DragEvent) {
	if c.onDrag == nil {
		return
	}
	if c.dragID == nil {
		id := c.id
		c.dragID = &id
	}
	c.onDrag(*c.dragID, e.Position)
}

func (c *cell) DragEnd() { c.dragID = nil }

func (c *cell) setSelected(selected bool) {
	var fill color.Color = color.Transparent
	if selected {
		fill = appTheme().Color(theme.ColorNameSelection, fyne.CurrentApp().Settings().ThemeVariant())
	}
	c.bg.FillColor = fill
	c.bg.Refresh()
}

type cellRenderer struct {
	fyne.WidgetRenderer
	bg *canvas.Rectangle
}

func (c *cell) CreateRenderer() fyne.WidgetRenderer {
	return &cellRenderer{c.Label.CreateRenderer(), c.bg}
}

func (r *cellRenderer) Objects() []fyne.CanvasObject {
	return append([]fyne.CanvasObject{r.bg}, r.WidgetRenderer.Objects()...)
}

func (r *cellRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.WidgetRenderer.Layout(size)
}

func (r *cellRenderer) Refresh() { r.WidgetRenderer.Refresh(); r.bg.Refresh() }

// compactTheme 缩小内边距和行距，让通信报文和解析面板一屏显示更多行（接近 Modbus Poll 的密度）。
// 包的是应用当前的主题，Windows 上的微软雅黑和 13 号字照样生效。
type compactTheme struct{ fyne.Theme }

func (t compactTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInnerPadding:
		return 2
	case theme.SizeNameLineSpacing:
		return 2
	}
	return t.Theme.Size(n)
}

func appTheme() fyne.Theme {
	if a := fyne.CurrentApp(); a != nil {
		if th := a.Settings().Theme(); th != nil {
			return th
		}
	}
	return theme.DefaultTheme()
}

func compact(obj fyne.CanvasObject) *container.ThemeOverride {
	return container.NewThemeOverride(obj, compactTheme{appTheme()})
}

// gridPadding 是数据表单元格的内边距：Fyne 默认 8，行太高，一屏放不下 Modbus Poll 那么多行。
const gridPadding = 3

// gridTheme 是读取窗口和调试窗口数据表的主题：行距收紧；地址、原始值、过期值用的“灰色”从 Fyne 的
// 禁用色（浅色主题下接近背景，几乎看不清）换成占位文字的颜色，浅色、深色主题下都看得清又有区别。
type gridTheme struct{ fyne.Theme }

func (t gridTheme) Size(n fyne.ThemeSizeName) float32 {
	if n == theme.SizeNameInnerPadding {
		return gridPadding
	}
	return t.Theme.Size(n)
}

func (t gridTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if n == theme.ColorNameDisabled {
		return t.Theme.Color(theme.ColorNamePlaceHolder, v)
	}
	return t.Theme.Color(n, v)
}

func dense(obj fyne.CanvasObject) *container.ThemeOverride {
	return container.NewThemeOverride(obj, gridTheme{appTheme()})
}

// denseCell 给数据表的单元格、表头各套一层紧凑主题，UpdateCell 里用 unwrap 取出来。Fyne 量行高、画表头时不给表头
// 套表格的主题覆盖，单元格也只在表格是同一个指针时才套得上（读取窗口的表格包了一层 grid，套不上），
// 按默认内边距会把每一行撑高，灰色也变回看不清的禁用色。
func denseCell(o fyne.CanvasObject) fyne.CanvasObject { return dense(o) }

func unwrap(o fyne.CanvasObject) fyne.CanvasObject { return o.(*container.ThemeOverride).Content }

// newGridHeader 是数据表的表头（粗体）。
func newGridHeader() fyne.CanvasObject {
	return denseCell(widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
}

func headerLabel(o fyne.CanvasObject) *widget.Label { return unwrap(o).(*widget.Label) }

// gridRowHeight 是数据表一行的高度（不含分隔线）。
func gridRowHeight() float32 {
	return fyne.MeasureText("0", theme.TextSize(), fyne.TextStyle{}).Height + 2*gridPadding
}
