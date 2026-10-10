package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/update"
)

func automaticUpdateWorkspace(t *testing.T, tags ...string) (*Workspace, *atomic.Int32) {
	t.Helper()
	if len(tags) == 0 {
		tags = []string{"v9.9.9"}
	}
	calls := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		json.NewEncoder(w).Encode(update.Release{Tag: tags[min(int(n)-1, len(tags)-1)], Body: "## 更新内容\n\n- 修复检测"})
	}))
	old := update.Sources
	oldInstalled := installedUpdate.Swap(nil)
	update.Sources = []update.Source{{Name: "本地发布", LatestURL: srv.URL}}
	updating.Store(false)
	t.Cleanup(func() {
		update.Sources = old
		installedUpdate.Store(oldInstalled)
		updating.Store(false)
		srv.Close()
	})
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() { ws.Version = "1.0.0" })
	return ws, calls
}

func TestAutomaticUpdateChecksAgainOnStartupAfterEarlierCheck(t *testing.T) {
	ws, calls := automaticUpdateWorkspace(t)
	locked(func() {
		// Earlier today the program could have checked before a new release existed.
		ws.app.Preferences().SetInt(prefLastCheck, int(time.Now().Unix()))
		ws.AutoCheckUpdate()
	})
	waitFor(t, 5*time.Second, "启动后重新查询发布并提示新版本", func() bool {
		return calls.Load() > 0 && strings.Contains(overlayText(ws), "发现新版本 9.9.9")
	})
}

func TestAutomaticUpdateDefersPromptUntilOtherDialogCloses(t *testing.T) {
	ws, calls := automaticUpdateWorkspace(t)
	var other dialog.Dialog
	locked(func() {
		other = dialog.NewInformation("连接参数", "正在查看连接参数", ws.win)
		other.Show()
		ws.checkUpdate(false)
	})
	waitFor(t, 3*time.Second, "自动查询完成", func() bool {
		return calls.Load() == 1 && !updating.Load()
	})
	locked(func() {
		if !strings.Contains(overlayText(ws), "正在查看连接参数") {
			t.Fatal("更新提示不能覆盖已有对话框")
		}
		other.Hide()
	})
	waitFor(t, 3*time.Second, "关闭原对话框后显示待提示的新版本", func() bool {
		return strings.Contains(overlayText(ws), "发现新版本 9.9.9")
	})
	if calls.Load() != 1 {
		t.Fatalf("提示已查询的新版本不应再次请求发布接口：%d", calls.Load())
	}
}

func TestEnablingAutomaticUpdatesStartsChecking(t *testing.T) {
	ws, calls := automaticUpdateWorkspace(t)
	locked(func() {
		ws.toggleAutoUpdate()
		ws.app.Preferences().SetInt(prefLastCheck, int(time.Now().Unix()))
		ws.toggleAutoUpdate()
	})
	waitFor(t, 5*time.Second, "开启自动检查后查询并提示新版本", func() bool {
		return calls.Load() > 0 && strings.Contains(overlayText(ws), "发现新版本 9.9.9")
	})
}

func TestClosingUpdatePromptAllowsOtherWindowToCheck(t *testing.T) {
	ws, _ := automaticUpdateWorkspace(t)
	other := openWS(t, ws.app, false)
	locked(func() { ws.checkUpdate(true) })
	waitFor(t, 3*time.Second, "第一个窗口提示更新", func() bool {
		return strings.Contains(overlayText(ws), "发现新版本 9.9.9")
	})
	locked(func() {
		ws.win.Close()
		other.checkUpdate(true)
	})
	waitFor(t, 3*time.Second, "关闭提示所在窗口后另一个窗口仍可检查", func() bool {
		return strings.Contains(overlayText(other), "发现新版本 9.9.9")
	})
}

func TestAutomaticUpdateContinuesAfterPrimaryWindowCloses(t *testing.T) {
	fixture, calls := automaticUpdateWorkspace(t)
	desktop := NewDesktop(fixture.app, "1.0.0")
	var first, second *Workspace
	locked(func() {
		first, second = desktop.Open(), desktop.Open()
		first.AutoCheckUpdate()
		first.win.Close()
	})
	t.Cleanup(func() { locked(desktop.Shutdown) })
	waitFor(t, 5*time.Second, "剩余主窗口继续自动检查", func() bool {
		return calls.Load() > 0 && strings.Contains(overlayText(second), "发现新版本 9.9.9")
	})
}

func TestAutomaticUpdateFindsReleasePublishedWhileRunning(t *testing.T) {
	ws, calls := automaticUpdateWorkspace(t, "v1.0.0", "v9.9.9")
	locked(func() {
		ws.AutoCheckUpdate()
		ws.checkUpdate(false)
	})
	waitFor(t, 3*time.Second, "第一次检查时已是最新", func() bool {
		return calls.Load() == 1 && !updating.Load()
	})
	locked(func() {
		ws.pollUpdates(time.Now().Add(31 * time.Minute))
	})
	waitFor(t, 3*time.Second, "运行期间再次查询并提示新版本", func() bool {
		return calls.Load() == 2 && strings.Contains(overlayText(ws), "发现新版本 9.9.9")
	})
}
