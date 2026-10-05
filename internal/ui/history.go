package ui

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/platform"
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
	if s.Faults > 0 {
		label += fmt.Sprintf("，故障 %d 次", s.Faults)
	}
	if s.Packets == 0 && s.Faults > 0 { // 一条报文都没发过就出了故障：连接没建立起来
		label += "（连接失败）"
	}
	return fmt.Sprintf("%s · #%d", label, s.ID)
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
	if ws.historyWin != nil { // 只开一个，再点切到已打开的
		ws.historyWin.RequestFocus()
		return
	}
	r := st.r
	w := ws.app.NewWindow(fmt.Sprintf("历史报文 · 窗口 %d", ws.no))
	ws.historyWin = w
	w.Resize(fyne.NewSize(1180, 720))
	tp := newTrafficPanel(ws)
	tp.pauseBtn.Hide()
	tp.title.Hide()
	in := newInspector(ws)
	tp.onSelect = in.showPacket
	fl := newFaultLog(ws.app)
	fl.onSelect = func(e logEntry) { in.show("日志", e.detail()) }
	logTab := container.NewTabItem("日志", fl.root)
	tabs := container.NewAppTabs(container.NewTabItem("报文", tp.root), logTab)
	fl.onChange = func(n int) {
		logTab.Text = fmt.Sprintf("日志 (%d)", n)
		tabs.Refresh()
	}
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
				events, err2 := r.Events(s.ID)
				uiDo(func() {
					if err = errors.Join(err, err2); err != nil {
						info.SetText("读取失败：" + err.Error())
						return
					}
					tp.setPackets(ps)
					var es []logEntry
					for _, e := range events {
						es = append(es, logEntry{Event: e})
					}
					fl.set(es)
					text := fmt.Sprintf("报文共 %d 条，显示最后 %d 条；日志 %d 条（连接失败、读取失败与恢复、断开、重连，带原因分析和原始报文）", s.Packets, len(ps), len(es))
					if len(ps) == 0 && len(es) > 0 {
						tabs.Select(logTab)
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
	dbBtns := container.NewHBox(
		widget.NewButtonWithIcon("打开数据库", theme.FileIcon(), func() { ws.openDatabase(w, false) }),
		widget.NewButtonWithIcon("所在文件夹", theme.FolderOpenIcon(), func() { ws.openDatabase(w, true) }))
	split := container.NewHSplit(tabs, in.root)
	split.Offset = 0.58
	top := container.NewBorder(nil, nil, widget.NewLabel("连接"), widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), load), sel)
	w.SetContent(container.NewBorder(container.NewVBox(top, info), container.NewBorder(nil, nil, nil, dbBtns, path), nil, nil, split))
	ws.addTool(w, func() { ws.historyWin = nil })
	w.Show()
}

// 测试时替换，不真的打开访达 / 资源管理器。
var (
	openFileFn     = platform.OpenFile
	showInFolderFn = platform.ShowInFolder
)

// openDatabase 用系统里的 SQLite 工具打开报文数据库（folder 为 true 时在文件夹里显示它）；
// 没有能打开 .db 文件的程序时改为在文件夹里显示，并说明可以装什么工具。parent 是显示提示的窗口。
func (ws *Workspace) openDatabase(parent fyne.Window, folder bool) {
	st := recorderState.Load()
	if st == nil || st.path == "" {
		dialog.ShowInformation("报文数据库", "报文记录没有启用，没有数据库文件。", parent)
		return
	}
	if _, err := os.Stat(st.path); err != nil {
		dialog.ShowError(fmt.Errorf("找不到报文数据库：%w", err), parent)
		return
	}
	if folder {
		if err := showInFolderFn(st.path); err != nil {
			dialog.ShowError(fmt.Errorf("打不开数据库所在文件夹：%w", err), parent)
		}
		return
	}
	err := openFileFn(st.path)
	if errors.Is(err, platform.ErrNoApp) {
		_ = showInFolderFn(st.path)
		dialog.ShowInformation("报文数据库", "这台电脑上没有能打开 .db 文件的程序，已在文件夹里显示数据库文件。\n"+
			"可以安装免费的 DB Browser for SQLite 后再打开；程序运行时也能打开查看。", parent)
		return
	}
	if err != nil {
		dialog.ShowError(fmt.Errorf("打不开报文数据库：%w", err), parent)
	}
}
