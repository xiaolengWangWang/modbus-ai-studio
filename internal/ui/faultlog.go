package ui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
)

// 日志：连接失败、读取失败与恢复、断开、重连。每条带原因分析和出错时抓到的原始报文（逐字段解析），
// 显示在主窗口的“日志”页，同时存进报文库的 events 表。同一个读取窗口连续出同一种错误只记第一次，
// 恢复正常时再记一条，每秒一次的轮询不会把日志刷满。

const maxLog = 1000

var logKindName = map[string]string{
	recorder.EventConnectFail: "连接失败",
	recorder.EventReadFail:    "读取失败",
	recorder.EventReadOK:      "恢复正常",
	recorder.EventDisconnect:  "连接断开",
	recorder.EventReconnect:   "重连成功",
}

// logEntry 是一条日志。rows 是选中时解析面板显示的内容；从报文库读回的记录没有 rows，按 Analysis 文字显示。
type logEntry struct {
	recorder.Event
	rows []decodeRow
}

// newLogEntry 生成一条日志：cause 是原因分析（每行一段），tx、res 是抓到的请求和结果，没有时 Raw 为空。
// Analysis 存进报文库，内容与解析面板看到的一样。
func newLogEntry(kind string, window int, detail string, cause []string, tx, res modbus.Packet, pts pointTable) logEntry {
	var rows []decodeRow
	for _, c := range cause {
		rows = append(rows, decodeRow{Meaning: c})
	}
	if tx.Raw != nil {
		rows = append(rows, decodeRow{}, decodeRow{Meaning: "发送的请求：" + frameText(tx.Mode, tx.Raw)})
		rows = append(rows, describePacket(tx, pts)...)
		rows = append(rows, decodeRow{})
		if res.Raw != nil {
			rows = append(rows, decodeRow{Meaning: "收到的响应：" + frameText(res.Mode, res.Raw)})
		} else {
			rows = append(rows, decodeRow{Meaning: "收到的响应：没有"})
		}
		rows = append(rows, describePacket(res, pts)...)
	}
	return logEntry{Event: recorder.Event{Time: time.Now(), Kind: kind, Window: window, Detail: detail,
		Analysis: rowsText(rows), TX: tx.Raw, RX: res.Raw}, rows: rows}
}

func (e logEntry) line() (string, widget.Importance) {
	imp := widget.DangerImportance
	switch e.Kind {
	case recorder.EventReadOK, recorder.EventReconnect:
		imp = widget.SuccessImportance
	case recorder.EventDisconnect:
		imp = widget.WarningImportance
	}
	return fmt.Sprintf("%s  %s  %s", e.Time.Format("15:04:05"), logKindName[e.Kind], e.Detail), imp
}

// detail 是选中一条日志时解析面板的内容。
func (e logEntry) detail() []decodeRow {
	rows := []decodeRow{
		{Name: "时间", Meaning: e.Time.Format("2006-01-02 15:04:05")},
		{Name: "类型", Meaning: logKindName[e.Kind]},
		{Name: "结论", Meaning: e.Detail},
		{},
	}
	if e.rows != nil {
		return append(rows, e.rows...)
	}
	for _, l := range strings.Split(e.Analysis, "\n") {
		rows = append(rows, decodeRow{Meaning: l})
	}
	return rows
}

func (e logEntry) text() string {
	line, _ := e.line()
	return line + "\n" + e.Analysis
}

// faultLog 是日志列表。
type faultLog struct {
	entries  []logEntry // 只在 UI 线程读写
	list     *widget.List
	count    *widget.Label
	root     fyne.CanvasObject
	onSelect func(logEntry)
	onChange func(n int) // 条数变化，用来更新页签标题
}

func newFaultLog(app fyne.App) *faultLog {
	l := &faultLog{}
	l.list = widget.NewList(
		func() int { return len(l.entries) },
		func() fyne.CanvasObject {
			lb := widget.NewLabel("")
			lb.Truncation = fyne.TextTruncateEllipsis
			return lb
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i >= len(l.entries) {
				return
			}
			lb := o.(*widget.Label)
			lb.Text, lb.Importance = l.entries[i].line()
			lb.Refresh()
		},
	)
	l.list.OnSelected = func(i widget.ListItemID) {
		if i < len(l.entries) && l.onSelect != nil {
			l.onSelect(l.entries[i])
		}
	}
	l.count = widget.NewLabel("0 条")
	btn := func(label string, icon fyne.Resource, fn func()) *widget.Button {
		b := widget.NewButtonWithIcon(label, icon, fn)
		b.Importance = widget.LowImportance
		return b
	}
	copyAll := func() {
		var parts []string
		for _, e := range l.entries {
			parts = append(parts, e.text())
		}
		app.Clipboard().SetContent(strings.Join(parts, "\n\n"))
	}
	head := container.NewBorder(nil, nil,
		container.NewHBox(btn("清空", theme.DeleteIcon(), func() { l.set(nil) }), btn("复制全部", theme.ContentCopyIcon(), copyAll)),
		l.count)
	l.root = container.NewBorder(head, nil, nil, nil, compact(l.list))
	return l
}

func (l *faultLog) add(e logEntry) {
	l.entries = append(l.entries, e)
	if n := len(l.entries); n > maxLog {
		l.entries = l.entries[n-maxLog:]
	}
	l.changed()
	l.list.ScrollToBottom()
}

func (l *faultLog) set(es []logEntry) {
	l.entries = es
	l.list.UnselectAll()
	l.changed()
}

func (l *faultLog) changed() {
	l.count.SetText(fmt.Sprintf("%d 条", len(l.entries)))
	l.list.Refresh()
	if l.onChange != nil {
		l.onChange(len(l.entries))
	}
}

// addLog 记一条日志：显示在“日志”页，同时存进报文库。session 是报文库里的会话，为 0 时只显示不存。
func (ws *Workspace) addLog(e logEntry, session int64) {
	ws.log.add(e)
	if r := currentRecorder(); r != nil && session != 0 {
		_ = r.Log(session, e.Event)
	}
}

// packetRing 保留最近的收发记录，出错时从里面取出这次请求的原始报文。
type packetRing struct {
	mu  sync.Mutex
	buf [64]modbus.Packet
	n   int
}

func (r *packetRing) push(p modbus.Packet) {
	r.mu.Lock()
	r.buf[r.n%len(r.buf)] = p
	r.n++
	r.mu.Unlock()
}

// exchange 从新到旧找第一条满足 match 的结果记录（响应、超时、校验错误等），返回它和对应的发送记录。
func (r *packetRing) exchange(match func(modbus.Packet) bool) (tx, res modbus.Packet, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := r.n - 1; i >= 0 && i >= r.n-len(r.buf); i-- {
		p := r.buf[i%len(r.buf)]
		if !ok {
			if p.Dir != modbus.DirTX && p.Status != modbus.StatusLate && match(p) {
				res, ok = p, true
			}
			continue
		}
		if p.Dir == modbus.DirTX && p.RequestID == res.RequestID {
			return p, res, true
		}
	}
	return tx, res, ok
}

func sameRequest(req modbus.Request) func(modbus.Packet) bool {
	return func(p modbus.Packet) bool {
		return p.Slave == req.Slave && p.Function == req.Function && p.Address == req.Address && int(p.Count) == req.Count()
	}
}

// faultKey 区分错误的种类：同一个读取窗口连续出同一种错误只记一条日志。
func faultKey(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("EXCEPTION %02X", byte(ex.Code))
	}
	for _, e := range []error{modbus.ErrTimeout, modbus.ErrCRC, modbus.ErrLRC} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return err.Error()
}

// windowText 说明是哪个读取窗口的哪条请求。
func windowText(no int, d readDef) string {
	return fmt.Sprintf("窗口 %d · Slave %d · %02X · %s", no, d.Slave, byte(d.Function), refSpan(d.area(), d.Start, d.Qty))
}

// diagnosisLines 把自动分析排成日志里的原因分析。
func diagnosisLines(dg diagnosis) []string {
	lines := []string{"分析：" + dg.Text}
	if dg.Hint != "" {
		lines = append(lines, dg.Hint)
	}
	if dg.Action != "" {
		lines = append(lines, "建议：读取窗口里可以一键“"+dg.Action+"”。")
	}
	return lines
}

// logReadFail 记一个读取窗口开始出错：自动分析的结论，加上这次请求的原始报文。
func (ws *Workspace) logReadFail(w *readWindow, d readDef, err error, tx, res modbus.Packet) {
	s := ws.session
	if s == nil || !slices.Contains(ws.windows, w) {
		return
	}
	dg := ws.diagnose(w, err)
	detail := windowText(w.no, d) + " · " + dg.Text
	ws.addLog(newLogEntry(recorder.EventReadFail, w.no, detail, diagnosisLines(dg), tx, res, ws.points), s.recID)
}

// logRecovered 记一个读取窗口恢复正常。
func (ws *Workspace) logRecovered(w *readWindow, d readDef, since time.Time, fails int) {
	s := ws.session
	if s == nil || !slices.Contains(ws.windows, w) {
		return
	}
	detail := windowText(w.no, d) + fmt.Sprintf(" · 恢复正常，出错 %d 次，持续 %s", fails, roundDur(time.Since(since)))
	ws.addLog(logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventReadOK, Window: w.no, Detail: detail}}, s.recID)
}

// connectFailCause 解释连接失败：连接被拒绝、超时、串口打不开分别给出检查方向。
func connectFailCause(cfg connConfig, err error) []string {
	if cfg.mode.Serial() {
		return []string{"分析：串口打不开（" + err.Error() + "）。",
			"检查：串口是否被别的程序或另一个主窗口占用；USB 转 485 是否插好、驱动是否正常（刚插上时等几秒，串口列表会刷新）。"}
	}
	lines := []string{"分析：" + dialErrText(err) + "。"}
	switch {
	case strings.Contains(lines[0], "拒绝"):
		lines = append(lines, "检查：IP 能到达，但这个端口没有 Modbus 服务。确认端口（默认 502）和设备的 Modbus TCP 功能已开启。")
	case strings.Contains(lines[0], "超时"):
		lines = append(lines, "检查：先 ping 设备 IP；网线、网段、网关、防火墙；设备是否上电。")
	}
	return lines
}

// logConnectFail 记一次连接失败。报文库里为它建一个会话（没有收发记录），历史报文里能看到。
func (ws *Workspace) logConnectFail(cfg connConfig, err error) {
	desc := cfg.target
	switch {
	case cfg.mode.Serial():
		desc = fmt.Sprintf("%s %d %d%s%d", cfg.serial.Port, cfg.serial.BaudRate, cfg.serial.DataBits, cfg.serial.Parity, cfg.serial.StopBits)
	case cfg.useSim:
		desc = "内置模拟器"
	}
	e := newLogEntry(recorder.EventConnectFail, 0, fmt.Sprintf("%s %s · %s", modeName[cfg.mode], desc, errSummary(err)),
		connectFailCause(cfg, err), modbus.Packet{}, modbus.Packet{}, nil)
	ws.log.add(e)
	if r := currentRecorder(); r != nil {
		if id, err := r.StartSession(cfg.mode, desc, ws.no); err == nil {
			_ = r.Log(id, e.Event)
			_ = r.EndSession(id)
		}
	}
}
