// Package update 检查 GitHub Releases 上的新版本，下载本平台的安装包、按发布说明里的 SHA-256 校验后
// 替换正在运行的程序：Windows 绿色版替换 exe 所在目录里的文件，macOS 替换 .app。界面在 internal/ui/update.go。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 发布安装包的仓库：GitHub 和它在 Gitee 上的镜像，两边发布同样的安装包和说明。
const (
	Repo      = "xiaolengWangWang/modbus-ai-studio"
	GiteeRepo = "sun_xuanqi/modbus_ai_studio"
)

// Source 是一个发布安装包的地方。
type Source struct {
	Name      string // 给用户看的名字：Gitee、GitHub
	LatestURL string // 最新正式版的 API 地址，GitHub 和 Gitee 返回的格式相同
	PageURL   string // 给用户手动下载的页面
}

// Sources 是检查更新时同时查询的地方，排在前面的优先：国内访问 GitHub 下载经常连不上，Gitee 在前。
// 测试时换成本地服务器。
var Sources = []Source{
	{"Gitee", "https://gitee.com/api/v5/repos/" + GiteeRepo + "/releases/latest", "https://gitee.com/" + GiteeRepo + "/releases"},
	{"GitHub", "https://api.github.com/repos/" + Repo + "/releases/latest", "https://github.com/" + Repo + "/releases/latest"},
}

// PageURL 是没查到发布时给用户的下载页面。
func PageURL() string { return Sources[0].PageURL }

// ErrUnsupported 表示当前平台或运行方式不能自动安装（Linux、从源码运行、不在 .app 里），只能手动下载。
var ErrUnsupported = errors.New("这个平台或运行方式不支持自动安装")

// Asset 是发布里的一个安装包。
type Asset struct {
	Name    string   `json:"name"`
	URL     string   `json:"browser_download_url"`
	Size    int64    `json:"size"`
	Mirrors []string `json:"-"` // 别的下载源上同一个文件的地址，下载出错时换着用
}

// Release 是一个发布。
type Release struct {
	Tag    string  `json:"tag_name"`
	Name   string  `json:"name"`
	Body   string  `json:"body"`
	Assets []Asset `json:"assets"`
	Source string  `json:"-"` // 从哪个下载源查到的
	Page   string  `json:"-"` // 这个下载源的下载页面
}

// Version 是去掉 v 前缀的版本号。
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// client 用于检查和下载更新。GitHub 在部分网络下握手、下载都很慢，超时比默认的宽松；
// 整体时长由调用方的 context 控制，卡住不动由 Download 自己检测。
var client = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	return t
}()}

var (
	retryDelay   = 3 * time.Second  // 网络出错后隔多久重试，测试时调小
	stallTimeout = 60 * time.Second // 下载时这么久收不到数据就断开重连
)

// tries 是网络出错时的尝试次数。下载时每次都从断开处接着下，不浪费已下载的部分。
const tries = 6

// sleep 等待 d，context 结束时提前返回 false。
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Latest 同时查询全部下载源，取版本最新的发布（一样新时取排在前面的下载源）；同一个安装包在别的下载源上
// 也有时记进 Mirrors，下载出错时换着用。全部查不到才报错。每个下载源出错时重试几次。
func Latest(ctx context.Context) (Release, error) {
	type result struct {
		r   Release
		err error
	}
	results := make([]result, len(Sources))
	var wg sync.WaitGroup
	for i, src := range Sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := latestFrom(ctx, src)
			r.Source, r.Page = src.Name, src.PageURL
			results[i] = result{r, err}
		}()
	}
	wg.Wait()
	best := -1
	var errs []error
	for i, res := range results {
		if res.err != nil {
			errs = append(errs, fmt.Errorf("%s：%w", Sources[i].Name, res.err))
			continue
		}
		if best < 0 || Newer(res.r.Version(), results[best].r.Version()) {
			best = i
		}
	}
	if best < 0 {
		return Release{}, errors.Join(errs...)
	}
	rel := results[best].r
	for i, res := range results {
		if i == best || res.err != nil || res.r.Version() != rel.Version() {
			continue
		}
		for k := range rel.Assets {
			for _, other := range res.r.Assets {
				if other.Name == rel.Assets[k].Name && other.URL != "" {
					rel.Assets[k].Mirrors = append(rel.Assets[k].Mirrors, other.URL)
				}
			}
		}
	}
	return rel, nil
}

func latestFrom(ctx context.Context, src Source) (Release, error) {
	var r Release
	var err error
	for i := 0; i < 3; i++ {
		if i > 0 && !sleep(ctx, retryDelay) {
			break
		}
		var retry bool
		r, retry, err = latestOnce(ctx, src.LatestURL)
		if err == nil || !retry {
			break
		}
	}
	return r, err
}

func latestOnce(ctx context.Context, url string) (r Release, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return r, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ModbusAIStudio")
	resp, err := client.Do(req)
	if err != nil {
		return r, ctx.Err() == nil, fmt.Errorf("连不上：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return r, resp.StatusCode >= 500, fmt.Errorf("返回 %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return r, ctx.Err() == nil, fmt.Errorf("发布信息没有读完整：%w", err)
	}
	if r.Version() == "" {
		return r, false, errors.New("发布信息里没有版本号")
	}
	return r, false, nil
}

// Newer 判断版本 a 是否比 b 新。版本是 x.y.z，可带 -dev 这类后缀，带后缀的比同号正式版旧；
// b 不是版本号（例如测试里的 "test"）时认为 a 更新。
func Newer(a, b string) bool {
	va, ok := parseVersion(a)
	if !ok {
		return false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return true
	}
	for i := 0; i < 3; i++ {
		if va.n[i] != vb.n[i] {
			return va.n[i] > vb.n[i]
		}
	}
	return va.pre == "" && vb.pre != "" // 0.10.0 比 0.10.0-dev 新
}

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}

// assetSuffix 是各平台安装包文件名的结尾，与 build/macos.sh、build/windows.sh 一致。
func assetSuffix(goos, goarch string) string {
	switch {
	case goos == "windows" && goarch == "amd64":
		return "-Windows-x64.zip"
	case goos == "darwin" && goarch == "amd64":
		return "-macOS-Intel.dmg"
	case goos == "darwin" && goarch == "arm64":
		return "-macOS-AppleSilicon.dmg"
	}
	return ""
}

// Asset 返回本平台的安装包。
func (r Release) Asset(goos, goarch string) (Asset, bool) {
	suffix := assetSuffix(goos, goarch)
	if suffix == "" {
		return Asset{}, false
	}
	for _, a := range r.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return Asset{}, false
}

var sumLine = regexp.MustCompile(`(?m)^\s*([0-9a-fA-F]{64})\s+\*?(\S+)\s*$`)

// Checksum 从发布说明的 SHA-256 段落里找安装包的校验值。
func (r Release) Checksum(name string) (string, bool) {
	for _, m := range sumLine.FindAllStringSubmatch(r.Body, -1) {
		if m[2] == name {
			return strings.ToLower(m[1]), true
		}
	}
	return "", false
}

// Notes 是给用户看的更新内容：发布说明里“下载”“SHA-256”之前的部分。
func (r Release) Notes() string {
	body := strings.ReplaceAll(r.Body, "\r\n", "\n")
	for _, h := range []string{"\n## 下载", "\n## SHA-256"} {
		if i := strings.Index(body, h); i >= 0 {
			body = body[:i]
		}
	}
	return strings.TrimSpace(body)
}

// CacheDir 是下载更新的目录：下了一半的文件留在这里，取消或关掉程序后下次接着下。
func CacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ModbusAIStudio", "update")
}

// Download 把安装包下载到 dir，下载完按 sum 校验 SHA-256，不一致时删掉文件并报错。
// 先写到 .part 文件；网络断开、连续 stallTimeout 收不到数据时，隔一会儿用 Range 从断开处接着下，
// 最多 tries 次。dir 里已有校验通过的同名文件时直接用。progress 不在界面线程调用，total 未知时为 0。
func Download(ctx context.Context, a Asset, sum, dir string, progress func(done, total int64)) (string, error) {
	sum = strings.ToLower(sum)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, filepath.Base(a.Name))
	part := final + ".part"
	// 只留这个安装包的文件，别的版本下了一半的删掉
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if n := e.Name(); n != filepath.Base(final) && n != filepath.Base(part) {
				os.RemoveAll(filepath.Join(dir, n))
			}
		}
	}
	if got, err := fileSum(final); err == nil && got == sum {
		return final, nil
	}
	os.Remove(final)

	// 出错后换下一个下载源接着下：同一个文件，Range 续传照样有效，最后统一校验
	urls := append([]string{a.URL}, a.Mirrors...)
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 && !sleep(ctx, retryDelay) {
			break
		}
		var retry bool
		retry, err = downloadOnce(ctx, a, urls[i%len(urls)], part, progress)
		if err == nil || !retry {
			break
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	got, err := fileSum(part)
	if err != nil {
		return "", err
	}
	if got != sum {
		os.Remove(part)
		return "", fmt.Errorf("安装包校验失败（SHA-256 %s，应为 %s），文件可能下载不完整或被改动", got[:12], sum[:min(12, len(sum))])
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

// downloadOnce 接着 part 已有的内容下载一次。retry 表示出错后值得再试（网络问题、服务器 5xx）。
func downloadOnce(ctx context.Context, a Asset, url, part string, progress func(done, total int64)) (retry bool, err error) {
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(attempt, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "ModbusAIStudio")
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	var f *os.File
	total := a.Size
	switch resp.StatusCode {
	case http.StatusPartialContent: // 接着下
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0o644)
	case http.StatusOK: // 服务器不支持 Range，或者是第一次下：从头来
		have = 0
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		f, err = os.Create(part)
	case http.StatusRequestedRangeNotSatisfiable: // 上次其实已经下完了，交给校验
		return false, nil
	default:
		return resp.StatusCode >= 500, fmt.Errorf("下载失败：服务器返回 %s", resp.Status)
	}
	if err != nil {
		return false, err
	}
	defer f.Close()

	// 连续 stallTimeout 收不到数据就断开这次连接，由外层重试接着下。看门狗在本函数返回前退出
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	stall := stallTimeout
	stalled := make(chan struct{})
	watchdog := make(chan struct{})
	defer func() { cancel(); <-watchdog }()
	go func() {
		defer close(watchdog)
		t := time.NewTicker(min(stall/4, time.Second))
		defer t.Stop()
		for {
			select {
			case <-attempt.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRead.Load())) > stall {
					close(stalled)
					cancel()
					return
				}
			}
		}
	}()

	done := have
	buf := make([]byte, 64<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			lastRead.Store(time.Now().UnixNano())
			if _, err := f.Write(buf[:n]); err != nil {
				return false, err
			}
			done += int64(n)
			if progress != nil && time.Since(last) > 100*time.Millisecond {
				progress(done, total)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			select {
			case <-stalled:
				return true, fmt.Errorf("下载停滞：%s 没有收到数据", stall)
			default:
			}
			return ctx.Err() == nil, fmt.Errorf("下载中断：%w", rerr)
		}
	}
	if progress != nil {
		progress(done, total)
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if total > 0 && done < total {
		return true, fmt.Errorf("下载中断：只收到 %d / %d 字节", done, total)
	}
	return false, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Install 用下载好的安装包替换正在运行的程序，返回重启时要运行的路径。
func Install(pkg string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	switch runtime.GOOS {
	case "windows":
		return installZip(pkg, filepath.Dir(exe))
	case "darwin":
		app, ok := appBundle(exe)
		if !ok {
			return "", ErrUnsupported
		}
		return app, installDMG(pkg, app)
	}
	return "", ErrUnsupported
}

// CanInstall 表示当前运行方式能否自动安装：Windows 的 exe、macOS 的 .app。从源码运行（go run）时不行。
func CanInstall() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return strings.EqualFold(filepath.Base(exe), "ModbusAIStudio.exe")
	case "darwin":
		_, ok := appBundle(exe)
		return ok
	}
	return false
}

// appBundle 从 .app/Contents/MacOS/modbus-ai 找到 .app 目录。
func appBundle(exe string) (string, bool) {
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return "", false
	}
	return app, true
}

// Cleanup 删除上次更新留下的旧文件（Windows 的 *.old、macOS 的 .app.old）。启动时调用，失败不影响运行。
func Cleanup() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	switch runtime.GOOS {
	case "windows":
		cleanupDir(filepath.Dir(exe))
	case "darwin":
		if app, ok := appBundle(exe); ok {
			os.RemoveAll(app + oldSuffix)
		}
	}
}
