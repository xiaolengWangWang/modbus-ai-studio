// release 在本机发版：等 GitHub Actions 测试和打包，下载并核对三个安装包，发布到 GitHub 和 Gitee，再核对发布结果。
// 版本号取自 cmd/modbus-ai/main.go，发布说明取自 dist/release-<版本>-summary.md（第一行“# Modbus AI Studio <版本>”）。
//
//	go run ./build/release prepare  # 等测试通过、打包，下载并核对安装包，生成带 SHA-256 的发布说明
//	go run ./build/release publish  # 打 tag，发布到 GitHub 和 Gitee，核对下载和检查更新，删掉旧版本的文件
//	go run ./build/release verify   # 只核对已发布的结果
//	go run ./build/release clean    # 删掉 dist/ 里旧版本的文件
//
// 每一步先查已有状态再动手，网络出错自动重试；仍失败时重跑同一条命令，做完的步骤会跳过。
// 过程同时记在 dist/release-<版本>.log。
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"modbus-ai-studio/internal/update"
)

type release struct {
	root, dist string
	version    string
	tag        string
	log        *log.Logger
}

func main() {
	if os.Getenv(askpassEnv) != "" {
		askpass()
		return
	}
	cmds := map[string]func(*release, context.Context) error{
		"prepare": (*release).prepare,
		"publish": (*release).publish,
		"verify":  (*release).verify,
		"clean":   func(r *release, _ context.Context) error { return r.clean() },
	}
	if len(os.Args) != 2 || cmds[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "用法：go run ./build/release prepare|publish|verify|clean")
		os.Exit(2)
	}
	r, err := open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r.log.Printf("== %s %s ==", os.Args[1], r.version)
	if err := cmds[os.Args[1]](r, ctx); err != nil {
		r.log.Printf("失败：%v", err)
		os.Exit(1)
	}
	r.log.Print("完成")
}

// open 找到仓库根目录，读出版本号，打开日志。
func open() (*release, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	var src []byte
	for {
		if src, err = os.ReadFile(filepath.Join(root, "cmd", "modbus-ai", "main.go")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return nil, errors.New("请在仓库目录里运行")
		}
		root = parent
	}
	v := versionOf(src)
	if v == "" {
		return nil, errors.New("cmd/modbus-ai/main.go 里没找到 x.y.z 形式的版本号")
	}
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dist, "release-"+v+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &release{root, dist, v, "v" + v, log.New(io.MultiWriter(os.Stdout, f), "", log.Ldate|log.Ltime)}, nil
}

// readmeTemplate 是打进 Windows zip 的明文说明模板；prepare 会检查提交中的内容没有被加密软件改成密文。
const readmeTemplate = "platform/windows/README.txt"

var versionLine = regexp.MustCompile(`(?m)^var version = "(\d+\.\d+\.\d+)"`)

func versionOf(src []byte) string {
	if m := versionLine.FindSubmatch(src); m != nil {
		return string(m[1])
	}
	return ""
}

// file 是 dist/ 下这个版本的发布文件：release-<版本><suffix>。
func (r *release) file(suffix string) string {
	return filepath.Join(r.dist, "release-"+r.version+suffix)
}

// prepare 确认要发布的提交已推送、版本号递增，等 CI 测试通过并打包，下载核对三个安装包，生成发布说明。
func (r *release) prepare(ctx context.Context) error {
	summary, err := r.summary()
	if err != nil {
		return err
	}
	head, err := r.git(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if c := r.tagCommit(ctx); c != "" && c != head {
		return fmt.Errorf("本地已有 %s，指向 %.8s，不是 HEAD %.8s", r.tag, c, head)
	}
	// 打进 zip 的说明要有这一版的更新记录；先查提交里的模板，免得等完 CI 才发现。
	readme, err := r.git(ctx, nil, "show", head+":"+readmeTemplate)
	if err != nil {
		return err
	}
	switch {
	case encrypted([]byte(readme)):
		return fmt.Errorf("提交里的 %s 是加密软件生成的密文，换成明文重新提交并推送", readmeTemplate)
	case !hasChangelog([]byte(readme), r.version):
		return fmt.Errorf("%s 的更新记录里还没有 %s（“### %s …”标题），补上并推送后再运行", readmeTemplate, r.version, r.version)
	}
	gh, err := newGitHub()
	if err != nil {
		return err
	}
	if err := retry(ctx, r.log, "核对 GitHub 上的 main", func() error { return r.checkPushed(ctx, gh, head) }); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Minute)
	defer cancel()
	run, err := r.waitCI(ctx, gh, head)
	if err != nil {
		return err
	}
	ci := filepath.Join(r.dist, "ci-"+r.version)
	if err := r.downloadArtifacts(ctx, gh, run, ci); err != nil {
		return err
	}
	m := manifest{Version: r.version, Commit: head, Run: run.HTMLURL}
	if m.Assets, err = r.checkInstallers(ci); err != nil {
		return err
	}
	notes := withChecksums(summary, m.Assets)
	if err := checkUpdaterView(localView(notes, m.Assets), m.Assets, false); err != nil {
		return err
	}
	for _, a := range m.Assets {
		line := a.SHA256 + "  " + a.Name + "\n"
		if err := os.WriteFile(filepath.Join(r.dist, a.Name+".sha256"), []byte(line), 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(r.file(".md"), []byte(notes), 0o644); err != nil {
		return err
	}
	if err := writeJSON(r.file("-assets.json"), m); err != nil {
		return err
	}
	if err := os.RemoveAll(ci); err != nil {
		r.log.Printf("没删掉 %s：%v", ci, err)
	}
	r.log.Printf("发布说明：%s", r.file(".md"))
	r.log.Print("看一遍发布说明，没问题就运行 go run ./build/release publish")
	return nil
}

// checkPushed 确认 GitHub 上的 main 就是本地 HEAD，且版本号比已发布的新。
func (r *release) checkPushed(ctx context.Context, gh *api, head string) error {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := gh.call(ctx, "GET", "/git/ref/heads/main", nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA != head {
		return permanent("GitHub 上的 main 是 %.8s，本地 HEAD 是 %.8s：先推送，或切到要发布的提交", ref.Object.SHA, head)
	}
	var latest ghRelease
	if err := gh.call(ctx, "GET", "/releases/latest", nil, &latest); err != nil && !isStatus(err, 404) {
		return err
	}
	if v := strings.TrimPrefix(latest.Tag, "v"); v != "" && v != r.version && !update.Newer(r.version, v) {
		return permanent("版本号 %s 不比已发布的 %s 新，先改 cmd/modbus-ai/main.go", r.version, v)
	}
	return nil
}

// publish 打 tag，发布到 GitHub 和 Gitee，核对发布结果，最后删掉旧版本的文件。
func (r *release) publish(ctx context.Context) error {
	m, notes, err := r.load()
	if err != nil {
		return err
	}
	gh, err := newGitHub()
	if err != nil {
		return err
	}
	gt, err := newGitee()
	if err != nil {
		return err
	}
	if err := r.pushTag(ctx, m.Commit); err != nil {
		return err
	}
	if err := retry(ctx, r.log, "发布到 GitHub", func() error { return r.publishGitHub(ctx, gh, m, notes) }); err != nil {
		return err
	}
	if err := retry(ctx, r.log, "推送到 Gitee", func() error { return r.pushGitee(ctx, m.Commit) }); err != nil {
		return err
	}
	if err := retry(ctx, r.log, "发布到 Gitee", func() error { return r.publishGitee(ctx, gt, m, notes) }); err != nil {
		return err
	}
	if err := r.verify(ctx); err != nil {
		return err
	}
	return r.clean()
}

// pushTag 在打包的提交上打 tag（已有就核对指向），推到 GitHub。
func (r *release) pushTag(ctx context.Context, commit string) error {
	switch c := r.tagCommit(ctx); c {
	case "":
		if _, err := r.git(ctx, nil, "tag", "-a", r.tag, "-m", "Modbus AI Studio "+r.version, commit); err != nil {
			return err
		}
		r.log.Printf("已创建 tag %s → %.8s", r.tag, commit)
	case commit:
	default:
		return fmt.Errorf("本地 %s 指向 %.8s，不是打包的提交 %.8s", r.tag, c, commit)
	}
	return retry(ctx, r.log, "推送 tag 到 GitHub", func() error {
		_, err := r.git(ctx, nil, "push", "origin", "refs/tags/"+r.tag)
		return err
	})
}

// tagCommit 是本地 tag 指向的提交，没有这个 tag 时为空。
func (r *release) tagCommit(ctx context.Context) string {
	c, err := r.git(ctx, nil, "rev-parse", "-q", "--verify", "refs/tags/"+r.tag+"^{commit}")
	if err != nil {
		return ""
	}
	return c
}

// verify 核对两边的发布：GitHub 的最新正式版、说明和文件摘要，Gitee 的说明和实际下载的字节，
// 以及程序自己的检查更新代码能查到新版本、两边的安装包和校验值一致。
func (r *release) verify(ctx context.Context) error {
	m, notes, err := r.load()
	if err != nil {
		return err
	}
	gh, err := newGitHub()
	if err != nil {
		return err
	}
	var ghRel ghRelease
	if err := retry(ctx, r.log, "核对 GitHub 发布", func() (err error) { ghRel, err = r.verifyGitHub(ctx, gh, m, notes); return }); err != nil {
		return err
	}
	r.log.Printf("GitHub 核对通过：%s", ghRel.HTMLURL)
	gt, err := newGitee()
	if err != nil {
		return err
	}
	var files []giteeFile
	if err := retry(ctx, r.log, "核对 Gitee 发布", func() (err error) { files, err = r.verifyGitee(ctx, gt, m, notes); return }); err != nil {
		return err
	}
	dir := filepath.Join(r.dist, "gitee-"+r.version)
	if err := r.downloadGitee(ctx, files, m.Assets, dir); err != nil {
		return err
	}
	r.log.Print("Gitee 核对通过：三个安装包下载后的 SHA-256 与 CI 产物一致")
	var rel update.Release
	if err := retry(ctx, r.log, "用检查更新代码核对", func() (err error) {
		if rel, err = update.Latest(ctx); err != nil {
			return err
		}
		if rel.Version() != r.version || rel.Source != update.Sources[0].Name {
			return fmt.Errorf("检查更新查到 %s 上的 %s，应该是 %s 上的 %s", rel.Source, rel.Version(), update.Sources[0].Name, r.version)
		}
		return checkUpdaterView(rel, m.Assets, true)
	}); err != nil {
		return err
	}
	r.log.Printf("检查更新核对通过：从 %s 查到 %s，三个平台的安装包和校验值一致，另一个下载源可备用", rel.Source, rel.Version())
	if err := writeJSON(r.file("-verification.json"), map[string]any{
		"version":     r.version,
		"commit":      m.Commit,
		"verified_at": time.Now().UTC().Format(time.RFC3339),
		"github":      ghRel.HTMLURL,
		"gitee":       "https://gitee.com/" + update.GiteeRepo + "/releases/tag/" + r.tag,
		"checks": []string{
			"GitHub 最新正式版是这个版本，说明一致，三个文件的 SHA-256 摘要与 CI 产物一致",
			"Gitee 说明一致，三个安装包下载后的 SHA-256 与 CI 产物一致",
			"检查更新优先查到 Gitee 上的这个版本，三个平台的安装包和校验值一致，GitHub 可备用",
		},
		"assets": m.Assets,
	}); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

var versionInName = regexp.MustCompile(`\d+\.\d+\.\d+`)

// clean 删掉 dist/ 里比当前版本旧的文件（安装包、解压目录、发布记录）。正在运行的程序删不掉，跳过。
func (r *release) clean() error {
	entries, err := os.ReadDir(r.dist)
	if err != nil {
		return err
	}
	for _, e := range entries {
		v := versionInName.FindString(e.Name())
		if v == "" || !update.Newer(r.version, v) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.dist, e.Name())); err != nil {
			r.log.Printf("没删掉 %s（程序正开着？）：%v", e.Name(), err)
			continue
		}
		r.log.Printf("已删除 %s", e.Name())
	}
	return nil
}

// git 在仓库目录运行 git，不弹出凭据输入框。
func (r *release) git(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.root
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s：%v\n%s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// readText 读本机文本文件。这台电脑的加密软件会把一部分文件（主要是 .md）存成密文，
// 只有 PowerShell 等受信任的程序读到明文，读到密文时改用 PowerShell 读。
func readText(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil && encrypted(b) {
		if runtime.GOOS != "windows" {
			return nil, fmt.Errorf("%s 是加密文件", path)
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"[Convert]::ToBase64String([IO.File]::ReadAllBytes($env:RELEASE_READ_FILE))")
		cmd.Env = append(os.Environ(), "RELEASE_READ_FILE="+path)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("用 PowerShell 读 %s：%w", path, err)
		}
		if b, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(out))); err != nil {
			return nil, err
		}
		if encrypted(b) {
			return nil, fmt.Errorf("%s 是加密文件，PowerShell 也读不出明文", path)
		}
	}
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), err
}

func encrypted(b []byte) bool {
	return bytes.Contains(b[:min(len(b), 64)], []byte("E-SafeNet"))
}
