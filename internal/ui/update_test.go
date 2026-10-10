package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
	var status atomic.Int32 // 服务器处理函数和测试都读写，用原子变量
	status.Store(http.StatusOK)
	tag := "v9.9.9"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "rate limited", code)
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
	old := update.Sources
	update.Sources = []update.Source{{Name: "测试", LatestURL: srv.URL}}
	defer func() { update.Sources = old }()

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

	status.Store(http.StatusForbidden) // GitHub 限流时返回 403，不重试
	if text := check(true); !strings.Contains(text, "检查更新失败") || !strings.Contains(text, "403") {
		t.Errorf("查询失败时应说明原因：\n%s", text)
	}
	if text := check(false); text != "" {
		t.Errorf("自动检查失败时不应打扰：\n%s", text)
	}

	status.Store(http.StatusOK)
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

// 下载并安装：下载到缓存目录、校验、安装，装好后问是否重启并删掉缓存；校验不过时说明原因、不安装。
func TestInstallUpdateFlow(t *testing.T) {
	oldInstalled := installedUpdate.Swap(nil)
	t.Cleanup(func() { installedUpdate.Store(oldInstalled) })
	data := []byte(strings.Repeat("新版本安装包", 1000))
	sum := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "update")
	var installed []byte
	oldDir, oldInstall := updateCacheDir, installUpdatePkg
	updateCacheDir = func() string { return dir }
	installUpdatePkg = func(pkg string) (string, error) {
		installed, _ = os.ReadFile(pkg)
		return "/fake/ModbusAIStudio.exe", nil
	}
	defer func() { updateCacheDir, installUpdatePkg = oldDir, oldInstall }()

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	rel := update.Release{Tag: "v9.9.9"}
	asset := update.Asset{Name: "ModbusAIStudio-9.9.9-Windows-x64.zip", URL: srv.URL, Size: int64(len(data))}
	run := func(sum string) string {
		t.Helper()
		locked(func() {
			for _, o := range ws.win.Canvas().Overlays().List() {
				ws.win.Canvas().Overlays().Remove(o)
			}
			updating.Store(true)
			ws.installUpdate(rel, asset, sum)
		})
		var text string
		waitFor(t, 5*time.Second, "下载安装结束", func() bool {
			text = overlayText(ws)
			return !updating.Load()
		})
		return text
	}

	text := run(hex.EncodeToString(sum[:]))
	if !strings.Contains(text, "已更新到 9.9.9") || string(installed) != string(data) {
		t.Errorf("应安装下载的文件并询问重启（安装了 %d 字节）：\n%s", len(installed), text)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("装好后应删掉下载缓存")
	}
	if s := updateStatus(); s != "" {
		t.Errorf("下载结束后状态栏不应再显示进度：%q", s)
	}

	installed = nil
	text = run(strings.Repeat("0", 64))
	if !strings.Contains(text, "更新失败") || !strings.Contains(text, "校验失败") || installed != nil {
		t.Errorf("校验不过时应报错且不安装：\n%s", text)
	}
}
