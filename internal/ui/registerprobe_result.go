package ui

import (
	"fmt"
	"slices"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

func (ws *Workspace) showProbeResult(w *readWindow, d readDef, res []int8, stopErr error, segs ...probeSegment) {
	ws.showProbeRun(w, d, res, stopErr, nil, segs...)
}

// showProbeRun 显示自动定位、逐个寄存器和按点表分段的结果；run 不为空时可以导出检测报告。
func (ws *Workspace) showProbeRun(w *readWindow, d readDef, res []int8, stopErr error, pr *probeRun, segs ...probeSegment) {
	out, bestStart, bestN := probeOutputFor(d, res, stopErr, segs...)
	ws.probeResultDialog(w, d, out, newAIProbeResult(d, res, stopErr), bestStart, bestN, pr)
}

// probeAction 是结果框里的附加按钮，点了先关结果框再执行。
type probeAction struct {
	label string
	do    func()
}

// showPointCheckResult 显示逐点独立验证的结果。这种检测从对话框启动，不对应读取窗口，不提供改为读取。
func (ws *Workspace) showPointCheckResult(d readDef, checks []pointCheck, segs []probeSegment, stopErr error, pr *probeRun) {
	result := newAIProbeResult(d, pointCheckStates(d, checks), stopErr)
	result.checks = slices.Clone(checks)
	ws.probeResultDialog(nil, d, eachPointOutput(d, checks, segs, stopErr), result, 0, 0, pr)
}

// probeResultDialog 显示检测结果；best 是常规读取可以改用的范围（相对 d.Start），n 为 0 表示不提供改为读取。
// pr 不为空时提供“导出报告”；actions 是附加按钮，例如快速排查后继续完整检测。
func (ws *Workspace) probeResultDialog(w *readWindow, d readDef, out probeOutput, result *aiProbeResult, bestStart, bestN int, pr *probeRun, actions ...probeAction) {
	s := ws.session
	best := struct{ start, n int }{bestStart, bestN}
	label := widget.NewLabel(out.Text)
	label.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(label)
	scroll.SetMinSize(fyne.NewSize(500, 260))
	summary := widget.NewLabelWithStyle(out.Summary, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	summary.Wrapping = fyne.TextWrapWord
	if out.Warn {
		summary.Importance = widget.WarningImportance
	}
	var dlg dialog.Dialog
	buttons := []fyne.CanvasObject{widget.NewButton("复制结果", func() { ws.app.Clipboard().SetContent(out.Text) }),
		widget.NewButton("AI分析结果", func() { ws.openAIFrom(aiTarget{probe: result}, nil) })}
	if pr != nil {
		buttons = append(buttons, widget.NewButton("导出报告", func() { dlg.Hide(); ws.exportReportDialog(pr, out) }))
	}
	for _, a := range actions {
		buttons = append(buttons, widget.NewButton(a.label, func() { dlg.Hide(); a.do() }))
	}
	body := container.NewBorder(container.NewVBox(summary, widget.NewSeparator()), container.New(flowLayout{}, buttons...), nil, nil, scroll)
	if w == nil || best.n == 0 || best.n == d.Qty {
		dlg = dialog.NewCustom("探测结果", "关闭", body, ws.win)
		dlg.Resize(fyne.NewSize(600, 440))
		dlg.Show()
		return
	}
	apply := fmt.Sprintf("改为读取 %s", refSpan(d.area(), d.Start+uint16(best.start), best.n))
	dlg = dialog.NewCustomConfirm("探测结果", apply, "关闭", body, func(ok bool) {
		if ok {
			if ws.closed || ws.session != s || !slices.Contains(ws.windows, w) ||
				w.def.Slave != d.Slave || w.def.Function != d.Function || w.def.Start != d.Start || w.def.Qty != d.Qty {
				if !ws.closed {
					dialog.ShowInformation("检测结果已过期", "连接或读取窗口已改变，请重新检测后再调整读取范围。", ws.win)
				}
				return
			}
			ws.redefine(w, func(nd *readDef) { nd.Start, nd.Qty = d.Start+uint16(best.start), best.n })
		}
	}, ws.win)
	dlg.Resize(fyne.NewSize(600, 440))
	dlg.Show()
}
