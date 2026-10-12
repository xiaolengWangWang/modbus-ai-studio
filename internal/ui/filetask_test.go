package ui

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// 读得很慢的工作区文件：读取在后台进行，界面照常响应；取消后原工作区不变，可以再次打开。
func TestWorkspaceOpenRunsInBackgroundAndCancels(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	locked(func() {
		ws.addWindow(defaultDef())
		ws.loadWorkspace("很大的工作区.json", func() (io.ReadCloser, error) { return pr, nil }, nil)
		if !ws.workspaceBusy || len(ws.windows) != 1 {
			t.Fatal("打开工作区应在后台进行，不替换当前工作区")
		}
	})
	waitFor(t, 3*time.Second, "读得慢时显示进度和取消", func() bool { return strings.Contains(overlayText(ws), "正在读取工作区") })
	locked(func() { test.Tap(findButtons(ws.win.Canvas().Overlays().Top(), "取消")[0]) })
	waitFor(t, 3*time.Second, "取消后结束", func() bool { return !ws.workspaceBusy })
	locked(func() {
		if len(ws.windows) != 1 || ws.path != "" || overlayCount(ws) != 0 {
			t.Errorf("取消后原工作区应保持不变：%d 个窗口，路径 %q，对话框 %d", len(ws.windows), ws.path, overlayCount(ws))
		}
	})
}

func TestWorkspaceOpenRejectsOversizedAndInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxWorkspaceBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"mode":"X"}`), 0o644)
	ws := openWS(t, test.NewTempApp(t), false)
	for _, tc := range []struct{ path, want string }{{big, "超过 64 MB"}, {bad, "不认识的协议"}} {
		failed := false
		locked(func() {
			clearOverlays(ws)
			ws.loadWorkspace(tc.path, func() (io.ReadCloser, error) { return os.Open(tc.path) }, func() { failed = true })
		})
		waitFor(t, 10*time.Second, "打开失败的说明", func() bool { return !ws.workspaceBusy && strings.Contains(overlayText(ws), tc.want) })
		if !failed {
			t.Errorf("%s：失败时应通知调用方（从最近列表里去掉）", filepath.Base(tc.path))
		}
	}
}

// 保存在后台写临时文件再替换；保存失败（这里用取消模拟）时原文件不变，也不留临时文件。
func TestWorkspaceSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "工作区.json")
	os.WriteFile(path, []byte("原内容"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeFileAtomic(ctx, path, []byte("新内容")); err == nil {
		t.Fatal("取消的保存应报错")
	}
	if b, _ := os.ReadFile(path); string(b) != "原内容" {
		t.Fatalf("保存失败不能破坏原文件：%q", b)
	}
	if err := writeFileAtomic(context.Background(), path, []byte("新内容")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "新内容" {
		t.Fatalf("保存后内容：%q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("不应留下临时文件：%v", entries)
	}

	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.addWindow(defaultDef())
		ws.path = path
		ws.saveWorkspace()
	})
	waitFor(t, 3*time.Second, "后台保存完成", func() bool { return !ws.workspaceBusy })
	data, _ := os.ReadFile(path)
	if _, err := parseWorkspace(data); err != nil {
		t.Fatalf("保存的工作区应能再打开：%v", err)
	}
}

// 历史报文的“异常报文”页来自单独的 anomalies 表：请求帧和异常响应成对列出。
func TestHistoryShowsAnomaliesFromSeparateTable(t *testing.T) {
	r, err := recorder.Open(filepath.Join(t.TempDir(), "packets.db"))
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, "packets.db", nil)
	t.Cleanup(func() { SetRecorder(nil, "", nil); r.Close() })
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.proto.SetSelected(protoTCP)
		d := defaultDef()
		d.Start, d.Qty = 8, 2
		ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
	locked(func() {
		ws.session.sim.SetFaults(simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 0, Count: 2}}})
	})
	waitFor(t, 5*time.Second, "非标准响应", func() bool { return strings.Contains(ws.windows[0].errLbl.Text, "非标准响应") })
	locked(func() { ws.disconnect() })
	waitFor(t, 3*time.Second, "异常写入数据库", func() bool {
		ss, _ := r.Sessions(10)
		if len(ss) == 0 {
			return false
		}
		as, _ := r.Anomalies(ss[0].ID, 10)
		return len(as) > 0
	})
	locked(ws.openHistory)
	waitFor(t, 5*time.Second, "异常报文页", func() bool {
		var found bool
		walk(ws.historyWin.Content(), func(o fyne.CanvasObject) {
			if l, ok := o.(*widget.Label); ok && strings.Contains(l.Text, "异常 ") && !strings.Contains(l.Text, "异常 0 条") {
				found = true
			}
		})
		return found
	})
	ss, _ := r.Sessions(10)
	as, _ := r.Anomalies(ss[0].ID, 100)
	ps := anomalyPackets(as)
	if len(ps) < 2 || ps[0].Dir != modbus.DirTX || ps[1].Status != modbus.StatusNonstandard || len(ps[0].Raw) == 0 {
		t.Fatalf("异常报文应先列请求帧，再列非标准响应：%+v", ps[:min(len(ps), 2)])
	}
}
