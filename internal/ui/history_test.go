package ui

import (
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
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
			if strings.HasPrefix(l.Text, "报文共 ") {
				return true
			}
		}
		return false
	})
}

func TestSessionLabelsDistinguishRepeatedConnectionFailures(t *testing.T) {
	start := time.Date(2026, 10, 5, 17, 0, 0, 0, time.Local)
	first := recorder.Session{ID: 41, Start: start, Mode: modbus.ModeTCP, Target: "127.0.0.1:502", Window: 1, Faults: 1}
	second := first
	second.ID = 42
	if sessionLabel(first, false) == sessionLabel(second, false) {
		t.Fatal("同一秒内重复连接失败的两个会话，在历史列表中必须能分别选择")
	}
}

func TestFaultLogIsStoredBeforeAddReturns(t *testing.T) {
	r, err := recorder.Open(filepath.Join(t.TempDir(), "packets.db"))
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, "packets.db", nil)
	t.Cleanup(func() { SetRecorder(nil, "", nil); r.Close() })
	id, err := r.StartSession(modbus.ModeTCP, "127.0.0.1:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{log: newFaultLog(test.NewTempApp(t))}
	ws.addLog(logEntry{Event: recorder.Event{Kind: recorder.EventReadFail, Detail: "超时"}}, id)
	events, err := r.Events(id)
	if err != nil || len(events) != 1 || events[0].Detail != "超时" {
		t.Fatalf("日志返回后数据库应已保存记录：%+v %v", events, err)
	}
	ws.no = 1
	ws.logConnectFail(connConfig{mode: modbus.ModeTCP, target: "127.0.0.1:502"}, errors.New("connection refused"))
	sessions, err := r.Sessions(10)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("连接失败返回后应已创建历史会话：%+v %v", sessions, err)
	}
	events, err = r.Events(sessions[0].ID)
	if err != nil || len(events) != 1 || events[0].Kind != recorder.EventConnectFail {
		t.Fatalf("连接失败返回后应已保存日志：%+v %v", events, err)
	}
}

// 读取失败和连接失败记进日志：同一种错误连续出现只记一条，带原因分析和抓到的原始报文，恢复时再记一条；
// 都存进报文库。
func TestFaultLog(t *testing.T) {
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
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty, d.Scan = 346, 4, 100*time.Millisecond
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "收到数据", func() bool { return hasData(w) })
	exc := simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 346, Count: 4}, Code: modbus.ExceptionIllegalDataAddress}}}
	locked(func() { ws.session.sim.SetFaults(exc) })
	waitFor(t, 5*time.Second, "出错 5 次以上", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.faultN >= 5
	})
	locked(func() { ws.session.sim.SetFaults(simulator.Faults{}) })
	waitFor(t, 5*time.Second, "恢复正常的日志", func() bool {
		n := len(ws.log.entries)
		return n > 0 && ws.log.entries[n-1].Kind == recorder.EventReadOK
	})
	var es []logEntry
	locked(func() { es = slices.Clone(ws.log.entries) })
	if len(es) != 2 || es[0].Kind != recorder.EventReadFail {
		t.Fatalf("应只有一条读取失败和一条恢复：%+v", es)
	}
	fail := es[0]
	for _, s := range []string{"窗口 1 · Slave 1 · 03 · 40347–40350 · 异常 02"} {
		if !strings.Contains(fail.Detail, s) {
			t.Errorf("结论 %q 缺少 %q", fail.Detail, s)
		}
	}
	for _, s := range []string{"分析：异常 02", "逐个探测可读地址", "发送的请求：", "收到的响应：", "异常码"} {
		if !strings.Contains(fail.Analysis, s) {
			t.Errorf("分析缺少 %q：\n%s", s, fail.Analysis)
		}
	}
	if len(fail.TX) != 8 || len(fail.RX) != 5 || fail.RX[1] != 0x83 {
		t.Errorf("抓到的报文 TX % X RX % X", fail.TX, fail.RX)
	}
	if !strings.Contains(es[1].Detail, "恢复正常，出错") {
		t.Errorf("恢复 %q", es[1].Detail)
	}
	locked(func() {
		ws.tabs.Select(ws.logTab)
		ws.log.list.Select(0)
	})
	snapshotPNG(t, ws.win, "modbus-ai-log.png")
	locked(func() { ws.disconnect() })

	// 连接失败：端口上没有服务
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.proto.SetSelected(protoTCP)
		ws.target.SetText(addr)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "连接失败的日志", func() bool {
		n := len(ws.log.entries)
		return n > 0 && ws.log.entries[n-1].Kind == recorder.EventConnectFail
	})
	locked(func() {
		e := ws.log.entries[len(ws.log.entries)-1]
		if !strings.Contains(e.Detail, addr) || !strings.Contains(e.Analysis, "连接被拒绝") {
			t.Errorf("连接失败 %+v", e)
		}
	})

	var ss []recorder.Session
	waitFor(t, 3*time.Second, "日志写进报文库", func() bool {
		ss, _ = r.Sessions(10)
		return len(ss) == 2 && ss[0].Faults == 1 && ss[1].Faults == 1
	})
	if !strings.Contains(sessionLabel(ss[0], false), "连接失败") {
		t.Errorf("连接失败的会话 %q", sessionLabel(ss[0], false))
	}
	ev, err := r.Events(ss[1].ID)
	if err != nil || len(ev) != 2 || ev[0].Kind != recorder.EventReadFail || !bytes.Equal(ev[0].RX, fail.RX) || ev[0].Analysis != fail.Analysis {
		t.Fatalf("报文库里的日志 %+v %v", ev, err)
	}
}

// 会话标签：进行中的连接写“进行中”，没有结束时间又不在进行的是程序异常退出留下的。
func TestSessionLabelRunning(t *testing.T) {
	s := recorder.Session{ID: 3, Start: time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local), Mode: modbus.ModeTCP, Target: "10.0.0.1:502", Window: 1}
	if l := sessionLabel(s, true); !strings.Contains(l, "进行中") {
		t.Errorf("进行中的会话：%s", l)
	}
	if l := sessionLabel(s, false); !strings.Contains(l, "未正常断开") {
		t.Errorf("异常退出留下的会话：%s", l)
	}
	s.End = s.Start.Add(time.Minute)
	if l := sessionLabel(s, false); !strings.Contains(l, "09:01:00") {
		t.Errorf("已结束的会话应写结束时间：%s", l)
	}
}
