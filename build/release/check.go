package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"modbus-ai-studio/internal/update"
)

// manifest 记录 prepare 核对过的安装包，publish 和 verify 只认这里的提交、大小和 SHA-256。
type manifest struct {
	Version string  `json:"version"`
	Commit  string  `json:"commit"`
	Run     string  `json:"package_run"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// installers 是桌面版三个安装包的文件名。结尾不能改：程序检查更新时按结尾找本机的安装包。
func installers(version string) []string {
	p := "ModbusAIStudio-" + version
	return []string{p + "-Windows-x64.zip", p + "-macOS-Intel.dmg", p + "-macOS-AppleSilicon.dmg"}
}

// webPackages 是 Linux Web 版的两个包。检查更新不认它们（按结尾找不到），只随发布一起上传。
func webPackages(version string) []string {
	p := "ModbusAIStudio-Web-" + version + "-Linux-"
	return []string{p + "x64.tar.gz", p + "arm64.tar.gz"}
}

// packages 是一次发布的全部文件：桌面版安装包和 Linux Web 版的包。
func packages(version string) []string { return append(installers(version), webPackages(version)...) }

// summary 读发布说明正文，第一行必须是这个版本的标题，免得用错上一版的说明。
func (r *release) summary() (string, error) {
	path := r.file("-summary.md")
	b, err := readText(path)
	if err != nil {
		return "", fmt.Errorf("先写发布说明 %s：%w", path, err)
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
	title, _, _ := strings.Cut(text, "\n")
	if want := "# Modbus AI Studio " + r.version; title != want {
		return "", fmt.Errorf("%s 第一行应该是“%s”", path, want)
	}
	if strings.Contains(text, "## SHA-256") {
		return "", fmt.Errorf("%s 里不要写 SHA-256 段落，prepare 会按核对过的安装包生成", path)
	}
	return text, nil
}

// withChecksums 在说明末尾加 SHA-256 段落，每行“校验值  文件名”：自动更新按它校验下载的安装包。
func withChecksums(summary string, assets []asset) string {
	var b strings.Builder
	b.WriteString(summary + "\n\n## SHA-256\n\n")
	for _, a := range assets {
		fmt.Fprintf(&b, "%s  %s\n", a.SHA256, a.Name)
	}
	return b.String()
}

// localView 是发布后程序检查更新时会看到的样子，发布前用它确认说明和文件名能被认出来。
func localView(notes string, assets []asset) update.Release {
	rel := update.Release{Body: notes}
	for _, a := range assets {
		rel.Assets = append(rel.Assets, update.Asset{Name: a.Name, URL: "https://example.invalid/" + a.Name})
	}
	return rel
}

// checkUpdaterView 用检查更新的代码在发布里找桌面版三个平台的安装包和校验值，与核对过的安装包比较。
// mirrors 为 true 时还要求另一个下载源上也有同一个文件。
func checkUpdaterView(rel update.Release, assets []asset, mirrors bool) error {
	for _, p := range [][2]string{{"windows", "amd64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		a, ok := rel.Asset(p[0], p[1])
		if !ok || a.URL == "" {
			return fmt.Errorf("检查更新找不到 %s/%s 的安装包", p[0], p[1])
		}
		i := slices.IndexFunc(assets, func(x asset) bool { return x.Name == a.Name })
		sum, _ := rel.Checksum(a.Name)
		if i < 0 || sum != assets[i].SHA256 {
			return fmt.Errorf("检查更新取到的 %s 和核对过的安装包对不上", a.Name)
		}
		if mirrors && len(a.Mirrors) == 0 {
			return fmt.Errorf("%s 只在 %s 上有，另一个下载源没查到同一版本", a.Name, rel.Source)
		}
	}
	return nil
}

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// lowerHex 判断摘要是否是 prepare 写出的定长、小写十六进制。
func lowerHex(s string, length int) bool {
	if len(s) != length || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// load 在任何远程操作前核对 prepare 的完整记录、安装包和可编辑的发布说明。
func (r *release) load() (manifest, string, error) {
	var m manifest
	if !releaseVersion.MatchString(r.version) {
		return m, "", fmt.Errorf("版本号 %q 不是 x.y.z", r.version)
	}
	b, err := os.ReadFile(r.file("-assets.json"))
	if err != nil {
		return m, "", fmt.Errorf("先运行 go run ./build/release prepare：%w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, "", err
	}
	names := packages(r.version)
	if m.Version != r.version || !lowerHex(m.Commit, 40) || len(m.Assets) != len(names) {
		return m, "", fmt.Errorf("%s 不是 %s 的完整记录，重新运行 prepare", r.file("-assets.json"), r.version)
	}
	seen := make(map[string]bool, len(names))
	for _, a := range m.Assets {
		// 只认预期的完整文件名，拒绝重复记录和任何目录、绝对路径或其他版本。
		if !slices.Contains(names, a.Name) || seen[a.Name] {
			return m, "", fmt.Errorf("安装包记录里的文件名 %q 不属于本版本或重复，重新运行 prepare", a.Name)
		}
		if a.Size <= 0 || !lowerHex(a.SHA256, 64) {
			return m, "", fmt.Errorf("%s 的大小或 SHA-256 记录无效，重新运行 prepare", a.Name)
		}
		seen[a.Name] = true
	}
	notes, err := readText(r.file(".md"))
	if err != nil {
		return m, "", err
	}
	title, _, _ := strings.Cut(strings.ReplaceAll(string(notes), "\r\n", "\n"), "\n")
	if want := "# Modbus AI Studio " + r.version; title != want {
		return m, "", fmt.Errorf("%s 第一行应该是“%s”", r.file(".md"), want)
	}
	if err := checkUpdaterView(localView(string(notes), m.Assets), m.Assets, false); err != nil {
		return m, "", fmt.Errorf("%s 的安装包校验段无效：%w", r.file(".md"), err)
	}
	for _, a := range m.Assets {
		if !fileMatches(filepath.Join(r.dist, a.Name), a.Size, a.SHA256) {
			return m, "", fmt.Errorf("dist/%s 和 prepare 核对时不一样，重新运行 prepare", a.Name)
		}
	}
	return m, string(notes), nil
}

// checkInstallers 从 CI 产物里取出全部安装包放到 dist/，逐个核对，返回大小和 SHA-256。
func (r *release) checkInstallers(ci string) ([]asset, error) {
	names := packages(r.version)
	if err := extractInstallers(ci, r.dist, names); err != nil {
		return nil, err
	}
	var assets []asset
	for _, name := range names {
		pkg := filepath.Join(r.dist, name)
		var err error
		switch {
		case strings.HasSuffix(name, ".dmg"):
			err = checkDMG(pkg)
		case strings.HasSuffix(name, ".tar.gz"):
			err = checkLinuxWeb(pkg, r.version)
		default:
			err = r.checkWindows(pkg, ci)
		}
		if err != nil {
			return nil, fmt.Errorf("%s：%w", name, err)
		}
		fi, err := os.Stat(pkg)
		if err != nil {
			return nil, err
		}
		sum, err := sha256File(pkg)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset{name, fi.Size(), sum})
		r.log.Printf("核对通过 %s（%d 字节）", name, fi.Size())
	}
	return assets, nil
}

// extractInstallers 从 dir 里的 CI 产物 zip 中找出安装包解压到 dst，每个安装包必须正好一份。
func extractInstallers(dir, dst string, names []string) error {
	zips, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		return err
	}
	found := map[string]int{}
	for _, z := range zips {
		zr, err := zip.OpenReader(z)
		if err != nil {
			return err
		}
		for _, f := range zr.File {
			name := path.Base(f.Name)
			if !slices.Contains(names, name) {
				continue
			}
			found[name]++
			if err := extractFile(f, filepath.Join(dst, name)); err != nil {
				zr.Close()
				return err
			}
		}
		zr.Close()
	}
	for _, name := range names {
		if found[name] != 1 {
			return fmt.Errorf("CI 产物里 %s 有 %d 个", name, found[name])
		}
	}
	return nil
}

func extractFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// checkDMG 看 DMG 结尾的 koly 块：文件不完整或不是磁盘映像时没有。
func checkDMG(pkg string) error {
	f, err := os.Open(pkg)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < 1<<20 {
		return errors.New("文件太小")
	}
	magic := make([]byte, 4)
	if _, err := f.ReadAt(magic, fi.Size()-512); err != nil {
		return err
	}
	if string(magic) != "koly" {
		return errors.New("结尾没有 koly 块，不是完整的 DMG")
	}
	return nil
}

// checkLinuxWeb 核对 Linux Web 版的 tar.gz：顶层目录下只有可执行的 modbus-web 和明文 README.txt，
// modbus-web 是对应架构（x64 / ARM64）的 Linux 程序，说明里有这一版的版本号。
func checkLinuxWeb(pkg, version string) error {
	f, err := os.Open(pkg)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	top := strings.TrimSuffix(filepath.Base(pkg), ".tar.gz") + "/"
	machine := elf.EM_X86_64
	if strings.HasSuffix(top, "-arm64/") {
		machine = elf.EM_AARCH64
	}
	seen := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		switch h.Name {
		case top + "modbus-web":
			if h.Typeflag != tar.TypeReg || h.Mode&0o111 == 0 {
				return errors.New("modbus-web 不是可执行的普通文件，解压后运行不了")
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			ef, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				return fmt.Errorf("modbus-web 不是 Linux 程序：%w", err)
			}
			if ef.Machine != machine {
				return fmt.Errorf("modbus-web 是 %s 的程序，应为 %s", ef.Machine, machine)
			}
		case top + "README.txt":
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			switch {
			case !utf8.Valid(b) || encrypted(b) || bytes.IndexByte(b, 0) >= 0:
				return errors.New("README.txt 不是可读的 UTF-8 文本（被加密软件加密了？）")
			case !bytes.Contains(b, []byte(version)):
				return fmt.Errorf("README.txt 里没有版本号 %s", version)
			}
		default:
			return fmt.Errorf("多了 %s，包里只该有 modbus-web 和 README.txt", h.Name)
		}
		seen[h.Name] = true
	}
	for _, name := range []string{"modbus-web", "README.txt"} {
		if !seen[top+name] {
			return fmt.Errorf("缺少 %s", top+name)
		}
	}
	_, err = io.Copy(io.Discard, zr) // 读到结尾，gzip 才会核对校验和
	return err
}

// checkWindows 核对 Windows zip 的内容和 exe 的资源，再解压一份到 dist/ 下同名目录，方便直接试用。
func (r *release) checkWindows(pkg, work string) error {
	dir := filepath.Join(work, "windows")
	if err := unpackWindows(pkg, r.version, dir); err != nil {
		return err
	}
	if err := r.checkResources(filepath.Join(dir, "ModbusAIStudio.exe")); err != nil {
		return err
	}
	if err := unpackWindows(pkg, r.version, strings.TrimSuffix(pkg, ".zip")); err != nil {
		r.log.Printf("没能解压到 %s（程序正开着？），不影响发版：%v", strings.TrimSuffix(pkg, ".zip"), err)
	}
	return nil
}

// unpackWindows 把 Windows zip 解压到 dir。包里只能有顶层目录下的 exe 和 README.txt，路径用 /（自动更新按这个解压），
// README 要能在记事本里正常显示。
func unpackWindows(pkg, version, dir string) error {
	zr, err := zip.OpenReader(pkg)
	if err != nil {
		return err
	}
	defer zr.Close()
	top := strings.TrimSuffix(filepath.Base(pkg), ".zip") + "/"
	want := map[string]bool{top + "ModbusAIStudio.exe": false, top + "README.txt": false}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if _, ok := want[f.Name]; !ok {
			return fmt.Errorf("多了 %s，包里只该有 ModbusAIStudio.exe 和 README.txt", f.Name)
		}
		want[f.Name] = true
		if path.Base(f.Name) == "README.txt" {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			if err := checkReadme(b, version); err != nil {
				return err
			}
		}
		if err := extractFile(f, filepath.Join(dir, path.Base(f.Name))); err != nil {
			return err
		}
	}
	for name, ok := range want {
		if !ok {
			return fmt.Errorf("缺少 %s", name)
		}
	}
	return nil
}

var loneLF = regexp.MustCompile(`(^|[^\r])\n`)

// checkReadme 核对 README.txt：UTF-8 BOM + CRLF（记事本打开不乱码），模板已展开，没有被加密软件加密。
func checkReadme(b []byte, version string) error {
	text, ok := bytes.CutPrefix(b, []byte("\xef\xbb\xbf"))
	switch {
	case !ok:
		return errors.New("README.txt 没有 UTF-8 BOM，记事本会乱码")
	case !utf8.Valid(text) || encrypted(text) || bytes.IndexByte(text, 0) >= 0:
		return errors.New("README.txt 不是可读的 UTF-8 文本（模板被加密了？）")
	case bytes.Contains(text, []byte("{{VERSION}}")):
		return errors.New("README.txt 里的版本号没替换")
	case loneLF.Match(text):
		return errors.New("README.txt 不是 CRLF 换行")
	case !hasChangelog(text, version):
		return fmt.Errorf("README.txt 里没有 %s 的更新记录", version)
	}
	for _, s := range []string{"50 MB", "托盘", "采集"} {
		if !bytes.Contains(text, []byte(s)) {
			return fmt.Errorf("README.txt 里没有“%s”", s)
		}
	}
	return nil
}

// hasChangelog 判断说明里有没有这一版的更新记录，即“### x.y.z …”标题。
func hasChangelog(text []byte, version string) bool {
	return regexp.MustCompile(`(?m)^### ` + regexp.QuoteMeta(version) + `(\s|$)`).Match(text)
}

// checkResources 用 go-winres 取出 exe 的资源，核对版本号、GLFW_ICON 图标和高 DPI 清单。
func (r *release) checkResources(exe string) error {
	dir, err := os.MkdirTemp("", "release-winres-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	args := []string{"extract", "--dir", dir, exe}
	cmd := exec.Command("go", append([]string{"run", "github.com/tc-hib/go-winres@v0.3.3"}, args...)...)
	if p := os.Getenv("GO_WINRES"); p != "" {
		cmd = exec.Command(p, args...)
	} else if p := filepath.Join(r.root, "bin", "go-winres.exe"); runtime.GOOS == "windows" && fileExists(p) {
		cmd = exec.Command(p, args...)
	}
	cmd.Dir = r.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go-winres extract：%v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "winres.json"))
	if err != nil {
		return err
	}
	return checkWinres(b, r.version)
}

func checkWinres(b []byte, version string) error {
	var res struct {
		Icon     map[string]json.RawMessage `json:"RT_GROUP_ICON"`
		Manifest map[string]map[string]struct {
			DPI string `json:"dpi-awareness"`
		} `json:"RT_MANIFEST"`
		Version map[string]map[string]struct {
			Fixed struct {
				File    string `json:"file_version"`
				Product string `json:"product_version"`
			} `json:"fixed"`
		} `json:"RT_VERSION"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	if res.Icon["GLFW_ICON"] == nil {
		return errors.New("exe 里没有 GLFW_ICON 图标")
	}
	dpi, ver := false, false
	for _, langs := range res.Manifest {
		for _, m := range langs {
			dpi = dpi || m.DPI == "per monitor v2"
		}
	}
	for _, langs := range res.Version {
		for _, v := range langs {
			ver = ver || v.Fixed.File == version+".0" && v.Fixed.Product == version+".0"
		}
	}
	if !dpi {
		return errors.New("exe 的清单不是 per monitor v2 高 DPI")
	}
	if !ver {
		return fmt.Errorf("exe 的文件版本不是 %s", version)
	}
	return nil
}

func sha256File(path string) (string, error) {
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

// fileMatches 判断 path 的大小和 SHA-256 是否与预期一致。
func fileMatches(path string, size int64, sum string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != size {
		return false
	}
	got, err := sha256File(path)
	return err == nil && got == sum
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
