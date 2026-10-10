package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/update"
)

func TestClosingDownloadWindowDoesNotAllowConcurrentUpdate(t *testing.T) {
	ws, _ := automaticUpdateWorkspace(t)
	other := openWS(t, ws.app, false)
	data := []byte("verified application download")
	sum := sha256.Sum256(data)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	oldDir, oldInstall := updateCacheDir, installUpdatePkg
	updateCacheDir = func() string { return dir }
	installUpdatePkg = func(string) (string, error) { return "", fmt.Errorf("fixture installation unavailable") }
	t.Cleanup(func() { updateCacheDir, installUpdatePkg = oldDir, oldInstall })
	t.Cleanup(func() {
		unblock()
		waitFor(t, 3*time.Second, "测试清理前下载结束", func() bool { return !updating.Load() })
	})
	locked(func() {
		updating.Store(true)
		ws.installUpdate(update.Release{Tag: "v9.9.9"}, update.Asset{Name: "new.zip", URL: srv.URL, Size: int64(len(data))}, hex.EncodeToString(sum[:]))
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("download did not start")
	}
	locked(func() {
		ws.win.Close()
		other.checkUpdate(true)
		if !updating.Load() || !strings.Contains(overlayText(other), "正在检查或下载更新") {
			t.Fatal("关闭下载所在窗口不能允许另一个更新同时启动")
		}
	})
	unblock()
	waitFor(t, 3*time.Second, "关闭原窗口后下载正常结束并释放忙状态", func() bool { return !updating.Load() })
}

func TestInstalledUpdateOffersRestartInsteadOfAnotherInstallation(t *testing.T) {
	ws, releaseCalls := automaticUpdateWorkspace(t)
	data := []byte("verified new application payload")
	sum := sha256.Sum256(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "new-program")
	oldDir, oldInstall := updateCacheDir, installUpdatePkg
	updateCacheDir = func() string { return filepath.Join(dir, "download") }
	// Confine installation writes to the fixture; discovery, downloading, checksum
	// verification and the UI's transition to awaiting restart remain real.
	installUpdatePkg = func(pkg string) (string, error) {
		got, err := os.ReadFile(pkg)
		if err != nil {
			return "", err
		}
		if string(got) != string(data) {
			return "", fmt.Errorf("installer received different bytes")
		}
		return target, os.WriteFile(target, got, 0o600)
	}
	t.Cleanup(func() { updateCacheDir, installUpdatePkg = oldDir, oldInstall })
	locked(func() {
		updating.Store(true)
		ws.installUpdate(update.Release{Tag: "v9.9.9"}, update.Asset{Name: "new.zip", URL: srv.URL, Size: int64(len(data))}, hex.EncodeToString(sum[:]))
	})
	waitFor(t, 5*time.Second, "安装完成并提示重启", func() bool {
		return !updating.Load() && strings.Contains(overlayText(ws), "已更新到 9.9.9")
	})
	// Another window still runs the old process version, but the installed
	// program is already new. Its update command must offer the pending restart.
	other := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		other.Version = "1.0.0"
		other.checkUpdate(true)
	})
	waitFor(t, 3*time.Second, "另一个窗口识别待重启状态", func() bool {
		return strings.Contains(overlayText(other), "已更新到 9.9.9") && strings.Contains(overlayText(other), "现在重启程序吗")
	})
	if releaseCalls.Load() != 0 {
		t.Fatalf("已安装的版本应直接提示重启，不应重新查询下载：%d", releaseCalls.Load())
	}
}
