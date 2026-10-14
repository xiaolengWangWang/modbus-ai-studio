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
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/eta"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/slavescan"
	"modbus-ai-studio/internal/transport"
)

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

// scanSlavesDialog 搜索总线上在应答的站号，找不到设备地址时用。算法在 internal/slavescan，Web 版、modbus-cli 共用：
// 每个站号依次读用户给的地址、40001、30001、线圈 1，有一次应答就算找到。
func (ws *Workspace) scanSlavesDialog() {
	s := ws.session
	if s == nil {
		dialog.ShowError(errors.New("先连接，再搜索站号"), ws.win)
		return
	}
	from, to, tmo := intEntry(1), intEntry(247), intEntry(200)
	addr := widget.NewEntry()
	addr.SetPlaceHolder("可不填，例如 40101")
	// 读取窗口的地址设备上多半有，默认先读它。线圈的写法解析不回线圈，没改时直接用窗口的定义
	var known slavescan.Probe
	knownText := ""
	if len(ws.windows) > 0 {
		d := ws.windows[0].def
		known = slavescan.Probe{Function: d.Function, Address: d.Start}
		knownText = strings.TrimSuffix(known.String(), fmt.Sprintf("（FC%02d）", byte(d.Function))) // 40101、线圈 5
		addr.SetText(knownText)
	}
	addrItem := widget.NewFormItem("先读这个地址", addr)
	addrItem.HintText = "设备上肯定有的点；之后再读 40001、30001、线圈 1"
	items := []*widget.FormItem{widget.NewFormItem("起始 Slave", from), widget.NewFormItem("结束 Slave", to),
		widget.NewFormItem("每次读取等待（ms）", tmo), addrItem}
	dialog.ShowForm("搜索站号", "开始", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		f, t, timeout := atoi(from), atoi(to), time.Duration(atoi(tmo))*time.Millisecond
		if err := slavescan.Validate(f, t, timeout); err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		var extra []slavescan.Probe
		switch a := strings.TrimSpace(addr.Text); {
		case a == "":
		case knownText != "" && a == knownText:
			extra = append(extra, known)
		default:
			p, err := slavescan.ParseProbe(a)
			if err != nil {
				dialog.ShowError(fmt.Errorf("地址 %s：%w", a, err), ws.win)
				return
			}
			extra = append(extra, p)
		}
		ws.runSlaveScan(s, f, t, timeout, slavescan.Probes(extra...))
	}, ws.win)
}

func (ws *Workspace) runSlaveScan(s *session, from, to int, tmo time.Duration, probes []slavescan.Probe) {
	ctx, cancel := context.WithCancel(s.ctx)
	prog := widget.NewProgressBar()
	prog.Min, prog.Max = float64(from-1), float64(to)
	found := widget.NewLabel("找到：—")
	var names []string
	for _, p := range probes {
		names = append(names, p.String())
	}
	intro := widget.NewLabel(fmt.Sprintf("Slave %d–%d，每个站号依次读 %s，有一次应答就算找到；每次最多等 %d ms，都不应答时最多要 %s",
		from, to, strings.Join(names, "、"), tmo.Milliseconds(), eta.Rough(slavescan.Worst(from, to, tmo, len(probes)))))
	intro.Wrapping = fyne.TextWrapWord
	dlg := dialog.NewCustom("搜索站号", "停止", container.NewVBox(intro, prog, found), ws.win)
	dlg.Resize(fyne.NewSize(480, 0))
	dlg.SetOnClosed(cancel)
	dlg.Show()
	// 搜索期间读取窗口不发请求：要搜站号时窗口用的站号多半不对，每次读取都要等到超时，
	// 搜索的请求会排在它们后面，应答时间也算长。先等在途的读取结束（pollGate）
	ws.scanRunning = true
	for _, w := range ws.windows {
		w.refreshState()
	}
	found.SetText("等读取窗口正在发的请求结束（窗口用的站号不对时要等到超时）…")
	go func() {
		ws.pollGate.Lock()
		defer ws.pollGate.Unlock()
		started := time.Now()
		hits, err := slavescan.Scan(ctx, s.client, from, to, tmo, probes, func(id int, hits []slavescan.Hit) {
			uiDo(func() {
				prog.SetValue(float64(id))
				found.SetText("找到：" + hitIDs(hits) + "\n" + progressText(float64(id-from+1)/float64(to-from+1), started))
			})
		})
		stopped := ctx.Err() != nil
		cancel()
		uiDo(func() {
			ws.scanRunning = false
			for _, w := range ws.windows {
				w.refreshState()
			}
			dlg.Hide()
			if !ws.closed {
				ws.showSlaveScanResult(hits, err, stopped)
			}
		})
	}()
}

func hitIDs(hits []slavescan.Hit) string {
	if len(hits) == 0 {
		return "—"
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, strconv.Itoa(h.ID))
	}
	return strings.Join(ids, "、")
}

// showSlaveScanResult 列出找到的站号；有读取窗口时可以一键让全部读取窗口改用找到的站号。
func (ws *Workspace) showSlaveScanResult(hits []slavescan.Hit, err error, stopped bool) {
	var lines []string
	for _, h := range hits {
		lines = append(lines, fmt.Sprintf("Slave %d：%s · %s", h.ID, h.Note(), formatRTT(h.RTT)))
	}
	switch {
	case err != nil:
		lines = append(lines, "搜索中断："+err.Error())
	case stopped:
		lines = append(lines, "已停止，上面是已经搜到的站号。")
	case len(hits) == 0:
		lines = append(lines, "没有设备应答。检查接线、串口参数和协议，或把每次读取的等待时间加大。")
	}
	body := widget.NewLabel(strings.Join(lines, "\n"))
	body.Wrapping = fyne.TextWrapWord
	var others []string // 和读取窗口现在用的不同的站号
	if len(ws.windows) > 0 {
		for _, h := range hits {
			if h.ID != ws.firstSlave() {
				others = append(others, fmt.Sprintf("Slave %d", h.ID))
			}
		}
	}
	if len(others) == 0 {
		dlg := dialog.NewCustom("搜索结果", "关闭", body, ws.win)
		dlg.Resize(fyne.NewSize(480, 0))
		dlg.Show()
		return
	}
	pick := widget.NewSelect(others, nil)
	pick.SetSelectedIndex(0)
	content := container.NewVBox(body, container.NewBorder(nil, nil, widget.NewLabel("读取窗口改用"), nil, pick))
	if len(others) == 1 {
		content = container.NewVBox(body) // 只有一个可选时按钮上直接写站号
	}
	confirm := "读取窗口改用 " + others[0]
	if len(others) > 1 {
		confirm = "改用所选站号"
	}
	dlg := dialog.NewCustomConfirm("搜索结果", confirm, "关闭", content, func(ok bool) {
		var id int
		if ok && pick.Selected != "" {
			fmt.Sscanf(pick.Selected, "Slave %d", &id)
			ws.setSlave(0, byte(id))
		}
	}, ws.win)
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
	started := time.Now()
	go func() {
		found, baud, format, err := scanSerial(ctx, mode, open, port, slave, tmo, ws, func(n, total int, try string) {
			uiDo(func() {
				prog.SetValue(float64(n) / float64(total))
				now.SetText("正在试 " + try + "\n" + progressText(float64(n)/float64(total), started))
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
				dialog.ShowInformation("扫描串口参数", fmt.Sprintf("所有组合都没有应答。检查 Slave ID %d、A/B 接线和终端电阻；也可能设备地址不是 %d，连接后用“搜索站号”；也可能协议不对（RTU / ASCII）。", slave, slave), ws.win)
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
