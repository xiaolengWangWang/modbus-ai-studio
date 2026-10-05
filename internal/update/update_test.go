package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.10.0", "0.9.1", true},
		{"0.9.1", "0.10.0", false},
		{"0.10.0", "0.10.0", false},
		{"v0.10.1", "0.10.0", true},
		{"0.10.0", "0.10.0-dev", true},
		{"0.10.0-rc1", "0.10.0", false},
		{"1.0.0", "0.99.99", true},
		{"0.10.0", "test", true}, // 当前版本不是版本号（测试、源码运行）
		{"abc", "0.9.1", false},  // 发布的版本号不对，不提示
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

const body = `## 新功能

- 导入四个数据区的点

## 下载

| 文件 | 适用 |
| --- | --- |

## SHA-256

` + "```" + `
d58356184fe45d42077f0c9c6c75070ca6b5bc5e189dfd4013776e5dd6543c6a  ModbusAIStudio-0.10.0-Windows-x64.zip
889A8573057886BE011810FCDA0DB1A84EE1BC84CDBC126FE9F42C6F930D0608  ModbusAIStudio-0.10.0-macOS-AppleSilicon.dmg
` + "```"

func TestReleaseAssetChecksumNotes(t *testing.T) {
	r := Release{Tag: "v0.10.0", Body: body, Assets: []Asset{
		{Name: "ModbusAIStudio-0.10.0-macOS-AppleSilicon.dmg"}, {Name: "ModbusAIStudio-0.10.0-macOS-Intel.dmg"},
		{Name: "ModbusAIStudio-0.10.0-Windows-x64.zip"},
	}}
	for _, c := range []struct{ goos, goarch, want string }{
		{"windows", "amd64", "ModbusAIStudio-0.10.0-Windows-x64.zip"},
		{"darwin", "amd64", "ModbusAIStudio-0.10.0-macOS-Intel.dmg"},
		{"darwin", "arm64", "ModbusAIStudio-0.10.0-macOS-AppleSilicon.dmg"},
		{"linux", "amd64", ""},
	} {
		a, _ := r.Asset(c.goos, c.goarch)
		if a.Name != c.want {
			t.Errorf("%s/%s 应取 %q，实际 %q", c.goos, c.goarch, c.want, a.Name)
		}
	}
	if sum, ok := r.Checksum("ModbusAIStudio-0.10.0-macOS-AppleSilicon.dmg"); !ok || sum != "889a8573057886be011810fcda0db1a84ee1bc84cdbc126fe9f42c6f930d0608" {
		t.Errorf("校验值 %q %v", sum, ok)
	}
	if _, ok := r.Checksum("ModbusAIStudio-0.10.0-macOS-Intel.dmg"); ok {
		t.Error("发布说明里没有的安装包不应有校验值")
	}
	if n := r.Notes(); n != "## 新功能\n\n- 导入四个数据区的点" {
		t.Errorf("更新内容应去掉下载表和校验值：%q", n)
	}
}

func TestLatestAndDownload(t *testing.T) {
	pkg := []byte("new package")
	sum := sha256.Sum256(pkg)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			if r.Header.Get("User-Agent") == "" {
				http.Error(w, "GitHub 要求 User-Agent", http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(Release{Tag: "v9.9.9", Assets: []Asset{{Name: "a.zip", URL: srv.URL + "/a.zip", Size: int64(len(pkg))}}})
		case "/a.zip":
			w.Write(pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := LatestURL
	LatestURL = srv.URL + "/latest"
	defer func() { LatestURL = old }()

	r, err := Latest(context.Background())
	if err != nil || r.Version() != "9.9.9" {
		t.Fatalf("Latest：%+v %v", r, err)
	}
	dir := t.TempDir()
	var last int64
	path, err := Download(context.Background(), r.Assets[0], hex.EncodeToString(sum[:]), dir, func(done, total int64) { last = done })
	if err != nil || last != int64(len(pkg)) {
		t.Fatalf("下载：%v，进度 %d", err, last)
	}
	if b, _ := os.ReadFile(path); string(b) != string(pkg) {
		t.Errorf("下载内容不对：%q", b)
	}
	// 校验值不对：报错并删掉文件
	if _, err := Download(context.Background(), r.Assets[0], strings.Repeat("0", 64), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Errorf("校验值不对应报错：%v", err)
	}
	LatestURL = srv.URL + "/missing"
	if _, err := Latest(context.Background()); err == nil {
		t.Error("没有发布时应报错")
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, body := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Windows 绿色版：旧文件改名为 .old、新文件放到原位置；下次启动清理 .old。
func TestInstallZip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ModbusAIStudio.exe"), []byte("old exe"), 0o755)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("old readme"), 0o644)
	os.WriteFile(filepath.Join(dir, "我的工作区.json"), []byte("keep"), 0o644)
	pkg := filepath.Join(t.TempDir(), "p.zip")
	writeZip(t, pkg, map[string]string{
		"ModbusAIStudio-9.9.9-Windows-x64/ModbusAIStudio.exe": "new exe",
		"ModbusAIStudio-9.9.9-Windows-x64/modbus-cli.exe":     "new cli",
		"ModbusAIStudio-9.9.9-Windows-x64/README.txt":         "new readme",
	})
	exe, err := installZip(pkg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if exe != filepath.Join(dir, "ModbusAIStudio.exe") {
		t.Errorf("重启路径 %s", exe)
	}
	for name, want := range map[string]string{
		"ModbusAIStudio.exe": "new exe", "modbus-cli.exe": "new cli", "README.txt": "new readme",
		"ModbusAIStudio.exe.old": "old exe", "我的工作区.json": "keep",
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s 内容 %q，期望 %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, stageDir)); !os.IsNotExist(err) {
		t.Error("解压目录应已删除")
	}
	cleanupDir(dir)
	if _, err := os.Stat(filepath.Join(dir, "ModbusAIStudio.exe.old")); !os.IsNotExist(err) {
		t.Error("启动时应删掉 .old")
	}
	if readFile(t, filepath.Join(dir, "ModbusAIStudio.exe")) != "new exe" {
		t.Error("清理不应碰新文件")
	}
}

func TestInstallZipRejectsBadPackages(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ModbusAIStudio.exe"), []byte("old exe"), 0o755)
	for name, files := range map[string]map[string]string{
		"没有主程序":   {"top/README.txt": "x"},
		"路径穿越出目录": {"top/../../evil.exe": "x", "top/ModbusAIStudio.exe": "new"},
	} {
		pkg := filepath.Join(t.TempDir(), "p.zip")
		writeZip(t, pkg, files)
		if _, err := installZip(pkg, dir); err == nil {
			t.Errorf("%s：应报错", name)
		}
		if got := readFile(t, filepath.Join(dir, "ModbusAIStudio.exe")); got != "old exe" {
			t.Errorf("%s：出错时不应改动程序，得到 %q", name, got)
		}
	}
	if _, err := installZip(filepath.Join(dir, "ModbusAIStudio.exe"), dir); err == nil {
		t.Error("不是 zip 应报错")
	}
}

// macOS：挂载 DMG，换掉 .app，旧版本留在 .app.old。
func TestInstallDMG(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上测试 DMG")
	}
	src := t.TempDir()
	newApp := filepath.Join(src, "Modbus AI Studio.app", "Contents", "MacOS")
	os.MkdirAll(newApp, 0o755)
	os.WriteFile(filepath.Join(newApp, "modbus-ai"), []byte("new"), 0o755)
	os.Symlink("/Applications", filepath.Join(src, "Applications"))
	pkg := filepath.Join(t.TempDir(), "p.dmg")
	if out, err := exec.Command("hdiutil", "create", "-volname", "test", "-srcfolder", src, "-ov", "-format", "UDZO", pkg).CombinedOutput(); err != nil {
		t.Skipf("hdiutil 不可用：%s", out)
	}
	dst := t.TempDir()
	app := filepath.Join(dst, "Modbus AI Studio.app")
	os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "MacOS", "modbus-ai"), []byte("old"), 0o755)
	if err := installDMG(pkg, app); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(app, "Contents", "MacOS", "modbus-ai")); got != "new" {
		t.Errorf("新版本没有装上：%q", got)
	}
	if got := readFile(t, filepath.Join(app+oldSuffix, "Contents", "MacOS", "modbus-ai")); got != "old" {
		t.Errorf("旧版本应留在 .app.old：%q", got)
	}
	if p, ok := appBundle(filepath.Join(app, "Contents", "MacOS", "modbus-ai")); !ok || p != app {
		t.Errorf("appBundle：%s %v", p, ok)
	}
	if _, ok := appBundle("/usr/local/bin/modbus-ai"); ok {
		t.Error("不在 .app 里时不能自动安装")
	}
}
