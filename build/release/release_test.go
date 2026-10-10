package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func preparedRelease(t *testing.T) (*release, manifest, string) {
	t.Helper()
	r := testRelease(t, "1.2.3")
	r.root = r.dist
	r.dist = filepath.Join(r.root, "dist")
	if err := os.Mkdir(r.dist, 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest{Version: "1.2.3", Commit: strings.Repeat("a", 40)}
	for _, name := range []string{
		"ModbusAIStudio-1.2.3-Windows-x64.zip",
		"ModbusAIStudio-1.2.3-macOS-Intel.dmg",
		"ModbusAIStudio-1.2.3-macOS-AppleSilicon.dmg",
	} {
		body := []byte("installer " + name)
		if err := os.WriteFile(filepath.Join(r.dist, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		m.Assets = append(m.Assets, asset{name, int64(len(body)), sha256Hex(body)})
	}
	return r, m, withChecksums("# Modbus AI Studio 1.2.3\n\n- 初稿", m.Assets)
}

func writePreparedRecord(t *testing.T, r *release, m manifest, notes string) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.file("-assets.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.file(".md"), []byte(notes), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllowsEditedReleaseBody(t *testing.T) {
	r, m, notes := preparedRelease(t)
	notes = strings.Replace(notes, "- 初稿", "- 补充本次修复的说明\n\n可以调整发布正文。", 1)
	// 记录里的安装包顺序无需与发布说明相同，但三个平台都必须在。
	m.Assets[0], m.Assets[2] = m.Assets[2], m.Assets[0]
	writePreparedRecord(t, r, m, notes)
	got, body, err := r.load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != m.Version || got.Commit != m.Commit || !slices.Equal(got.Assets, m.Assets) || body != notes {
		t.Fatalf("load 没有保留核对记录与编辑后的正文：%+v %q", got, body)
	}
}

func TestLoadRejectsInvalidManifest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*manifest)
	}{
		{"different version", func(m *manifest) { m.Version = "1.2.4" }},
		{"nonhex commit", func(m *manifest) { m.Commit = strings.Repeat("g", 40) }},
		{"uppercase commit", func(m *manifest) { m.Commit = strings.Repeat("A", 40) }},
		{"short commit", func(m *manifest) { m.Commit = strings.Repeat("a", 39) }},
		{"missing installer", func(m *manifest) { m.Assets = m.Assets[:2] }},
		{"extra installer", func(m *manifest) { m.Assets = append(m.Assets, m.Assets[0]) }},
		{"duplicate installer", func(m *manifest) { m.Assets[1] = m.Assets[0] }},
		{"nonhex checksum", func(m *manifest) { m.Assets[0].SHA256 = strings.Repeat("g", 64) }},
		{"uppercase checksum", func(m *manifest) { m.Assets[0].SHA256 = strings.ToUpper(m.Assets[0].SHA256) }},
		{"short checksum", func(m *manifest) { m.Assets[0].SHA256 = m.Assets[0].SHA256[:63] }},
		{"zero size", func(m *manifest) { m.Assets[0].Size = 0 }},
		{"negative size", func(m *manifest) { m.Assets[0].Size = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, m, notes := preparedRelease(t)
			tc.mutate(&m)
			writePreparedRecord(t, r, m, notes)
			if _, _, err := r.load(); err == nil {
				t.Fatal("无效的安装包记录应在发布前报错")
			}
		})
	}
}

func TestLoadRejectsInvalidReleaseVersion(t *testing.T) {
	for _, version := range []string{"dev", "1.2", "v1.2.3", "1.2.3-beta"} {
		t.Run(version, func(t *testing.T) {
			r, m, notes := preparedRelease(t)
			r.version, m.Version = version, version
			writePreparedRecord(t, r, m, notes)
			if _, _, err := r.load(); err == nil {
				t.Fatal("不是 x.y.z 的版本号应报错")
			}
		})
	}
}

func TestLoadRejectsUnexpectedInstallerPaths(t *testing.T) {
	for _, name := range []string{
		"unrelated.zip",
		"ModbusAIStudio-1.2.4-Windows-x64.zip",
		"../outside.zip",
		`..\outside.zip`,
		"absolute",
	} {
		t.Run(name, func(t *testing.T) {
			r, m, _ := preparedRelease(t)
			pkg := filepath.Join(r.dist, name)
			if name == "absolute" {
				name = filepath.Join(r.root, "outside.zip")
				pkg = name
			}
			body := []byte("unexpected installer")
			if err := os.WriteFile(pkg, body, 0o644); err != nil {
				t.Fatal(err)
			}
			m.Assets[0] = asset{name, int64(len(body)), sha256Hex(body)}
			writePreparedRecord(t, r, m, withChecksums("# Modbus AI Studio 1.2.3", m.Assets))
			if _, _, err := r.load(); err == nil {
				t.Fatal("不属于本版本的安装包或越界路径应报错")
			}
		})
	}
}

func TestLoadRejectsEmptyOrChangedInstaller(t *testing.T) {
	for _, body := range []string{"", "changed installer"} {
		t.Run(body, func(t *testing.T) {
			r, m, notes := preparedRelease(t)
			if err := os.WriteFile(filepath.Join(r.dist, m.Assets[0].Name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if body == "" {
				m.Assets[0].Size = 0
				m.Assets[0].SHA256 = sha256Hex(nil)
				notes = withChecksums("# Modbus AI Studio 1.2.3", m.Assets)
			}
			writePreparedRecord(t, r, m, notes)
			if _, _, err := r.load(); err == nil {
				t.Fatal("空安装包或已修改的安装包应报错")
			}
		})
	}
}

func TestLoadRejectsBrokenReleaseNotes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string, manifest) string
	}{
		{"different title", func(notes string, _ manifest) string {
			return strings.Replace(notes, "# Modbus AI Studio 1.2.3", "# Modbus AI Studio 1.2.4", 1)
		}},
		{"missing title", func(notes string, _ manifest) string {
			return strings.TrimPrefix(notes, "# Modbus AI Studio 1.2.3\n")
		}},
		{"missing checksum", func(notes string, m manifest) string {
			return strings.Replace(notes, m.Assets[0].SHA256+"  "+m.Assets[0].Name+"\n", "", 1)
		}},
		{"changed checksum", func(notes string, m manifest) string {
			return strings.Replace(notes, m.Assets[0].SHA256, strings.Repeat("0", 64), 1)
		}},
		{"changed checksum filename", func(notes string, m manifest) string {
			return strings.Replace(notes, m.Assets[0].Name, "previous-Windows-x64.zip", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, m, notes := preparedRelease(t)
			writePreparedRecord(t, r, m, tc.mutate(notes, m))
			if _, _, err := r.load(); err == nil {
				t.Fatal("标题或校验段被破坏的发布说明应报错")
			}
		})
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
