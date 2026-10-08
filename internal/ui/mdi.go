package ui

import (
	"image/color"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
)

// 读取窗口的多文档区域，照 Modbus Poll 的 MDI：每个读取窗口是一个子窗口，拖标题栏移动、拖右下角改大小、
// 点最大化按钮铺满（之后切换窗口也保持最大化，再点还原）；新窗口层叠摆放，不并列平铺。
// 点子窗口的标题栏或内容，把它提到最上面并设为当前窗口。“窗口”菜单里可以层叠、平铺、最大化或从列表里切换。
// Fyne 自带的 MultipleWindows 每次刷新都会改写标题栏的点击回调、不处理最大化，所以自己排。

type mdi struct {
	ws    *Workspace
	box   *fyne.Container          // 子窗口，按叠放顺序，最后一个在最上面
	scope *container.ThemeOverride // 子窗口边框的配色
	bg    *canvas.Rectangle
	root  fyne.CanvasObject
	maxed bool      // 最大化：最上面的子窗口铺满区域
	size  fyne.Size // 区域大小，上次排列时记下
}

func newMDI(ws *Workspace) *mdi {
	m := &mdi{ws: ws, bg: canvas.NewRectangle(color.Transparent)}
	m.box = container.New(mdiLayout{m})
	m.scope = container.NewThemeOverride(m.box, mdiTheme{appTheme()})
	// 子窗口不会超出区域（排列时收进来），滚动容器只用来裁掉阴影等越界的绘制
	m.root = container.NewStack(m.bg, container.NewScroll(m.scope))
	return m
}

// mdiTheme 让当前子窗口的边框（标题栏）带主题色、其他窗口是灰色。Fyne 默认两者几乎一样，浅色主题下还和背景同色。
// 颜色从主题的背景、文字、主题色调出来，不按浅色 / 深色写死：自定义主题、测试主题里标题文字照样看得清。
type mdiTheme struct{ fyne.Theme }

func (t mdiTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	bg := t.Theme.Color(theme.ColorNameBackground, v)
	switch n {
	case theme.ColorNameInnerWindowBorder:
		return mix(bg, t.Theme.Color(theme.ColorNamePrimary, v), 0.3)
	case theme.ColorNameInnerWindowBorderInactive:
		return mix(bg, t.Theme.Color(theme.ColorNameForeground, v), 0.12)
	}
	return t.Theme.Color(n, v)
}

// The work area uses a soft neutral tint, with contrast in either theme variant.
func areaColor() color.Color {
	bg := appTheme().Color(theme.ColorNameBackground, fyne.CurrentApp().Settings().ThemeVariant())
	return mix(bg, appTheme().Color(theme.ColorNameForeground, fyne.CurrentApp().Settings().ThemeVariant()), 0.055)
}

// mix 按比例 k（0–1）把 b 混进 a。
func mix(a, b color.Color, k float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	ch := func(x, y uint32) uint8 { return uint8((float64(x)*(1-k) + float64(y)*k) / 257) }
	return color.NRGBA{R: ch(ar, br), G: ch(ag, bg), B: ch(ab, bb), A: 0xff}
}

// attach 把读取窗口放进区域：层叠在已有窗口的右下方，大小按内容，不超出区域。
func (m *mdi) attach(w *readWindow) {
	w.inner.CloseIntercept = func() { m.ws.removeWindow(w) }
	w.inner.OnTappedBar = func() { m.ws.setCurrent(w) }
	w.inner.OnDragged = func(ev *fyne.DragEvent) {
		if m.maxed {
			return
		}
		w.pos = w.pos.Add(ev.Dragged)
		m.box.Refresh()
	}
	w.inner.OnResized = func(ev *fyne.DragEvent) {
		if m.maxed {
			return
		}
		w.size = w.size.Add(ev.Dragged).Max(w.inner.MinSize())
		m.box.Refresh()
	}
	w.inner.OnMaximized = func() { m.setMaxed(!m.maxed) }
	k := float32(len(m.box.Objects) % 8)
	w.pos = fyne.NewPos(k*cascadeStep(), k*cascadeStep())
	m.sizeForCascade(w)
	w.inner.SetMaximized(m.maxed)
	m.box.Objects = append(m.box.Objects, w.inner)
	m.scope.Refresh() // 新窗口套上边框配色
}

func (m *mdi) detach(w *readWindow) {
	for i, o := range m.box.Objects {
		if o == w.inner {
			m.box.Objects = append(m.box.Objects[:i], m.box.Objects[i+1:]...)
			break
		}
	}
	m.box.Refresh()
}

// raise 把 w 提到最上面，标题栏显示为当前窗口。
func (m *mdi) raise(w *readWindow) {
	objs := m.box.Objects
	for i, o := range objs {
		if o == w.inner && i != len(objs)-1 {
			m.box.Objects = append(append(objs[:i:i], objs[i+1:]...), w.inner)
			break
		}
	}
	for i, o := range m.box.Objects {
		o.(*container.InnerWindow).SetActive(i == len(m.box.Objects)-1)
	}
	m.box.Refresh()
}

// top 是最上面的子窗口。
func (m *mdi) top() *container.InnerWindow {
	if n := len(m.box.Objects); n > 0 {
		return m.box.Objects[n-1].(*container.InnerWindow)
	}
	return nil
}

func (m *mdi) setMaxed(on bool) {
	m.maxed = on
	for _, o := range m.box.Objects {
		o.(*container.InnerWindow).SetMaximized(on)
	}
	m.box.Refresh()
	m.ws.refreshWindowMenu()
}

func cascadeStep() float32 { return theme.Size(theme.SizeNameWindowTitleBarHeight) }

// Keep the requested cascade offset visible by shortening a new window's
// preferred size to the remaining space, rather than moving its title upward.
func (m *mdi) sizeForCascade(w *readWindow) {
	w.size = w.prefSize()
	if m.size.Width > 0 && m.size.Height > 0 {
		minimum := w.inner.MinSize()
		w.size.Width = min(w.size.Width, max(minimum.Width, m.size.Width-w.pos.X))
		w.size.Height = min(w.size.Height, max(minimum.Height, m.size.Height-w.pos.Y))
	}
}

// cascade 按叠放顺序层叠，每个窗口回到按内容的大小（Modbus Poll 的“Cascade”）。
func (m *mdi) cascade() {
	m.setMaxed(false)
	for i, o := range m.box.Objects {
		w := m.ws.windowOf(o)
		k := float32(i % 8)
		w.pos = fyne.NewPos(k*cascadeStep(), k*cascadeStep())
		m.sizeForCascade(w)
	}
	m.box.Refresh()
}

// tile 按窗口编号平铺铺满区域：列数取 ⌈√n⌉，靠后的列多放一个（Modbus Poll 的“Tile”）。
func (m *mdi) tile() {
	m.setMaxed(false)
	wins := m.ws.windows
	n := len(wins)
	if n == 0 || m.size.IsZero() {
		return
	}
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	base, extra := n/cols, n%cols
	colW := m.size.Width / float32(cols)
	i := 0
	for c := 0; c < cols; c++ {
		k := base
		if c >= cols-extra {
			k++
		}
		rowH := m.size.Height / float32(k)
		for r := 0; r < k; r++ {
			wins[i].pos = fyne.NewPos(float32(c)*colW, float32(r)*rowH)
			wins[i].size = fyne.NewSize(colW, rowH)
			i++
		}
	}
	m.box.Refresh()
}

// mdiLayout 摆放子窗口：最大化时最上面的铺满（下面的也铺满，切换时不跳）；否则按各自的位置和大小，
// 收进区域里，标题栏总能看见、拖得到。
type mdiLayout struct{ m *mdi }

func (l mdiLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	m := l.m
	m.size = size
	m.bg.FillColor = areaColor()
	m.bg.Refresh()
	for _, o := range objs {
		w := m.ws.windowOf(o)
		if w == nil {
			continue
		}
		if m.maxed {
			o.Move(fyne.Position{})
			o.Resize(size)
			continue
		}
		s := fyne.NewSize(min(w.size.Width, size.Width), min(w.size.Height, size.Height))
		p := fyne.NewPos(max(0, min(w.pos.X, size.Width-s.Width)), max(0, min(w.pos.Y, size.Height-s.Height)))
		w.pos = p
		o.Move(p)
		o.Resize(s)
	}
}

func (mdiLayout) MinSize([]fyne.CanvasObject) fyne.Size { return fyne.Size{} }
