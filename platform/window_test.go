package platform

import (
	"testing"

	"fyne.io/fyne/v2"
)

func TestFitSize(t *testing.T) {
	want := fyne.NewSize(1280, 820)
	got := FitSize(want, 1920, 1035, 1.5)
	if got.Width > 1248 || got.Height > 642 || got.Width < 1200 || got.Height < 600 {
		t.Fatalf("1080p 150%% 缩放，窗口应留在工作区内：%v", got)
	}
	if got = FitSize(want, 2560, 1440, 1); got != want {
		t.Fatalf("大屏幕应保持默认大小：%v", got)
	}
	if got = FitSize(want, 0, 0, 0); got != want {
		t.Fatalf("无法查询屏幕时应使用默认大小：%v", got)
	}
}

func TestFitSizeTinyWorkArea(t *testing.T) {
	got := FitSize(fyne.NewSize(1280, 780), 40, 60, 2)
	if got.Width <= 0 || got.Height <= 0 {
		t.Fatalf("极小可用区域也不能得到负窗口尺寸：%v", got)
	}
}
