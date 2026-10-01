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
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
)

// slaveHit 是扫描到的一个从站；err 为 nil 表示正常响应，否则是异常响应（同样说明设备在线）。
type slaveHit struct {
	id  int
	err error
	rtt time.Duration
}

// scanSlaves 向 from–to 逐个发 FC03 读 40001 一个寄存器。连接断开时停止并返回错误。
func scanSlaves(ctx context.Context, c *modbus.Client, from, to int, timeout time.Duration, progress func(id int, hits []slaveHit)) ([]slaveHit, error) {
	var hits []slaveHit
	for id := from; id <= to && ctx.Err() == nil; id++ {
		start := time.Now()
		_, err := c.Do(ctx, modbus.Request{Slave: byte(id), Function: modbus.FuncReadHoldingRegisters, Quantity: 1, Timeout: timeout})
		if _, isEx := modbus.AsException(err); err == nil || isEx {
			hits = append(hits, slaveHit{id, err, time.Since(start)})
		} else if errors.Is(err, modbus.ErrConnection) {
			return hits, err
		}
		progress(id, hits)
	}
	return hits, nil
}

var scanBauds = []int{9600, 19200, 38400, 57600, 115200, 4800, 2400, 1200}

// serialConfig 把“8E1”这类写法拆成数据位、校验位、停止位。
func serialConfig(port string, baud int, format string) transport.SerialConfig {
	return transport.SerialConfig{Port: port, BaudRate: baud, DataBits: int(format[0] - '0'), Parity: format[1:2], StopBits: int(format[2] - '0')}
}

// scanSerial 依次用常见波特率和数据格式打开串口，读 slave 的 1 个寄存器，返回第一个有应答（含异常响应）的组合；
// baud 为 0 表示都没应答。先试当前模式，都不应答再试另一种（RTU ⇄ ASCII）：选错模式的设备根本不回，
// 只有换模式才能发现。每种波特率先试 8N1（ASCII 先试 7E1），因为 8N1 的设备往往也能听懂 8N2。
func scanSerial(ctx context.Context, first modbus.Mode, open func(transport.SerialConfig) (modbus.Transport, error), port string, slave byte,
	timeout time.Duration, obs modbus.Observer, progress func(n, total int, try string)) (mode modbus.Mode, baud int, format string, err error) {
	modes := []modbus.Mode{modbus.ModeRTU, modbus.ModeASCII}
	if first.IsASCII() {
		modes = []modbus.Mode{modbus.ModeASCII, modbus.ModeRTU}
	}
	n, total := 0, len(scanBauds)*(len(rtuFormats)+len(asciiFormats))
	for _, m := range modes {
		formats := rtuFormats
		if m.IsASCII() {
			formats = asciiFormats
		}
		for _, b := range scanBauds {
			for _, f := range formats {
				if ctx.Err() != nil {
					return "", 0, "", nil
				}
				n++
				progress(n, total, fmt.Sprintf("%s %d %s", modeName[m], b, f))
				t, err := open(serialConfig(port, b, f))
				if err != nil {
					return "", 0, "", err
				}
				c := modbus.NewClient(t, modbus.Options{Mode: m, Timeout: timeout, Observer: obs})
				_, err = c.Do(ctx, modbus.Request{Slave: slave, Function: modbus.FuncReadHoldingRegisters, Quantity: 1})
				c.Close()
				if _, isEx := modbus.AsException(err); err == nil || isEx {
					return m, b, f, nil
				}
			}
		}
	}
	return "", 0, "", nil
}

func intEntry(v int) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(strconv.Itoa(v))
	return e
}

func atoi(e *widget.Entry) int {
	v, _ := strconv.Atoi(strings.TrimSpace(e.Text))
	return v
}

func (ws *Workspace) firstSlave() int {
	if len(ws.windows) > 0 {
		return int(ws.windows[0].def.Slave)
	}
	return 1
}

// scanSlavesDialog 扫描总线上有哪些从站地址在应答，找不到设备地址时用。
func (ws *Workspace) scanSlavesDialog() {
	s := ws.session
	if s == nil {
		dialog.ShowError(errors.New("先连接，再扫描从站地址"), ws.win)
		return
	}
	from, to, tmo := intEntry(1), intEntry(247), intEntry(200)
	items := []*widget.FormItem{widget.NewFormItem("起始 Slave", from), widget.NewFormItem("结束 Slave", to),
		widget.NewFormItem("每个地址等待（ms）", tmo)}
	dialog.ShowForm("扫描从站地址", "开始", "取消", items, func(ok bool) {
		f, t, ms := atoi(from), atoi(to), atoi(tmo)
		switch {
		case !ok:
			return
		case f < 1 || t > 255 || f > t:
			dialog.ShowError(errors.New("Slave 范围应在 1–255 之间，起始不大于结束"), ws.win)
			return
		case ms < 20 || ms > 10000:
			dialog.ShowError(errors.New("等待时间应为 20–10000 ms"), ws.win)
			return
		}
		ws.runSlaveScan(s, f, t, time.Duration(ms)*time.Millisecond)
	}, ws.win)
}

func (ws *Workspace) runSlaveScan(s *session, from, to int, tmo time.Duration) {
	ctx, cancel := context.WithCancel(s.ctx)
	prog := widget.NewProgressBar()
	prog.Min, prog.Max = float64(from-1), float64(to)
	found := widget.NewLabel("找到：—")
	dlg := dialog.NewCustom("扫描从站地址", "停止", container.NewVBox(
		widget.NewLabel(fmt.Sprintf("FC03 读 40001，Slave %d–%d，每个最多等 %d ms", from, to, tmo.Milliseconds())), prog, found), ws.win)
	dlg.SetOnClosed(cancel)
	dlg.Show()
	go func() {
		hits, err := scanSlaves(ctx, s.client, from, to, tmo, func(id int, hits []slaveHit) {
			uiDo(func() {
				prog.SetValue(float64(id))
				found.SetText("找到：" + hitIDs(hits))
			})
		})
		stopped := ctx.Err() != nil
		cancel()
		uiDo(func() {
			dlg.Hide()
			if !ws.closed {
				ws.showSlaveScanResult(hits, err, stopped)
			}
		})
	}()
}

func hitIDs(hits []slaveHit) string {
	if len(hits) == 0 {
		return "—"
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, strconv.Itoa(h.id))
	}
	return strings.Join(ids, "、")
}

func (ws *Workspace) showSlaveScanResult(hits []slaveHit, err error, stopped bool) {
	var lines []string
	for _, h := range hits {
		r := "正常响应 · " + formatRTT(h.rtt)
		if h.err != nil {
			r = errSummary(h.err) + "（设备在线，只是 40001 不可读）"
		}
		lines = append(lines, fmt.Sprintf("Slave %d：%s", h.id, r))
	}
	switch {
	case err != nil:
		lines = append(lines, "扫描中断："+err.Error())
	case stopped:
		lines = append(lines, "已停止。")
	case len(hits) == 0:
		lines = append(lines, "没有设备应答。检查接线、串口参数和协议，或把每个地址的等待时间加大。")
	}
	body := widget.NewLabel(strings.Join(lines, "\n"))
	body.Wrapping = fyne.TextWrapWord
	// 只找到一个设备、而读取窗口用的是别的地址时，一键改过去
	if len(hits) == 1 && len(ws.windows) > 0 && ws.firstSlave() != hits[0].id {
		id := hits[0].id
		dlg := dialog.NewCustomConfirm("扫描结果", fmt.Sprintf("读取窗口改用 Slave %d", id), "关闭", body, func(ok bool) {
			if !ok {
				return
			}
			for _, w := range append([]*readWindow(nil), ws.windows...) {
				ws.redefine(w, func(d *readDef) { d.Slave = byte(id) })
			}
		}, ws.win)
		dlg.Resize(fyne.NewSize(480, 0))
		dlg.Show()
		return
	}
	dlg := dialog.NewCustom("扫描结果", "关闭", body, ws.win)
	dlg.Resize(fyne.NewSize(480, 0))
	dlg.Show()
}

// scanSerialDialog 不知道设备的波特率和校验位时逐个试。要独占串口，已连接时先断开，找到后按新参数重新连接。
func (ws *Workspace) scanSerialDialog() {
	if !ws.serialMode() {
		dialog.ShowInformation("扫描串口参数", "串口参数扫描只用于“RTU 串口”和“ASCII 串口”。网络设备用“识别”判断协议。", ws.win)
		return
	}
	port := ws.port.Selected
	if port == "" {
		dialog.ShowError(errors.New("先选择串口"), ws.win)
		return
	}
	slave, tmo := intEntry(ws.firstSlave()), intEntry(300)
	items := []*widget.FormItem{widget.NewFormItem("Slave ID", slave), widget.NewFormItem("每种组合等待（ms）", tmo)}
	dialog.ShowForm("扫描串口参数", "开始", "取消", items, func(ok bool) {
		id, ms := atoi(slave), atoi(tmo)
		switch {
		case !ok:
			return
		case id < 1 || id > 247:
			dialog.ShowError(errors.New("Slave ID 应为 1–247"), ws.win)
			return
		case ms < 50 || ms > 10000:
			dialog.ShowError(errors.New("等待时间应为 50–10000 ms"), ws.win)
			return
		}
		reconnect := ws.session != nil
		ws.disconnect()
		if err := claimPort(port, ws.no); err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.runSerialScan(protoModes[ws.proto.Selected], port, byte(id), time.Duration(ms)*time.Millisecond, reconnect)
	}, ws.win)
}

func (ws *Workspace) runSerialScan(mode modbus.Mode, port string, slave byte, tmo time.Duration, reconnect bool) {
	ctx, cancel := context.WithCancel(context.Background())
	prog := widget.NewProgressBar()
	now := widget.NewLabel("")
	dlg := dialog.NewCustom("扫描串口参数", "停止", container.NewVBox(
		widget.NewLabel(fmt.Sprintf("%s · Slave %d · 先试 %s 的常见波特率 × 数据格式，再试另一种模式", port, slave, modeName[mode])), prog, now), ws.win)
	dlg.SetOnClosed(cancel)
	dlg.Show()
	open := func(cfg transport.SerialConfig) (modbus.Transport, error) {
		s, err := transport.OpenSerial(cfg)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	go func() {
		found, baud, format, err := scanSerial(ctx, mode, open, port, slave, tmo, ws, func(n, total int, try string) {
			uiDo(func() {
				prog.SetValue(float64(n) / float64(total))
				now.SetText("正在试 " + try)
			})
		})
		stopped := ctx.Err() != nil
		cancel()
		releasePort(port)
		uiDo(func() {
			dlg.Hide()
			if ws.closed {
				return
			}
			switch {
			case err != nil:
				dialog.ShowError(err, ws.win)
			case baud != 0:
				msg := fmt.Sprintf("找到：%s %d %s，Slave %d 有应答，已填入连接栏。", modeName[found], baud, format, slave)
				if found != mode {
					msg += "\n注意：设备用的是 " + modeName[found] + "，不是原来选的 " + modeName[mode] + "。"
					ws.proto.SetSelected(protoName(found))
				}
				ws.baud.SetText(strconv.Itoa(baud))
				ws.frameFmt.SetSelected(format)
				dialog.ShowInformation("扫描串口参数", msg, ws.win)
				if reconnect {
					ws.connect()
				}
			case !stopped:
				dialog.ShowInformation("扫描串口参数", fmt.Sprintf("所有组合都没有应答。检查 Slave ID %d、A/B 接线和终端电阻；也可能设备地址不是 %d，连接后用“扫描从站地址”；也可能协议不对（RTU / ASCII）。", slave, slave), ws.win)
			}
		})
	}()
}

// diagCount 是一个 FC08 诊断计数器。
type diagCount struct {
	sub, n int
}

// readDiagCounters 用 FC08 依次读设备的通信计数器（子功能 0B–12）。设备不支持时第一条就返回异常。
func readDiagCounters(ctx context.Context, c *modbus.Client, slave byte) ([]diagCount, error) {
	var out []diagCount
	for sub := 0x0B; sub <= 0x12; sub++ {
		resp, err := c.DoRaw(ctx, slave, []byte{byte(modbus.FuncDiagnostics), 0x00, byte(sub), 0x00, 0x00})
		if err != nil {
			return out, err
		}
		if len(resp) != 5 {
			return out, fmt.Errorf("子功能 %02X 的响应长度 %d 不对", sub, len(resp))
		}
		out = append(out, diagCount{sub, int(resp[3])<<8 | int(resp[4])})
	}
	return out, nil
}

// diagCountersDialog 读取并显示设备的通信计数器，可一键清零。排查 485 总线干扰、丢帧时用。
func (ws *Workspace) diagCountersDialog() {
	s := ws.session
	if s == nil {
		dialog.ShowError(errors.New("先连接，再读取诊断计数器"), ws.win)
		return
	}
	slave := intEntry(ws.firstSlave())
	dialog.ShowForm("读取诊断计数器", "读取", "取消", []*widget.FormItem{widget.NewFormItem("Slave ID", slave)}, func(ok bool) {
		id := atoi(slave)
		if !ok {
			return
		}
		if id < 1 || id > 247 {
			dialog.ShowError(errors.New("Slave ID 应为 1–247"), ws.win)
			return
		}
		go func() {
			counts, err := readDiagCounters(s.ctx, s.client, byte(id))
			uiDo(func() {
				if !ws.closed {
					ws.showDiagCounters(s, byte(id), counts, err)
				}
			})
		}()
	}, ws.win)
}

func (ws *Workspace) showDiagCounters(s *session, slave byte, counts []diagCount, err error) {
	if ex, ok := modbus.AsException(err); ok && len(counts) == 0 {
		dialog.ShowInformation("诊断计数器", fmt.Sprintf("Slave %d 回了异常 %02X %s：设备不支持 FC08 诊断。串口设备多数支持，Modbus TCP 设备通常不支持。",
			slave, byte(ex.Code), ex.Code.Name()), ws.win)
		return
	}
	var lines []string
	for _, c := range counts {
		lines = append(lines, fmt.Sprintf("%02X %s：%d", c.sub, diagSubNames[c.sub], c.n))
	}
	if err != nil {
		lines = append(lines, "读取中断："+errSummary(err))
	}
	lines = append(lines, "", "通信错误计数一直在涨，说明总线上有干扰或波特率 / 校验位不一致；无响应计数涨说明设备收到了但没回（多为广播或只听模式）。")
	body := widget.NewLabel(strings.Join(lines, "\n"))
	body.Wrapping = fyne.TextWrapWord
	if ws.readOnly { // 清零会改设备里的计数器
		dlg := dialog.NewCustom(fmt.Sprintf("诊断计数器 · Slave %d", slave), "关闭", body, ws.win)
		dlg.Resize(fyne.NewSize(520, 0))
		dlg.Show()
		return
	}
	dlg := dialog.NewCustomConfirm(fmt.Sprintf("诊断计数器 · Slave %d", slave), "计数器清零", "关闭", body, func(clear bool) {
		if !clear {
			return
		}
		go func() {
			_, err := s.client.DoRaw(s.ctx, slave, []byte{byte(modbus.FuncDiagnostics), 0x00, 0x0A, 0x00, 0x00})
			uiDo(func() {
				if err != nil {
					dialog.ShowError(fmt.Errorf("清零失败：%s", errSummary(err)), ws.win)
					return
				}
				dialog.ShowInformation("诊断计数器", "已清零。", ws.win)
			})
		}()
	}, ws.win)
	dlg.Resize(fyne.NewSize(520, 0))
	dlg.Show()
}

// probeRange 逐个地址读 1 个寄存器（或位），找出读取范围里哪些地址可读，定位异常 02 的来源。
// 探测期间暂停该窗口的轮询；遇到超时等非异常错误就停止，因为那说明问题不在地址。
func (ws *Workspace) probeRange(w *readWindow) {
	s := ws.session
	if s == nil {
		return
	}
	d := w.def
	w.setPaused(true)
	ctx, cancel := context.WithCancel(s.ctx)
	prog := widget.NewProgressBar()
	prog.Max = float64(d.Qty)
	dlg := dialog.NewCustom("逐个探测可读地址", "停止", container.NewVBox(
		widget.NewLabel(fmt.Sprintf("Slave %d · %s · 逐个读取 %s", d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty))), prog), ws.win)
	dlg.SetOnClosed(cancel)
	dlg.Show()
	go func() {
		defer cancel()
		res, stopErr := probe(ctx, s.client, d, func(n int) { uiDo(func() { prog.SetValue(float64(n)) }) })
		uiDo(func() {
			dlg.Hide()
			if !ws.closed {
				ws.showProbeResult(w, d, res, stopErr)
			}
		})
	}()
}

// probe 逐个读取 d 范围内的地址：1 表示可读，-1 表示返回异常，0 表示未探测。
func probe(ctx context.Context, c *modbus.Client, d readDef, progress func(int)) ([]int8, error) {
	res := make([]int8, d.Qty)
	for i := 0; i < d.Qty; i++ {
		_, err := c.Do(ctx, modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start + uint16(i), Quantity: 1})
		if ctx.Err() != nil {
			return res, nil
		}
		if _, isEx := modbus.AsException(err); isEx {
			res[i] = -1
		} else if err != nil {
			return res, fmt.Errorf("%s %s", modbus.Reference(d.area(), d.Start+uint16(i)), errSummary(err))
		} else {
			res[i] = 1
		}
		progress(i + 1)
	}
	return res, nil
}

func (ws *Workspace) showProbeResult(w *readWindow, d readDef, res []int8, stopErr error) {
	type run struct{ start, n int }
	var good, bad []string
	var best run
	for i := 0; i < len(res); {
		j := i
		for j < len(res) && res[j] == res[i] {
			j++
		}
		span := refSpan(d.area(), d.Start+uint16(i), j-i)
		switch res[i] {
		case 1:
			good = append(good, span)
			if j-i > best.n {
				best = run{i, j - i}
			}
		case -1:
			bad = append(bad, span)
		}
		i = j
	}
	join := func(s []string) string {
		if len(s) == 0 {
			return "无"
		}
		out := s[0]
		for _, x := range s[1:] {
			out += "、" + x
		}
		return out
	}
	text := fmt.Sprintf("可读：%s\n不可读（异常）：%s", join(good), join(bad))
	if stopErr != nil {
		text += fmt.Sprintf("\n\n探测在 %v 停止：这不是地址问题，先按超时 / 连接问题排查。", stopErr)
	}
	body := widget.NewLabel(text)
	body.Wrapping = fyne.TextWrapWord
	resume := func() {
		if w.paused {
			w.setPaused(false)
		}
	}
	if best.n == 0 || best.n == d.Qty {
		dlg := dialog.NewCustom("探测结果", "关闭", body, ws.win)
		dlg.SetOnClosed(resume)
		dlg.Resize(fyne.NewSize(480, 0))
		dlg.Show()
		return
	}
	apply := fmt.Sprintf("改为读取 %s", refSpan(d.area(), d.Start+uint16(best.start), best.n))
	dlg := dialog.NewCustomConfirm("探测结果", apply, "关闭", body, func(ok bool) {
		if ok {
			w.paused = false
			w.pauseBtn.SetIcon(theme.MediaPauseIcon())
			ws.redefine(w, func(d *readDef) { d.Start, d.Qty = d.Start+uint16(best.start), best.n })
			return
		}
		resume()
	}, ws.win)
	dlg.Resize(fyne.NewSize(480, 0))
	dlg.Show()
}
