package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// fixed 给输入框一个固定宽度，HBox 里的 Entry 默认会缩成最小宽度。
func fixed(w float32, o fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(w, o.MinSize().Height), o)
}

// cell 是表格单元：单击选中，双击打开写入（Modbus Poll 的习惯）。
type cell struct {
	widget.Label
	id       widget.TableCellID
	onTap    func(widget.TableCellID)
	onDouble func(widget.TableCellID)
}

func newCell(tap, double func(widget.TableCellID)) *cell {
	c := &cell{onTap: tap, onDouble: double}
	c.Truncation = fyne.TextTruncateEllipsis
	c.ExtendBaseWidget(c)
	return c
}

func (c *cell) Tapped(*fyne.PointEvent) { c.onTap(c.id) }

func (c *cell) DoubleTapped(*fyne.PointEvent) { c.onDouble(c.id) }

// compactTheme 缩小内边距和行距，让通信报文和解析面板一屏显示更多行（接近 Modbus Poll 的密度）。
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

func compact(obj fyne.CanvasObject) *container.ThemeOverride {
	return container.NewThemeOverride(obj, compactTheme{theme.DefaultTheme()})
}
