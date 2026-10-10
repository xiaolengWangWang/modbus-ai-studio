package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// validateProbeDef allows multiple batches and validates the full range before
// allocating or sending to prevent address wrap.
func validateProbeDef(d readDef) error {
	if d.Slave == 0 {
		return errors.New("Slave ID 应为 1–255（0 是广播，没有读响应）")
	}
	if !slices.Contains(readFuncs, d.Function) {
		return errors.New("检测只支持 FC01、FC02、FC03、FC04")
	}
	if d.Qty < 1 || d.Qty > 65536-int(d.Start) {
		return fmt.Errorf("起始 Offset %d 的检测数量应为 1–%d，不能超出 65535", d.Start, 65536-int(d.Start))
	}
	return nil
}

func probeOffset(text string) (uint16, error) {
	text = strings.TrimSpace(text)
	base := 10
	if strings.HasPrefix(strings.ToLower(text), "0x") {
		text, base = text[2:], 16
	}
	n, err := strconv.ParseUint(text, base, 16)
	if err != nil {
		return 0, errors.New("起始 Offset 应为 0–65535，也可输入十六进制，例如 0x015A")
	}
	return uint16(n), nil
}

func (ws *Workspace) registerProbeDialog() {
	if ws.probeRunning {
		return
	}
	if _, err := ws.probeConfig(); err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	d := defaultDef()
	if w := ws.current(); w != nil {
		d = w.def
	}
	slave, addr, qty := intEntry(int(d.Slave)), intEntry(int(d.Start)), intEntry(d.Qty)
	var names []string
	for _, f := range readFuncs {
		names = append(names, f.String())
	}
	fn := widget.NewSelect(names, nil)
	fn.SetSelected(d.Function.String())
	mode := widget.NewSelect(probeModeNames, nil)
	initialMode := probeAuto
	if _, _, ok := pointProbeDef(ws.points, d); ok {
		initialMode = probePoints
	}
	mode.SetSelected(probeModeNames[initialMode])
	modeOf := func() probeMode { return probeMode(max(0, slices.Index(probeModeNames, mode.Selected))) }
	preview := widget.NewLabel("")
	preview.Wrapping = fyne.TextWrapWord
	// definition 是要检测的范围；按点表分段时取点表里这个功能码数据区的全部点，不用填起始地址和数量。
	definition := func() (readDef, []probeSegment, error) {
		sv, err := strconv.Atoi(strings.TrimSpace(slave.Text))
		if err != nil || sv < 1 || sv > 255 {
			return readDef{}, nil, errors.New("Slave ID 应为 1–255")
		}
		nd := readDef{Slave: byte(sv), Function: funcByName(fn.Selected)}
		if modeOf() == probePoints {
			pd, segs, ok := pointProbeDef(ws.points, nd)
			if !ok {
				return nd, nil, fmt.Errorf("点表里没有 %s 能读的点：先导入点表，或换功能码", nd.Function)
			}
			return pd, segs, validateProbeDef(pd)
		}
		offset, err := probeOffset(addr.Text)
		if err != nil {
			return readDef{}, nil, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(qty.Text))
		if err != nil {
			return readDef{}, nil, errors.New("检测数量应为正整数")
		}
		nd.Start, nd.Qty = offset, n
		return nd, nil, validateProbeDef(nd)
	}
	validate := func(string) error {
		nd, segs, err := definition()
		switch {
		case err != nil:
			preview.SetText(err.Error())
		case modeOf() == probePoints:
			points := 0
			var spans []string
			for _, s := range segs {
				points += len(s.points)
				if len(spans) < 6 {
					spans = append(spans, refSpan(nd.area(), nd.Start+uint16(s.start), s.n))
				}
			}
			if len(segs) > len(spans) {
				spans = append(spans, "……")
			}
			preview.SetText(fmt.Sprintf("按点表分段：%d 个点，地址相连的合成一段，共 %d 段：%s\n每段一次读完；读不通的段逐点再读，找出无效的点。",
				points, len(segs), strings.Join(spans, "、")))
		case modeOf() == probeSingle:
			preview.SetText(fmt.Sprintf("逐个寄存器：%s 共 %d 个地址，每个地址单独读一次，找出全部读不通的地址。\nOffset 从 0 起始，例如保持寄存器 40001 对应 Offset 0。",
				refSpan(nd.area(), nd.Start, nd.Qty), nd.Qty))
		default:
			preview.SetText(fmt.Sprintf("检测 %s · Offset %d–%d\n正常段批量验证，异常段定位到单个地址；超时后继续检测。\nOffset 从 0 起始，例如保持寄存器 40001 对应 Offset 0。", refSpan(nd.area(), nd.Start, nd.Qty), nd.Start, int(nd.Start)+nd.Qty-1))
		}
		return err
	}
	// Cross-field validation disables Start until the complete range is valid.
	for _, e := range []*widget.Entry{slave, addr, qty} {
		e.Validator = validate
		e.OnChanged = func(string) { slave.Validate(); addr.Validate(); qty.Validate() }
	}
	fn.OnChanged = func(string) { qty.Validate() }
	mode.OnChanged = func(string) {
		if modeOf() == probePoints { // 范围按点表算，起始地址和数量不用填
			addr.Disable()
			qty.Disable()
		} else {
			addr.Enable()
			qty.Enable()
		}
		slave.Validate()
		addr.Validate()
		qty.Validate()
	}
	mode.OnChanged(mode.Selected)
	items := []*widget.FormItem{
		widget.NewFormItem("Slave ID", slave),
		widget.NewFormItem("功能码", fn),
		widget.NewFormItem("检测方式", mode),
		widget.NewFormItem("起始 Offset（0 起始）", addr),
		widget.NewFormItem("检测数量", qty),
		widget.NewFormItem("", preview),
	}
	dlg := dialog.NewForm("检测寄存器", "开始检测", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		nd, segs, err := definition()
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.runRegisterProbeMode(nil, nd, modeOf(), segs)
	}, ws.win)
	dlg.Resize(fyne.NewSize(580, 0))
	dlg.Show()
}

func (ws *Workspace) runRegisterProbe(w *readWindow, d readDef) {
	ws.runRegisterProbeMode(w, d, probeAuto, nil)
}

// runRegisterProbeMode 按检测方式检测 d；按点表分段时 segs 是各段。检测期间暂停这个连接上的轮询。
func (ws *Workspace) runRegisterProbeMode(w *readWindow, d readDef, mode probeMode, segs []probeSegment) {
	if ws.probeRunning || ws.closed {
		return
	}
	s := ws.session
	cfg, err := ws.probeConfig()
	if err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	if err := validateProbeDef(d); err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	ws.probeRunning = true
	// Pause polling on this connection and remember only windows we paused.
	// Existing paused windows stay paused on success, stop, or error.
	var resume []*readWindow
	for _, x := range ws.windows {
		if !x.paused {
			resume = append(resume, x)
			x.setPaused(true)
		}
	}
	parent := context.Background()
	var reconnectDone <-chan struct{}
	if s != nil {
		parent = s.ctx
		if !cfg.mode.Serial() {
			reconnectDone = s.reconnectDone
			if s.reconnectCancel != nil {
				s.reconnectCancel()
			}
			s.client.Close()
		}
	}
	ctx, cancel := context.WithCancel(parent)
	ws.probeCancel = cancel
	if s == nil {
		ws.setInputsEnabled(false)
		ws.timeoutE.Disable()
		ws.connBtn.Disable()
	}
	ws.refreshStatus()
	total := d.Qty // 进度按寄存器数；按点表分段时只算点占的寄存器
	if mode == probePoints {
		total = 0
		for _, sg := range segs {
			total += sg.n
		}
	}
	prog := widget.NewProgressBar()
	prog.Max = float64(total)
	now := widget.NewLabel(fmt.Sprintf("已检测 0 / %d", total))
	title := map[probeMode]string{probeAuto: "定位异常地址", probeSingle: "逐个寄存器", probePoints: "按点表分段"}[mode]
	dlg := dialog.NewCustom("检测寄存器 · "+title, "停止", container.NewVBox(
		widget.NewLabel(fmt.Sprintf("Slave %d · %s · %s", d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty))), prog, now), ws.win)
	dlg.SetOnClosed(cancel)
	stop := widget.NewButton("停止", nil)
	stop.OnTapped = func() {
		cancel()
		stop.Disable()
		now.SetText("正在停止，保留已完成的结果…")
	}
	dlg.SetButtons([]fyne.CanvasObject{stop})
	dlg.Resize(fyne.NewSize(540, 0))
	dlg.Show()
	go func() {
		defer cancel()
		if reconnectDone != nil {
			select {
			case <-reconnectDone:
			case <-ctx.Done():
			}
		}
		rechecking := false
		lastProgress := 0 // 以下状态只在 UI 线程读写。
		progress := func(n int) {
			uiDo(func() {
				if !ws.closed && ctx.Err() == nil && !rechecking {
					lastProgress = n
					prog.SetValue(float64(n))
					text := fmt.Sprintf("已检测 %d / %d", n, total)
					if mode != probePoints { // 分段检测不按地址顺序推进，不显示当前地址
						text += " · " + modbus.Reference(d.area(), d.Start+uint16(n-1))
					}
					now.SetText(text)
				}
			})
		}
		var res []int8
		var stopErr error
		waiting := func(waiting bool) {
			uiDo(func() {
				if ws.closed || ctx.Err() != nil {
					return
				}
				if waiting {
					now.SetText(fmt.Sprintf("已检测 %d / %d · 设备离线，等待自动重连…（可停止）", lastProgress, total))
				} else if rechecking {
					now.SetText("连接已恢复，继续复核未响应地址…")
				} else {
					now.SetText(fmt.Sprintf("已检测 %d / %d · 连接已恢复，继续检测…", lastProgress, total))
				}
			})
		}
		var client probeClient
		var network *networkProbeClient
		if cfg.mode.Serial() {
			client = &sessionProbeClient{ws: ws, session: s, waiting: waiting}
		} else {
			network = &networkProbeClient{cfg: cfg, observer: ws, waiting: waiting}
			defer network.close()
			client = network
		}
		if mode == probeAuto {
			res, stopErr = probe(ctx, client, d, progress, func(offset int) {
				uiDo(func() {
					if !ws.closed && ctx.Err() == nil {
						rechecking = true
						now.SetText(fmt.Sprintf("初检完成，正在复核未响应地址 · %s", modbus.Reference(d.area(), d.Start+uint16(offset))))
					}
				})
			})
		} else {
			res, stopErr = probeUnits(ctx, client, d, mode, segs, progress)
		}
		if ctx.Err() != nil && stopErr == nil {
			stopErr = context.Canceled
		}
		if network != nil {
			network.close()
		}
		uiDo(func() {
			dlg.Hide()
			ws.probeRunning = false
			ws.probeCancel = nil
			if network != nil {
				ws.restoreProbeSession(s)
			}
			if ws.closed {
				return
			}
			if ws.session == nil {
				ws.setInputsEnabled(true)
				ws.timeoutE.Enable()
				ws.connBtn.Enable()
			}
			for _, x := range resume {
				if slices.Contains(ws.windows, x) {
					x.setPaused(false)
				}
			}
			for _, x := range ws.windows {
				x.start()
			}
			ws.refreshStatus()
			if ws.session != s || !slices.Contains(ws.windows, w) {
				w = nil
			}
			if !ws.dialogOpen() {
				ws.showProbeResult(w, d, res, stopErr, segs...)
			}
		})
	}()
}
