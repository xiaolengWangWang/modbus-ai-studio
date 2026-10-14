//go:build windows

package platform

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"modbus-ai-studio/internal/edition"
	"modbus-ai-studio/internal/update"
)

// Setup 启动后最先调用，要在任何地方读配置目录之前：
//   - 兼容版：程序目录能写时，数据和日志放在 exe 旁边的 data 文件夹（绿色便携，%AppData% 不能写也能用）；
//   - exe 旁边有 Mesa 的 opengl32.dll 时（兼容版自带，也可能是用户自己放的）固定用 llvmpipe 软件渲染：
//     新版 Mesa 默认先试 d3d12 驱动，缺 dxil.dll 时程序直接退出；
//   - Windows 版没有控制台窗口，日志写到 %AppData%\ModbusAIStudio\app.log（兼容版在 data\ModbusAIStudio\app.log）。
//     界面因为 OpenGL 起不来时 Fyne 只写一行日志就退出，用户看到的是“双击没反应”，所以截住这行日志，先弹窗说明怎么处理。
func Setup() {
	var notes []string // 日志打开前的情况，打开后再写进去
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if edition.Compat && exeDir != "" {
		if err := portable(filepath.Join(exeDir, "data")); err != nil {
			notes = append(notes, fmt.Sprintf("startup data: 程序目录不能写（%v），数据仍放在 %%AppData%%", err))
		}
	}
	if exeDir != "" && fileExists(filepath.Join(exeDir, "opengl32.dll")) {
		mesa = true
		if os.Getenv("GALLIUM_DRIVER") == "" {
			setenv("GALLIUM_DRIVER", "llvmpipe")
		}
		notes = append(notes, "startup render: Mesa "+os.Getenv("GALLIUM_DRIVER"))
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "ModbusAIStudio")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	logPath = filepath.Join(dir, "app.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(glWatch{f})
	log.SetPrefix(fmt.Sprintf("[pid %d] ", os.Getpid()))
	for _, n := range notes {
		log.Print(n)
	}
}

var (
	logPath = `%AppData%\ModbusAIStudio\app.log` // Setup 打开日志后换成实际路径
	mesa    bool                                 // 界面用 Mesa 软件渲染
)

// portable 把本进程的配置目录改到 dir（APPDATA）、缓存目录改到 dir\cache（LOCALAPPDATA）：
// os.UserConfigDir、os.UserCacheDir 和 Fyne 的偏好设置都跟着改。dir 不能写时什么都不改。
func portable(dir string) error {
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil { // Fyne 只建最后一级缓存目录
		return err
	}
	probe, err := os.CreateTemp(dir, "write-test-*")
	if err != nil {
		return err
	}
	probe.Close()
	os.Remove(probe.Name())
	if err := setenv("APPDATA", dir); err != nil {
		return err
	}
	return setenv("LOCALAPPDATA", cache)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

var systemParametersInfo = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")

// WorkArea 返回任务栏以外的主屏幕可用像素；无法取得时由调用方使用默认尺寸。
func WorkArea() (int, int) {
	var rect windows.Rect
	r, _, _ := systemParametersInfo.Call(0x30, 0, uintptr(unsafe.Pointer(&rect)), 0) // SPI_GETWORKAREA
	if r == 0 {
		return 0, 0
	}
	return int(rect.Right - rect.Left), int(rect.Bottom - rect.Top)
}

type glWatch struct{ io.Writer }

var glWarned atomic.Bool

func (w glWatch) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if glFailed(p) && glWarned.CompareAndSwap(false, true) {
		if mesa {
			messageBox(glHelpMesa(logPath), windows.MB_OK|windows.MB_ICONERROR)
		} else if messageBox(glHelp(logPath, update.PageURL()), windows.MB_YESNO|windows.MB_ICONERROR) == idYes {
			OpenFile(update.PageURL())
		}
	}
	return n, err
}

const idYes = 6 // MessageBox 返回“是”；x/sys/windows 没有这个常量

func messageBox(text string, style uint32) int32 {
	t, _ := windows.UTF16PtrFromString(text)
	title, _ := windows.UTF16PtrFromString("Modbus AI Studio")
	r, _ := windows.MessageBox(0, t, title, style)
	return r
}
