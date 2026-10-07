package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

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
		if x > 0 && x+s.Width > width {
			x, y, rowH = 0, y+rowH+pad, 0
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

func toolbarHeight(rows []fyne.CanvasObject, width float32) float32 {
	var h float32
	for _, o := range rows {
		if o.Visible() {
			h += flowHeight(o.(*fyne.Container).Objects, width, false) + theme.Padding()
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
		h := flowHeight(o.(*fyne.Container).Objects, size.Width, false)
		o.Move(fyne.NewPos(0, y))
		o.Resize(fyne.NewSize(size.Width, h))
		y += h + theme.Padding()
	}
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
