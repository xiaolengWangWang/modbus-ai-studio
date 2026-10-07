package ui

import (
	"bytes"
	"image/png"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

func TestApplicationIconAvailableWithoutPackaging(t *testing.T) {
	a := &iconTestApp{App: test.NewTempApp(t)}
	fyne.SetCurrentApp(a)
	t.Cleanup(func() { fyne.SetCurrentApp(a.App) })
	ws := openWS(t, a, false)
	if a.Icon() == nil || ws.win.Icon() == nil {
		t.Fatal("直接运行与打包运行都应有应用和窗口图标")
	}
	img, err := png.Decode(bytes.NewReader(a.Icon().Content()))
	if err != nil || img.Bounds().Dx() < 64 {
		t.Fatalf("应用图标应为有效的高分辨率 PNG：%v", err)
	}
}

// The software app discards SetIcon; record only the OS-facing resource.
type iconTestApp struct {
	fyne.App
	icon fyne.Resource
}

func (a *iconTestApp) SetIcon(r fyne.Resource) { a.icon = r }
func (a *iconTestApp) Icon() fyne.Resource     { return a.icon }

// Only the native tray boundary is replaced; windows, connections and polling
// still use the real workspace and Fyne software driver.
type trayTestApp struct{ fyne.App }

func (*trayTestApp) SetSystemTrayMenu(*fyne.Menu)    {}
func (*trayTestApp) SetSystemTrayIcon(fyne.Resource) {}
func (*trayTestApp) SetSystemTrayWindow(fyne.Window) {}

func TestTrayHideKeepsPollingAndCloseIsIndependent(t *testing.T) {
	a := &trayTestApp{test.NewTempApp(t)}
	d := NewDesktop(a, "test")
	var one, two *Workspace
	locked(func() {
		one, two = d.Open(), d.Open()
		one.loadDemo()
		two.loadDemo()
	})
	t.Cleanup(func() { locked(d.Shutdown) })
	waitFor(t, 5*time.Second, "两个窗口均读到数据", func() bool {
		return hasData(one.windows[0]) && hasData(two.windows[0])
	})
	var before int64
	locked(func() {
		before = one.stats.polls.Load()
		one.requestClose()
		if one.closed || one.session == nil {
			t.Fatal("隐藏到托盘不能关闭连接")
		}
	})
	waitFor(t, 3*time.Second, "隐藏后继续轮询", func() bool { return one.stats.polls.Load() > before })
	locked(func() {
		one.show()
		one.win.Close()
		if two.closed || two.session == nil {
			t.Fatal("关闭一个工作区不应停止另一个")
		}
		d.Quit()
		if !two.closed || two.session != nil {
			t.Fatal("托盘退出必须关闭连接和所有工作区")
		}
	})
}

func TestCloseWithoutTrayNeverLeavesHiddenProcess(t *testing.T) {
	a := test.NewTempApp(t)
	d := NewDesktop(a, "test")
	locked(func() {
		ws := d.Open()
		ws.requestClose()
		if !ws.closed {
			t.Fatal("没有系统托盘时关闭按钮必须关闭工作区")
		}
	})
}

func TestCloseToTrayCanBeDisabled(t *testing.T) {
	a := &trayTestApp{test.NewTempApp(t)}
	a.Preferences().SetBool("desktop.close-to-tray", false)
	d := NewDesktop(a, "test")
	locked(func() {
		ws := d.Open()
		ws.requestClose()
		if !ws.closed {
			t.Fatal("关闭到托盘选项禁用后应真正关闭")
		}
	})
}

// After the native event loop stops, GLFW window operations are invalid. Only
// that driver boundary is replaced; both workspaces poll the real simulator.
type stoppedDriverWindow struct{ fyne.Window }

func (stoppedDriverWindow) Close() { panic("native window driver already stopped") }

func TestShutdownAfterDriverStopsClosesEveryConnection(t *testing.T) {
	a := &trayTestApp{test.NewTempApp(t)}
	d := NewDesktop(a, "test")
	var one, two *Workspace
	locked(func() {
		one, two = d.Open(), d.Open()
		one.loadDemo()
		two.loadDemo()
	})
	waitFor(t, 5*time.Second, "两个窗口均读到数据", func() bool {
		return hasData(one.windows[0]) && hasData(two.windows[0])
	})
	locked(func() {
		originalOne, originalTwo := one.win, two.win
		one.win, two.win = stoppedDriverWindow{one.win}, stoppedDriverWindow{two.win}
		tool := a.NewWindow("driver-stopped tool")
		one.tools = append(one.tools, stoppedDriverWindow{tool})
		defer func() {
			one.win, two.win = originalOne, originalTwo
			one.tools = nil
			one.shutdown()
			two.shutdown()
			tool.Close()
			originalOne.Close()
			originalTwo.Close()
		}()
		d.Shutdown()
		d.Shutdown() // cleanup must be safe to repeat
		for _, ws := range []*Workspace{one, two} {
			if !ws.closed || ws.session != nil {
				t.Fatal("驱动退出后必须停止每个工作区的连接")
			}
			select {
			case <-ws.done:
			default:
				t.Fatal("驱动退出后必须停止后台刷新")
			}
		}
	})
}
