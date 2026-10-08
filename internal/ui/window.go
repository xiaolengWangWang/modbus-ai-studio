package ui

import (
	"math"

	"fyne.io/fyne/v2"

	"modbus-ai-studio/platform"
)

func preferredWindowSize(app fyne.App) fyne.Size {
	p := app.Preferences()
	w, h := p.FloatWithFallback("window.width", 1040), p.FloatWithFallback("window.height", 680)
	if math.IsNaN(w) || math.IsInf(w, 0) || w < 640 || w > 10000 {
		w = 1040
	}
	if math.IsNaN(h) || math.IsInf(h, 0) || h < 400 || h > 10000 {
		h = 680
	}
	// Apply the compact layout once to existing installations as well. Later
	// manual resizing continues to use the usual remembered dimensions.
	if !p.BoolWithFallback("window.compactSize", false) {
		w, h = math.Min(w, 1040), math.Min(h, 680)
		p.SetFloat("window.width", w)
		p.SetFloat("window.height", h)
		p.SetBool("window.compactSize", true)
	}
	return fyne.NewSize(float32(w), float32(h))
}

func (ws *Workspace) saveWindowSize() {
	size := ws.win.Canvas().Size()
	if size.Width < 640 || size.Height < 400 {
		return
	}
	p := ws.app.Preferences()
	p.SetFloat("window.width", float64(size.Width))
	p.SetFloat("window.height", float64(size.Height))
}

// showTool fits tool windows after the driver knows the screen's actual scale.
func showTool(w fyne.Window) {
	want := w.Canvas().Size()
	w.Show()
	workW, workH := platform.WorkArea()
	w.Resize(platform.FitSize(want, workW, workH, w.Canvas().Scale()))
	w.CenterOnScreen()
	w.RequestFocus()
}

func (ws *Workspace) quitMenuItem() *fyne.MenuItem {
	i := fyne.NewMenuItem("退出程序", ws.quitApp)
	i.IsQuit = true
	return i
}

func (ws *Workspace) viewMenu() *fyne.Menu {
	closeToTray := fyne.NewMenuItem("关闭按钮隐藏到托盘", nil)
	closeToTray.Disabled = ws.desktop == nil || ws.desktop.tray == nil
	closeToTray.Checked = !closeToTray.Disabled && ws.app.Preferences().BoolWithFallback(prefCloseToTray, true)
	closeToTray.Action = func() {
		ws.app.Preferences().SetBool(prefCloseToTray, !closeToTray.Checked)
		if ws.desktop != nil {
			for _, w := range ws.desktop.workspaces {
				w.setMenu()
			}
		}
	}
	return fyne.NewMenu("视图", closeToTray, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("恢复默认窗口大小与布局", func() {
			workW, workH := platform.WorkArea()
			ws.win.Resize(platform.FitSize(fyne.NewSize(1040, 680), workW, workH, ws.win.Canvas().Scale()))
			ws.mainSplit.SetOffset(0.72)
			ws.detailsSplit.SetOffset(0.58)
			ws.win.CenterOnScreen()
		}))
}
