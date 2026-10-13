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
var probeStateName = map[int8]string{1: "可读", 2: "批量有返回（未单独验证）", -1: "非法地址（异常 02）", -2: "读取出错",
	-3: "未响应（超时或网关 0B）", -4: "非标准响应", 0: "未检测"}

// batchNote 说明批量有返回为什么不等于每个地址都能读。
const batchNote = "批量读取有返回，不能证明其中每个地址或点单独都能读：有的设备（例如 MCGS 数据转发接口）会给没有数据的地址补 0。"

// nonstandardNote 说明非标准响应。
const nonstandardNote = "非标准响应：设备回了属于这次请求的帧，但既不是正常数据也不是标准异常（例如功能码 89 原样回显请求的地址和数量），没有寄存器数据。详情见报文记录。"

// pointState 是点占的寄存器里最差的标记：有失败或未检测就取它，否则有批量返回就是 2，全部单独读通才是 1。
func pointState(d readDef, res []int8, p point) int8 {
	off := int(p.Offset) - int(d.Start)
	st := int8(1)
	for i := off; i < off+p.Regs() && i < len(res); i++ {
		if res[i] != 1 && res[i] != 2 {
			return res[i]
		}
		st = max(st, res[i])
	}
	return st
}

// pointProbeText 是按点表分段检测的结果：先列读不通的点（地址、名称、类型、原因），再列各段的情况。
// 整段读不通、逐点都能读，多半是设备一次能读的数量有限，或段里有点的地址写错、跨过了设备的寄存器边界。
func pointProbeText(d readDef, res []int8, segs []probeSegment) (text, summary string, bad int) {
	stateOf := func(p point) int8 { return pointState(d, res, p) }
	counts := map[int8]int{}
	var badLines, segLines []string
	points := 0
	for _, s := range segs {
		segCounts := map[int8]int{}
		for _, p := range s.points {
			st := stateOf(p)
			counts[st]++
			segCounts[st]++
			if st < 0 {
				badLines = append(badLines, fmt.Sprintf("  %s %s（%s）：%s", modbus.Reference(d.area(), p.Offset), p.Name, p.Type, probeStateName[st]))
			}
		}
		points += len(s.points)
		line := fmt.Sprintf("  %s（%d 个点）：", refSpan(d.area(), d.Start+uint16(s.start), s.n), len(s.points))
		switch {
		case s.state == 0:
			line += "未检测"
		case len(s.points) == 1:
			line += probeStateName[s.state]
		case s.state == 1:
			line += "整段有返回，各点未单独验证"
		default:
			var parts []string
			for _, st := range []int8{1, -1, -3, -4, -2, 0} {
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
	bad = counts[-1] + counts[-2] + counts[-3] + counts[-4]
	text = fmt.Sprintf("Slave %d · %s · 按点表分段：%d 个点，共 %d 段\n\n", d.Slave, d.Function, points, len(segs))
	if bad == 0 {
		text += "读不通的点：无\n"
	} else {
		text += fmt.Sprintf("读不通的点（%d）：\n%s\n", bad, strings.Join(badLines, "\n"))
	}
	text += "\n各段：\n" + strings.Join(segLines, "\n")
	if counts[2] > 0 {
		text += fmt.Sprintf("\n\n%d 个点所在的段整段读取有返回，这些点没有逐个单独读。%s要确认每个点，用“逐点独立验证”。", counts[2], batchNote)
	}
	if counts[-4] > 0 {
		text += "\n\n" + nonstandardNote
	}
	summary = fmt.Sprintf("%d 个点：可读 %d%s  ·  非法地址 %d  ·  未响应 %d%s  ·  读取出错 %d  ·  未检测 %d",
		points, counts[1], optCount("  ·  整段有返回 %d（未逐点验证）", counts[2]), counts[-1], counts[-3], optCount("  ·  非标准响应 %d", counts[-4]), counts[-2], counts[0])
	return text, summary, bad
}

// optCount 只在 n 不为 0 时显示这一项。
func optCount(format string, n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(format, n)
}

func (ws *Workspace) showProbeResult(w *readWindow, d readDef, res []int8, stopErr error, segs ...probeSegment) {
	ws.showProbeRun(w, d, res, stopErr, nil, segs...)
}

// showProbeRun 显示自动定位、逐个寄存器和按点表分段的结果；run 不为空时可以导出检测报告。
func (ws *Workspace) showProbeRun(w *readWindow, d readDef, res []int8, stopErr error, pr *probeRun, segs ...probeSegment) {
	out, bestStart, bestN := probeOutputFor(d, res, stopErr, segs...)
	ws.probeResultDialog(w, d, out, newAIProbeResult(d, res, stopErr), bestStart, bestN, pr)
}

// probeOutputFor 是自动定位、逐个寄存器和按点表分段的结果文字与失败清单，以及常规读取可以改用的最长可读范围。
func probeOutputFor(d readDef, res []int8, stopErr error, segs ...probeSegment) (probeOutput, int, int) {
	type run struct{ start, n int }
	spans := map[int8][]string{}
	counts := map[int8]int{}
	var noResponse []string
	noResponseCount := 0
	var best run
	readable := func(st int8) bool { return st == 1 || st == 2 }
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
		i = j
	}
	// 改为读取的范围是常规批量读取用的，单独读通和批量有返回的地址都算
	for i := 0; i < len(res); {
		j := i
		for j < len(res) && readable(res[j]) {
			j++
		}
		if j-i > best.n {
			best = run{i, j - i}
		}
		i = max(j, i+1)
	}
	join := func(s []string) string {
		if len(s) == 0 {
			return "无"
		}
		return strings.Join(s, "、")
	}
	text := fmt.Sprintf("Slave %d · %s · %s（Offset %d–%d）\n\n", d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty), d.Start, int(d.Start)+d.Qty-1)
	if counts[2] > 0 {
		text += fmt.Sprintf("批量有返回、未单独验证（%d）：%s\n", counts[2], join(spans[2]))
	}
	text += fmt.Sprintf("单独读取可读（%d）：%s\n非法地址（%d，异常 02）：%s\n", counts[1], join(spans[1]), counts[-1], join(spans[-1]))
	if counts[-4] > 0 {
		text += fmt.Sprintf("非标准响应（%d）：%s\n", counts[-4], join(spans[-4]))
	}
	text += fmt.Sprintf("无法判断（%d）：%s\n未检测（%d）：%s", counts[-2], join(spans[-2]), counts[0], join(spans[0]))
	if noResponseCount > 0 {
		text += fmt.Sprintf("\n\n未响应 / 疑似未转发（%d）：%s\n这些单地址请求超时或返回网关异常 0B；其余地址继续检测，不能仅凭未响应断定寄存器不存在。", noResponseCount, join(noResponse))
	}
	if counts[-4] > 0 {
		text += "\n\n" + nonstandardNote
	}
	if counts[2] > 0 {
		text += "\n\n" + batchNote + "要逐个确认，用“逐个寄存器”，或导入点表后用“逐点独立验证”。"
	}
	summaryText := fmt.Sprintf("%s可读 %d  ·  非法地址 %d%s  ·  无法判断 %d  ·  未检测 %d",
		optCount("批量有返回 %d（未单独验证）  ·  单独", counts[2]), counts[1], counts[-1], optCount("  ·  非标准响应 %d", counts[-4]), counts[-2], counts[0])
	warn := counts[-1]+counts[-2]+counts[-4] > 0
	out := probeOutput{csvHeader: []string{"地址", "Offset", "结果"}}
	for i, st := range res {
		if st < 0 {
			off := d.Start + uint16(i)
			out.csvRows = append(out.csvRows, []string{modbus.Reference(d.area(), off), fmt.Sprint(off), probeStateName[st]})
		}
	}
	if len(segs) > 0 { // 按点表分段：按点列结果，不在点表里的地址没有检测，不算“未检测”
		var bad int
		text, summaryText, bad = pointProbeText(d, res, segs)
		warn = bad > 0
		out.csvHeader, out.csvRows = []string{"地址", "Offset", "名称", "类型", "寄存器数", "结果"}, nil
		for _, s := range segs {
			for _, p := range s.points {
				if st := pointState(d, res, p); st < 0 {
					out.csvRows = append(out.csvRows, []string{modbus.Reference(d.area(), p.Offset), fmt.Sprint(p.Offset), p.Name, string(p.Type), fmt.Sprint(p.Regs()), probeStateName[st]})
				}
			}
		}
	}
	if errors.Is(stopErr, context.Canceled) {
		text += "\n\n检测已停止，保留已完成的结果；尚未完成的复核地址保留原检测结果。"
	} else if stopErr != nil {
		text += fmt.Sprintf("\n\n检测停止：%v。相关地址无法判断；剩余地址未检测。", stopErr)
	} else if counts[0] > 0 && len(segs) == 0 {
		text += "\n\n检测已停止，保留已完成的结果。"
	}
	text += "\n\n" + illegalNote
	out.text, out.summary, out.warn = text, summaryText, warn
	return out, best.start, best.n
}

// probeAction 是结果框里的附加按钮，点了先关结果框再执行。
type probeAction struct {
	label string
	do    func()
}

// stopText 是检测停止时附在结果后面的说明。
func stopText(stopErr error) string {
	if errors.Is(stopErr, context.Canceled) {
		return "\n\n检测已停止，保留已完成的结果；未检测的点不算读通。"
	} else if stopErr != nil {
		return fmt.Sprintf("\n\n检测停止：%v。剩余的点未检测，不算读通。", stopErr)
	}
	return ""
}

const illegalNote = "非法地址表示设备拒绝以当前功能码读取该单个地址，不能据此断定物理寄存器不存在。超时、设备忙或其他异常不归为非法地址。"

// showPointCheckResult 显示逐点独立验证的结果。这种检测从对话框启动，不对应读取窗口，不提供改为读取。
func (ws *Workspace) showPointCheckResult(d readDef, checks []pointCheck, segs []probeSegment, stopErr error, pr *probeRun) {
	result := newAIProbeResult(d, pointCheckStates(d, checks), stopErr)
	result.checks = slices.Clone(checks)
	ws.probeResultDialog(nil, d, eachPointOutput(d, checks, segs, stopErr), result, 0, 0, pr)
}

// eachPointOutput 是逐点独立验证的结果文字和失败清单。
func eachPointOutput(d readDef, checks []pointCheck, segs []probeSegment, stopErr error) probeOutput {
	text, summary, bad := eachPointText(d, checks, segs)
	out := probeOutput{text: text + stopText(stopErr) + "\n\n" + illegalNote, summary: summary, warn: bad > 0,
		csvHeader: []string{"地址", "Offset", "名称", "类型", "寄存器数", "整段读取", "整段返回原始字", "单独读取", "整段有返回、单独读不通"}}
	for _, c := range checks {
		if c.single < 0 {
			out.csvRows = append(out.csvRows, []string{modbus.Reference(d.area(), c.Offset), fmt.Sprint(c.Offset), c.Name, string(c.Type), fmt.Sprint(c.Regs()),
				probeStateName[c.batch], wordsText(c.words), probeStateName[c.single], map[bool]string{true: "是", false: "否"}[c.contradicts()]})
		}
	}
	return out
}

// wordsText 把原始字写成 16 进制，空格分隔。
func wordsText(words []uint16) string {
	var parts []string
	for _, v := range words {
		parts = append(parts, fmt.Sprintf("%04X", v))
	}
	return strings.Join(parts, " ")
}

// eachPointText 是逐点独立验证的结果：先给整段读取和逐点单独读取各自的统计，再列整段有返回、单独却读不通的点
// （附整段返回的原始字），最后列其余读不通的点。整段返回的 0 只用来提示可能是补 0，不当作真实值，也不据此断定原因。
func eachPointText(d readDef, checks []pointCheck, segs []probeSegment) (text, summary string, bad int) {
	segCounts := map[int8]int{}
	for _, s := range segs {
		segCounts[s.state]++
	}
	counts := map[int8]int{}
	var contra, other []string
	zero := 0
	for _, c := range checks {
		counts[c.single]++
		if c.single >= 0 {
			continue
		}
		line := fmt.Sprintf("  %s %s（%s）：单独读取%s", modbus.Reference(d.area(), c.Offset), c.Name, c.Type, probeStateName[c.single])
		if c.contradicts() {
			contra = append(contra, line+"；整段返回 "+wordsText(c.words))
			if c.allZero() {
				zero++
			}
			continue
		}
		other = append(other, line+"；整段"+probeStateName[c.batch])
	}
	bad = len(contra) + len(other)
	segBad := len(segs) - segCounts[1] - segCounts[0]
	text = fmt.Sprintf("Slave %d · %s · 逐点独立验证：%d 个点，%d 段\n\n整段读取：有返回 %d 段，读不通 %d 段%s\n逐点单独读取：可读 %d  ·  非法地址 %d%s  ·  未响应 %d  ·  读取出错 %d  ·  未检测 %d\n",
		d.Slave, d.Function, len(checks), len(segs), segCounts[1], segBad, optCount("，未读 %d 段", segCounts[0]),
		counts[1], counts[-1], optCount("  ·  非标准响应 %d", counts[-4]), counts[-3], counts[-2], counts[0])
	if len(contra) > 0 {
		text += fmt.Sprintf("\n整段有返回、单独读不通（%d 个点）：\n%s\n", len(contra), strings.Join(contra, "\n"))
		if zero > 0 {
			text += fmt.Sprintf("其中 %d 个点整段返回全为 0：设备可能给没有数据的地址补 0（已知 MCGS 数据转发接口有这种表现），这些点的整段读数不能当作真实值。"+
				"请到设备的转发通道里核对这些变量的连接；仅凭报文不能确定是未建通道、变量未连接还是其他配置原因。\n", zero)
		}
		if zero < len(contra) {
			text += fmt.Sprintf("其中 %d 个点整段返回有非零值：不是补 0，单独读不通另有原因，请对照报文核对。\n", len(contra)-zero)
		}
	}
	if len(other) > 0 {
		text += fmt.Sprintf("\n整段也没有返回的读不通点（%d）：\n%s\n", len(other), strings.Join(other, "\n"))
	}
	switch {
	case counts[1] == 0 && bad > 0:
		text += "\n没有一个点单独读通，缺少可读的对照：链路或设备状态不确定，不能据此判断哪些点没有数据。\n"
	case bad == 0 && counts[0] == 0:
		text += "\n全部点单独读取都有正常响应，没有发现整段读取与逐点单独读取不一致的点。读数是否对应正确的变量仍需现场核对。\n"
	}
	if counts[-4] > 0 {
		text += "\n" + nonstandardNote
	}
	summary = fmt.Sprintf("整段有返回 %d / %d 段  ·  %d 个点单独读：可读 %d  ·  读不通 %d%s  ·  未检测 %d",
		segCounts[1], len(segs), len(checks), counts[1], bad, optCount("（其中整段有返回 %d）", len(contra)), counts[0])
	return text, summary, bad
}

// probeResultDialog 显示检测结果；best 是常规读取可以改用的范围（相对 d.Start），n 为 0 表示不提供改为读取。
// pr 不为空时提供“导出报告”；actions 是附加按钮，例如快速排查后继续完整检测。
func (ws *Workspace) probeResultDialog(w *readWindow, d readDef, out probeOutput, result *aiProbeResult, bestStart, bestN int, pr *probeRun, actions ...probeAction) {
	s := ws.session
	best := struct{ start, n int }{bestStart, bestN}
	label := widget.NewLabel(out.text)
	label.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(label)
	scroll.SetMinSize(fyne.NewSize(500, 260))
	summary := widget.NewLabelWithStyle(out.summary, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	summary.Wrapping = fyne.TextWrapWord
	if out.warn {
		summary.Importance = widget.WarningImportance
	}
	var dlg dialog.Dialog
	buttons := []fyne.CanvasObject{widget.NewButton("复制结果", func() { ws.app.Clipboard().SetContent(out.text) }),
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
