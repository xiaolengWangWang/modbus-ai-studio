//go:build capture

package ui

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

// 调试构建（go build -tags capture）专用：按顺序操作界面，用真实的 OpenGL 渲染截图，检查布局和弹窗。
// 正式安装包不编译这个文件。终端没有屏幕录制权限时也能用：截图取的是程序自己的画面。

// CaptureScenario 依次：空白启动、打开示例并连接、选中窗口 2、控制条切换格式 / 字节序 / 原始值、
// 用菜单连开两次读取定义（应只有一个）、写入、调整点表字节序、历史报文、右键菜单、字节序调试窗口、1024 宽，
// 每步截图存到 dir，完成后退出。
func (ws *Workspace) CaptureScenario(dir string) {
	updating.Store(true) // 不让自动检查更新的弹窗挡住截图
	n := 0
	shot := func(name string, w fyne.Window) {
		n++
		var path string
		do(func() {
			img := w.Canvas().Capture()
			path = filepath.Join(dir, fmt.Sprintf("%02d-%s.png", n, name))
			if f, err := os.Create(path); err == nil {
				_ = png.Encode(f, img)
				f.Close()
			}
		})
		fmt.Println("captured", path)
	}
	menu := func(label string) func() {
		for _, m := range ws.win.MainMenu().Items {
			for _, it := range m.Items {
				if it.Label == label {
					return it.Action
				}
			}
		}
		return func() { fmt.Println("菜单里没有", label) }
	}
	clear := func() {
		for _, o := range ws.win.Canvas().Overlays().List() {
			ws.win.Canvas().Overlays().Remove(o)
		}
	}
	go func() {
		time.Sleep(2 * time.Second)
		shot("启动", ws.win)
		do(ws.loadDemo)
		time.Sleep(2500 * time.Millisecond)
		shot("示例", ws.win)
		do(func() {
			w2 := ws.windows[1]
			w2.root.Tapped(&fyne.PointEvent{}) // 点窗口 2 的空白处
			w2.tapCell(widget.TableCellID{Row: 0, Col: 2})
		})
		time.Sleep(time.Second)
		shot("选中窗口2", ws.win)
		do(func() {
			w1 := ws.windows[0]
			w1.bar.kind.SetSelected("Hex")
			w1.bar.order.SetSelected("BA")
			w1.bar.raw.SetChecked(true)
			ws.windows[1].bar.raw.SetChecked(true)
		})
		time.Sleep(2 * time.Second)
		shot("控制条", ws.win)
		do(func() {
			act := menu("读取定义…")
			act()
			act() // 连开两次，应只有一个对话框
			fmt.Println("读取定义对话框个数", len(ws.win.Canvas().Overlays().List()))
		})
		time.Sleep(700 * time.Millisecond)
		shot("读取定义", ws.win)
		do(func() {
			clear()
			ws.windows[1].tapCell(widget.TableCellID{Row: 0, Col: 2})
			menu("写入选中的值…")()
		})
		time.Sleep(700 * time.Millisecond)
		shot("写入", ws.win)
		do(func() { clear(); menu("调整点表字节序…")() })
		time.Sleep(700 * time.Millisecond)
		shot("字节序", ws.win)
		do(func() { clear(); menu("历史报文…")() })
		time.Sleep(2 * time.Second)
		var hist fyne.Window
		do(func() { hist = ws.historyWin })
		if hist != nil {
			shot("历史报文", hist)
		}
		do(func() {
			clear()
			ws.windows[1].showCellMenu(widget.TableCellID{Row: 0, Col: 2}, fyne.NewPos(760, 220))
		})
		time.Sleep(700 * time.Millisecond)
		shot("右键菜单", ws.win)
		do(func() { clear(); ws.openTypeTool(ws.windows[1]) })
		time.Sleep(2 * time.Second)
		var tool fyne.Window
		do(func() {
			if ws.typeTool != nil {
				tool = ws.typeTool.win
			}
		})
		if tool != nil {
			shot("字节序调试", tool)
		}
		do(func() { ws.win.Resize(fyne.NewSize(1024, 700)) })
		time.Sleep(1500 * time.Millisecond)
		shot("1024宽", ws.win)
		os.Exit(0)
	}()
}

// do 在界面线程执行 fn 并等它完成。
func do(fn func()) {
	done := make(chan struct{})
	fyne.Do(func() {
		defer close(done)
		fn()
	})
	<-done
}
