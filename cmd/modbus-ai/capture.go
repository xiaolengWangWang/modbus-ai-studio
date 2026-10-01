//go:build capture

package main

import (
	"fmt"
	"image/png"
	"os"
	"time"

	"fyne.io/fyne/v2"
)

// 调试构建（go build -tags capture）：启动后第 2 s 和第 8 s 把每个窗口的真实渲染画面存到 CAPTURE_DIR 然后退出。
// 终端没有屏幕录制权限时，用它检查 OpenGL 驱动下的界面。
func init() {
	go func() {
		for i, d := range []int{2, 6} {
			time.Sleep(time.Duration(d) * time.Second)
			done := make(chan struct{})
			fyne.Do(func() {
				defer close(done)
				for j, w := range fyne.CurrentApp().Driver().AllWindows() {
					img := w.Canvas().Capture()
					f, err := os.Create(fmt.Sprintf("%s/cap-%d-%d.png", os.Getenv("CAPTURE_DIR"), i, j))
					if err != nil {
						fmt.Println(err)
						continue
					}
					_ = png.Encode(f, img)
					f.Close()
					fmt.Println("captured", i, j, w.Title(), img.Bounds(), w.Canvas().Size(), w.Canvas().Content().Size())
				}
			})
			<-done
		}
		os.Exit(0)
	}()
}
