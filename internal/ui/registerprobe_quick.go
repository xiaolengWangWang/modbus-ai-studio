package ui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"modbus-ai-studio/internal/modbus"
)

// quickBudget 是快速排查的验证请求上限：候选点、控制点和边界对照都算，开头批量读取这个范围的请求不算。
// 16 是按现场每次请求几百毫秒估的初始值，不是协议常数。
const quickBudget = 16

// quickCandidates 是最多抽查几个全 0 段。
const quickCandidates = 4

// quickUnit 是快速排查的一个读取单位：点表里的一个点（按它的宽度），没有点表时是单个寄存器。
type quickUnit struct {
	off, n int    // 相对检测范围起点
	p      *point // 没有点表时为空
	batch  int8   // 所在批量读取的结果，跨两次批量读取时取较差的
	words  []uint16
}

func (u quickUnit) zero() bool {
	return u.batch == 1 && !slices.ContainsFunc(u.words, func(w uint16) bool { return w != 0 })
}

// quickCheck 是一次单独读取：读的是哪个单位、结果如何。
type quickCheck struct {
	unit  int
	state int8
	run   [2]int // 候选点所在全 0 段（相对起点的寄存器范围）
}

// quickBoundary 是边界对照：起点不变，A 读到全 0 段末尾，B 只多读一个已单独读通的控制点，交替各读两次。
type quickBoundary struct {
	start, a, b int
	ctrl        int     // 控制点
	results     [4]int8 // A、B、A、B
	bZero       bool    // B 在 A 范围内返回的全是 0
}

// consistent 表示两轮结果一致：A 两次都读不通，B 两次都正常返回。
func (b quickBoundary) consistent() bool {
	return b.results[0] < 0 && b.results[2] < 0 && b.results[1] == 1 && b.results[3] == 1
}

type quickResult struct {
	units      []quickUnit
	batches    []int8
	runs       int // 批量返回全为 0 的连续段数
	zeroUnits  int
	candidates []quickCheck
	control    *quickCheck
	boundary   *quickBoundary
	budget     int  // 验证请求上限
	used       int  // 已用的验证请求
	exhausted  bool // 验证请求用完，计划的读取没有做完
}

// quickUnits 取点表里完整落在范围内的点作读取单位（地址重叠的点只取前一个）；范围内没有点时按单个寄存器。
func quickUnits(pts pointTable, d readDef) []quickUnit {
	var us []quickUnit
	end := 0
	for _, p := range pts.List() {
		off := int(p.Offset) - int(d.Start)
		if p.Area != d.area() || off < end || off+p.Regs() > d.Qty {
			continue
		}
		p := p
		us = append(us, quickUnit{off: off, n: p.Regs(), p: &p})
		end = off + p.Regs()
	}
	if len(us) == 0 {
		for i := range d.Qty {
			us = append(us, quickUnit{off: i, n: 1})
		}
	}
	return us
}

// quickCheckRange 快速排查批量读与单独读的差异：先批量读整个范围，记下原始值；从批量返回全为 0 的连续段里
// 各抽首个单位单独读；有读不通的，读紧挨在该段后面的单位作控制点，控制点读通就做边界对照。验证请求不超过 quickBudget。
// 只抽样，不能列出全部读不通的点；零值只用来挑候选，不当作结论。
func quickCheckRange(ctx context.Context, c probeClient, d readDef, pts pointTable, budget int, progress func(int)) (*quickResult, error) {
	q := &quickResult{units: quickUnits(pts, d), budget: budget}
	done := 0
	step := func() { done++; progress(done) }
	stop := func(err error) (*quickResult, error) {
		if ctx.Err() != nil {
			return q, nil
		}
		return q, err
	}
	// 批量读取：按寄存器记下结果，点跨两次批量读取时也能拼起来
	regState := make([]int8, d.Qty)
	regWords := make([]uint16, d.Qty)
	for start := 0; start < d.Qty; start += d.maxQty() {
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		n := min(d.Qty-start, d.maxQty())
		st, words, err := probeRead(withPhase(ctx, phaseBatch), c, d, start, n)
		q.batches = append(q.batches, st)
		for i := range n {
			regState[start+i] = st
			if st == 1 {
				regWords[start+i] = words[i]
			}
		}
		step()
		if err != nil {
			return stop(err)
		}
	}
	for i := range q.units {
		u := &q.units[i]
		u.batch = 1
		for r := u.off; r < u.off+u.n; r++ {
			if regState[r] != 1 {
				u.batch = regState[r]
				break
			}
		}
		if u.batch == 1 {
			u.words = slices.Clone(regWords[u.off : u.off+u.n])
		}
	}
	// 全 0 的连续段：首尾相接、批量返回全为 0 的单位
	type zeroRun struct{ first, last int }
	var runs []zeroRun
	for i, u := range q.units {
		if !u.zero() {
			continue
		}
		q.zeroUnits++
		if k := len(runs); k > 0 && runs[k-1].last == i-1 && q.units[i-1].off+q.units[i-1].n == u.off {
			runs[k-1].last = i
		} else {
			runs = append(runs, zeroRun{i, i})
		}
	}
	q.runs = len(runs)
	span := func(r zeroRun) [2]int { return [2]int{q.units[r.first].off, q.units[r.last].off + q.units[r.last].n} }
	slices.SortStableFunc(runs, func(a, b zeroRun) int { return cmp.Compare(span(b)[1]-span(b)[0], span(a)[1]-span(a)[0]) })
	runs = runs[:min(len(runs), quickCandidates)]

	spend := func() bool {
		if q.used >= q.budget {
			q.exhausted = true
			return false
		}
		q.used++
		return true
	}
	single := func(phase string, i int) (int8, error) {
		u := q.units[i]
		st, _, err := probeRead(withPhase(ctx, phase), c, d, u.off, u.n)
		step()
		return st, err
	}
	tried := map[int]int8{} // 读过的控制点及结果
	control := func(i int) (int8, error) {
		if st, ok := tried[i]; ok {
			return st, nil
		}
		if !spend() {
			return 0, nil
		}
		st, err := single(phaseControl, i)
		tried[i] = st
		if q.control == nil || q.control.state != 1 {
			q.control = &quickCheck{unit: i, state: st}
		}
		return st, err
	}
	for _, r := range runs {
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		if !spend() {
			break
		}
		st, err := single(phaseCandidate, r.first)
		q.candidates = append(q.candidates, quickCheck{unit: r.first, state: st, run: span(r)})
		if err != nil {
			return stop(err)
		}
		if st >= 0 {
			continue
		}
		// 控制点：先试紧挨在全 0 段后面、批量返回非零的单位，读通了还可以做边界对照；不行再试离得最近的其他非零单位
		next := r.last + 1
		adjacent := next < len(q.units) && q.units[next].off == span(r)[1] && q.units[next].batch == 1 && !q.units[next].zero()
		if adjacent && q.boundary == nil || q.control == nil || q.control.state != 1 {
			var choices []int
			if adjacent {
				choices = append(choices, next)
			}
			if q.control == nil || q.control.state != 1 {
				var others []int
				for i, u := range q.units {
					if u.batch == 1 && !u.zero() && i != next {
						others = append(others, i)
					}
				}
				slices.SortFunc(others, func(a, b int) int {
					return cmp.Compare(abs(q.units[a].off-q.units[r.first].off), abs(q.units[b].off-q.units[r.first].off))
				})
				choices = append(choices, others...)
			}
			for _, i := range choices[:min(len(choices), 2)] {
				st, err := control(i)
				if err != nil {
					return stop(err)
				}
				if st == 1 {
					break
				}
			}
		}
		if !adjacent || tried[next] != 1 || q.boundary != nil {
			continue
		}
		// 边界对照：起点不变，只改数量；A 和 B 都不超过单次读取上限
		ctrl := q.units[next]
		end := span(r)[1]
		start := max(span(r)[0], end+ctrl.n-d.maxQty())
		b := &quickBoundary{start: start, a: end - start, b: end - start + ctrl.n, ctrl: next}
		if q.budget-q.used < len(b.results) {
			q.exhausted = true
			continue
		}
		for k := range b.results {
			if ctx.Err() != nil {
				return stop(ctx.Err())
			}
			spend()
			n := b.a
			if k%2 == 1 {
				n = b.b
			}
			st, words, err := probeRead(withPhase(ctx, phaseBoundary), c, d, start, n)
			b.results[k] = st
			if k == 1 && st == 1 {
				b.bZero = !slices.ContainsFunc(words[:b.a], func(w uint16) bool { return w != 0 })
			}
			step()
			if err != nil {
				q.boundary = b
				return stop(err)
			}
		}
		q.boundary = b
	}
	return q, nil
}

func abs(x int) int { return max(x, -x) }

// quickUnitName 是一个读取单位的地址和名称。
func quickUnitName(d readDef, u quickUnit) string {
	ref := refSpan(d.area(), d.Start+uint16(u.off), u.n)
	if u.p != nil {
		return fmt.Sprintf("%s %s（%s）", modbus.Reference(d.area(), u.p.Offset), u.p.Name, u.p.Type)
	}
	return ref
}

// quickText 是快速排查的结果：批量读取的情况、抽查的候选点和控制点、边界对照，最后是能下的结论和下一步。
// 只描述抽样到的事实，不外推全表。
func quickText(d readDef, q *quickResult, stopErr error) probeOutput {
	unitName := "个寄存器"
	if len(q.units) > 0 && q.units[0].p != nil {
		unitName = "个点"
	}
	okBatches := 0
	for _, st := range q.batches {
		if st == 1 {
			okBatches++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Slave %d · %s · 快速排查：%s（%d %s）\n\n", d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty), len(q.units), unitName)
	fmt.Fprintf(&b, "批量读取：%d 次请求，有返回 %d 次，读不通 %d 次\n", len(q.batches), okBatches, len(q.batches)-okBatches)
	fmt.Fprintf(&b, "批量返回全为 0 的连续段：%d 段，共 %d %s；只从中抽查，非零位置也没有逐个验证\n", q.runs, q.zeroUnits, unitName)
	fmt.Fprintf(&b, "验证请求：%d / %d\n", q.used, q.budget)
	var contra []quickCheck
	if len(q.candidates) > 0 {
		fmt.Fprintf(&b, "\n抽查的候选（每个全 0 段的第一个，%d 个）：\n", len(q.candidates))
		for _, c := range q.candidates {
			u := q.units[c.unit]
			mark := ""
			if c.state < 0 {
				contra = append(contra, c)
				mark = "  ← 批量有返回、单独读不通"
			}
			fmt.Fprintf(&b, "  %s：单独读取%s；批量返回 %s；所在全 0 段 %s%s\n", quickUnitName(d, u), probeStateName[c.state], wordsText(u.words),
				refSpan(d.area(), d.Start+uint16(c.run[0]), c.run[1]-c.run[0]), mark)
		}
	}
	controlOK := q.control != nil && q.control.state == 1
	if q.control != nil {
		u := q.units[q.control.unit]
		fmt.Fprintf(&b, "\n控制点：%s 单独读取%s（批量返回 %s）\n", quickUnitName(d, u), probeStateName[q.control.state], wordsText(u.words))
	}
	if bd := q.boundary; bd != nil {
		names := func(i, j int) string { return probeStateName[bd.results[i]] + "、" + probeStateName[bd.results[j]] }
		fmt.Fprintf(&b, "\n边界对照（起点不变，B 只比 A 多读紧挨着的 %s，它已单独读通；交替各读两次）：\n", quickUnitName(d, q.units[bd.ctrl]))
		fmt.Fprintf(&b, "  A %s（%d 个寄存器）：%s\n", refSpan(d.area(), d.Start+uint16(bd.start), bd.a), bd.a, names(0, 2))
		fmt.Fprintf(&b, "  B %s（%d 个寄存器）：%s", refSpan(d.area(), d.Start+uint16(bd.start), bd.b), bd.b, names(1, 3))
		if bd.bZero {
			b.WriteString("；B 在 A 范围内返回的全是 0")
		}
		b.WriteString("\n")
		if bd.consistent() {
			b.WriteString("  两轮结果一致：范围里含有能单独读通的地址时整段正常返回，不含时读不通。起点相同，不是起点地址的问题。\n")
		} else {
			b.WriteString("  两轮结果不一致或不符合上面的规律，可能有网络或设备波动，不据此下结论。\n")
		}
	}
	b.WriteString("\n结论：")
	switch {
	case len(contra) > 0 && !controlOK:
		fmt.Fprintf(&b, "抽查的 %d 个候选中 %d 个单独读不通，但没有读通的控制点：链路或设备状态不确定，不判定为补零。", len(q.candidates), len(contra))
	case len(contra) > 0:
		fmt.Fprintf(&b, "已发现批量读取与单独读取不一致：抽查的 %d 个候选中 %d 个单独读不通，批量读取在这些位置返回 0，控制点单独读通。"+
			"这是抽样结果，不代表其他点；批量读取的这些 0 不能当作真实值，需到设备的转发配置里核对，仅凭报文不能确定具体原因。", len(q.candidates), len(contra))
	case len(q.candidates) == 0 && q.runs == 0:
		b.WriteString("批量返回里没有全为 0 的段，没有补零候选。快速排查只抽查全 0 段，不能据此说明每个地址都能单独读。")
	default:
		fmt.Fprintf(&b, "抽查的 %d 个候选单独读取都正常，抽样范围内没有发现不一致；不代表全表正常，未抽查的点和非零位置没有验证。", len(q.candidates))
	}
	if q.exhausted {
		fmt.Fprintf(&b, "\n验证请求已用完（%d 次）：存在没做完的验证，覆盖未完成。", q.budget)
	}
	b.WriteString("\n下一步：要列出全部读不通的点，用“逐点独立验证”（需要点表）；要逐个确认寄存器，用“逐个寄存器”。")
	if hasNonstandard := slices.ContainsFunc(q.candidates, func(c quickCheck) bool { return c.state == -4 }); hasNonstandard {
		b.WriteString("\n\n" + nonstandardNote)
	}
	b.WriteString(stopText(stopErr))
	out := probeOutput{text: b.String(), warn: len(contra) > 0 || q.exhausted,
		csvHeader: []string{"地址", "Offset", "名称", "类型", "寄存器数", "批量返回原始字", "单独读取", "所在全 0 段"}}
	out.summary = fmt.Sprintf("批量有返回 %d / %d 次  ·  抽查 %d 个：单独读不通 %d  ·  验证请求 %d / %d%s",
		okBatches, len(q.batches), len(q.candidates), len(contra), q.used, q.budget, map[bool]string{true: "  ·  覆盖未完成"}[q.exhausted])
	for _, c := range contra {
		u := q.units[c.unit]
		name, typ := "", ""
		if u.p != nil {
			name, typ = u.p.Name, string(u.p.Type)
		}
		off := d.Start + uint16(u.off)
		out.csvRows = append(out.csvRows, []string{modbus.Reference(d.area(), off), fmt.Sprint(off), name, typ, fmt.Sprint(u.n),
			wordsText(u.words), probeStateName[c.state], refSpan(d.area(), d.Start+uint16(c.run[0]), c.run[1]-c.run[0])})
	}
	return out
}

// showQuickResult 显示快速排查的结果，可以接着做完整检测：有点表时逐点独立验证全部点，否则逐个寄存器读这个范围。
func (ws *Workspace) showQuickResult(d readDef, q *quickResult, stopErr error, pr *probeRun) {
	out := quickText(d, q, stopErr)
	out.text += "\n\n" + illegalNote
	result := newAIProbeResult(d, quickStates(d, q), stopErr)
	result.extra = quickEvidence(d, q)
	var actions []probeAction
	if pd, segs, ok := pointProbeDef(ws.points, readDef{Slave: d.Slave, Function: d.Function}); ok {
		actions = append(actions, probeAction{"继续逐点独立验证（全部点）", func() { ws.runRegisterProbeMode(nil, pd, probeEachPoint, segs) }})
	}
	actions = append(actions, probeAction{"继续逐个寄存器", func() { ws.runRegisterProbeMode(nil, d, probeSingle, nil) }})
	ws.probeResultDialog(nil, d, out, result, 0, 0, pr, actions...)
}

// quickStates 把快速排查按地址展开给 AI：批量有返回记 2，批量读不通记它的结果，单独读过的单位记单独读取的结果。
func quickStates(d readDef, q *quickResult) []int8 {
	res := make([]int8, d.Qty)
	for _, u := range q.units {
		st := u.batch
		if st == 1 {
			st = 2
		}
		for r := u.off; r < u.off+u.n; r++ {
			res[r] = st
		}
	}
	checks := slices.Clone(q.candidates)
	if q.control != nil {
		checks = append(checks, *q.control)
	}
	for _, c := range checks {
		u := q.units[c.unit]
		for r := u.off; r < u.off+u.n; r++ {
			res[r] = c.state
		}
	}
	return res
}

// quickEvidence 是交给 AI 的快速排查细节：只有地址、类型和结果，不带点名。
func quickEvidence(d readDef, q *quickResult) map[string]any {
	names := map[int8]string{1: "readable_individually", -1: "illegal_address", -2: "inconclusive", -3: "no_response", -4: "nonstandard_response", 0: "untested"}
	unit := func(c quickCheck) map[string]any {
		u := q.units[c.unit]
		v := map[string]any{"offset": int(d.Start) + u.off, "registers": u.n, "single_state": names[c.state], "batch_words": u.words}
		if u.p != nil {
			v["type"] = string(u.p.Type)
		}
		return v
	}
	var cands []map[string]any
	for _, c := range q.candidates {
		cands = append(cands, unit(c))
	}
	v := map[string]any{"mode": "quick_sample", "verify_budget": q.budget, "verify_used": q.used, "budget_exhausted": q.exhausted,
		"zero_runs": q.runs, "candidates": cands,
		"note_quick": "只从批量返回全为 0 的连续段里抽查，结论只覆盖抽到的候选，不能推断其余点；批量返回的 0 只用来挑候选。"}
	if q.control != nil {
		v["control"] = unit(*q.control)
	}
	if bd := q.boundary; bd != nil {
		var rs []string
		for _, st := range bd.results {
			rs = append(rs, names[st])
		}
		v["boundary"] = map[string]any{"start_offset": int(d.Start) + bd.start, "quantity_a": bd.a, "quantity_b": bd.b, "results_abab": rs, "consistent": bd.consistent(), "b_zero_in_a": bd.bZero}
	}
	return v
}
