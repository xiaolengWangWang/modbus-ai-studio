package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	useSources(t, Source{Name: "测试", LatestURL: srv.URL + "/latest"})

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
	useSources(t, Source{Name: "测试", LatestURL: srv.URL + "/missing"})
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

// flakyServer 提供一个安装包：第 fail 次及以前的请求只发一半就断开（stall 为 true 时发一半后卡住不动），
// 之后的请求按 Range 接着发。rangeOK 为 false 时忽略 Range、总是从头发。
type flakyServer struct {
	data    []byte
	fail    int
	stall   bool
	rangeOK bool
	mu      sync.Mutex
	reqs    []string // 每次请求的 Range 头
}

func (s *flakyServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *flakyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Header.Get("Range"))
	n, fail := len(s.reqs), s.fail
	s.mu.Unlock()
	start := 0
	if rg := r.Header.Get("Range"); rg != "" && s.rangeOK {
		fmt.Sscanf(rg, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.data)-1, len(s.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)-start))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
	}
	body := s.data[start:]
	if n <= fail {
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		if s.stall {
			time.Sleep(time.Second) // 比测试里的 stallTimeout 长
		}
		panic(http.ErrAbortHandler) // 断开连接
	}
	w.Write(body)
}

func fastRetry(t *testing.T) {
	d, st := retryDelay, stallTimeout
	retryDelay, stallTimeout = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { retryDelay, stallTimeout = d, st })
}

// 网络不好时：断开后用 Range 接着下，卡住不动时断开重连，下好后校验；服务器不支持 Range 时从头下。
func TestDownloadResumes(t *testing.T) {
	fastRetry(t)
	data := []byte(strings.Repeat("Modbus AI Studio 安装包。", 4000))
	sum := sha256.Sum256(data)
	for name, s := range map[string]*flakyServer{
		"断开两次后续传":      {data: data, fail: 2, rangeOK: true},
		"卡住后重连续传":      {data: data, fail: 1, stall: true, rangeOK: true},
		"服务器不支持 Range": {data: data, fail: 1},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(s)
			defer srv.Close()
			dir := t.TempDir()
			var last int64
			path, err := Download(context.Background(), Asset{Name: "p.zip", URL: srv.URL, Size: int64(len(data))}, hex.EncodeToString(sum[:]), dir,
				func(done, total int64) { last = done })
			if err != nil {
				t.Fatalf("%v，请求 %q", err, s.requests())
			}
			if b, _ := os.ReadFile(path); string(b) != string(data) || last != int64(len(data)) {
				t.Errorf("内容或进度不对：%d 字节，进度 %d", len(b), last)
			}
			if reqs := s.requests(); s.rangeOK && (len(reqs) != s.fail+1 || reqs[1] == "") {
				t.Errorf("应在断开处用 Range 接着下：%q", reqs)
			}
			if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
				t.Error("下好后不应留下 .part")
			}
			// 再下一次：已有校验通过的文件，直接用，不再请求
			n := len(s.requests())
			if p2, err := Download(context.Background(), Asset{Name: "p.zip", URL: srv.URL}, hex.EncodeToString(sum[:]), dir, nil); err != nil || p2 != path || len(s.requests()) != n {
				t.Errorf("已下好的文件应直接复用：%v，请求 %d → %d", err, n, len(s.requests()))
			}
		})
	}
}

// 一直下不下来时报错，下了一半的 .part 留着，下次接着下；取消时返回 context.Canceled。
func TestDownloadGivesUpAndCancels(t *testing.T) {
	fastRetry(t)
	data := []byte(strings.Repeat("x", 100000))
	sum := sha256.Sum256(data)
	s := &flakyServer{data: data, fail: 100, rangeOK: true}
	srv := httptest.NewServer(s)
	defer srv.Close()
	dir := t.TempDir()
	a := Asset{Name: "p.zip", URL: srv.URL}
	if _, err := Download(context.Background(), a, hex.EncodeToString(sum[:]), dir, nil); err == nil || len(s.requests()) != tries {
		t.Fatalf("应重试 %d 次后报错：%v，请求 %d 次", tries, err, len(s.requests()))
	}
	if fi, err := os.Stat(filepath.Join(dir, "p.zip.part")); err != nil || fi.Size() == 0 {
		t.Error("下了一半的文件应留着下次续传")
	}
	s.mu.Lock()
	s.fail = 0
	s.mu.Unlock()
	if _, err := Download(context.Background(), a, hex.EncodeToString(sum[:]), dir, nil); err != nil || !strings.HasPrefix(s.requests()[len(s.requests())-1], "bytes=") {
		t.Errorf("下次应接着下：%v %q", err, s.requests())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Download(ctx, Asset{Name: "q.zip", URL: srv.URL}, hex.EncodeToString(sum[:]), t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("取消时应返回 context.Canceled：%v", err)
	}
}

// GitHub 偶尔返回 502：重试；4xx 不重试。
func TestLatestRetries(t *testing.T) {
	fastRetry(t)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(Release{Tag: "v1.2.3"})
	}))
	defer srv.Close()
	useSources(t, Source{Name: "测试", LatestURL: srv.URL})
	if r, err := Latest(context.Background()); err != nil || r.Version() != "1.2.3" || n != 2 {
		t.Fatalf("502 后应重试成功：%+v %v，请求 %d 次", r, err, n)
	}
	useSources(t, Source{Name: "测试", LatestURL: srv.URL + "/missing"})
	srv.Config.Handler = http.NotFoundHandler()
	n = 0
	if _, err := Latest(context.Background()); err == nil {
		t.Error("404 应报错")
	}
}

func useSources(t *testing.T, ss ...Source) {
	old := Sources
	Sources = ss
	t.Cleanup(func() { Sources = old })
}

// 同时查询各下载源：取最新的；一样新时取排在前面的（Gitee），别的下载源上同名安装包记成镜像；
// 一个下载源连不上不影响，全都查不到才报错。
func TestLatestFromSources(t *testing.T) {
	fastRetry(t)
	serve := func(tag string, assetURL string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(Release{Tag: tag, Assets: []Asset{{Name: "p.zip", URL: assetURL}}})
		}))
	}
	gitee, github := serve("v1.0.1", "http://gitee/p.zip"), serve("v1.0.1", "http://github/p.zip")
	defer gitee.Close()
	defer github.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", http.StatusForbidden) }))
	defer down.Close()

	useSources(t, Source{"Gitee", gitee.URL, "https://gitee/page"}, Source{"GitHub", github.URL, "https://github/page"})
	r, err := Latest(context.Background())
	if err != nil || r.Source != "Gitee" || r.Page != "https://gitee/page" || r.Assets[0].URL != "http://gitee/p.zip" ||
		len(r.Assets[0].Mirrors) != 1 || r.Assets[0].Mirrors[0] != "http://github/p.zip" {
		t.Fatalf("一样新时应取 Gitee，GitHub 的同名安装包记成镜像：%+v %v", r, err)
	}

	newer := serve("v1.0.2", "http://github/p2.zip")
	defer newer.Close()
	useSources(t, Source{"Gitee", gitee.URL, ""}, Source{"GitHub", newer.URL, ""})
	if r, err := Latest(context.Background()); err != nil || r.Source != "GitHub" || r.Version() != "1.0.2" || len(r.Assets[0].Mirrors) != 0 {
		t.Errorf("GitHub 上更新时应取 GitHub：%+v %v", r, err)
	}

	useSources(t, Source{"Gitee", down.URL, ""}, Source{"GitHub", github.URL, ""})
	if r, err := Latest(context.Background()); err != nil || r.Source != "GitHub" {
		t.Errorf("Gitee 查不到时应用 GitHub：%+v %v", r, err)
	}
	useSources(t, Source{"Gitee", down.URL, ""}, Source{"GitHub", down.URL, ""})
	if _, err := Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "Gitee") || !strings.Contains(err.Error(), "GitHub") {
		t.Errorf("都查不到时应说明两边的原因：%v", err)
	}
}

// 发版时 Gitee 先建发行版再上传安装包：这期间 Gitee 上的同一版本缺本平台安装包，改用齐全的 GitHub。
func TestLatestSkipsIncompleteSource(t *testing.T) {
	fastRetry(t)
	suffix := assetSuffix(runtime.GOOS, runtime.GOARCH)
	if suffix == "" {
		t.Skip("这个平台没有安装包")
	}
	name := "ModbusAIStudio-1.0.3" + suffix
	sum := strings.Repeat("a", 64)
	serve := func(rel Release) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(rel)
		}))
	}
	complete := Release{Tag: "v1.0.3", Body: sum + "  " + name, Assets: []Asset{{Name: name, URL: "http://github/p"}}}
	github := serve(complete)
	defer github.Close()
	for what, rel := range map[string]Release{
		"还没上传安装包": {Tag: "v1.0.3", Body: sum + "  " + name},
		"说明里没有校验值": {Tag: "v1.0.3", Assets: []Asset{{Name: name, URL: "http://gitee/p"}}},
	} {
		gitee := serve(rel)
		useSources(t, Source{"Gitee", gitee.URL, ""}, Source{"GitHub", github.URL, ""})
		if r, err := Latest(context.Background()); err != nil || r.Source != "GitHub" || !installable(r) {
			t.Errorf("Gitee %s时应改用 GitHub：%+v %v", what, r, err)
		}
		gitee.Close()
	}

	gitee := serve(Release{Tag: "v1.0.3", Body: sum + "  " + name, Assets: []Asset{{Name: name, URL: "http://gitee/p"}}})
	defer gitee.Close()
	useSources(t, Source{"Gitee", gitee.URL, ""}, Source{"GitHub", github.URL, ""})
	if r, err := Latest(context.Background()); err != nil || r.Source != "Gitee" || len(r.Assets[0].Mirrors) != 1 {
		t.Errorf("两边都齐全时仍取 Gitee，GitHub 作镜像：%+v %v", r, err)
	}
}

// 主地址下载失败时换镜像接着下（Range 续传），最后校验。
func TestDownloadUsesMirror(t *testing.T) {
	fastRetry(t)
	data := []byte(strings.Repeat("镜像", 5000))
	sum := sha256.Sum256(data)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", http.StatusBadGateway) }))
	defer bad.Close()
	good := &flakyServer{data: data, rangeOK: true}
	srv := httptest.NewServer(good)
	defer srv.Close()
	a := Asset{Name: "p.zip", URL: bad.URL, Mirrors: []string{srv.URL}}
	path, err := Download(context.Background(), a, hex.EncodeToString(sum[:]), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(data) {
		t.Error("从镜像下载的内容不对")
	}
}
