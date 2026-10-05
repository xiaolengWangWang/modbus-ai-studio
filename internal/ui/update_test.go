package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/update"
)

// overlayText 收集主窗口最上层对话框里的文字和按钮。
func overlayText(ws *Workspace) string {
	top := ws.win.Canvas().Overlays().Top()
	if top == nil {
		return ""
	}
	var sb strings.Builder
	var walk func(o fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Label:
			sb.WriteString(x.Text + "\n")
		case *widget.Button:
			sb.WriteString("[" + x.Text + "]\n")
		case *widget.RichText:
			sb.WriteString(x.String() + "\n")
		}
		if w, ok := o.(fyne.Widget); ok {
			for _, c := range test.WidgetRenderer(w).Objects() {
				walk(c)
			}
		}
		if c, ok := o.(*fyne.Container); ok {
			for _, x := range c.Objects {
				walk(x)
			}
		}
	}
	walk(top)
	return sb.String()
}

// 检查更新：有新版本时说明内容和操作；已是最新、查询失败也要告诉用户（手动检查时）；
// 自动检查跳过用户忽略的版本，失败时不打扰。
func TestCheckUpdate(t *testing.T) {
	status := http.StatusOK
	tag := "v9.9.9"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "busy", status)
			return
		}
		rel := update.Release{Tag: tag, Body: "## 新功能\n\n- 软件更新\n\n## SHA-256\n\n```\n"}
		for _, name := range []string{"ModbusAIStudio-9.9.9-Windows-x64.zip", "ModbusAIStudio-9.9.9-macOS-Intel.dmg", "ModbusAIStudio-9.9.9-macOS-AppleSilicon.dmg"} {
			rel.Assets = append(rel.Assets, update.Asset{Name: name, URL: "http://127.0.0.1/" + name, Size: 1 << 20})
			rel.Body += strings.Repeat("a", 64) + "  " + name + "\n"
		}
		rel.Body += "```"
		json.NewEncoder(w).Encode(rel)
	}))
	defer srv.Close()
	old := update.LatestURL
	update.LatestURL = srv.URL
	defer func() { update.LatestURL = old }()

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	check := func(manual bool) string {
		t.Helper()
		locked(func() {
			for _, o := range ws.win.Canvas().Overlays().List() {
				ws.win.Canvas().Overlays().Remove(o)
			}
			updating.Store(false)
			ws.checkUpdate(manual)
		})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var busy bool
			var text string
			locked(func() { busy, text = updating.Load(), overlayText(ws) })
			if !strings.Contains(text, "正在从 GitHub") && (!busy || strings.Contains(text, "发现新版本")) {
				return text
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("检查更新没有结束")
		return ""
	}

	text := check(true)
	for _, want := range []string{"发现新版本 9.9.9（当前 test）", "软件更新", "[打开下载页面]", "[以后再说"} {
		if !strings.Contains(text, want) {
			t.Errorf("更新对话框缺少 %q：\n%s", want, text)
		}
	}
	// 测试程序不是从安装包运行的：不提供自动安装，只能手动下载（Linux 没有安装包）；校验值不显示给用户
	want := "当前程序不是从安装包运行的"
	if runtime.GOOS == "linux" {
		want = "没有适合本机的安装包"
	}
	if strings.Contains(text, "[下载并安装]") || strings.Contains(text, strings.Repeat("a", 64)) || !strings.Contains(text, want) {
		t.Errorf("从源码运行时只能手动下载，应提示 %q：\n%s", want, text)
	}

	locked(func() { ws.Version = "9.9.9" })
	if text := check(true); !strings.Contains(text, "已是最新版本 9.9.9") {
		t.Errorf("已是最新时应告诉用户：\n%s", text)
	}
	locked(func() { ws.Version = "test" })

	status = http.StatusBadGateway
	if text := check(true); !strings.Contains(text, "检查更新失败") || !strings.Contains(text, "502") {
		t.Errorf("查询失败时应说明原因：\n%s", text)
	}
	if text := check(false); text != "" {
		t.Errorf("自动检查失败时不应打扰：\n%s", text)
	}

	status = http.StatusOK
	locked(func() { a.Preferences().SetString(prefSkip, "9.9.9") })
	if text := check(false); text != "" {
		t.Errorf("自动检查应跳过用户忽略的版本：\n%s", text)
	}
	if text := check(true); !strings.Contains(text, "发现新版本 9.9.9") {
		t.Errorf("手动检查不受“跳过这个版本”影响：\n%s", text)
	}

	locked(func() {
		if !ws.autoUpdItem.Checked {
			t.Error("自动检查更新默认打开")
		}
		ws.toggleAutoUpdate()
		if ws.autoUpdItem.Checked || autoUpdate(a) {
			t.Error("关掉自动检查更新后菜单和设置都应更新")
		}
	})
}
