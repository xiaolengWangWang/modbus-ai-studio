//go:build darwin

package update

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// installDMG 挂载 DMG，把里面的 .app 复制到 app 旁边，再改名换掉 app。换下来的旧版本改名为 .app.old，
// 下次启动时删除（正在运行的程序还在用它）。
func installDMG(pkg, app string) error {
	mount, err := os.MkdirTemp("", "modbus-ai-update-")
	if err != nil {
		return err
	}
	defer os.Remove(mount)
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mount, pkg).CombinedOutput(); err != nil {
		return fmt.Errorf("挂载安装包失败：%s", bytes.TrimSpace(out))
	}
	defer exec.Command("hdiutil", "detach", mount, "-force").Run()

	apps, _ := filepath.Glob(filepath.Join(mount, "*.app"))
	if len(apps) != 1 {
		return fmt.Errorf("安装包里应有一个 .app，找到 %d 个", len(apps))
	}
	staged := app + ".new"
	os.RemoveAll(staged)
	if out, err := exec.Command("ditto", apps[0], staged).CombinedOutput(); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("复制新版本失败（%s），可能没有写入 %s 的权限，请手动安装", bytes.TrimSpace(out), filepath.Dir(app))
	}
	exec.Command("xattr", "-dr", "com.apple.quarantine", staged).Run() // 自己下载的文件本来就没有，保险起见

	old := app + oldSuffix
	os.RemoveAll(old)
	if err := os.Rename(app, old); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("不能替换 %s：%w", app, err)
	}
	if err := os.Rename(staged, app); err != nil {
		os.Rename(old, app)
		os.RemoveAll(staged)
		return fmt.Errorf("不能替换 %s：%w", app, err)
	}
	return nil
}
