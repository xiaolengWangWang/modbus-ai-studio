package ui

import (
	"fmt"
	"slices"
	"sync/atomic"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/recorder"
)

// HistoryDays 是报文数据库保留的天数，main 启动时按它清理。
const HistoryDays = 7

type recState struct {
	r    *recorder.Recorder
	path string
	err  error
}

// recorderState 是报文数据库，由 main 打开后交给界面；没有设置或打不开时不记录。
var recorderState atomic.Pointer[recState]

// SetRecorder 设置报文数据库。err 不为 nil 表示打不开，历史报文窗口会显示原因。
func SetRecorder(r *recorder.Recorder, path string, err error) {
	recorderState.Store(&recState{r, path, err})
}

func currentRecorder() *recorder.Recorder {
	if s := recorderState.Load(); s != nil {
		return s.r
	}
	return nil
}

// addTool 登记一个工具窗口：主窗口关闭时一起关闭，自己关闭时从列表里去掉。
func (ws *Workspace) addTool(w fyne.Window, onClose func()) {
	ws.tools = append(ws.tools, w)
	w.SetOnClosed(func() {
		if onClose != nil {
			onClose()
		}
		ws.tools = slices.DeleteFunc(ws.tools, func(x fyne.Window) bool { return x == w })
	})
}

func sessionLabel(s recorder.Session) string {
	end := "未正常断开"
	if !s.End.IsZero() {
		end = s.End.Format("15:04:05")
	}
	label := fmt.Sprintf("%s – %s · %s %s · 窗口 %d · %d 条，错误 %d", s.Start.Format("01-02 15:04:05"), end,
		modeName[s.Mode], s.Target, s.Window, s.Packets, s.Errors)
	if s.Disconnects > 0 {
		label += fmt.Sprintf("，断开 %d 次", s.Disconnects)
	}
	return label
}

// openHistory 打开历史报文窗口：选一次连接，查看它的收发记录，可筛选、查找、复制、保存，单击逐字段解析。
func (ws *Workspace) openHistory() {
	st := recorderState.Load()
	if st == nil || st.r == nil {
		msg := "报文记录没有启用。"
		if st != nil && st.err != nil {
			msg = "报文数据库打不开，本次运行不记录：" + st.err.Error()
		}
		dialog.ShowInformation("历史报文", msg, ws.win)
		return
	}
	r := st.r
	w := ws.app.NewWindow(fmt.Sprintf("历史报文 · 窗口 %d", ws.no))
	w.Resize(fyne.NewSize(1180, 720))
	tp := newTrafficPanel(ws)
	tp.pauseBtn.Hide()
	in := newInspector(ws)
	tp.onSelect = in.showPacket
	info := widget.NewLabel("")
	var sessions []recorder.Session
	sel := widget.NewSelect(nil, nil)
	sel.PlaceHolder = "选择一次连接"
	sel.OnChanged = func(name string) {
		for _, s := range sessions {
			if sessionLabel(s) != name {
				continue
			}
			go func() {
				ps, err := r.Packets(s.ID, maxTraffic)
				events, _ := r.Events(s.ID)
				uiDo(func() {
					if err != nil {
						info.SetText("读取失败：" + err.Error())
						return
					}
					tp.setPackets(ps)
					text := fmt.Sprintf("共 %d 条，显示最后 %d 条", s.Packets, len(ps))
					// 断开和重连记录：最近 5 条，时间对照报文列表里的连接错误行
					for _, e := range events[max(0, len(events)-5):] {
						kind := "断开"
						if e.Kind == recorder.EventReconnect {
							kind = "重连"
						}
						text += fmt.Sprintf("\n%s %s：%s", e.Time.Format("15:04:05"), kind, e.Detail)
					}
					info.SetText(text)
				})
			}()
		}
	}
	load := func() {
		ss, err := r.Sessions(200)
		if err != nil {
			info.SetText("读取失败：" + err.Error())
			return
		}
		sessions = ss
		var names []string
		for _, s := range ss {
			names = append(names, sessionLabel(s))
		}
		sel.SetOptions(names)
		if len(ss) == 0 {
			info.SetText(fmt.Sprintf("还没有记录。连接后全部收发会自动记录，保留 %d 天。", HistoryDays))
		}
	}
	load()
	path := widget.NewLabel(fmt.Sprintf("数据库：%s（保留 %d 天，可用 SQLite 工具直接打开）", st.path, HistoryDays))
	path.Selectable = true
	path.Truncation = fyne.TextTruncateEllipsis
	split := container.NewHSplit(tp.root, in.root)
	split.Offset = 0.58
	top := container.NewBorder(nil, nil, widget.NewLabel("连接"), widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), load), sel)
	w.SetContent(container.NewBorder(container.NewVBox(top, info), path, nil, nil, split))
	ws.addTool(w, nil)
	w.Show()
}
