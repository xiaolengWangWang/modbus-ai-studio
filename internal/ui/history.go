package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/assets/icon"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/platform"
)

type recState struct {
	r       *recorder.Recorder
	path    string
	err     error
	dynamic bool
}

func (s *recState) databasePath() string {
	if s.dynamic {
		return s.r.Path()
	}
	return s.path
}

// recorderState 是报文数据库，由 main 打开后交给界面；没有设置或打不开时不记录。
var recorderState atomic.Pointer[recState]

// SetRecorder 设置报文数据库。err 不为 nil 表示打不开，历史报文窗口会显示原因。
func SetRecorder(r *recorder.Recorder, path string, err error) {
	dynamic := false
	if r != nil {
		files, _ := r.Files()
		dynamic = slices.Contains(files, path)
	}
	recorderState.Store(&recState{r: r, path: path, err: err, dynamic: dynamic})
}

func currentRecorder() *recorder.Recorder {
	if s := recorderState.Load(); s != nil {
		return s.r
	}
	return nil
}

// addTool 登记一个工具窗口：主窗口关闭时一起关闭，自己关闭时从列表里去掉。
func (ws *Workspace) addTool(w fyne.Window, onClose func()) {
	w.SetIcon(icon.Application())
	ws.tools = append(ws.tools, w)
	w.SetOnClosed(func() {
		if onClose != nil {
			onClose()
		}
		ws.tools = slices.DeleteFunc(ws.tools, func(x fyne.Window) bool { return x == w })
	})
}

func sessionLabel(s recorder.Session, running bool) string {
	end := "未正常断开" // 程序异常退出，没来得及记结束时间
	switch {
	case running:
		end = "进行中"
	case !s.End.IsZero():
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
		ws.historyWin.Show()
		ws.historyWin.RequestFocus()
		return
	}
	r := st.r
	selectedPath := r.Path()
	followCurrent := true
	w := ws.app.NewWindow(fmt.Sprintf("历史报文 · 窗口 %d", ws.no))
	ws.historyWin = w
	w.Resize(fyne.NewSize(1180, 720))
	tp := newTrafficPanel(ws)
	tp.pauseBtn.Hide()
	tp.title.Hide()
	in := newInspector(ws)
	in.selectionOnly = true
	in.placeholder = "单击报文，逐字段解析；单击日志，查看原因分析和出错时的原始报文。"
	in.orderBtn.Hide() // 历史窗口里没有读取窗口，调字节序没有对象，点了还会在主窗口上叠对话框
	in.clear()
	tp.onSelect = in.showPacket
	tp.onUnselect = func() {
		if in.title.Text == "报文解析" {
			in.clear()
		}
	}
	// 异常报文来自单独的 anomalies 表：不受报文页只显示最后 maxTraffic 条的限制，每条带上同一请求的发送帧
	ap := newTrafficPanel(ws)
	ap.pauseBtn.Hide()
	ap.title.Hide()
	ap.onSelect = in.showPacket
	ap.onUnselect = tp.onUnselect
	anomalyTab := container.NewTabItem("异常报文", ap.root)
	fl := newFaultLog(ws.app)
	fl.onSelect = in.showLog
	logTab := container.NewTabItem("日志", fl.root)
	tabs := container.NewAppTabs(container.NewTabItem("报文", tp.root), anomalyTab, logTab)
	fl.onChange = func(n int) {
		logTab.Text = fmt.Sprintf("日志 (%d)", n)
		tabs.Refresh()
	}
	info := widget.NewLabel("")
	info.Truncation = fyne.TextTruncateEllipsis
	var sessions []recorder.Session
	var selectedID int64
	var generation uint64
	closed := false
	// 用“打开文件…”选的其他数据库文件：下拉框里的名字 → 完整路径
	other := map[string]string{}
	var otherNames []string
	nameOf := func(p string) string {
		for name, x := range other {
			if x == p {
				return name
			}
		}
		return filepath.Base(p)
	}
	sel := widget.NewSelect(nil, nil)
	sel.PlaceHolder = "选择一次连接"
	sel.OnChanged = func(name string) {
		// 使用列表里保存的标签匹配，连接可能已在后台结束，不能重新计算标签。
		index := slices.Index(sel.Options, name)
		if index >= 0 && index < len(sessions) {
			s := sessions[index]
			selectedID = s.ID
			generation++
			request := generation
			queryPath := selectedPath
			info.SetText("正在读取记录…")
			tp.clear()
			ap.clear()
			in.packetContext = nil
			fl.set(nil)
			in.clear()
			go func() {
				ps, err := r.PacketsFile(queryPath, s.ID, maxTraffic)
				anomalies, err3 := r.AnomaliesFile(queryPath, s.ID, maxTraffic)
				events, err2 := r.EventsFile(queryPath, s.ID)
				aps := anomalyPackets(anomalies)
				uiDo(func() {
					if closed || request != generation {
						return // 切换会话或关闭窗口后，丢弃旧查询结果
					}
					if err = errors.Join(err, err2, err3); err != nil {
						info.SetText("读取失败：" + err.Error())
						return
					}
					tp.setPackets(ps)
					ap.setPackets(aps)
					anomalyTab.Text = fmt.Sprintf("异常报文 (%d)", len(anomalies))
					tabs.Refresh()
					in.packetContext = ps
					var es []logEntry
					for _, e := range events {
						es = append(es, logEntry{Event: e, mode: s.Mode})
					}
					fl.set(es)
					text := fmt.Sprintf("报文共 %d 条，显示最后 %d 条；异常 %d 条；日志 %d 条", s.Packets, len(ps), len(anomalies), len(es))
					if len(ps) == 0 && len(es) > 0 {
						tabs.Select(logTab)
					} else {
						tabs.SelectIndex(0)
					}
					info.SetText(text)
				})
			}()
		}
	}
	// load 在后台读连接列表：大文件统计每次连接的报文数要一些时间，界面照常响应；换文件或关窗后丢弃旧结果。
	load := func() {
		generation++
		request := generation
		queryPath := selectedPath
		info.SetText("正在读取连接列表…")
		sel.SetOptions(nil) // 读完之前不能选旧文件里的连接
		go func() {
			ss, err := r.SessionsFile(queryPath, 200)
			uiDo(func() {
				if closed || request != generation {
					return
				}
				if err != nil {
					info.SetText("读取失败：" + err.Error())
					return
				}
				sessions = ss
				sel.ClearSelected()
				var names []string
				selected := 0
				for i, s := range ss {
					label := sessionLabel(s, r.Running(s.ID))
					if queryPath != r.Path() && s.End.IsZero() && !r.Running(s.ID) {
						label = strings.Replace(label, "未正常断开", "文件内的连接分段", 1)
					}
					names = append(names, label)
					if s.ID == selectedID {
						selected = i
					}
				}
				sel.SetOptions(names)
				if len(ss) == 0 {
					selectedID = 0
					tp.clear()
					ap.clear()
					fl.set(nil)
					in.clear()
					info.SetText("还没有记录。连接后全部收发会自动记录；单个文件上限 50 MB，旧文件保留。")
				} else {
					sel.SetSelectedIndex(selected) // 保留所选会话并重读；首次打开选最近一次连接
				}
			})
		}()
	}
	path := widget.NewLabel("")
	path.Selectable = true
	path.Truncation = fyne.TextTruncateEllipsis
	files := widget.NewSelect(nil, nil)
	files.PlaceHolder = "选择数据库文件"
	changeFile := func(name string) {
		selectedPath = filepath.Join(filepath.Dir(r.Path()), name)
		if p, ok := other[name]; ok {
			selectedPath = p
		}
		followCurrent = selectedPath == r.Path()
		selectedID = 0
		path.SetText("SQLite 文件：" + selectedPath + " · 单个文件上限 50 MB，旧文件保留")
		load()
	}
	files.OnChanged = changeFile
	refresh := func() {
		paths, err := r.Files()
		if err != nil {
			info.SetText("读取文件列表失败：" + err.Error())
			return
		}
		if followCurrent {
			selectedPath = r.Path()
		}
		var names []string
		for i := len(paths) - 1; i >= 0; i-- {
			names = append(names, filepath.Base(paths[i]))
		}
		names = append(names, otherNames...)
		// Refresh the same file without resetting its selected session.
		files.OnChanged = nil
		files.SetOptions(names)
		files.SetSelected(nameOf(selectedPath))
		files.OnChanged = changeFile
		path.SetText("SQLite 文件：" + selectedPath + " · 单个文件上限 50 MB，旧文件保留")
		load()
	}
	// 打开其他数据库文件：Web 版下载的按天记录、别的电脑拷来的数据库，表结构相同，按路径只读查询。
	openOther := func() {
		pickDatabaseFn(w, func(p string) {
			paths, _ := r.Files()
			if slices.Contains(paths, p) { // 本机的编号文件，直接在列表里选中
				selectedPath, followCurrent = p, p == r.Path()
			} else {
				name := filepath.Base(p) + "（其他文件）"
				if q, ok := other[name]; ok && q != p { // 不同文件夹里的同名文件
					name = p + "（其他文件）"
				}
				if _, ok := other[name]; !ok {
					otherNames = append(otherNames, name)
				}
				other[name] = p
				selectedPath, followCurrent = p, false
			}
			selectedID = 0
			refresh()
		})
	}
	refresh()
	dbBtns := container.NewHBox(
		widget.NewButtonWithIcon("复制路径", theme.ContentCopyIcon(), func() { ws.app.Clipboard().SetContent(selectedPath) }),
		widget.NewButtonWithIcon("管理工具打开", theme.FileIcon(), func() { ws.openDatabaseFile(w, selectedPath) }),
		widget.NewButtonWithIcon("所在文件夹", theme.FolderOpenIcon(), func() {
			if err := showInFolderFn(selectedPath); err != nil {
				dialog.ShowError(fmt.Errorf("打不开数据库所在文件夹：%w", err), w)
			}
		}))
	split := container.NewHSplit(tabs, in.root)
	split.Offset = 0.58
	top := container.NewBorder(nil, nil, widget.NewLabel("连接"), nil, sel)
	fileRow := container.NewBorder(nil, nil, widget.NewLabel("数据库文件"), container.NewHBox(
		widget.NewButtonWithIcon("打开文件…", theme.FolderOpenIcon(), openOther),
		widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), refresh)), files)
	w.SetContent(container.NewBorder(container.NewVBox(fileRow, top, info), container.NewBorder(nil, nil, nil, dbBtns, path), nil, nil, split))
	ws.addTool(w, func() { closed = true; in.closed = true; ws.historyWin = nil })
	showTool(w)
}

// anomalyPackets 把异常记录排成报文列表：先列同一请求的发送帧（同一请求有多条异常时只列一次），再列异常那一行。
func anomalyPackets(as []recorder.Anomaly) []modbus.Packet {
	var out []modbus.Packet
	type key struct {
		conn string
		req  uint64
	}
	var last key
	for _, a := range as {
		p := a.Packet
		if k := (key{p.ConnectionID, p.RequestID}); a.TX != nil && p.Dir == modbus.DirRX && k != last {
			tx := p
			tx.Time, tx.Dir, tx.Raw, tx.Status, tx.Err, tx.RTT = a.TXTime, modbus.DirTX, a.TX, modbus.StatusSent, nil, 0
			out = append(out, tx)
			last = k
		}
		out = append(out, p)
	}
	return out
}

// 测试时替换，不真的打开访达 / 资源管理器。
var showInFolderFn = platform.ShowInFolder

var openFileFn = platform.OpenFile

// pickDatabaseFn 让用户选一个 SQLite 数据库文件，取消时不调用 done；测试时替换。
var pickDatabaseFn = func(parent fyne.Window, done func(path string)) {
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil {
			dialog.ShowError(err, parent)
			return
		}
		if rc == nil {
			return
		}
		path := filepath.FromSlash(rc.URI().Path())
		rc.Close()
		done(path)
	}, parent)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".db", ".sqlite3", ".sqlite"}))
	d.Resize(fyne.NewSize(900, 600))
	d.Show()
}

// openDatabaseTool 使用已关联的 SQLite 管理工具打开标准数据库文件。
func (ws *Workspace) openDatabaseTool(parent fyne.Window) {
	st := recorderState.Load()
	path := ""
	if st != nil {
		path = st.databasePath()
	}
	ws.openDatabaseFile(parent, path)
}

func (ws *Workspace) openDatabaseFile(parent fyne.Window, path string) {
	if parent.Canvas().Overlays().Top() != nil {
		return
	}
	if path == "" {
		dialog.ShowInformation("报文数据库", "报文记录没有启用，没有数据库文件。", parent)
		return
	}
	if _, err := os.Stat(path); err != nil {
		dialog.ShowError(fmt.Errorf("找不到报文数据库：%w", err), parent)
		return
	}
	if err := openFileFn(path); errors.Is(err, platform.ErrNoApp) {
		dialog.ShowInformation("报文数据库", "这是标准 SQLite 文件，可在管理工具中选择打开：\n"+path+"\n也可继续使用程序内的历史记录查看。", parent)
	} else if err != nil {
		dialog.ShowError(fmt.Errorf("打不开报文数据库：%w", err), parent)
	}
}

// openDatabase 在内置历史窗口查看 SQLite 数据，无需安装工具或关联 .db 文件。
// folder 为 true 时在文件夹里显示文件；parent 是显示提示的窗口。
func (ws *Workspace) openDatabase(parent fyne.Window, folder bool) {
	st := recorderState.Load()
	if st == nil || st.path == "" {
		dialog.ShowInformation("报文数据库", "报文记录没有启用，没有数据库文件。", parent)
		return
	}
	path := st.databasePath()
	if _, err := os.Stat(path); err != nil {
		dialog.ShowError(fmt.Errorf("找不到报文数据库：%w", err), parent)
		return
	}
	if folder {
		if err := showInFolderFn(path); err != nil {
			dialog.ShowError(fmt.Errorf("打不开数据库所在文件夹：%w", err), parent)
		}
		return
	}
	ws.openHistory()
}
