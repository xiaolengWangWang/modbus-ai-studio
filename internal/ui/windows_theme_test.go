//go:build windows

package ui

import (
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/platform"
)

// 套用 Windows 版的主题（Microsoft YaHei UI、13 号字）画主窗口，CI 上把截图作为附件上传，用来检查中文字体和布局。
func TestWindowsThemeSnapshot(t *testing.T) {
	a := test.NewTempApp(t)
	platform.ConfigureApp(a)
	ws := openWS(t, a, false)
	snapshotPNG(t, ws.win, "windows-theme-empty.png")
	locked(func() {
		ws.win.Resize(fyne.NewSize(1280, 760))
		ws.loadDemo()
	})
	waitFor(t, 5*time.Second, "读到数据", func() bool { return hasData(ws.windows[0]) })
	time.Sleep(1500 * time.Millisecond)
	snapshotPNG(t, ws.win, "windows-theme-main.png")
	locked(func() { ws.win.Resize(fyne.NewSize(960, 620)) })
	snapshotPNG(t, ws.win, "windows-theme-small.png")
	locked(func() { ws.win.Resize(fyne.NewSize(1280, 760)) })
	locked(func() { ws.showDefinition(ws.windows[0]) })
	snapshotPNG(t, ws.win, "windows-theme-definition.png")
	locked(func() { clearOverlays(ws) })
	tap(ws.connBtn)
}
