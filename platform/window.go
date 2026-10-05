package platform

import "fyne.io/fyne/v2"

// FitSize 把期望的 Fyne 窗口尺寸限制在可用屏幕像素内，留出窗口边框和拖动空间。
func FitSize(want fyne.Size, workWidth, workHeight int, scale float32) fyne.Size {
	if workWidth <= 0 || workHeight <= 0 || scale <= 0 {
		return want
	}
	width := float32(workWidth-48) / scale
	height := float32(workHeight-72) / scale
	if width < want.Width {
		want.Width = width
	}
	if height < want.Height {
		want.Height = height
	}
	return want
}
