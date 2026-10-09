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

	"modbus-ai-studio/internal/modbus"
)

// probeStateName 是检测结果里各标记的说明。
var probeStateName = map[int8]string{1: "可读", -1: "非法地址（异常 02）", -2: "读取出错", -3: "未响应（超时或网关 0B）", 0: "未检测"}

// pointProbeText 是按点表分段检测的结果：先列读不通的点（地址、名称、类型、原因），再列各段的情况。
// 整段读不通、逐点都能读，多半是设备一次能读的数量有限，或段里有点的地址写错、跨过了设备的寄存器边界。
func pointProbeText(d readDef, res []int8, segs []probeSegment) (text, summary string, bad int) {
	stateOf := func(p point) int8 {
		off := int(p.Offset) - int(d.Start)
		for i := off; i < off+p.regs() && i < len(res); i++ {
			if res[i] != 1 {
				return res[i]
			}
		}
		return 1
	}
	counts := map[int8]int{}
	var badLines, segLines []string
	points := 0
	for _, s := range segs {
		segCounts := map[int8]int{}
		for _, p := range s.points {
			st := stateOf(p)
			counts[st]++
			segCounts[st]++
			if st != 1 && st != 0 {
				badLines = append(badLines, fmt.Sprintf("  %s %s（%s）：%s", modbus.Reference(d.area(), p.Offset), p.Name, p.Type, probeStateName[st]))
			}
		}
		points += len(s.points)
		line := fmt.Sprintf("  %s（%d 个点）：", refSpan(d.area(), d.Start+uint16(s.start), s.n), len(s.points))
		switch {
		case s.state == 0:
			line += "未检测"
		case s.state == 1:
			line += "整段可读"
		case len(s.points) == 1:
			line += probeStateName[s.state]
		default:
			var parts []string
			for _, st := range []int8{1, -1, -3, -2, 0} {
				if segCounts[st] > 0 {
					parts = append(parts, fmt.Sprintf("%s %d 个", probeStateName[st], segCounts[st]))
				}
			}
			line += fmt.Sprintf("整段读不通（%s），逐点再读：%s", probeStateName[s.state], strings.Join(parts, "、"))
			if segCounts[1] == len(s.points) {
				line += "。每个点单独都能读，多半是设备一次能读的数量有限，或段里有点跨过了设备的寄存器边界"
			}
		}
		segLines = append(segLines, line)
	}
	bad = counts[-1] + counts[-2] + counts[-3]
	text = fmt.Sprintf("Slave %d · %s · 按点表分段：%d 个点，共 %d 段\n\n", d.Slave, d.Function, points, len(segs))
	if bad == 0 {
		text += "读不通的点：无\n"
	} else {
		text += fmt.Sprintf("读不通的点（%d）：\n%s\n", bad, strings.Join(badLines, "\n"))
	}
	text += "\n各段：\n" + strings.Join(segLines, "\n")
	summary = fmt.Sprintf("%d 个点：可读 %d  ·  非法地址 %d  ·  未响应 %d  ·  读取出错 %d  ·  未检测 %d",
		points, counts[1], counts[-1], counts[-3], counts[-2], counts[0])
	return text, summary, bad
}

func (ws *Workspace) showProbeResult(w *readWindow, d readDef, res []int8, stopErr error, segs ...probeSegment) {
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
	summaryText := fmt.Sprintf("可读 %d  ·  非法地址 %d  ·  无法判断 %d  ·  未检测 %d", counts[1], counts[-1], counts[-2], counts[0])
	warn := counts[-1]+counts[-2] > 0
	if len(segs) > 0 { // 按点表分段：按点列结果，不在点表里的地址没有检测，不算“未检测”
		var bad int
		text, summaryText, bad = pointProbeText(d, res, segs)
		warn = bad > 0
	}
	if errors.Is(stopErr, context.Canceled) {
		text += "\n\n检测已停止，保留已完成的结果；尚未完成的复核地址保留原检测结果。"
	} else if stopErr != nil {
		text += fmt.Sprintf("\n\n检测停止：%v。相关地址无法判断；剩余地址未检测。", stopErr)
	} else if counts[0] > 0 && len(segs) == 0 {
		text += "\n\n检测已停止，保留已完成的结果。"
	}
	text += "\n\n非法地址表示设备拒绝以当前功能码读取该单个地址，不能据此断定物理寄存器不存在。超时、设备忙或其他异常不归为非法地址。"
	label := widget.NewLabel(text)
	label.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(label)
	scroll.SetMinSize(fyne.NewSize(500, 260))
	summary := widget.NewLabelWithStyle(summaryText, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	if warn {
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
