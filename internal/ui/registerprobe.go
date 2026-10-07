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
	if ws.session == nil {
		dialog.ShowError(errors.New("先连接，再检测寄存器"), ws.win)
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
	preview := widget.NewLabel("")
	preview.Wrapping = fyne.TextWrapWord
	definition := func() (readDef, error) {
		sv, err := strconv.Atoi(strings.TrimSpace(slave.Text))
		if err != nil || sv < 1 || sv > 255 {
			return readDef{}, errors.New("Slave ID 应为 1–255")
		}
		offset, err := probeOffset(addr.Text)
		if err != nil {
			return readDef{}, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(qty.Text))
		if err != nil {
			return readDef{}, errors.New("检测数量应为正整数")
		}
		nd := readDef{Slave: byte(sv), Function: funcByName(fn.Selected), Start: offset, Qty: n}
		return nd, validateProbeDef(nd)
	}
	validate := func(string) error {
		nd, err := definition()
		if err != nil {
			preview.SetText(err.Error())
		} else {
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
	validate("")
	items := []*widget.FormItem{
		widget.NewFormItem("Slave ID", slave),
		widget.NewFormItem("功能码", fn),
		widget.NewFormItem("起始 Offset（0 起始）", addr),
		widget.NewFormItem("检测数量", qty),
		widget.NewFormItem("", preview),
	}
	dlg := dialog.NewForm("检测寄存器", "开始检测", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		nd, err := definition()
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.runRegisterProbe(nil, nd)
	}, ws.win)
	dlg.Resize(fyne.NewSize(580, 0))
	dlg.Show()
}

func (ws *Workspace) runRegisterProbe(w *readWindow, d readDef) {
	if ws.probeRunning {
		return
	}
	s := ws.session
	if s == nil {
		dialog.ShowError(errors.New("先连接，再检测寄存器"), ws.win)
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
	ctx, cancel := context.WithCancel(s.ctx)
	prog := widget.NewProgressBar()
	prog.Max = float64(d.Qty)
	now := widget.NewLabel(fmt.Sprintf("已检测 0 / %d", d.Qty))
	dlg := dialog.NewCustom("检测寄存器 · 定位异常地址", "停止", container.NewVBox(
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
		rechecking := false
		res, stopErr := probe(ctx, s.client, d, func(n int) {
			uiDo(func() {
				if !ws.closed && ctx.Err() == nil && !rechecking {
					prog.SetValue(float64(n))
					now.SetText(fmt.Sprintf("已检测 %d / %d · %s", n, d.Qty, modbus.Reference(d.area(), d.Start+uint16(n-1))))
				}
			})
		}, func(offset int) {
			uiDo(func() {
				if !ws.closed && ctx.Err() == nil {
					rechecking = true
					now.SetText(fmt.Sprintf("初检完成，正在复核未响应地址 · %s", modbus.Reference(d.area(), d.Start+uint16(offset))))
				}
			})
		})
		if ctx.Err() != nil && stopErr == nil {
			stopErr = context.Canceled
		}
		uiDo(func() {
			dlg.Hide()
			ws.probeRunning = false
			if ws.closed {
				return
			}
			for _, x := range resume {
				if slices.Contains(ws.windows, x) {
					x.setPaused(false)
				}
			}
			for _, x := range ws.windows {
				x.start()
			}
			if ws.session != s || !slices.Contains(ws.windows, w) {
				w = nil
			}
			if !ws.dialogOpen() {
				ws.showProbeResult(w, d, res, stopErr)
			}
		})
	}()
}
