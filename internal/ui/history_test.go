package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
)

// 连接期间的全部收发存进 SQLite，每次连接一个会话；历史报文窗口能选会话并载入记录。
func TestRecordingAndHistory(t *testing.T) {
	r, err := recorder.Open(filepath.Join(t.TempDir(), "packets.db"))
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, "packets.db", nil)
	t.Cleanup(func() {
		SetRecorder(nil, "", nil)
		r.Close()
	})
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() { ws.addWindow(defaultDef()) })
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "收到数据", func() bool { return hasData(ws.windows[0]) })
	locked(func() { ws.disconnect() })
	var ss []recorder.Session
	waitFor(t, 3*time.Second, "记录写入数据库", func() bool {
		ss, _ = r.Sessions(10)
		return len(ss) == 1 && ss[0].Packets >= 2 && !ss[0].End.IsZero()
	})
	if ss[0].Mode != modbus.ModeRTUOverTCP || !strings.Contains(ss[0].Target, "内置模拟器") || ss[0].Window != ws.no {
		t.Errorf("会话 %+v", ss[0])
	}
	ps, err := r.Packets(ss[0].ID, 10)
	if err != nil || ps[0].Dir != modbus.DirTX || ps[0].Status != modbus.StatusSent || len(ps[0].Raw) != 8 {
		t.Errorf("第一条应是请求：%+v %v", ps, err)
	}

	locked(func() { ws.openHistory() })
	hw := ws.tools[len(ws.tools)-1]
	var sel *widget.Select
	var labels []*widget.Label
	var walk func(o fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Select:
			sel = x
		case *widget.Label:
			labels = append(labels, x)
		case *fyne.Container:
			for _, c := range x.Objects {
				walk(c)
			}
		}
	}
	walk(hw.Content())
	if sel == nil || len(sel.Options) != 1 || !strings.Contains(sel.Options[0], "内置模拟器") {
		t.Fatalf("历史报文应列出这次连接：%v", sel)
	}
	locked(func() { sel.SetSelected(sel.Options[0]) })
	waitFor(t, 3*time.Second, "载入记录", func() bool {
		for _, l := range labels {
			if strings.HasPrefix(l.Text, "共 ") {
				return true
			}
		}
		return false
	})
}
