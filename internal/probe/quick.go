package probe

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"modbus-ai-studio/internal/modbus"
)

// QuickBudget 是快速排查的验证请求上限：候选点、控制点和边界对照都算，开头批量读取这个范围的请求不算。
// 16 是按现场每次请求几百毫秒估的初始值，不是协议常数。
const QuickBudget = 16

// QuickCandidates 是最多抽查几个全 0 段。
const QuickCandidates = 4

// QuickUnit 是快速排查的一个读取单位：点表里的一个点（按它的宽度），没有点表时是单个寄存器。
type QuickUnit struct {
	Off, N int    // 相对检测范围起点
	P      *point // 没有点表时为空
	Batch  int8   // 所在批量读取的结果，跨两次批量读取时取较差的
	Words  []uint16
}

func (u QuickUnit) zero() bool {
	return u.Batch == 1 && !slices.ContainsFunc(u.Words, func(w uint16) bool { return w != 0 })
}

// QuickSample 是一次单独读取：读的是哪个单位、结果如何。
type QuickSample struct {
	Unit  int
	State int8
	Run   [2]int // 候选点所在全 0 段（相对起点的寄存器范围）
}

// QuickBoundary 是边界对照：起点不变，A 读到全 0 段末尾，B 只多读一个已单独读通的控制点，交替各读两次。
type QuickBoundary struct {
	Start, A, B int
	Ctrl        int     // 控制点
	Results     [4]int8 // A、B、A、B
	BZero       bool    // B 在 A 范围内返回的全是 0
}

// consistent 表示两轮结果一致：A 两次都读不通，B 两次都正常返回。
func (b QuickBoundary) Consistent() bool {
	return b.Results[0] < 0 && b.Results[2] < 0 && b.Results[1] == 1 && b.Results[3] == 1
}

type QuickResult struct {
	Units      []QuickUnit
	Batches    []int8
	Runs       int // 批量返回全为 0 的连续段数
	ZeroUnits  int
	Candidates []QuickSample
	Control    *QuickSample
	Boundary   *QuickBoundary
	Budget     int  // 验证请求上限
	Used       int  // 已用的验证请求
	Exhausted  bool // 验证请求用完，计划的读取没有做完
}

// QuickUnits 取点表里完整落在范围内的点作读取单位（地址重叠的点只取前一个）；范围内没有点时按单个寄存器。
func QuickUnits(pts pointTable, d Def) []QuickUnit {
	var us []QuickUnit
	end := 0
	for _, p := range pts.List() {
		off := int(p.Offset) - int(d.Start)
		if p.Area != d.area() || off < end || off+p.Regs() > d.Qty {
			continue
		}
		p := p
		us = append(us, QuickUnit{Off: off, N: p.Regs(), P: &p})
		end = off + p.Regs()
	}
	if len(us) == 0 {
		for i := range d.Qty {
			us = append(us, QuickUnit{Off: i, N: 1})
		}
	}
	return us
}

// QuickCheck 快速排查批量读与单独读的差异：先批量读整个范围，记下原始值；从批量返回全为 0 的连续段里
// 各抽首个单位单独读；有读不通的，读紧挨在该段后面的单位作控制点，控制点读通就做边界对照。验证请求不超过 QuickBudget。
// 只抽样，不能列出全部读不通的点；零值只用来挑候选，不当作结论。
func QuickCheck(ctx context.Context, c Client, d Def, pts pointTable, budget int, progress func(int)) (*QuickResult, error) {
	q := &QuickResult{Units: QuickUnits(pts, d), Budget: budget}
	done := 0
	step := func() { done++; progress(done) }
	stop := func(err error) (*QuickResult, error) {
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
		st, words, err := probeRead(withPhase(ctx, PhaseBatch), c, d, start, n)
		q.Batches = append(q.Batches, st)
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
	for i := range q.Units {
		u := &q.Units[i]
		u.Batch = 1
		for r := u.Off; r < u.Off+u.N; r++ {
			if regState[r] != 1 {
				u.Batch = regState[r]
				break
			}
		}
		if u.Batch == 1 {
			u.Words = slices.Clone(regWords[u.Off : u.Off+u.N])
		}
	}
	// 全 0 的连续段：首尾相接、批量返回全为 0 的单位
	type zeroRun struct{ first, last int }
	var runs []zeroRun
	for i, u := range q.Units {
		if !u.zero() {
			continue
		}
		q.ZeroUnits++
		if k := len(runs); k > 0 && runs[k-1].last == i-1 && q.Units[i-1].Off+q.Units[i-1].N == u.Off {
			runs[k-1].last = i
		} else {
			runs = append(runs, zeroRun{i, i})
		}
	}
	q.Runs = len(runs)
	span := func(r zeroRun) [2]int { return [2]int{q.Units[r.first].Off, q.Units[r.last].Off + q.Units[r.last].N} }
	slices.SortStableFunc(runs, func(a, b zeroRun) int { return cmp.Compare(span(b)[1]-span(b)[0], span(a)[1]-span(a)[0]) })
	runs = runs[:min(len(runs), QuickCandidates)]

	spend := func() bool {
		if q.Used >= q.Budget {
			q.Exhausted = true
			return false
		}
		q.Used++
		return true
	}
	single := func(phase string, i int) (int8, error) {
		u := q.Units[i]
		st, _, err := probeRead(withPhase(ctx, phase), c, d, u.Off, u.N)
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
		st, err := single(PhaseControl, i)
		tried[i] = st
		if q.Control == nil || q.Control.State != 1 {
			q.Control = &QuickSample{Unit: i, State: st}
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
		st, err := single(PhaseCandidate, r.first)
		q.Candidates = append(q.Candidates, QuickSample{Unit: r.first, State: st, Run: span(r)})
		if err != nil {
			return stop(err)
		}
		if st >= 0 {
			continue
		}
		// 控制点：先试紧挨在全 0 段后面、批量返回非零的单位，读通了还可以做边界对照；不行再试离得最近的其他非零单位
		next := r.last + 1
		adjacent := next < len(q.Units) && q.Units[next].Off == span(r)[1] && q.Units[next].Batch == 1 && !q.Units[next].zero()
		if adjacent && q.Boundary == nil || q.Control == nil || q.Control.State != 1 {
			var choices []int
			if adjacent {
				choices = append(choices, next)
			}
			if q.Control == nil || q.Control.State != 1 {
				var others []int
				for i, u := range q.Units {
					if u.Batch == 1 && !u.zero() && i != next {
						others = append(others, i)
					}
				}
				slices.SortFunc(others, func(a, b int) int {
					return cmp.Compare(abs(q.Units[a].Off-q.Units[r.first].Off), abs(q.Units[b].Off-q.Units[r.first].Off))
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
		if !adjacent || tried[next] != 1 || q.Boundary != nil {
			continue
		}
		// 边界对照：起点不变，只改数量；A 和 B 都不超过单次读取上限
		ctrl := q.Units[next]
		end := span(r)[1]
		start := max(span(r)[0], end+ctrl.N-d.maxQty())
		b := &QuickBoundary{Start: start, A: end - start, B: end - start + ctrl.N, Ctrl: next}
		if q.Budget-q.Used < len(b.Results) {
			q.Exhausted = true
			continue
		}
		for k := range b.Results {
			if ctx.Err() != nil {
				return stop(ctx.Err())
			}
			spend()
			n := b.A
			if k%2 == 1 {
				n = b.B
			}
			st, words, err := probeRead(withPhase(ctx, PhaseBoundary), c, d, start, n)
			b.Results[k] = st
			if k == 1 && st == 1 {
				b.BZero = !slices.ContainsFunc(words[:b.A], func(w uint16) bool { return w != 0 })
			}
			step()
			if err != nil {
				q.Boundary = b
				return stop(err)
			}
		}
		q.Boundary = b
	}
	return q, nil
}

func abs(x int) int { return max(x, -x) }

// quickUnitName 是一个读取单位的地址和名称。
func quickUnitName(d Def, u QuickUnit) string {
	ref := refSpan(d.area(), d.Start+uint16(u.Off), u.N)
	if u.P != nil {
		return fmt.Sprintf("%s %s（%s）", modbus.Reference(d.area(), u.P.Offset), u.P.Name, u.P.Type)
	}
	return ref
}

// QuickText 是快速排查的结果：批量读取的情况、抽查的候选点和控制点、边界对照，最后是能下的结论和下一步。
// 只描述抽样到的事实，不外推全表。
func QuickText(d Def, q *QuickResult, stopErr error) Output {
	unitName := "个寄存器"
	if len(q.Units) > 0 && q.Units[0].P != nil {
		unitName = "个点"
	}
	okBatches := 0
	for _, st := range q.Batches {
		if st == 1 {
			okBatches++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Slave %d · %s · 快速排查：%s（%d %s）\n\n", d.Slave, d.Function, refSpan(d.area(), d.Start, d.Qty), len(q.Units), unitName)
	fmt.Fprintf(&b, "批量读取：%d 次请求，有返回 %d 次，读不通 %d 次\n", len(q.Batches), okBatches, len(q.Batches)-okBatches)
	fmt.Fprintf(&b, "批量返回全为 0 的连续段：%d 段，共 %d %s；只从中抽查，非零位置也没有逐个验证\n", q.Runs, q.ZeroUnits, unitName)
	fmt.Fprintf(&b, "验证请求：%d / %d\n", q.Used, q.Budget)
	var contra []QuickSample
	if len(q.Candidates) > 0 {
		fmt.Fprintf(&b, "\n抽查的候选（每个全 0 段的第一个，%d 个）：\n", len(q.Candidates))
		for _, c := range q.Candidates {
			u := q.Units[c.Unit]
			mark := ""
			if c.State < 0 {
				contra = append(contra, c)
				mark = "  ← 批量有返回、单独读不通"
			}
			fmt.Fprintf(&b, "  %s：单独读取%s；批量返回 %s；所在全 0 段 %s%s\n", quickUnitName(d, u), probeStateName[c.State], wordsText(u.Words),
				refSpan(d.area(), d.Start+uint16(c.Run[0]), c.Run[1]-c.Run[0]), mark)
		}
	}
	controlOK := q.Control != nil && q.Control.State == 1
	if q.Control != nil {
		u := q.Units[q.Control.Unit]
		fmt.Fprintf(&b, "\n控制点：%s 单独读取%s（批量返回 %s）\n", quickUnitName(d, u), probeStateName[q.Control.State], wordsText(u.Words))
	}
	if bd := q.Boundary; bd != nil {
		names := func(i, j int) string { return probeStateName[bd.Results[i]] + "、" + probeStateName[bd.Results[j]] }
		fmt.Fprintf(&b, "\n边界对照（起点不变，B 只比 A 多读紧挨着的 %s，它已单独读通；交替各读两次）：\n", quickUnitName(d, q.Units[bd.Ctrl]))
		fmt.Fprintf(&b, "  A %s（%d 个寄存器）：%s\n", refSpan(d.area(), d.Start+uint16(bd.Start), bd.A), bd.A, names(0, 2))
		fmt.Fprintf(&b, "  B %s（%d 个寄存器）：%s", refSpan(d.area(), d.Start+uint16(bd.Start), bd.B), bd.B, names(1, 3))
		if bd.BZero {
			b.WriteString("；B 在 A 范围内返回的全是 0")
		}
		b.WriteString("\n")
		if bd.Consistent() {
			b.WriteString("  两轮结果一致：范围里含有能单独读通的地址时整段正常返回，不含时读不通。起点相同，不是起点地址的问题。\n")
		} else {
			b.WriteString("  两轮结果不一致或不符合上面的规律，可能有网络或设备波动，不据此下结论。\n")
		}
	}
	b.WriteString("\n结论：")
	switch {
	case len(contra) > 0 && !controlOK:
		fmt.Fprintf(&b, "抽查的 %d 个候选中 %d 个单独读不通，但没有读通的控制点：链路或设备状态不确定，不判定为补零。", len(q.Candidates), len(contra))
	case len(contra) > 0:
		fmt.Fprintf(&b, "已发现批量读取与单独读取不一致：抽查的 %d 个候选中 %d 个单独读不通，批量读取在这些位置返回 0，控制点单独读通。"+
			"这是抽样结果，不代表其他点；批量读取的这些 0 不能当作真实值，需到设备的转发配置里核对，仅凭报文不能确定具体原因。", len(q.Candidates), len(contra))
	case len(q.Candidates) == 0 && q.Runs == 0:
		b.WriteString("批量返回里没有全为 0 的段，没有补零候选。快速排查只抽查全 0 段，不能据此说明每个地址都能单独读。")
	default:
		fmt.Fprintf(&b, "抽查的 %d 个候选单独读取都正常，抽样范围内没有发现不一致；不代表全表正常，未抽查的点和非零位置没有验证。", len(q.Candidates))
	}
	if q.Exhausted {
		fmt.Fprintf(&b, "\n验证请求已用完（%d 次）：存在没做完的验证，覆盖未完成。", q.Budget)
	}
	b.WriteString("\n下一步：要列出全部读不通的点，用“逐点独立验证”（需要点表）；要逐个确认寄存器，用“逐个寄存器”。")
	if hasNonstandard := slices.ContainsFunc(q.Candidates, func(c QuickSample) bool { return c.State == -4 }); hasNonstandard {
		b.WriteString("\n\n" + nonstandardNote)
	}
	b.WriteString(stopText(stopErr))
	out := Output{Text: b.String(), Warn: len(contra) > 0 || q.Exhausted,
		CSVHeader: []string{"地址", "Offset", "名称", "类型", "寄存器数", "批量返回原始字", "单独读取", "所在全 0 段"}}
	out.Summary = fmt.Sprintf("批量有返回 %d / %d 次  ·  抽查 %d 个：单独读不通 %d  ·  验证请求 %d / %d%s",
		okBatches, len(q.Batches), len(q.Candidates), len(contra), q.Used, q.Budget, map[bool]string{true: "  ·  覆盖未完成"}[q.Exhausted])
	for _, c := range contra {
		u := q.Units[c.Unit]
		name, typ := "", ""
		if u.P != nil {
			name, typ = u.P.Name, string(u.P.Type)
		}
		off := d.Start + uint16(u.Off)
		out.CSVRows = append(out.CSVRows, []string{modbus.Reference(d.area(), off), fmt.Sprint(off), name, typ, fmt.Sprint(u.N),
			wordsText(u.Words), probeStateName[c.State], refSpan(d.area(), d.Start+uint16(c.Run[0]), c.Run[1]-c.Run[0])})
	}
	return out
}

// QuickStates 把快速排查按地址展开给 AI：批量有返回记 2，批量读不通记它的结果，单独读过的单位记单独读取的结果。
func QuickStates(d Def, q *QuickResult) []int8 {
	res := make([]int8, d.Qty)
	for _, u := range q.Units {
		st := u.Batch
		if st == 1 {
			st = 2
		}
		for r := u.Off; r < u.Off+u.N; r++ {
			res[r] = st
		}
	}
	checks := slices.Clone(q.Candidates)
	if q.Control != nil {
		checks = append(checks, *q.Control)
	}
	for _, c := range checks {
		u := q.Units[c.Unit]
		for r := u.Off; r < u.Off+u.N; r++ {
			res[r] = c.State
		}
	}
	return res
}

// QuickEvidence 是交给 AI 的快速排查细节：只有地址、类型和结果，不带点名。
func QuickEvidence(d Def, q *QuickResult) map[string]any {
	names := map[int8]string{1: "readable_individually", -1: "illegal_address", -2: "inconclusive", -3: "no_response", -4: "nonstandard_response", 0: "untested"}
	unit := func(c QuickSample) map[string]any {
		u := q.Units[c.Unit]
		v := map[string]any{"offset": int(d.Start) + u.Off, "registers": u.N, "single_state": names[c.State], "batch_words": u.Words}
		if u.P != nil {
			v["type"] = string(u.P.Type)
		}
		return v
	}
	var cands []map[string]any
	for _, c := range q.Candidates {
		cands = append(cands, unit(c))
	}
	v := map[string]any{"mode": "quick_sample", "verify_budget": q.Budget, "verify_used": q.Used, "budget_exhausted": q.Exhausted,
		"zero_runs": q.Runs, "candidates": cands,
		"note_quick": "只从批量返回全为 0 的连续段里抽查，结论只覆盖抽到的候选，不能推断其余点；批量返回的 0 只用来挑候选。"}
	if q.Control != nil {
		v["control"] = unit(*q.Control)
	}
	if bd := q.Boundary; bd != nil {
		var rs []string
		for _, st := range bd.Results {
			rs = append(rs, names[st])
		}
		v["boundary"] = map[string]any{"start_offset": int(d.Start) + bd.Start, "quantity_a": bd.A, "quantity_b": bd.B, "results_abab": rs, "consistent": bd.Consistent(), "b_zero_in_a": bd.BZero}
	}
	return v
}
