package ui

import (
	"fmt"
	"os"
	"slices"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"

	"modbus-ai-studio/assets/icon"
)

const prefCloseToTray = "desktop.close-to-tray"

// Desktop owns this process's workspaces and tray. Each executable invocation
// creates its own Desktop; there is no single-instance lock or shared connection.
type Desktop struct {
	app        fyne.App
	version    string
	tray       desktop.App
	workspaces []*Workspace
	quitting   bool
}

func NewDesktop(app fyne.App, version string) *Desktop {
	d := &Desktop{app: app, version: version}
	app.SetIcon(icon.Application())
	d.tray, _ = app.(desktop.App)
	if d.tray != nil {
		d.refreshTray()
		d.tray.SetSystemTrayIcon(icon.Tray())
	}
	return d
}

func (d *Desktop) Open() *Workspace {
	ws := Open(d.app, d.version)
	ws.desktop = d
	d.workspaces = append(d.workspaces, ws)
	ws.win.SetCloseIntercept(ws.requestClose)
	ws.setMenu()
	d.refreshTray()
	return ws
}

func (d *Desktop) refreshTray() {
	if d.tray == nil || d.quitting {
		return
	}
	header := fyne.NewMenuItem(fmt.Sprintf("Modbus AI Studio %s · 进程 %d", d.version, os.Getpid()), nil)
	header.Disabled = true
	items := []*fyne.MenuItem{header, fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("显示主窗口", func() {
			if len(d.workspaces) == 0 {
				d.Open()
			} else {
				d.workspaces[len(d.workspaces)-1].show()
			}
		}), fyne.NewMenuItem("新建主窗口", func() { d.Open() })}
	for _, ws := range d.workspaces {
		items = append(items, fyne.NewMenuItem(ws.win.Title(), ws.show))
	}
	items = append(items, fyne.NewMenuItemSeparator(), fyne.NewMenuItem("全部隐藏到后台", func() {
		for _, ws := range d.workspaces {
			ws.hide()
		}
	}))
	quit := fyne.NewMenuItem("退出所有窗口", d.Quit)
	quit.IsQuit = true
	items = append(items, fyne.NewMenuItemSeparator(), quit)
	d.tray.SetSystemTrayMenu(fyne.NewMenu("Modbus AI Studio", items...))
	if len(d.workspaces) > 0 {
		ws := d.workspaces[len(d.workspaces)-1]
		d.tray.SetSystemTrayWindow(ws.win)
		// Fyne assigns Hide as the close interceptor; restore our policy after it.
		ws.win.SetCloseIntercept(ws.requestClose)
	}
}

func (d *Desktop) forget(ws *Workspace) {
	d.workspaces = slices.DeleteFunc(d.workspaces, func(w *Workspace) bool { return w == ws })
	if d.quitting {
		return
	}
	if len(d.workspaces) == 0 {
		d.Quit()
		return
	}
	if ws.updates.automatic {
		d.workspaces[0].AutoCheckUpdate()
	}
	d.refreshTray()
}

// Shutdown runs after the native event loop returns. The driver is already
// stopped, so release connections without calling native window operations.
func (d *Desktop) Shutdown() {
	if d.quitting {
		return
	}
	d.quitting = true
	for _, ws := range slices.Clone(d.workspaces) {
		ws.stop()
	}
	d.workspaces = nil
}

func (d *Desktop) Quit() {
	if d.quitting {
		return
	}
	d.quitting = true
	// User-triggered quit runs while the driver is alive. OnClosed stops each
	// workspace and closes its tools before the application event loop ends.
	for _, ws := range slices.Clone(d.workspaces) {
		ws.win.Close()
	}
	d.app.Quit()
}

func (ws *Workspace) requestClose() {
	if ws.desktop != nil && ws.desktop.tray != nil && ws.app.Preferences().BoolWithFallback(prefCloseToTray, true) {
		ws.hide()
		return
	}
	ws.win.Close()
}

func (ws *Workspace) hide() {
	if ws.desktop == nil || ws.desktop.tray == nil {
		return
	}
	ws.saveWindowSize()
	for _, tool := range ws.tools {
		tool.Hide()
	}
	ws.win.Hide()
}

func (ws *Workspace) show() {
	if ws.closed {
		return
	}
	ws.win.Show()
	ws.win.RequestFocus()
}

func (ws *Workspace) quitApp() {
	if ws.desktop != nil {
		ws.desktop.Quit()
	} else {
		ws.shutdown()
		ws.app.Quit()
	}
}
