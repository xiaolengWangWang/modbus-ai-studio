//go:build windows

package update

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Windows 上替换正在运行的程序：运行中的 exe 删不掉、也不能覆盖，但可以改名。用系统的 ping.exe 冒充
// ModbusAIStudio.exe 跑着，安装新版本后原位置是新文件，旧文件改名为 .old；进程退出后才能清理。
func TestInstallZipReplacesRunningExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, mainExe)
	src, err := os.Open(filepath.Join(os.Getenv("WINDIR"), "System32", "PING.EXE"))
	if err != nil {
		t.Skip("找不到 ping.exe")
	}
	dst, _ := os.Create(exe)
	io.Copy(dst, src)
	src.Close()
	dst.Close()
	cmd := exec.Command(exe, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(500 * time.Millisecond)
	if err := os.Remove(exe); err == nil {
		t.Fatal("运行中的 exe 应删不掉，测试前提不成立")
	}

	pkg := filepath.Join(t.TempDir(), "p.zip")
	writeZip(t, pkg, map[string]string{"ModbusAIStudio-9.9.9-Windows-x64/ModbusAIStudio.exe": "new exe"})
	got, err := installZip(pkg, dir)
	if err != nil {
		t.Fatalf("程序运行中也应能替换：%v", err)
	}
	if got != exe || readFile(t, exe) != "new exe" {
		t.Errorf("原位置应是新文件：%s", got)
	}
	if _, err := os.Stat(exe + oldSuffix); err != nil {
		t.Errorf("旧程序应改名为 .old：%v", err)
	}

	cmd.Process.Kill()
	cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	cleanupDir(dir)
	if _, err := os.Stat(exe + oldSuffix); !os.IsNotExist(err) {
		t.Errorf("旧程序退出后应能清理掉 .old：%v", err)
	}
}
