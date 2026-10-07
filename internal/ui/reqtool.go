package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// requestTemplates 是自定义请求的常用模板，地址和数值取自换热站示例点表。
// 名称里的功能码是十进制（FC16 = 0x10），PDU 是十六进制。
var requestTemplates = []struct{ name, pdu string }{
	{"FC03 读保持寄存器 40001 × 10", "03 00 00 00 0A"},
	{"FC03 读温差设定 40347–40348（FLOAT32 CDAB）", "03 01 5A 00 02"},
	{"FC04 读输入寄存器 30001 × 10", "04 00 00 00 0A"},
	{"FC01 读线圈 1–16", "01 00 00 00 10"},
	{"FC02 读离散输入 10001–10016", "02 00 00 00 10"},
	{"FC05 写线圈 1 = ON", "05 00 00 FF 00"},
	{"FC06 写阀门手动开度 40352 = 65.0 %（650）", "06 01 5F 02 8A"},
	{"FC16 写温差设定 40347–40348 = 15.0（FLOAT32 CDAB）", "10 01 5A 00 02 04 00 00 41 70"},
	{"FC22 掩码写 40352：只改低 4 位为 0101", "16 01 5F FF F0 00 05"},
	{"FC23 读写多个：写 40352 = 650，读 40351–40352", "17 01 5E 00 02 01 5F 00 01 02 02 8A"},
	{"FC43 读设备标识（基本：厂商、产品代码、版本）", "2B 0E 01 00"},
	{"FC43 读设备标识（常规：再加产品名称、型号）", "2B 0E 02 00"},
	{"FC17 报告从站 ID（仅串口）", "11"},
	{"FC07 读异常状态（仅串口）", "07"},
	{"FC11 读通信事件计数（仅串口）", "0B"},
	{"FC08 诊断：回送 12 34", "08 00 00 12 34"},
	{"FC08 诊断：总线报文计数", "08 00 0B 00 00"},
	{"FC08 诊断：总线通信错误计数（CRC / LRC）", "08 00 0C 00 00"},
	{"FC08 诊断：本站无响应计数", "08 00 0F 00 00"},
	{"FC08 诊断：计数器清零", "08 00 0A 00 00"},
}

// requestTool 是自定义请求窗口：发送任意 PDU（自动加 MBAP 头或 CRC），逐字段解析请求和响应，可循环发送。
// 请求和轮询共用同一条连接，排队依次发送；收发同样记录在主窗口的通信报文里。
type requestTool struct {
	ws      *Workspace
	win     fyne.Window
	slave   *widget.Entry
	pdu     *widget.Entry
	frame   *widget.Label
	result  *widget.Label
	loop    *widget.Check
	period  *widget.Entry
	sendBtn *widget.Button
	view    *inspector
	stop    context.CancelFunc // 循环发送进行中
	sending bool
	reqRows []decodeRow
}

func (ws *Workspace) openRequestTool() {
	if ws.requestWin != nil { // 只开一个，再点切到已打开的
		ws.requestWin.Show()
		ws.requestWin.RequestFocus()
		return
	}
	w := ws.app.NewWindow(fmt.Sprintf("自定义请求 · 窗口 %d", ws.no))
	w.Resize(fyne.NewSize(780, 600))
	t := &requestTool{ws: ws, win: w}
	w.SetContent(t.build())
	ws.requestWin = w
	ws.addTool(w, func() {
		t.stopLoop()
		ws.requestWin = nil
	})
	showTool(w)
}

func (t *requestTool) build() fyne.CanvasObject {
	var names []string
	for _, tp := range requestTemplates {
		names = append(names, tp.name)
	}
	tmpl := widget.NewSelect(names, func(s string) {
		for _, tp := range requestTemplates {
			if tp.name == s {
				t.pdu.SetText(tp.pdu)
			}
		}
	})
	tmpl.PlaceHolder = "常用请求模板…"
	t.slave = widget.NewEntry()
	t.slave.SetText("1")
	if len(t.ws.windows) > 0 {
		t.slave.SetText(strconv.Itoa(int(t.ws.windows[0].def.Slave)))
	}
	t.pdu = widget.NewEntry()
	t.pdu.TextStyle = fyne.TextStyle{Monospace: true}
	t.pdu.SetPlaceHolder("PDU：功能码 + 数据，例如 03 00 00 00 0A")
	t.frame = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	t.frame.Wrapping = fyne.TextWrapBreak
	t.result = widget.NewLabel("")
	t.result.Truncation = fyne.TextTruncateEllipsis
	t.loop = widget.NewCheck("循环发送", func(on bool) {
		if !on {
			t.stopLoop()
		}
	})
	t.period = widget.NewEntry()
	t.period.SetText("1000")
	t.sendBtn = widget.NewButtonWithIcon("发送", theme.MailSendIcon(), t.onSend)
	t.sendBtn.Importance = widget.HighImportance
	t.view = newInspector(t.ws)
	t.slave.OnChanged = func(string) { t.preview() }
	t.pdu.OnChanged = func(string) { t.preview() }
	tmpl.SetSelected(names[0])

	top := container.NewVBox(
		tmpl,
		container.NewBorder(nil, nil, container.NewHBox(widget.NewLabel("Slave"), fixed(56, t.slave), widget.NewLabel("PDU")), nil, t.pdu),
		t.frame,
		container.NewHBox(t.sendBtn, t.loop, widget.NewLabel("周期"), fixed(72, t.period), widget.NewLabel("ms"), layout.NewSpacer()),
		t.result,
		widget.NewSeparator(),
	)
	return container.NewBorder(top, nil, nil, nil, t.view.root)
}

func (t *requestTool) parse() (byte, []byte, error) {
	v, err := strconv.Atoi(strings.TrimSpace(t.slave.Text))
	if err != nil || v < 0 || v > 255 {
		return 0, nil, errors.New("Slave ID 应为 0–255")
	}
	pdu, err := parseHex(t.pdu.Text)
	if err != nil {
		return 0, nil, err
	}
	if len(pdu) == 0 || len(pdu) > 253 {
		return 0, nil, errors.New("PDU 应为 1–253 字节")
	}
	return byte(v), pdu, nil
}

// mode 是当前连接的模式；未连接时按主窗口选中的协议预览。
func (t *requestTool) mode() modbus.Mode {
	if s := t.ws.session; s != nil {
		return s.mode
	}
	return protoModes[t.ws.proto.Selected]
}

func (t *requestTool) preview() {
	slave, pdu, err := t.parse()
	if err != nil {
		t.frame.SetText("错误：" + err.Error())
		t.reqRows = nil
		t.view.show("请求解析", []decodeRow{{Meaning: err.Error()}})
		return
	}
	m := t.mode()
	note := "已加 CRC"
	switch {
	case m == modbus.ModeTCP:
		note = "已加 MBAP 头，事务号发送时分配"
	case m.IsASCII():
		note = "已编成字符并加 LRC"
	}
	t.frame.SetText(fmt.Sprintf("完整报文  %s（%s，%s）", frameText(m, modbus.EncodeADU(m, slave, 1, pdu)), modeName[m], note))
	t.reqRows = describePDUOnly(modbus.DirTX, nil, pdu, t.ws.points)
	t.view.show("请求解析", t.reqRows)
}

func (t *requestTool) onSend() {
	if t.stop != nil {
		t.stopLoop()
		return
	}
	if !t.loop.Checked {
		t.send()
		return
	}
	ms, err := strconv.Atoi(strings.TrimSpace(t.period.Text))
	if err != nil || ms < 20 {
		t.showError("循环周期应为不小于 20 的整数（ms）")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.stop = cancel
	t.sendBtn.SetText("停止")
	t.sendBtn.SetIcon(theme.MediaStopIcon())
	go func() {
		tick := time.NewTicker(time.Duration(ms) * time.Millisecond)
		defer tick.Stop()
		for {
			uiDo(func() {
				if t.stop != nil && !t.sending {
					t.send()
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-t.ws.done:
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *requestTool) stopLoop() {
	if t.stop == nil {
		return
	}
	t.stop()
	t.stop = nil
	t.sendBtn.SetText("发送")
	t.sendBtn.SetIcon(theme.MailSendIcon())
}

func (t *requestTool) showError(msg string) {
	t.result.SetText(msg)
	t.result.Importance = widget.DangerImportance
	t.result.Refresh()
}

func (t *requestTool) send() {
	s := t.ws.session
	if s == nil {
		t.stopLoop()
		t.showError("未连接：先在主窗口点“连接”")
		return
	}
	slave, pdu, err := t.parse()
	if err != nil {
		t.stopLoop()
		t.showError(err.Error())
		return
	}
	if t.ws.readOnly && !readOnlyPDU(pdu) {
		t.stopLoop()
		t.showError(fmt.Sprintf("只读模式下不发送 %s：它可能改变设备状态。要发送，先在主窗口“连接”菜单里关掉只读模式。", modbus.FunctionCode(pdu[0])))
		return
	}
	t.sending = true
	timeout := t.ws.timeout
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 2*timeout+5*time.Second)
		start := time.Now()
		resp, err := s.client.DoRaw(ctx, slave, pdu)
		cost := time.Since(start)
		cancel()
		uiDo(func() {
			t.sending = false
			t.showResult(slave, pdu, resp, err, cost)
		})
	}()
}

func (t *requestTool) showResult(slave byte, pdu, resp []byte, err error, cost time.Duration) {
	stamp := time.Now().Format("15:04:05.000")
	rows := append([]decodeRow{}, t.reqRows...)
	rows = append(rows, decodeRow{Meaning: " "})
	_, isEx := modbus.AsException(err)
	switch {
	case err == nil && slave == 0 && t.mode() != modbus.ModeTCP:
		t.result.SetText(stamp + "  广播已发送，从站不应答")
		t.result.Importance = widget.MediumImportance
	case err == nil || isEx:
		kind := "正常响应"
		t.result.Importance = widget.SuccessImportance
		if isEx {
			kind = "异常响应"
			t.result.Importance = widget.WarningImportance
		}
		t.result.SetText(fmt.Sprintf("%s  %s · 耗时 %s（含排队等待轮询）", stamp, kind, formatRTT(cost)))
		rows = append(rows, decodeRow{Name: "响应 PDU", Hex: "", Meaning: hexs(resp)})
		rows = append(rows, describePDUOnly(modbus.DirRX, pdu, resp, t.ws.points)...)
	default:
		t.result.SetText(fmt.Sprintf("%s  %s · 耗时 %s", stamp, errSummary(err), formatRTT(cost)))
		t.result.Importance = widget.DangerImportance
		rows = append(rows, decodeRow{Name: "结果", Meaning: errSummary(err) + "。报文详情见主窗口的通信报文。"})
	}
	t.result.Refresh()
	t.view.show("请求 / 响应解析", rows)
}
