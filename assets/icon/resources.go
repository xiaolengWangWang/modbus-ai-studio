// Package icon embeds the application artwork so unpackaged builds and every
// window use the same icon as the installers.
package icon

import (
	_ "embed"
	"runtime"

	"fyne.io/fyne/v2"
)

//go:embed AppIcon.png
var appPNG []byte

//go:embed windows/icon256.png
var windowsPNG []byte

//go:embed windows/icon32.png
var trayPNG []byte

var (
	application = fyne.NewStaticResource("AppIcon.png", appPNG)
	windowsIcon = fyne.NewStaticResource("WindowsIcon.png", windowsPNG)
	trayIcon    = fyne.NewStaticResource("TrayIcon.png", trayPNG)
)

func Application() fyne.Resource {
	if runtime.GOOS == "windows" {
		return windowsIcon
	}
	return application
}

// Tray uses the simplified artwork drawn specifically for small sizes.
func Tray() fyne.Resource { return trayIcon }
