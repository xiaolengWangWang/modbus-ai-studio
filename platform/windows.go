//go:build windows

package platform

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Setup 在程序启动最早调用。Windows 版没有控制台窗口，日志写到 %AppData%\ModbusAIStudio\app.log；
// 界面因为 OpenGL 起不来时 Fyne 只写一行日志就退出，用户看到的是“双击没反应”，
// 所以截住这行日志，先弹窗说明怎么处理。
func Setup() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "ModbusAIStudio")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(glWatch{f})
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
		text, _ := windows.UTF16PtrFromString(glHelp)
		title, _ := windows.UTF16PtrFromString("Modbus AI Studio")
		windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
	}
	return n, err
}
