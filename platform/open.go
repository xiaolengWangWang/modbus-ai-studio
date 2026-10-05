package platform

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrNoApp 表示系统里没有能打开这类文件的程序（macOS 的 open 会这样报错）。
var ErrNoApp = errors.New("没有能打开这个文件的程序")

// OpenFile 用系统默认程序打开文件，例如用 DB Browser for SQLite 打开报文数据库。
func OpenFile(path string) error {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("open", path).CombinedOutput(); err != nil {
			if bytes.Contains(out, []byte("No application")) {
				return ErrNoApp
			}
			return errors.New(string(bytes.TrimSpace(out)))
		}
		return nil
	case "windows":
		// 没有关联程序时 Windows 会自己弹出“选择打开方式”
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	}
	return exec.Command("xdg-open", path).Start()
}

// ShowInFolder 在访达 / 资源管理器里显示文件。
func ShowInFolder(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Run()
	case "windows":
		return exec.Command("explorer", "/select,"+path).Start() // explorer 成功时退出码也不是 0，不等它
	}
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
