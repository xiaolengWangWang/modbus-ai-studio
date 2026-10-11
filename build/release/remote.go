package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/update"
)

const (
	githubAPI  = "https://api.github.com/repos/" + update.Repo
	giteeAPI   = "https://gitee.com/api/v5/repos/" + update.GiteeRepo
	giteeGit   = "https://gitee.com/" + update.GiteeRepo + ".git"
	userAgent  = "ModbusAIStudio-Release"
	askpassEnv = "MODBUS_RELEASE_ASKPASS"
)

// client 不设总超时：上传下载按各自的 context 控制。代理不稳时握手慢，超时比默认宽松。
var client = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = 2 * time.Minute
	return t
}()}

// api 调用 GitHub 或 Gitee 的接口，令牌放在 Authorization 头里，不进网址和日志。
type api struct {
	base, token string
	github      bool
}

func newGitHub() (*api, error) {
	token, err := githubToken()
	return &api{githubAPI, token, true}, err
}

func newGitee() (*api, error) {
	token, err := giteeToken()
	return &api{giteeAPI, token, false}, err
}

// call 发一个 JSON 请求，in 不为 nil 时作为请求体，返回的 JSON 解到 out。
func (a *api) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.send(req, out)
}

func (a *api) send(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("User-Agent", userAgent)
	if a.github {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &httpError{resp.StatusCode, req.Method + " " + req.URL.Path, string(b)}
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

type httpError struct {
	status int
	what   string
	body   string
}

func (e *httpError) Error() string {
	body := strings.TrimSpace(e.body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("%s 返回 %d：%s", e.what, e.status, body)
}

func isStatus(err error, code int) bool {
	var h *httpError
	return errors.As(err, &h) && h.status == code
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// permanent 标记重试也没用的错误，例如核对不通过。
func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// retry 运行 f，网络和服务器出错时最多重试 3 次；核对不通过、4xx 这类重试也没用的错误直接返回。
func retry(ctx context.Context, lg *log.Logger, what string, f func() error) error {
	for i := 1; ; i++ {
		err := f()
		if err == nil {
			return nil
		}
		var p permanentError
		var h *httpError
		if i == 4 || ctx.Err() != nil || errors.As(err, &p) || errors.As(err, &h) && h.status < 500 && h.status != 429 {
			return fmt.Errorf("%s：%w", what, err)
		}
		lg.Printf("%s出错，%d 秒后重试：%v", what, 5*i, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(5*i) * time.Second):
		}
	}
}

// download 把 url 下载到 path 并核对大小和 SHA-256。已下了一部分时从断开处接着下，下完不对就删掉报错。
// token 只发给 url 所在的主机，跳转到别的主机时 Go 会去掉 Authorization。
func download(ctx context.Context, url, token, path string, size int64, sum string) error {
	if fileMatches(path, size, sum) {
		return nil
	}
	part := path + ".part"
	var have int64
	if fi, err := os.Stat(part); err == nil && fi.Size() < size {
		have = fi.Size()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	switch resp.StatusCode {
	case http.StatusOK:
		have = 0
	case http.StatusPartialContent:
		flag = os.O_WRONLY | os.O_APPEND
	default:
		return &httpError{resp.StatusCode, "下载 " + filepath.Base(path), ""}
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, size-have+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err // 留着 .part，下次接着下
	}
	if !fileMatches(part, size, sum) {
		os.Remove(part)
		return fmt.Errorf("%s 下载后的大小或 SHA-256 不对", filepath.Base(path))
	}
	return os.Rename(part, path)
}

// githubToken 取 GitHub 令牌：环境变量 GH_TOKEN / GITHUB_TOKEN，或 git 凭据管理器里推送用的那个。
func githubToken() (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if t := os.Getenv(k); t != "" {
			return t, nil
		}
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	if out, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if t, ok := strings.CutPrefix(strings.TrimSpace(line), "password="); ok && t != "" {
				return t, nil
			}
		}
	}
	return "", errors.New("没有 GitHub 令牌：设置 GH_TOKEN，或先用 git 推送一次 GitHub 让凭据管理器记住")
}

// giteeToken 取 Gitee 私人令牌（勾选 projects）：环境变量 GITEE_TOKEN 或 ~/.gitee_token。
func giteeToken() (string, error) {
	if t := os.Getenv("GITEE_TOKEN"); t != "" {
		return t, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".gitee_token")); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		}
	}
	return "", errors.New("没有 Gitee 令牌：把私人令牌（勾选 projects）存到 ~/.gitee_token，或设置 GITEE_TOKEN")
}

// ---- GitHub Actions ----

type run struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Attempt    int    `json:"run_attempt"`
}

func latestRun(ctx context.Context, gh *api, workflow, sha string) (*run, error) {
	var res struct {
		Runs []run `json:"workflow_runs"`
	}
	if err := gh.call(ctx, "GET", "/actions/workflows/"+workflow+"/runs?per_page=5&head_sha="+sha, nil, &res); err != nil {
		return nil, err
	}
	if len(res.Runs) == 0 {
		return nil, nil
	}
	return &res.Runs[0], nil
}

var runStates = map[string]string{
	"queued": "排队", "requested": "排队", "waiting": "等待", "pending": "等待", "in_progress": "运行中",
	"success": "通过", "failure": "失败", "cancelled": "已取消", "timed_out": "超时",
}

func describe(x *run) string {
	switch {
	case x == nil:
		return "未开始"
	case x.Status != "completed":
		return runStates[x.Status] + "（" + x.HTMLURL + "）"
	case runStates[x.Conclusion] != "":
		return runStates[x.Conclusion]
	}
	return x.Conclusion
}

// waitCI 等 test 和 package 两个工作流在 head 上都成功，返回 package 那次运行。
// 还没打过包就触发 package 工作流；macOS runner 排不上时任务会被取消，自动重跑失败的任务，最多两次。
func (r *release) waitCI(ctx context.Context, gh *api, head string) (run, error) {
	dispatched := false
	reran := map[int64]int{}
	settle := func(name string, x *run) (bool, error) {
		switch {
		case x == nil || x.Status != "completed":
			return false, nil
		case x.Conclusion == "success":
			return true, nil
		case x.Conclusion != "cancelled" && x.Conclusion != "timed_out" || x.Attempt >= 3:
			return false, permanent("%s没有通过（%s）：%s", name, x.Conclusion, x.HTMLURL)
		case reran[x.ID] == x.Attempt:
			return false, nil // 已经要求重跑，等它开始
		}
		reran[x.ID] = x.Attempt
		r.log.Printf("%s被取消（多半是 macOS runner 排不上），重跑失败的任务", name)
		return false, retry(ctx, r.log, "重跑"+name, func() error {
			return gh.call(ctx, "POST", fmt.Sprintf("/actions/runs/%d/rerun-failed-jobs", x.ID), nil, nil)
		})
	}
	last := ""
	for {
		var test, pkg *run
		if err := retry(ctx, r.log, "查询 CI", func() (err error) {
			if test, err = latestRun(ctx, gh, "test.yml", head); err != nil {
				return err
			}
			pkg, err = latestRun(ctx, gh, "package.yml", head)
			return err
		}); err != nil {
			return run{}, err
		}
		if pkg == nil && !dispatched {
			if err := retry(ctx, r.log, "触发打包", func() error {
				return gh.call(ctx, "POST", "/actions/workflows/package.yml/dispatches",
					map[string]any{"ref": "main", "inputs": map[string]string{"version": r.version}}, nil)
			}); err != nil {
				return run{}, err
			}
			dispatched = true
			r.log.Print("已触发 package 工作流")
		}
		if s := "测试：" + describe(test) + "；打包：" + describe(pkg); s != last {
			r.log.Print(s)
			last = s
		}
		testOK, err := settle("测试", test)
		if err != nil {
			return run{}, err
		}
		pkgOK, err := settle("打包", pkg)
		if err != nil {
			return run{}, err
		}
		if testOK && pkgOK {
			return *pkg, nil
		}
		select {
		case <-ctx.Done():
			return run{}, fmt.Errorf("等 CI 超时：%w", ctx.Err())
		case <-time.After(30 * time.Second):
		}
	}
}

// downloadArtifacts 下载 package 工作流的两个产物，按 GitHub 记录的 SHA-256 摘要核对。
func (r *release) downloadArtifacts(ctx context.Context, gh *api, x run, dir string) error {
	var res struct {
		Artifacts []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size_in_bytes"`
			URL     string `json:"archive_download_url"`
			Expired bool   `json:"expired"`
			Digest  string `json:"digest"`
		} `json:"artifacts"`
	}
	if err := retry(ctx, r.log, "查询 CI 产物", func() error {
		return gh.call(ctx, "GET", fmt.Sprintf("/actions/runs/%d/artifacts", x.ID), nil, &res)
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"installers-Linux", "installers-macOS"} {
		i := -1
		for j, a := range res.Artifacts {
			if a.Name == name {
				i = j
			}
		}
		if i < 0 || res.Artifacts[i].Expired {
			return fmt.Errorf("%s 里没有 %s 或已过期，到 Actions 重跑 package 后再运行 prepare", x.HTMLURL, name)
		}
		a := res.Artifacts[i]
		sum, ok := strings.CutPrefix(a.Digest, "sha256:")
		if !ok {
			return fmt.Errorf("GitHub 没给出 %s 的 SHA-256 摘要", name)
		}
		if err := retry(ctx, r.log, "下载 "+name, func() error {
			return download(ctx, a.URL, gh.token, filepath.Join(dir, name+".zip"), a.Size, sum)
		}); err != nil {
			return err
		}
		r.log.Printf("已下载 %s，SHA-256 与 GitHub 记录一致", name)
	}
	return nil
}

// ---- GitHub Releases ----

type ghAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type ghRelease struct {
	ID         int64     `json:"id"`
	Tag        string    `json:"tag_name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	HTMLURL    string    `json:"html_url"`
	Assets     []ghAsset `json:"assets"`
}

func ghAssetOK(g ghAsset, a asset) bool {
	return g.Name == a.Name && g.State == "uploaded" && g.Size == a.Size && g.Digest == "sha256:"+a.SHA256
}

func checkGitHubAssets(got []ghAsset, want []asset) error {
	if len(got) != len(want) {
		return permanent("GitHub 上有 %d 个文件，应该是 %d 个", len(got), len(want))
	}
	for _, a := range want {
		ok := false
		for _, g := range got {
			ok = ok || ghAssetOK(g, a)
		}
		if !ok {
			return permanent("GitHub 上的 %s 缺少或摘要不对", a.Name)
		}
	}
	return nil
}

// publishGitHub 建草稿、逐个上传安装包（网络慢时一次传完会超时）并按 GitHub 算出的摘要核对，全对了再发布为最新版。
func (r *release) publishGitHub(ctx context.Context, gh *api, m manifest, notes string) error {
	var list []ghRelease
	if err := gh.call(ctx, "GET", "/releases?per_page=20", nil, &list); err != nil {
		return err
	}
	var rel ghRelease
	for _, x := range list {
		if x.Tag == r.tag {
			rel = x
		}
	}
	switch {
	case rel.ID == 0:
		if err := gh.call(ctx, "POST", "/releases", map[string]any{
			"tag_name": r.tag, "name": "Modbus AI Studio " + r.version, "body": notes,
			"draft": true, "prerelease": false, "target_commitish": m.Commit,
		}, &rel); err != nil {
			return err
		}
		r.log.Printf("已建 GitHub 草稿 %s", r.tag)
	case rel.Draft && rel.Body != notes:
		if err := gh.call(ctx, "PATCH", fmt.Sprintf("/releases/%d", rel.ID), map[string]any{"body": notes}, &rel); err != nil {
			return err
		}
	}
	if rel.Body != notes || rel.Prerelease {
		return permanent("GitHub 上 %s 的说明和本地不同或标成了预发布，到网页上处理：%s", r.tag, rel.HTMLURL)
	}
	for _, a := range m.Assets {
		var have *ghAsset
		for i := range rel.Assets {
			if rel.Assets[i].Name == a.Name {
				have = &rel.Assets[i]
			}
		}
		if have != nil && ghAssetOK(*have, a) {
			continue
		}
		if !rel.Draft {
			return permanent("GitHub 上 %s 已公开，但 %s 缺少或不对，到网页上处理：%s", r.tag, a.Name, rel.HTMLURL)
		}
		if have != nil {
			if err := gh.call(ctx, "DELETE", fmt.Sprintf("/releases/assets/%d", have.ID), nil, nil); err != nil {
				return err
			}
		}
		up, err := r.uploadGitHub(ctx, gh, rel.ID, a)
		if err != nil {
			return err
		}
		if !ghAssetOK(up, a) {
			return fmt.Errorf("上传后 GitHub 记录的 %s 摘要不对", a.Name)
		}
		r.log.Printf("已上传到 GitHub：%s", a.Name)
	}
	var fresh ghRelease
	if err := gh.call(ctx, "GET", fmt.Sprintf("/releases/%d", rel.ID), nil, &fresh); err != nil {
		return err
	}
	if err := checkGitHubAssets(fresh.Assets, m.Assets); err != nil {
		return err
	}
	if fresh.Draft {
		if err := gh.call(ctx, "PATCH", fmt.Sprintf("/releases/%d", rel.ID), map[string]any{"draft": false, "make_latest": "true"}, &fresh); err != nil {
			return err
		}
		r.log.Printf("GitHub 已发布：%s", fresh.HTMLURL)
	}
	return writeJSON(r.file("-github.json"), fresh)
}

func (r *release) uploadGitHub(ctx context.Context, gh *api, id int64, a asset) (ghAsset, error) {
	var up ghAsset
	f, err := os.Open(filepath.Join(r.dist, a.Name))
	if err != nil {
		return up, err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	u := fmt.Sprintf("https://uploads.github.com/repos/%s/releases/%d/assets?name=%s", update.Repo, id, url.QueryEscape(a.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, f)
	if err != nil {
		return up, err
	}
	req.ContentLength = a.Size
	req.Header.Set("Content-Type", "application/octet-stream")
	return up, gh.send(req, &up)
}

func (r *release) verifyGitHub(ctx context.Context, gh *api, m manifest, notes string) (ghRelease, error) {
	var rel ghRelease
	if err := gh.call(ctx, "GET", "/releases/latest", nil, &rel); err != nil {
		return rel, err
	}
	if rel.Tag != r.tag || rel.Draft || rel.Prerelease {
		return rel, fmt.Errorf("GitHub 的最新正式版是 %s，不是 %s", rel.Tag, r.tag)
	}
	if rel.Body != notes {
		return rel, permanent("GitHub 上的说明和 %s 不同", r.file(".md"))
	}
	return rel, checkGitHubAssets(rel.Assets, m.Assets)
}

// ---- Gitee ----

type giteeRelease struct {
	ID         int64  `json:"id"`
	Tag        string `json:"tag_name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
}

type giteeFile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

// pushGitee 把打包的提交推到 Gitee 的 main，并推 tag。用 https + 私人令牌：
// git 问用户名和密码时由本程序回答（askpass），令牌不进命令行；清空 credential.helper，不让凭据管理器插手。
func (r *release) pushGitee(ctx context.Context, commit string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_, err = r.git(ctx, []string{"GIT_ASKPASS=" + exe, askpassEnv + "=1"},
		"-c", "credential.helper=", "push", giteeGit, commit+":refs/heads/main", "refs/tags/"+r.tag)
	return err
}

// askpass 回答 git 推送 Gitee 时的提问：用户名是仓库所有者，密码是 Gitee 私人令牌。
func askpass() {
	prompt := strings.Join(os.Args[1:], " ")
	switch {
	case strings.HasPrefix(prompt, "Username"):
		owner, _, _ := strings.Cut(update.GiteeRepo, "/")
		fmt.Println(owner)
	case strings.HasPrefix(prompt, "Password"):
		token, err := giteeToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(token)
	default:
		os.Exit(1)
	}
}

func giteeFiles(ctx context.Context, gt *api, id int64) ([]giteeFile, error) {
	var files []giteeFile
	return files, gt.call(ctx, "GET", fmt.Sprintf("/releases/%d/attach_files?per_page=100", id), nil, &files)
}

// checkGiteeFiles 确认每个安装包各有一份且大小对。Gitee 自动生成的源码包不算。
func checkGiteeFiles(files []giteeFile, want []asset) error {
	for _, a := range want {
		n := 0
		for _, f := range files {
			if f.Name == a.Name && f.Size == a.Size {
				n++
			}
		}
		if n != 1 {
			return permanent("Gitee 上的 %s 缺少、重复或大小不对", a.Name)
		}
	}
	return nil
}

// publishGitee 建发行版（Gitee 没有草稿，建了就公开），逐个上传安装包并核对大小。
func (r *release) publishGitee(ctx context.Context, gt *api, m manifest, notes string) error {
	var rel giteeRelease
	if err := gt.call(ctx, "GET", "/releases/tags/"+r.tag, nil, &rel); err != nil && !isStatus(err, 404) {
		return err
	}
	body := map[string]any{"tag_name": r.tag, "name": "Modbus AI Studio " + r.version, "body": notes, "prerelease": false}
	switch {
	case rel.ID == 0:
		body["target_commitish"] = m.Commit
		if err := gt.call(ctx, "POST", "/releases", body, &rel); err != nil {
			return err
		}
		r.log.Printf("已建 Gitee 发行版 %s", r.tag)
	case rel.Body != notes:
		if err := gt.call(ctx, "PATCH", fmt.Sprintf("/releases/%d", rel.ID), body, &rel); err != nil {
			return err
		}
	}
	files, err := giteeFiles(ctx, gt, rel.ID)
	if err != nil {
		return err
	}
	for _, a := range m.Assets {
		var have *giteeFile
		for i := range files {
			if files[i].Name == a.Name {
				have = &files[i]
			}
		}
		if have != nil && have.Size == a.Size {
			continue
		}
		if have != nil {
			if err := gt.call(ctx, "DELETE", fmt.Sprintf("/releases/%d/attach_files/%d", rel.ID, have.ID), nil, nil); err != nil {
				return err
			}
		}
		if err := r.uploadGitee(ctx, gt, rel.ID, a); err != nil {
			return err
		}
		r.log.Printf("已上传到 Gitee：%s", a.Name)
	}
	if files, err = giteeFiles(ctx, gt, rel.ID); err != nil {
		return err
	}
	if err := checkGiteeFiles(files, m.Assets); err != nil {
		return err
	}
	return writeJSON(r.file("-gitee.json"), map[string]any{"release": rel, "files": files})
}

func (r *release) uploadGitee(ctx context.Context, gt *api, id int64, a asset) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", a.Name)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(r.dist, a.Name))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, f)
	f.Close()
	if err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/releases/%d/attach_files", gt.base, id), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return gt.send(req, nil)
}

func (r *release) verifyGitee(ctx context.Context, gt *api, m manifest, notes string) ([]giteeFile, error) {
	var rel giteeRelease
	if err := gt.call(ctx, "GET", "/releases/latest", nil, &rel); err != nil {
		return nil, err
	}
	if rel.Tag != r.tag || rel.Prerelease {
		return nil, fmt.Errorf("Gitee 的最新正式版是 %s，不是 %s", rel.Tag, r.tag)
	}
	if rel.Body != notes {
		return nil, permanent("Gitee 上的说明和 %s 不同", r.file(".md"))
	}
	files, err := giteeFiles(ctx, gt, rel.ID)
	if err != nil {
		return nil, err
	}
	return files, checkGiteeFiles(files, m.Assets)
}

// downloadGitee 不带令牌，像用户一样从 Gitee 下载全部安装包，核对 SHA-256。Gitee 不提供摘要，只能下载来比。
func (r *release) downloadGitee(ctx context.Context, files []giteeFile, assets []asset, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(assets))
	for i, a := range assets {
		var src string
		for _, f := range files {
			if f.Name == a.Name {
				src = f.URL
			}
		}
		if u, err := url.Parse(src); err != nil || u.Scheme != "https" || u.Hostname() != "gitee.com" {
			return fmt.Errorf("Gitee 上 %s 的下载地址不对：%q", a.Name, src)
		}
		wg.Go(func() {
			errs[i] = retry(ctx, r.log, "从 Gitee 下载 "+a.Name, func() error {
				return download(ctx, src, "", filepath.Join(dir, a.Name), a.Size, a.SHA256)
			})
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
