package update

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	oldSuffix = ".old"           // 被替换下来的旧文件，下次启动时删除
	stageDir  = ".update-staged" // 解压新文件的临时目录，与程序在同一个磁盘上，改名才不会跨盘
	mainExe   = "ModbusAIStudio.exe"
)

// installZip 用 Windows 绿色版 zip 替换 dir 里的程序文件。zip 里是一个顶层目录
// ModbusAIStudio-x.y.z-Windows-x64/，下面是 exe 和说明。Windows 不能覆盖正在运行的 exe，
// 但可以改名：旧文件改名为 *.old，新文件放到原位置，任何一步失败都把已改名的文件改回来。
func installZip(pkg, dir string) (string, error) {
	z, err := zip.OpenReader(pkg)
	if err != nil {
		return "", fmt.Errorf("安装包不是 zip：%w", err)
	}
	defer z.Close()

	stage := filepath.Join(dir, stageDir)
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", fmt.Errorf("程序目录不能写入（%w），请手动下载解压", err)
	}
	defer os.RemoveAll(stage)

	var files []string // 相对 dir 的路径
	for _, f := range z.File {
		name := f.Name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:] // 去掉顶层目录
		}
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		clean := path.Clean(name)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
			return "", fmt.Errorf("安装包里的路径不安全：%s", f.Name)
		}
		dst := filepath.Join(stage, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := extract(f, dst); err != nil {
			return "", err
		}
		files = append(files, filepath.FromSlash(clean))
	}
	found := false
	for _, f := range files {
		if strings.EqualFold(f, mainExe) {
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("安装包里没有 %s", mainExe)
	}

	type moved struct{ target, old string }
	var done []moved
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			m := done[i]
			if m.old != "" {
				os.Remove(m.target)
				os.Rename(m.old, m.target)
			} else {
				os.Remove(m.target)
			}
		}
	}
	for _, rel := range files {
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			rollback()
			return "", err
		}
		m := moved{target: target}
		if _, err := os.Stat(target); err == nil {
			m.old = target + oldSuffix
			os.Remove(m.old) // 上次更新留下、还没删掉的
			if err := os.Rename(target, m.old); err != nil {
				rollback()
				return "", fmt.Errorf("不能替换 %s（%w），请关掉正在运行的 modbus-sim、modbus-cli 后再试，或手动下载", rel, err)
			}
		}
		done = append(done, m)
		if err := os.Rename(filepath.Join(stage, rel), target); err != nil {
			rollback()
			return "", fmt.Errorf("不能写入 %s：%w", rel, err)
		}
	}
	return filepath.Join(dir, mainExe), nil
}

func extract(f *zip.File, dst string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// cleanupDir 删除 dir 里上次更新换下来的 *.old 文件和残留的解压目录。
func cleanupDir(dir string) {
	os.RemoveAll(filepath.Join(dir, stageDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), oldSuffix) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Restart 启动新安装的程序。调用方随后退出当前进程。
func Restart(target string) error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command(target)
		cmd.Dir = filepath.Dir(target)
		return cmd.Start()
	case "darwin":
		return exec.Command("open", "-n", target).Start()
	}
	return errors.ErrUnsupported
}
