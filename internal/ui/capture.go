//go:build capture

package ui

import (
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/modbus"
)

// 调试构建（go build -tags capture）专用：按顺序操作界面，用真实的 OpenGL 渲染截图，检查布局和弹窗。
// 正式安装包不编译这个文件。终端没有屏幕录制权限时也能用：截图取的是程序自己的画面。

// CaptureScenario 依次：空白启动、打开示例并连接、选中窗口 2、控制条切换格式 / 字节序 / 原始值、
// 用菜单连开两次读取定义（应只有一个）、写入、调整点表字节序、历史报文、右键菜单、字节序调试窗口、1024 宽，
// 每步截图存到 dir，完成后退出。
func (ws *Workspace) CaptureScenario(dir string) {
	updating.Store(true) // 不让自动检查更新的弹窗挡住截图
	n := 0
	sizes := make(map[fyne.Window]fyne.Size)
	shot := func(name string, w fyne.Window) {
		n++
		var path string
		// A minimized/hidden native window has a zero-sized framebuffer. Restore
		// it and allow the driver to paint before asking OpenGL to read pixels.
		do(func() { w.Show(); w.RequestFocus() })
		time.Sleep(250 * time.Millisecond)
		for attempt := 0; attempt < 3 && path == ""; attempt++ {
			do(func() {
				size := w.Canvas().Size()
				if size.Width <= 0 || size.Height <= 0 {
					fmt.Println("waiting for drawable framebuffer", name)
					want := sizes[w]
					if want.Width <= 0 {
						want = fyne.NewSize(960, 620)
					}
					w.Resize(want)
					w.Show()
					w.RequestFocus()
					return
				}
				sizes[w] = size
				fmt.Println("capturing", name, "canvas", w.Canvas().Size(), "scale", w.Canvas().Scale())
				img := w.Canvas().Capture()
				path = filepath.Join(dir, fmt.Sprintf("%02d-%s.png", n, name))
				if f, err := os.Create(path); err == nil {
					_ = png.Encode(f, img)
					f.Close()
				}
			})
			if path == "" {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if path == "" {
			fmt.Println("native window unavailable", name)
			os.Exit(2)
		}
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
			ws.readOnlyCheck.SetChecked(true)
			ws.pauseAllBtn.OnTapped()
		})
		time.Sleep(700 * time.Millisecond)
		shot("只读与全部暂停", ws.win)
		do(func() {
			ws.pauseAllBtn.OnTapped()
			ws.readOnlyCheck.SetChecked(false)
		})
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
		do(func() { clear(); menu("调整字节序…")() })
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
		do(func() {
			ws.traffic.filter.SetSelected(filterRX)
			ws.traffic.search.SetText("01 03")
		})
		time.Sleep(700 * time.Millisecond)
		shot("接收报文筛选", ws.win)
		do(func() {
			ws.traffic.resetBtn.OnTapped()
			ws.traffic.list.Select(0)
		})
		time.Sleep(700 * time.Millisecond)
		shot("报文暂停与计数", ws.win)
		do(func() {
			ws.traffic.setPaused(false)
			ws.win.Resize(fyne.NewSize(1024, 700))
		})
		time.Sleep(1500 * time.Millisecond)
		shot("1024宽", ws.win)
		do(func() { ws.win.Resize(fyne.NewSize(960, 620)) })
		time.Sleep(700 * time.Millisecond)
		shot("960宽", ws.win)
		// Synthetic local provider exercises the real async GUI without a paid
		// service request or exposing any saved credential to the capture.
		var failAI atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if failAI.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"model":"deepseek-flash","usage":{"prompt_tokens":123,"completion_tokens":88,"total_tokens":211},"choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"已整理当前选中对象的诊断证据。\",\"observations\":[{\"text\":\"证据 E1 记录了当前对象的状态或事件元数据。\",\"evidence_ids\":[\"E1\"]}],\"hypotheses\":[{\"text\":\"超时可能小于设备实际应答时间，需对照晚到响应进一步验证。\",\"evidence_ids\":[]}],\"next_checks\":[\"查看晚到响应的实际延迟。\",\"核对设备手册和扫描周期。\"]}"}}]}`)
		}))
		defer srv.Close()
		var assistant fyne.Window
		do(func() {
			ws.openAI(nil)
			assistant = ws.ai.win
			ws.ai.key.SetText("capture-test-key")
			ws.ai.endpoint = srv.URL
			ws.ai.start(false)
		})
		time.Sleep(1500 * time.Millisecond)
		shot("AI诊断报告", assistant)
		do(func() { ws.ai.tabs.Select(ws.ai.previewTab) })
		time.Sleep(500 * time.Millisecond)
		shot("AI发送内容", assistant)
		do(func() { ws.ai.tabs.Select(ws.ai.settingsTab) })
		time.Sleep(500 * time.Millisecond)
		shot("AI模型设置", assistant)
		do(func() { ws.windows[2].aiBtn.OnTapped(); ws.ai.start(false) })
		time.Sleep(1000 * time.Millisecond)
		shot("AI故障来源", assistant)
		do(func() {
			if len(ws.log.entries) > 0 {
				ws.inspect.showLog(ws.log.entries[len(ws.log.entries)-1])
				ws.inspect.openAI()
				ws.ai.start(false)
			}
		})
		time.Sleep(1000 * time.Millisecond)
		do(func() { ws.ai.tabs.Select(ws.ai.previewTab) })
		shot("AI日志证据", assistant)
		do(func() { ws.ai.exportReport() })
		time.Sleep(700 * time.Millisecond)
		shot("AI报告导出", assistant)
		do(func() {
			for _, overlay := range assistant.Canvas().Overlays().List() {
				assistant.Canvas().Overlays().Remove(overlay)
			}
			ws.ai.tabs.SelectIndex(0)
			assistant.Resize(fyne.NewSize(520, 600))
		})
		time.Sleep(500 * time.Millisecond)
		shot("AI紧凑布局", assistant)
		do(func() {
			ws.traffic.setPaused(false)
			ws.traffic.filter.SetSelected(filterRX)
			ws.traffic.search.SetText("DEADBEEF0123456789")
		})
		time.Sleep(500 * time.Millisecond)
		shot("报文无匹配提示", ws.win)
		do(func() {
			ws.traffic.resetBtn.OnTapped()
			w := ws.windows[2]
			ws.setCurrent(w)
			ws.mdi.setMaxed(false)
			w.pos, w.size = fyne.NewPos(0, 0), fyne.NewSize(430, 245)
			ws.mdi.box.Refresh()
		})
		time.Sleep(600 * time.Millisecond)
		shot("读取窄窗口", ws.win)
		do(func() { ws.windows[2].headScroll.ScrollToBottom() })
		time.Sleep(500 * time.Millisecond)
		shot("读取诊断滚动", ws.win)
		failAI.Store(true)
		do(func() { ws.ai.start(false) })
		time.Sleep(700 * time.Millisecond)
		shot("AI重试保留报告", assistant)
		do(func() { ws.ai.showEvidence("E1") })
		shot("AI引用证据详情", assistant)
		do(func() { ws.ai.evidencePages.SelectIndex(1) })
		shot("AI完整JSON", assistant)
		do(func() { ws.ai.tabs.Select(ws.ai.settingsTab) })
		shot("AI窄窗口设置", assistant)
		do(func() {
			d := defaultDef()
			d.Qty = 4
			ws.openAIFrom(aiTarget{probe: newAIProbeResult(d, []int8{1, -1, -3, 0}, nil)}, nil)
			ws.ai.tabs.Select(ws.ai.previewTab)
		})
		shot("AI寄存器检测结果", assistant)
		do(func() {
			d := defaultDef()
			d.Kind, d.Order, d.Qty = kindFloat32, modbus.OrderABCD, 2
			w := ws.addWindow(d)
			w.setPaused(true)
			ws.mdi.setMaxed(true)
			regs, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
			w.mu.Lock()
			w.regs = regs
			w.lastOK = time.Now()
			w.mu.Unlock()
			w.refresh()
			w.tapCell(w.cellOf(0, 1))
			ws.inspect.orderBtn.OnTapped()
			for _, s := range findCaptureSelects(ws.win.Canvas().Overlays().Top()) {
				for _, option := range s.Options {
					if option == "CDAB" {
						s.SetSelected(option)
					}
				}
			}
		})
		shot("暂停读数字节序调整", ws.win)
		do(func() {
			for _, b := range findCaptureButtons(ws.win.Canvas().Overlays().Top()) {
				if b.Text == "应用" {
					b.OnTapped()
					break
				}
			}
			w := ws.current()
			value, _ := w.valueText(0)
			if value != "85.5" || !w.paused {
				panic("paused byte order did not reinterpret retained data")
			}
			w.table.ScrollToTop()
		})
		shot("暂停读数字节序生效", ws.win)
		// Quit through the UI loop so OpenGL stops before native DLL teardown
		// and the recorder flushes through main's deferred cleanup.
		do(ws.quitApp)
	}()
}

func captureObjects(o fyne.CanvasObject, visit func(fyne.CanvasObject)) {
	if o == nil {
		return
	}
	visit(o)
	switch o.(type) {
	case *widget.Select, *widget.Button:
		return
	}
	if c, ok := o.(*fyne.Container); ok {
		for _, child := range c.Objects {
			captureObjects(child, visit)
		}
	} else if w, ok := o.(fyne.Widget); ok {
		for _, child := range w.CreateRenderer().Objects() {
			captureObjects(child, visit)
		}
	}
}
func findCaptureSelects(o fyne.CanvasObject) (out []*widget.Select) {
	captureObjects(o, func(x fyne.CanvasObject) {
		if s, ok := x.(*widget.Select); ok {
			out = append(out, s)
		}
	})
	return
}
func findCaptureButtons(o fyne.CanvasObject) (out []*widget.Button) {
	captureObjects(o, func(x fyne.CanvasObject) {
		if b, ok := x.(*widget.Button); ok {
			out = append(out, b)
		}
	})
	return
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
