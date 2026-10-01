package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

const maxTraffic = 5000

const (
	filterAll    = "全部"
	filterErrors = "仅错误"
)

// trafficPanel 是 Modbus Poll 式通信报文窗口：Tx:000001-01 03 …。收发回调只把记录放进 pending，
// 由工作区每 100 ms 合并刷新一次，报文再多界面也不会卡。选中一行会自动暂停，方便对照解析。
type trafficPanel struct {
	ws *Workspace

	mu      sync.Mutex
	pending []modbus.Packet

	all    []modbus.Packet // 以下只在 UI 线程读写
	held   []modbus.Packet // 暂停期间到达的记录，继续时并入
	view   []int           // all 中通过筛选的下标
	total  int
	paused bool

	filter   *widget.Select
	search   *widget.Entry
	showTS   *widget.Check
	list     *widget.List
	count    *widget.Label
	pauseBtn *widget.Button
	root     fyne.CanvasObject
	onSelect func(modbus.Packet) // 选中一行时调用，默认交给主窗口的解析面板
}

func newTrafficPanel(ws *Workspace) *trafficPanel {
	t := &trafficPanel{ws: ws}
	t.list = widget.NewList(
		func() int { return len(t.view) },
		func() fyne.CanvasObject {
			l := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
			l.Truncation = fyne.TextTruncateEllipsis
			return l
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			l := o.(*widget.Label)
			if i >= len(t.view) {
				l.SetText("")
				return
			}
			text, imp := trafficLine(t.all[t.view[i]], t.showTS.Checked)
			l.Importance = imp
			l.SetText(text)
		},
	)
	t.list.OnSelected = func(i widget.ListItemID) {
		if i >= len(t.view) {
			return
		}
		if !t.paused {
			t.setPaused(true)
		}
		t.onSelect(t.all[t.view[i]])
	}
	t.onSelect = func(p modbus.Packet) { ws.inspect.showPacket(p) }
	t.count = widget.NewLabel("0 条")
	t.pauseBtn = widget.NewButtonWithIcon("暂停", theme.MediaPauseIcon(), func() { t.setPaused(!t.paused) })
	t.showTS = widget.NewCheck("时间戳", func(bool) { t.list.Refresh() })
	t.showTS.SetChecked(true)
	t.filter = widget.NewSelect([]string{filterAll, filterErrors}, func(string) { t.rebuild() })
	t.filter.SetSelected(filterAll)
	t.search = widget.NewEntry()
	t.search.SetPlaceHolder("查找字节")
	t.search.OnChanged = func(string) { t.rebuild() }
	btn := func(label string, icon fyne.Resource, fn func()) *widget.Button {
		b := widget.NewButtonWithIcon(label, icon, fn)
		b.Importance = widget.LowImportance
		return b
	}
	head := container.NewBorder(nil, nil,
		container.NewHBox(widget.NewLabelWithStyle("通信报文", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), t.pauseBtn,
			btn("清空", theme.DeleteIcon(), t.clear), btn("复制", theme.ContentCopyIcon(), t.copy), btn("保存", theme.DocumentSaveIcon(), t.save)),
		container.NewHBox(t.showTS, fixed(96, t.filter), t.count), t.search)
	t.root = container.NewBorder(head, nil, nil, nil, compact(t.list))
	return t
}

// trafficLine 把一条记录排成 Modbus Poll 的格式，出错时附上原因。
func trafficLine(p modbus.Packet, ts bool) (string, widget.Importance) {
	tag, imp := "Rx", widget.MediumImportance
	if p.Dir == modbus.DirTX {
		tag, imp = "Tx", widget.HighImportance
	}
	data := frameText(p.Mode, p.Raw)
	if p.Raw == nil {
		data = "（无数据）"
	}
	text := fmt.Sprintf("%s:%06d-%s", tag, p.RequestID, data)
	if p.Status == modbus.StatusSuccess && p.RTT > 0 {
		text += "   " + formatRTT(p.RTT)
	}
	if s, ok := statusText[p.Status]; ok {
		if p.Status == modbus.StatusCRCError && p.Mode.IsASCII() {
			s = "LRC 错误，已丢弃"
		}
		text += "   ← " + s
		imp = widget.DangerImportance
		if p.Status == modbus.StatusLate || p.Status == modbus.StatusUnexpected {
			imp = widget.WarningImportance
		}
	}
	if ts {
		text = p.Time.Format("15:04:05.000") + "  " + text
	}
	return text, imp
}

func isError(p modbus.Packet) bool {
	_, ok := statusText[p.Status]
	return ok
}

// push 可以在任意 goroutine 调用。
func (t *trafficPanel) push(p modbus.Packet) {
	t.mu.Lock()
	t.pending = append(t.pending, p)
	if len(t.pending) > maxTraffic {
		t.pending = t.pending[len(t.pending)-maxTraffic:]
	}
	t.mu.Unlock()
}

func (t *trafficPanel) hasPending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending) > 0
}

func trim(ps []modbus.Packet) []modbus.Packet {
	if len(ps) > maxTraffic {
		return append([]modbus.Packet(nil), ps[len(ps)-maxTraffic:]...)
	}
	return ps
}

// flush 把 pending 并入列表，只在 UI 线程调用。
func (t *trafficPanel) flush() {
	t.mu.Lock()
	batch := t.pending
	t.pending = nil
	t.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	t.total += len(batch)
	if t.paused {
		t.held = trim(append(t.held, batch...))
		t.updateCount()
		return
	}
	t.all = trim(append(t.all, batch...))
	t.rebuild()
	t.list.ScrollToBottom()
}

func (t *trafficPanel) updateCount() {
	text := fmt.Sprintf("%d 条", t.total)
	if n := len(t.held); n > 0 {
		text += fmt.Sprintf("（暂停中 +%d）", n)
	}
	t.count.SetText(text)
}

// rebuild 按筛选条件重建可见行。“仅错误”同时保留出错请求的 Tx 行，方便对照。
func (t *trafficPanel) rebuild() {
	if t.filter == nil || t.search == nil {
		return
	}
	errOnly := t.filter.Selected == filterErrors
	query := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(t.search.Text), " ", ""))
	bad := map[uint64]bool{}
	if errOnly {
		for _, p := range t.all {
			if isError(p) {
				bad[p.RequestID] = true
			}
		}
	}
	t.view = t.view[:0]
	for i, p := range t.all {
		if errOnly && !bad[p.RequestID] {
			continue
		}
		if query != "" && !strings.Contains(strings.ToUpper(strings.ReplaceAll(frameText(p.Mode, p.Raw), " ", "")), query) {
			continue
		}
		t.view = append(t.view, i)
	}
	t.updateCount()
	t.list.Refresh()
}

func (t *trafficPanel) setPaused(p bool) {
	t.paused = p
	if p {
		t.pauseBtn.SetText("继续")
		t.pauseBtn.SetIcon(theme.MediaPlayIcon())
		return
	}
	t.pauseBtn.SetText("暂停")
	t.pauseBtn.SetIcon(theme.MediaPauseIcon())
	t.all = trim(append(t.all, t.held...))
	t.held = nil
	t.list.UnselectAll()
	t.rebuild()
	t.list.ScrollToBottom()
}

// setPackets 显示一批已有的记录（历史报文），替换原来的内容。
func (t *trafficPanel) setPackets(ps []modbus.Packet) {
	t.all, t.held, t.total = ps, nil, len(ps)
	t.list.UnselectAll()
	t.rebuild()
	t.list.ScrollToBottom()
}

func (t *trafficPanel) clear() {
	t.mu.Lock()
	t.pending = nil
	t.mu.Unlock()
	t.all, t.held, t.view, t.total = nil, nil, nil, 0
	t.list.UnselectAll()
	t.updateCount()
	t.list.Refresh()
}

// text 返回当前可见的全部行（带时间戳），用于复制和保存。
func (t *trafficPanel) text() string {
	var b strings.Builder
	for _, i := range t.view {
		line, _ := trafficLine(t.all[i], true)
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (t *trafficPanel) copy() {
	t.ws.app.Clipboard().SetContent(t.text())
}

func (t *trafficPanel) save() {
	d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil || wc == nil {
			return
		}
		defer wc.Close()
		if _, err := wc.Write([]byte(t.text())); err != nil {
			dialog.ShowError(err, t.ws.win)
		}
	}, t.ws.win)
	d.SetFileName("modbus-traffic-" + time.Now().Format("20060102-150405") + ".txt")
	d.Show()
}
