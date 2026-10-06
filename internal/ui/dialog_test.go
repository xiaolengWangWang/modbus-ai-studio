package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/update"
	"modbus-ai-studio/platform"
)

// menuAction 取出菜单项的动作：菜单栏、macOS 原生菜单和快捷键调用的都是它。
func menuAction(t *testing.T, ws *Workspace, label string) func() {
	t.Helper()
	for _, m := range ws.win.MainMenu().Items {
		for _, it := range m.Items {
			if it.Label == label {
				return it.Action
			}
		}
	}
	t.Fatalf("菜单里没有“%s”", label)
	return nil
}

// findSelects 找出界面里全部下拉框。
func findSelects(o fyne.CanvasObject) []*widget.Select {
	switch x := o.(type) {
	case *widget.Select:
		return []*widget.Select{x}
	case *fyne.Container:
		var out []*widget.Select
		for _, c := range x.Objects {
			out = append(out, findSelects(c)...)
		}
		return out
	case fyne.Widget:
		var out []*widget.Select
		for _, c := range test.WidgetRenderer(x).Objects() {
			out = append(out, findSelects(c)...)
		}
		return out
	}
	return nil
}

func overlayCount(ws *Workspace) int { return len(ws.win.Canvas().Overlays().List()) }

// clearOverlays 关掉主窗口上的全部对话框。
func clearOverlays(ws *Workspace) {
	for _, o := range ws.win.Canvas().Overlays().List() {
		ws.win.Canvas().Overlays().Remove(o)
	}
}

// 每个会弹对话框的菜单项：弹出的是对的对话框；对话框开着时，再点同一项或别的项
// （对话框挡不住 macOS 原生菜单和快捷键）都不会再叠一个。
func TestDialogsDoNotStack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(update.Release{Tag: "v9.9.9", Body: "## 新功能\n\n- 测试"})
	}))
	defer srv.Close()
	oldSources := update.Sources
	update.Sources = []update.Source{{Name: "测试", LatestURL: srv.URL}}
	defer func() { update.Sources = oldSources }()

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.win.Resize(fyne.NewSize(1280, 820))
		ws.loadDemo() // 连上内置模拟器
	})
	waitFor(t, 5*time.Second, "读到数据", func() bool { return hasData(ws.windows[1]) })
	locked(func() { ws.windows[1].tapCell(widget.TableCellID{Row: 0, Col: 2}) }) // 40347 温差设定，可写

	for _, c := range []struct {
		label, want string
		async       bool
	}{
		{"读取定义…", "读取定义 · 窗口 2", false},
		{"写入选中的值…", "温差设定", false},
		{"调整点表字节序…", "调整点表字节序", false},
		{"导入点表…", "", false}, // 选择文件的对话框
		{"打开工作区…", "", false},
		{"工作区另存为…", "", false},
		{"保存工作区", "", false},
		{"识别协议", "识别协议", false},
		{"扫描串口参数…", "扫描串口参数", false},
		{"扫描从站地址…", "扫描从站地址", false},
		{"读取诊断计数器…", "读取诊断计数器", false},
		{"打开报文数据库", "报文数据库", false},
		{"报文数据库所在文件夹", "报文数据库", false},
		{"检查更新…", "发现新版本 9.9.9", true},
		{"新建读取窗口", "读取定义", false},
	} {
		var before int
		locked(func() {
			clearOverlays(ws)
			before = len(ws.windows)
			act := menuAction(t, ws, c.label)
			act()
			if overlayCount(ws) == 0 && !c.async {
				t.Errorf("“%s”没有弹出对话框", c.label)
			}
			act()
			menuAction(t, ws, "读取定义…")() // 换一个菜单项也不行
		})
		if c.async {
			// 查询进度换成结果对话框；更新对话框开着时 updating 一直为真
			waitFor(t, 5*time.Second, c.label+"出结果", func() bool { return strings.Contains(overlayText(ws), c.want) })
		}
		locked(func() {
			if n := overlayCount(ws); n != 1 {
				t.Errorf("“%s”：应只有一个对话框，实际 %d 个", c.label, n)
			}
			if text := overlayText(ws); c.want != "" && !strings.Contains(text, c.want) {
				t.Errorf("“%s”：对话框应包含 %q：\n%s", c.label, c.want, text)
			}
			if c.label == "新建读取窗口" && len(ws.windows) != before+1 {
				t.Errorf("连按两次新建读取窗口只应建一个，建了 %d 个", len(ws.windows)-before)
			}
		})
	}
	locked(func() { clearOverlays(ws) })
	tap(ws.connBtn)
}

// 历史报文只开一个；报文数据库可以直接打开，没有能打开 .db 的程序时在文件夹里显示。
func TestHistoryWindowAndDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "packets.db")
	r, err := recorder.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, dbPath, nil)
	t.Cleanup(func() { SetRecorder(nil, "", nil); r.Close() })
	var opened, shown []string
	openResult := error(nil)
	oldOpen, oldShow := openFileFn, showInFolderFn
	openFileFn = func(p string) error { opened = append(opened, p); return openResult }
	showInFolderFn = func(p string) error { shown = append(shown, p); return nil }
	t.Cleanup(func() { openFileFn, showInFolderFn = oldOpen, oldShow })

	if id, err := r.StartSession(modbus.ModeTCP, "192.168.1.10:502", 1); err == nil {
		r.EndSession(id)
	}
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.openHistory()
		ws.openHistory()
		if len(ws.tools) != 1 || ws.historyWin == nil {
			t.Fatalf("历史报文应只开一个，开了 %d 个", len(ws.tools))
		}
		selected := false
		for _, sel := range findSelects(ws.historyWin.Content()) {
			selected = selected || strings.Contains(sel.Selected, "192.168.1.10:502")
		}
		if !selected {
			t.Error("打开历史报文时应直接选中最近一次连接")
		}
		ws.historyWin.Close()
		if ws.historyWin != nil || len(ws.tools) != 0 {
			t.Error("关掉后应能再开")
		}
		ws.openHistory()
		if ws.historyWin == nil {
			t.Error("关掉后再点应重新打开")
		}

		ws.openDatabase(ws.win, false)
		if len(opened) != 1 || opened[0] != dbPath || overlayCount(ws) != 0 {
			t.Errorf("应直接用系统程序打开数据库：%v，对话框 %d", opened, overlayCount(ws))
		}
		ws.openDatabase(ws.win, true)
		if len(shown) != 1 || shown[0] != dbPath {
			t.Errorf("应在文件夹里显示数据库：%v", shown)
		}
		openResult = platform.ErrNoApp
		ws.openDatabase(ws.win, false)
		if len(shown) != 2 || !strings.Contains(overlayText(ws), "DB Browser for SQLite") {
			t.Errorf("没有能打开 .db 的程序时应在文件夹里显示并说明：%v\n%s", shown, overlayText(ws))
		}
		clearOverlays(ws)
		openResult = errors.New("boom")
		ws.openDatabase(ws.win, false)
		if !strings.Contains(overlayText(ws), "打不开报文数据库") {
			t.Errorf("打开失败时应说明：%s", overlayText(ws))
		}
		clearOverlays(ws)
	})
	// 数据库文件不在（Windows 上 SQLite 开着的文件删不掉，所以换成指向一个不存在的路径）
	SetRecorder(r, filepath.Join(filepath.Dir(dbPath), "missing.db"), nil)
	locked(func() {
		ws.openDatabase(ws.win, false)
		if !strings.Contains(overlayText(ws), "找不到报文数据库") {
			t.Errorf("数据库文件不在时应说明：%s", overlayText(ws))
		}
		ws.historyWin.Close()
	})
}
