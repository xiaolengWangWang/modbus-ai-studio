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
	"time"
)

// Repo 是发布安装包的 GitHub 仓库。
const Repo = "xiaolengWangWang/modbus-ai-studio"

var (
	// LatestURL 是最新正式版的 API 地址，测试时换成本地服务器。
	LatestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"
	// PageURL 是给用户手动下载的页面。
	PageURL = "https://github.com/" + Repo + "/releases/latest"
)

// ErrUnsupported 表示当前平台或运行方式不能自动安装（Linux、从源码运行、不在 .app 里），只能手动下载。
var ErrUnsupported = errors.New("这个平台或运行方式不支持自动安装")

// Asset 是发布里的一个安装包。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release 是 GitHub 上的一个发布。
type Release struct {
	Tag    string  `json:"tag_name"`
	Name   string  `json:"name"`
	Body   string  `json:"body"`
	Page   string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

// Version 是去掉 v 前缀的版本号。
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// Latest 取最新的正式版（GitHub 的 latest 不含草稿和预发布）。
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ModbusAIStudio")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("连不上 GitHub：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("发布信息格式不对：%w", err)
	}
	if r.Version() == "" {
		return Release{}, errors.New("发布信息里没有版本号")
	}
	return r, nil
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

// Download 把安装包下载到 dir，边下边算 SHA-256，与 sum 不一致时删掉文件并报错。
// progress 在下载过程中被调用（不在界面线程），total 未知时为 0。
func Download(ctx context.Context, a Asset, sum, dir string, progress func(done, total int64)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ModbusAIStudio")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败：服务器返回 %s", resp.Status)
	}
	path := filepath.Join(dir, filepath.Base(a.Name))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 64<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(path)
				return "", err
			}
			h.Write(buf[:n])
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
			f.Close()
			os.Remove(path)
			return "", fmt.Errorf("下载中断：%w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if progress != nil {
		progress(done, total)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(sum) {
		os.Remove(path)
		return "", fmt.Errorf("安装包校验失败（SHA-256 %s，应为 %s），文件可能下载不完整或被改动", got[:12], strings.ToLower(sum)[:min(12, len(sum))])
	}
	return path, nil
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
