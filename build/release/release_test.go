package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRelease(t *testing.T, version string) *release {
	dist := t.TempDir()
	return &release{dist: dist, version: version, tag: "v" + version, log: log.New(io.Discard, "", 0)}
}

func TestVersionOf(t *testing.T) {
	src := []byte("package main\n\n// version 在打包时覆盖。\nvar version = \"1.2.3\"\r\n\nfunc main() {}\n")
	if v := versionOf(src); v != "1.2.3" {
		t.Fatalf("versionOf = %q", v)
	}
	if v := versionOf([]byte(`var version = "dev"`)); v != "" {
		t.Fatalf("非 x.y.z 版本号应为空，得到 %q", v)
	}
}

func testAssets(version string) []asset {
	var assets []asset
	for i, name := range installers(version) {
		assets = append(assets, asset{name, int64(i + 1), strings.Repeat(string(rune('a'+i)), 64)})
	}
	return assets
}

// 生成的说明要能被程序的检查更新代码认出三个平台的安装包和校验值，更新内容不含 SHA-256 段落。
func TestNotesReadableByUpdater(t *testing.T) {
	assets := testAssets("1.2.3")
	summary := "# Modbus AI Studio 1.2.3\n\n- 修复"
	rel := localView(withChecksums(summary, assets), assets)
	if err := checkUpdaterView(rel, assets, false); err != nil {
		t.Fatal(err)
	}
	if rel.Notes() != summary {
		t.Fatalf("更新内容 = %q", rel.Notes())
	}
	if err := checkUpdaterView(rel, assets, true); err == nil {
		t.Fatal("没有备用下载源时应报错")
	}
	assets[0].SHA256 = strings.Repeat("f", 64)
	if err := checkUpdaterView(rel, assets, false); err == nil {
		t.Fatal("校验值对不上时应报错")
	}
}

func TestSummary(t *testing.T) {
	r := testRelease(t, "1.2.3")
	write := func(s string) {
		if err := os.WriteFile(r.file("-summary.md"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("\xef\xbb\xbf# Modbus AI Studio 1.2.3\r\n\r\n- 修复\r\n")
	if s, err := r.summary(); err != nil || s != "# Modbus AI Studio 1.2.3\n\n- 修复" {
		t.Fatalf("summary = %q, %v", s, err)
	}
	for _, bad := range []string{"# Modbus AI Studio 1.2.2\n\n- 上一版", "# Modbus AI Studio 1.2.3\n\n## SHA-256\n"} {
		write(bad)
		if _, err := r.summary(); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

const goodReadme = "\xef\xbb\xbfModbus AI Studio 1.2.3\r\n\r\n日志超过 50 MB 时分卷；关闭窗口隐藏到托盘，采集继续。\r\n\r\n### 1.2.3 修复\r\n\r\n- 修复。\r\n\r\n### 1.2.2 上一版\r\n"

func TestHasChangelog(t *testing.T) {
	for text, want := range map[string]bool{
		"说明\n\n### 1.2.3 修复检测\n- 修复":  true,
		"说明\r\n### 1.2.3\r\n":         true,
		"### 1.2.3":                   true,
		"### 1.2.30 以后\n":             false,
		"### 1.2.2 上一版\n- 提到 1.2.3\n": false,
		"#### 1.2.3 小标题\n":            false,
	} {
		if got := hasChangelog([]byte(text), "1.2.3"); got != want {
			t.Errorf("hasChangelog(%q) = %v", text, got)
		}
	}
}

func TestCheckReadme(t *testing.T) {
	if err := checkReadme([]byte(goodReadme), "1.2.3"); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"没有 BOM": strings.TrimPrefix(goodReadme, "\xef\xbb\xbf"),
		"LF 换行":  strings.ReplaceAll(goodReadme, "\r\n", "\n"),
		"被加密":    "\xef\xbb\xbfb\x14#e+\x00E-SafeNet\x00LOCK",
		"模板没展开":  goodReadme + "{{VERSION}}\r\n",
		"版本号不对":  strings.ReplaceAll(goodReadme, "1.2.3", "1.2.4"),
		"没有更新记录": strings.ReplaceAll(goodReadme, "### 1.2.3 修复", "说明"),
		"少了托盘说明": strings.ReplaceAll(goodReadme, "托盘", "后台"),
	} {
		if err := checkReadme([]byte(bad), "1.2.3"); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, body)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnpackWindows(t *testing.T) {
	top := "ModbusAIStudio-1.2.3-Windows-x64/"
	good := map[string]string{top: "", top + "ModbusAIStudio.exe": "MZ", top + "README.txt": goodReadme}
	pkg := filepath.Join(t.TempDir(), "ModbusAIStudio-1.2.3-Windows-x64.zip")
	writeZip(t, pkg, good)
	dir := t.TempDir()
	if err := unpackWindows(pkg, "1.2.3", dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ModbusAIStudio.exe")); string(b) != "MZ" {
		t.Fatalf("exe 没解压出来：%q", b)
	}
	for name, files := range map[string]map[string]string{
		"多了命令行工具": {top + "ModbusAIStudio.exe": "MZ", top + "README.txt": goodReadme, top + "modbus-cli.exe": "MZ"},
		"反斜杠路径":   {top + "ModbusAIStudio.exe": "MZ", "ModbusAIStudio-1.2.3-Windows-x64\\README.txt": goodReadme},
		"缺少说明":    {top + "ModbusAIStudio.exe": "MZ"},
		"说明是 LF":  {top + "ModbusAIStudio.exe": "MZ", top + "README.txt": strings.ReplaceAll(goodReadme, "\r\n", "\n")},
	} {
		writeZip(t, pkg, files)
		if err := unpackWindows(pkg, "1.2.3", t.TempDir()); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func TestExtractInstallers(t *testing.T) {
	ci, dst := t.TempDir(), t.TempDir()
	names := installers("1.2.3")
	writeZip(t, filepath.Join(ci, "installers-Linux.zip"), map[string]string{names[0]: "win"})
	writeZip(t, filepath.Join(ci, "installers-macOS.zip"), map[string]string{names[1]: "intel", names[2]: "arm", "other.txt": "x"})
	if err := extractInstallers(ci, dst, names); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, names[2])); string(b) != "arm" {
		t.Fatalf("没取出 %s", names[2])
	}
	os.Remove(filepath.Join(ci, "installers-Linux.zip"))
	if err := extractInstallers(ci, dst, names); err == nil {
		t.Fatal("缺少 Windows 安装包时应报错")
	}
}

func TestCheckDMG(t *testing.T) {
	dmg := filepath.Join(t.TempDir(), "a.dmg")
	b := make([]byte, 2<<20)
	os.WriteFile(dmg, b, 0o644)
	if checkDMG(dmg) == nil {
		t.Fatal("没有 koly 块时应报错")
	}
	copy(b[len(b)-512:], "koly")
	os.WriteFile(dmg, b, 0o644)
	if err := checkDMG(dmg); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dmg, b[len(b)-1024:], 0o644)
	if checkDMG(dmg) == nil {
		t.Fatal("文件太小时应报错")
	}
}

func TestCheckWinres(t *testing.T) {
	res := `{
  "RT_GROUP_ICON": {"GLFW_ICON": {"0000": "GLFW_ICON_0000.ico"}},
  "RT_MANIFEST": {"#1": {"0409": {"dpi-awareness": "per monitor v2"}}},
  "RT_VERSION": {"#1": {"0409": {"fixed": {"file_version": "1.2.3.0", "product_version": "1.2.3.0"}}}}
}`
	if err := checkWinres([]byte(res), "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if checkWinres([]byte(res), "1.2.4") == nil {
		t.Fatal("版本号不对时应报错")
	}
	for _, bad := range []string{
		strings.Replace(res, "GLFW_ICON", "APP_ICON", 1),
		strings.Replace(res, "per monitor v2", "system", 1),
	} {
		if checkWinres([]byte(bad), "1.2.3") == nil {
			t.Errorf("应报错：%s", bad)
		}
	}
}

// 下载断开后从 .part 接着下；内容不对时删掉 .part 报错。
func TestDownloadResumes(t *testing.T) {
	body := bytes.Repeat([]byte("modbus"), 1000)
	var mu sync.Mutex
	var ranges []string
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ranges)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		ranges = append(ranges, req.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, req, "a.zip", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(path+".part", body[:1000], 0o644)
	sum := sha256Hex(body)
	if err := download(context.Background(), srv.URL, "", path, int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	if got := requests(); !fileMatches(path, int64(len(body)), sum) || !slices.Equal(got, []string{"bytes=1000-"}) {
		t.Fatalf("没有从断开处接着下：%q", got)
	}
	if err := download(context.Background(), srv.URL, "", path, int64(len(body)), sum); err != nil || len(requests()) != 1 {
		t.Fatalf("已下好的文件不该再下：%v %q", err, requests())
	}
	bad := filepath.Join(t.TempDir(), "b.zip")
	if download(context.Background(), srv.URL, "", bad, int64(len(body)), strings.Repeat("0", 64)) == nil {
		t.Fatal("SHA-256 不对时应报错")
	}
	if fileExists(bad) || fileExists(bad+".part") {
		t.Fatal("SHA-256 不对的文件应删掉")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCleanRemovesOlderVersionsOnly(t *testing.T) {
	r := testRelease(t, "1.0.3")
	for _, name := range []string{"ModbusAIStudio-1.0.2-Windows-x64.zip", "release-0.11.15.md", "release-1.0.3.md", "release-1.0.10-summary.md", "AppIcon.icns"} {
		os.WriteFile(filepath.Join(r.dist, name), nil, 0o644)
	}
	os.MkdirAll(filepath.Join(r.dist, "ModbusAIStudio-1.0.2-Windows-x64"), 0o755)
	if err := r.clean(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(r.dist)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"AppIcon.icns", "release-1.0.10-summary.md", "release-1.0.3.md"}; !slices.Equal(left, want) {
		t.Fatalf("剩下 %v，应为 %v", left, want)
	}
}
