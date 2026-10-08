package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"image/color"
)

// The scroll extent must reflect wrapped rows at the actual viewport width.
// A plain VBox minimum only accounts for one row of flow controls.
type wrappingScrollLayout struct {
	content *fyne.Container
	extent  *canvas.Rectangle
	scroll  *container.Scroll
}

func newWrappingScroll(content *fyne.Container) fyne.CanvasObject {
	extent := canvas.NewRectangle(color.Transparent)
	scroll := container.NewVScroll(container.NewStack(extent, content))
	return container.New(wrappingScrollLayout{content, extent, scroll}, scroll)
}

func (l wrappingScrollLayout) Layout(_ []fyne.CanvasObject, size fyne.Size) {
	width := max(0, size.Width-theme.ScrollBarSize())
	l.extent.SetMinSize(fyne.NewSize(width, toolbarHeight(l.content.Objects, width)))
	l.scroll.Move(fyne.NewPos(0, 0))
	l.scroll.Resize(size)
	l.scroll.Refresh()
}

func (l wrappingScrollLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(l.content.MinSize().Width+theme.ScrollBarSize(), 100)
}

// flowLayout wraps complete label/input groups. The parent computes its height
// from width, so controls stay visible without a horizontal toolbar scrollbar.
type flowLayout struct{}

func flowHeight(objects []fyne.CanvasObject, width float32, place bool) float32 {
	pad := theme.Padding()
	var x, y, rowH float32
	for _, o := range objects {
		if !o.Visible() {
			continue
		}
		s := o.MinSize()
		label, wraps := o.(*widget.Label)
		wraps = wraps && label.Wrapping != fyne.TextWrapOff
		if wraps {
			// A wrapping label reports a tiny minimum width; keep its text on one
			// line when the row allows it and wrap only within the row width.
			s.Width = min(width, fyne.MeasureText(label.Text, theme.TextSize(), label.TextStyle).Width+2*theme.InnerPadding()+1)
		}
		if x > 0 && x+s.Width > width {
			x, y, rowH = 0, y+rowH+pad, 0
		}
		if wraps {
			label.Resize(fyne.NewSize(s.Width, label.MinSize().Height))
			s.Height = label.MinSize().Height
		}
		if place {
			o.Move(fyne.NewPos(x, y))
			o.Resize(s)
		}
		x += s.Width + pad
		rowH = max(rowH, s.Height)
	}
	return y + rowH
}

func (flowLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	flowHeight(objects, size.Width, true)
}

func (flowLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var s fyne.Size
	for _, o := range objects {
		if o.Visible() {
			s = s.Max(o.MinSize())
		}
	}
	return s
}

type toolbarLayout struct{}

func toolbarRowHeight(o fyne.CanvasObject, width float32) float32 {
	if label, ok := o.(*widget.Label); ok && label.Wrapping != fyne.TextWrapOff {
		label.Resize(fyne.NewSize(width, label.MinSize().Height))
		return label.MinSize().Height
	}
	if c, ok := o.(*fyne.Container); ok {
		switch c.Layout.(type) {
		case flowLayout:
			return flowHeight(c.Objects, width, false)
		case toolbarLayout:
			return toolbarHeight(c.Objects, width)
		}
	}
	return o.MinSize().Height
}

func toolbarHeight(rows []fyne.CanvasObject, width float32) float32 {
	var h float32
	for _, o := range rows {
		if o.Visible() {
			h += toolbarRowHeight(o, width) + theme.Padding()
		}
	}
	return h
}

func (toolbarLayout) Layout(rows []fyne.CanvasObject, size fyne.Size) {
	var y float32
	for _, o := range rows {
		if !o.Visible() {
			continue
		}
		h := toolbarRowHeight(o, size.Width)
		o.Move(fyne.NewPos(0, y))
		o.Resize(fyne.NewSize(size.Width, h))
		y += h + theme.Padding()
	}
}

// panelLayout reserves the full wrapped header height before placing its body.
// An optional footer stays at the bottom independently of the header rows.
// With wrapped set, the minimum height counts the header rows at the current
// width, so a split can shrink the panel only down to the full header.
type panelLayout struct {
	header  *fyne.Container
	wrapped bool
}

// Wrapped empty-state text uses the panel width, rather than the tiny minimum
// width of a wrapping label, and is centered vertically in the available body.
type centerTextLayout struct{}

func (centerTextLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objects {
		height := toolbarRowHeight(o, size.Width)
		o.Move(fyne.NewPos(0, max(0, (size.Height-height)/2)))
		o.Resize(fyne.NewSize(size.Width, height))
	}
}

func (centerTextLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var size fyne.Size
	for _, o := range objects {
		if o.Visible() {
			size = size.Max(o.MinSize())
		}
	}
	return size
}

func (l panelLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	top := toolbarHeight(l.header.Objects, size.Width)
	bottom := float32(0)
	if len(objects) > 2 {
		bottom = toolbarRowHeight(objects[2], size.Width) + theme.Padding()
		objects[2].Move(fyne.NewPos(0, max(top, size.Height-bottom)))
		objects[2].Resize(fyne.NewSize(size.Width, max(0, bottom-theme.Padding())))
	}
	objects[0].Move(fyne.NewPos(0, 0))
	objects[0].Resize(fyne.NewSize(size.Width, top))
	objects[1].Move(fyne.NewPos(0, top))
	objects[1].Resize(fyne.NewSize(size.Width, max(0, size.Height-top-bottom)))
}

func (l panelLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	s := objects[0].MinSize()
	if width := l.header.Size().Width; l.wrapped && width > 0 {
		s.Height = toolbarHeight(l.header.Objects, width)
	}
	s.Width = max(s.Width, objects[1].MinSize().Width)
	s.Height += objects[1].MinSize().Height
	if len(objects) > 2 {
		s.Width = max(s.Width, objects[2].MinSize().Width)
		s.Height += objects[2].MinSize().Height + theme.Padding()
	}
	return s
}

func (toolbarLayout) MinSize(rows []fyne.CanvasObject) fyne.Size {
	var s fyne.Size
	for _, o := range rows {
		if o.Visible() {
			s.Width = max(s.Width, o.MinSize().Width)
			s.Height += o.MinSize().Height + theme.Padding()
		}
	}
	return s
}

// workspaceLayout measures the toolbar at the current width before assigning
// remaining space to registers and communication details.
type workspaceLayout struct{ bar *fyne.Container }

func (l workspaceLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	pad := theme.Padding()
	width := max(0, size.Width-2*pad)
	top := toolbarHeight(l.bar.Objects, width)
	status := objects[1].MinSize().Height
	objects[0].Move(fyne.NewPos(pad, pad))
	objects[0].Resize(fyne.NewSize(width, top))
	objects[1].Move(fyne.NewPos(pad, max(top+pad, size.Height-status)))
	objects[1].Resize(fyne.NewSize(width, status))
	objects[2].Move(fyne.NewPos(pad, top+2*pad))
	objects[2].Resize(fyne.NewSize(width, max(0, size.Height-top-status-3*pad)))
}

func (l workspaceLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	pad := theme.Padding()
	return fyne.NewSize(max(l.bar.MinSize().Width, objects[2].MinSize().Width)+2*pad,
		l.bar.MinSize().Height+objects[1].MinSize().Height+objects[2].MinSize().Height+3*pad)
}
