package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

func (ws *Workspace) showProbeResult(w *readWindow, d readDef, res []int8, stopErr error) {
	s := ws.session
	type run struct{ start, n int }
	spans := map[int8][]string{}
	counts := map[int8]int{}
	var noResponse []string
	noResponseCount := 0
	var best run
	for i := 0; i < len(res); {
		j := i
		for j < len(res) && res[j] == res[i] {
			j++
		}
		span := refSpan(d.area(), d.Start+uint16(i), j-i)
		state := res[i]
		if state == -3 {
			noResponse = append(noResponse, span)
			noResponseCount += j - i
			state = -2
		}
		spans[state] = append(spans[state], span)
		counts[state] += j - i
		if res[i] == 1 && j-i > best.n {
			best = run{i, j - i}
		}
		i = j
	}
	join := func(s []string) string {
		if len(s) == 0 {
			return "无"
		}
		return strings.Join(s, "、")
	}
	text := fmt.Sprintf("Slave %d · %s · %s（Offset %d–%d）\n\n可读（%d）：%s\n非法地址（%d，异常 02）：%s\n无法判断（%d）：%s\n未检测（%d）：%s",
		d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty), d.Start, int(d.Start)+d.Qty-1,
		counts[1], join(spans[1]), counts[-1], join(spans[-1]), counts[-2], join(spans[-2]), counts[0], join(spans[0]))
	if noResponseCount > 0 {
		text += fmt.Sprintf("\n\n未响应 / 疑似未转发（%d）：%s\n这些单地址请求超时或返回网关异常 0B；其余地址继续检测，不能仅凭未响应断定寄存器不存在。", noResponseCount, join(noResponse))
	}
	if errors.Is(stopErr, context.Canceled) {
		text += "\n\n检测已停止，保留已完成的结果；尚未完成的复核地址保留原检测结果。"
	} else if stopErr != nil {
		text += fmt.Sprintf("\n\n检测停止：%v。相关地址无法判断；剩余地址未检测。", stopErr)
	} else if counts[0] > 0 {
		text += "\n\n检测已停止，保留已完成的结果。"
	}
	text += "\n\n非法地址表示设备拒绝以当前功能码读取该单个地址，不能据此断定物理寄存器不存在。超时、设备忙或其他异常不归为非法地址。"
	label := widget.NewLabel(text)
	label.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(label)
	scroll.SetMinSize(fyne.NewSize(500, 260))
	summary := widget.NewLabelWithStyle(fmt.Sprintf("可读 %d  ·  非法地址 %d  ·  无法判断 %d  ·  未检测 %d", counts[1], counts[-1], counts[-2], counts[0]), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	if counts[-1]+counts[-2] > 0 {
		summary.Importance = widget.WarningImportance
	}
	result := newAIProbeResult(d, res, stopErr)
	aiBtn := widget.NewButton("AI分析结果", func() { ws.openAIFrom(aiTarget{probe: result}, nil) })
	body := container.NewBorder(container.NewVBox(summary, widget.NewSeparator()), container.New(flowLayout{}, widget.NewButton("复制结果", func() { ws.app.Clipboard().SetContent(text) }), aiBtn), nil, nil, scroll)
	if w == nil || best.n == 0 || best.n == d.Qty {
		dlg := dialog.NewCustom("探测结果", "关闭", body, ws.win)
		dlg.Resize(fyne.NewSize(600, 440))
		dlg.Show()
		return
	}
	apply := fmt.Sprintf("改为读取 %s", refSpan(d.area(), d.Start+uint16(best.start), best.n))
	dlg := dialog.NewCustomConfirm("探测结果", apply, "关闭", body, func(ok bool) {
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
