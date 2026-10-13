package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"modbus-ai-studio/internal/modbus"
)

// probeRange 检测读取范围，批量验证正常段，再定位到具体异常地址。
// 和独立检测入口共用检测逻辑，异常 02 才归为非法地址。
func (ws *Workspace) probeRange(w *readWindow) {
	ws.runRegisterProbe(w, w.def)
}

// probeMode 是检测方式。
type probeMode int

const (
	probeAuto      probeMode = iota // 批量读验证正常段，异常段定位到单个地址
	probeSingle                     // 逐个寄存器：每个地址单独读一次，找出全部读不通的地址
	probePoints                     // 按点表分段：地址相连的点一段读一次，读不通的段逐点再读
	probeEachPoint                  // 逐点独立验证：每段整段读一次，再每个点单独读一次，两者对照
	probeQuick                      // 快速排查：批量读范围，从全 0 段里抽查单独读，加控制点和边界对照，验证请求有上限
)

var probeModeNames = []string{"自动定位（批量读，异常段逐个定位）", "逐个寄存器", "按点表分段", "逐点独立验证（整段读与逐点单独读对照）",
	fmt.Sprintf("快速排查（批量读与单独读差异，最多 %d 次验证请求）", quickBudget)}

// usesPoints 表示检测范围按点表算。
func (m probeMode) usesPoints() bool { return m == probePoints || m == probeEachPoint }

// probeSegment 是按点表分段检测的一段：地址首尾相接的点合成一段，一次读完。
type probeSegment struct {
	start, n int     // 相对检测范围起点的偏移和数量
	points   []point // 段里的点，按地址排序
	state    int8    // 整段读一次的结果（同 probe 的标记），0 表示还没读；probeUnits 填写
}

// pointProbeDef 按点表给出 d 的功能码对应数据区里全部点的检测范围和分段：地址首尾相接的点合成一段，
// 一段不超过一次能读的数量；中间隔着不在点表里的地址就另起一段，不把空档读进来。没有这个区的点时 ok 为 false。
func pointProbeDef(pts pointTable, d readDef) (readDef, []probeSegment, bool) {
	var ps []point
	for _, p := range pts.List() {
		if p.Area == d.area() {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return d, nil, false
	}
	first, end := int(ps[0].Offset), int(ps[0].Offset)
	var segs []probeSegment
	for _, p := range ps {
		n := p.Regs()
		if k := len(segs); k > 0 && int(p.Offset) == end && segs[k-1].n+n <= d.maxQty() {
			segs[k-1].n += n
			segs[k-1].points = append(segs[k-1].points, p)
		} else {
			segs = append(segs, probeSegment{start: int(p.Offset) - first, n: n, points: []point{p}})
		}
		end = max(end, int(p.Offset)+n)
	}
	d.Start, d.Qty = uint16(first), end-first
	return d, segs, true
}

// nonstandardReply 表示设备回了属于这次请求的帧，但不是正常数据也不是标准异常：
// 89 原样回显请求，或超时前收到过事务号对上、内容不符的帧。
func nonstandardReply(err error) bool {
	_, ok := modbus.AsNonstandard(err)
	return ok || errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch)
}

// probeRead 读一段并归类：可读 1，异常 02（非法地址）-1，超时或网关 0B -3，非标准响应 -4，其他异常码 -2。
// 读通时同时返回原始字（线圈和离散输入按位给出 0 / 1）。这里的可读只说明这一次读取有正常响应；
// 一次读了多个点时由调用方记为批量有返回（2）。
// 连接错误、校验错误和 RTU / ASCII 超时返回错误，检测停止：RTU / ASCII 没有事务编号，超时后接着发，
// 会把迟到的响应算到别的地址上。
func probeRead(ctx context.Context, c probeClient, d readDef, start, n int) (int8, []uint16, error) {
	resp, err := c.Do(ctx, modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start + uint16(start), Quantity: uint16(n)})
	ex, isEx := modbus.AsException(err)
	span := refSpan(d.area(), d.Start+uint16(start), n)
	switch {
	case err == nil:
		words := resp.Registers
		for _, b := range resp.Bits {
			words = append(words, map[bool]uint16{true: 1}[b])
		}
		return 1, words, nil
	case ctx.Err() != nil:
		return 0, nil, ctx.Err()
	case errors.Is(err, errProbeUnstable):
		return -2, nil, nil
	case nonstandardReply(err):
		return -4, nil, nil
	case errors.Is(err, modbus.ErrTimeout) && c.Mode() != modbus.ModeTCP:
		return -2, nil, fmt.Errorf("%s 请求超时；RTU/ASCII 无事务编号，已停止以免将迟到响应归到其他地址。请增大超时并重新连接后检测", span)
	case isEx && ex.Code == modbus.ExceptionIllegalDataAddress:
		return -1, nil, nil
	case errors.Is(err, modbus.ErrTimeout), isEx && ex.Code == modbus.ExceptionGatewayTargetFailed:
		return -3, nil, nil
	case isEx:
		return -2, nil, nil
	}
	return -2, nil, fmt.Errorf("%s %s", span, errSummary(err))
}

// probeUnits 按检测方式读：逐个寄存器时每个地址单独读一次；按点表分段时一段读一次，读不通的段逐点再读，
// 找出是哪个点。异常码不中断检测。结果和 probe 一样按地址标记，不在点表里的地址保持 0。
func probeUnits(ctx context.Context, c probeClient, d readDef, mode probeMode, segs []probeSegment, progress func(int)) ([]int8, error) {
	res := make([]int8, d.Qty)
	done := 0
	mark := func(start, n int, state int8) {
		if state != 0 {
			for i := start; i < start+n; i++ {
				if res[i] == 0 {
					done++
				}
				res[i] = state
			}
			progress(done)
		}
	}
	read := func(phase string, start, n int) (int8, error) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		state, _, err := probeRead(withPhase(ctx, phase), c, d, start, n)
		mark(start, n, state)
		return state, err
	}
	stop := func(err error) ([]int8, error) {
		if ctx.Err() != nil {
			return res, nil
		}
		return res, err
	}
	if mode == probeSingle {
		for i := range d.Qty {
			if _, err := read(phaseWord, i, 1); err != nil {
				return stop(err)
			}
		}
		return res, nil
	}
	for i := range segs {
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		s := &segs[i]
		phase := phaseBatch
		if len(s.points) == 1 {
			phase = phasePoint
		}
		state, _, err := probeRead(withPhase(ctx, phase), c, d, s.start, s.n)
		s.state = state
		// 整段失败不能证明每个点都失败。需要逐点复核的段保持未检测，
		// 只有点的读取完成后才记录结论和进度，停止时不沿用整段异常。
		// 整段有返回也不能证明每个点单独都能读（有的设备给没数据的地址补 0），记为批量有返回。
		switch {
		case state == 1 && len(s.points) > 1:
			mark(s.start, s.n, 2)
		case state == 1 || len(s.points) == 1 || err != nil:
			mark(s.start, s.n, state)
		}
		if err != nil {
			return stop(err)
		}
		if state == 1 || len(s.points) == 1 {
			continue
		}
		for _, p := range s.points {
			if _, err := read(phasePoint, int(p.Offset)-int(d.Start), p.Regs()); err != nil {
				return stop(err)
			}
		}
	}
	return res, nil
}

// pointCheck 是逐点独立验证里一个点的两次读取：所在段整段读一次，自己按宽度单独读一次。
// 两个结果分开保存，同一个点可以同时是“整段有返回”和“单独读不通”。
type pointCheck struct {
	point
	batch  int8     // 所在段整段读取的结果（同 probeRead），0 表示没读这段
	words  []uint16 // 整段读通时这个点位置上返回的原始字，否则为空
	single int8     // 单独读取的结果，0 表示未检测
}

// contradicts 表示整段读取有返回，这个点单独读却读不通。
func (c pointCheck) contradicts() bool { return c.batch == 1 && c.single < 0 }

// allZero 表示整段读取在这个点的位置上返回的全是 0。
func (c pointCheck) allZero() bool {
	return !slices.ContainsFunc(c.words, func(w uint16) bool { return w != 0 })
}

// checkEachPoint 逐点独立验证：先把每段整段读一次，记下返回的原始字；再把每个点按自己的宽度单独读一次。
// 整段有返回不当作点的结论。progress 按已发出的请求数推进，共 len(segs) + 点数 次。
func checkEachPoint(ctx context.Context, c probeClient, d readDef, segs []probeSegment, progress func(int)) ([]pointCheck, error) {
	var checks []pointCheck
	for _, s := range segs {
		for _, p := range s.points {
			checks = append(checks, pointCheck{point: p})
		}
	}
	done := 0
	stop := func(err error) ([]pointCheck, error) {
		if ctx.Err() != nil {
			return checks, nil
		}
		return checks, err
	}
	k := 0
	for i := range segs {
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		s := &segs[i]
		state, words, err := probeRead(withPhase(ctx, phaseBatch), c, d, s.start, s.n)
		s.state = state
		for _, p := range s.points {
			checks[k].batch = state
			if state == 1 {
				off := int(p.Offset) - int(d.Start) - s.start
				checks[k].words = words[off : off+p.Regs()]
			}
			k++
		}
		done++
		progress(done)
		if err != nil {
			return stop(err)
		}
	}
	for i := range checks {
		if ctx.Err() != nil {
			return stop(ctx.Err())
		}
		p := checks[i].point
		state, _, err := probeRead(withPhase(ctx, phasePoint), c, d, int(p.Offset)-int(d.Start), p.Regs())
		checks[i].single = state
		done++
		progress(done)
		if err != nil {
			return stop(err)
		}
	}
	return checks, nil
}

// pointCheckStates 把逐点单独读取的结果按地址展开，供 AI 证据使用；不在点表里的地址为 0。
func pointCheckStates(d readDef, checks []pointCheck) []int8 {
	res := make([]int8, d.Qty)
	for _, c := range checks {
		off := int(c.Offset) - int(d.Start)
		for i := off; i < off+c.Regs() && i < len(res); i++ {
			if res[i] == 0 || c.single < 0 { // 点有重叠时，读不通的结论优先
				res[i] = c.single
			}
		}
	}
	return res
}

// probe reads whole ranges in batches, bisects illegal address responses, and
// falls back to single reads for other batch failures.
// 1 单独读取可读，2 批量读取有返回（未单独验证），-1 单地址异常 02，-2 其他错误，-3 超时/网关 0B，
// -4 非标准响应，0 未检测。
// TCP 单地址未响应继续检测；无事务编号的协议超时后停止，避免错认迟到响应。
func probe(ctx context.Context, c probeClient, d readDef, progress func(int), recheck ...func(int)) ([]int8, error) {
	if err := validateProbeDef(d); err != nil {
		return nil, err
	}
	res := make([]int8, d.Qty)
	done := 0
	batchTimedOut := false
	mark := func(start, n int, state int8) {
		for i := start; i < start+n; i++ {
			if res[i] == 0 {
				done++
			}
			res[i] = state
		}
		progress(done)
	}
	var readRange func(int, int) error
	readRange = func(start, n int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		phase := phaseBatch
		if n == 1 {
			phase = phaseWord
		}
		_, err := c.Do(withPhase(ctx, phase), modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start + uint16(start), Quantity: uint16(n)})
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			if n > 1 {
				mark(start, n, 2)
			} else {
				mark(start, n, 1)
			}
			return nil
		}
		ex, isEx := modbus.AsException(err)
		illegal := isEx && ex.Code == modbus.ExceptionIllegalDataAddress
		if errors.Is(err, modbus.ErrTimeout) && c.Mode() != modbus.ModeTCP {
			mark(start, n, -2)
			return fmt.Errorf("%s 请求超时；RTU/ASCII 无事务编号，已停止以免将迟到响应归到其他地址。请增大超时并重新连接后检测", refSpan(d.area(), d.Start+uint16(start), n))
		}
		if n > 1 && errors.Is(err, modbus.ErrTimeout) {
			batchTimedOut = true
		}
		if n > 1 && !errors.Is(err, modbus.ErrConnection) {
			if illegal {
				left := n / 2
				if err := readRange(start, left); err != nil {
					return err
				}
				return readRange(start+left, n-left)
			}
			// Avoid repeated timeout trees: once a batch times out, test its
			// addresses individually and continue past isolated silent holes.
			for i := start; i < start+n; i++ {
				if err := readRange(i, 1); err != nil {
					return err
				}
			}
			return nil
		}
		switch {
		case illegal:
			mark(start, n, -1)
		case nonstandardReply(err):
			mark(start, n, -4)
		case errors.Is(err, modbus.ErrTimeout) || isEx && ex.Code == modbus.ExceptionGatewayTargetFailed:
			mark(start, n, -3)
		case isEx || errors.Is(err, errProbeUnstable):
			mark(start, n, -2)
		default:
			mark(start, n, -2)
			return fmt.Errorf("%s %s", refSpan(d.area(), d.Start+uint16(start), n), errSummary(err))
		}
		return nil
	}
	for start := 0; start < d.Qty; {
		n := min(d.Qty-start, d.maxQty())
		if err := readRange(start, n); err != nil {
			if ctx.Err() != nil {
				return res, nil
			}
			return res, err
		}
		start += n
	}
	if batchTimedOut {
		// A late batch response can delay the first single reads. Recheck
		// silent addresses after the full range so healthy neighbours are
		// not reported as forwarding holes merely because of that backlog.
		for i, state := range res {
			if state == -3 {
				if len(recheck) > 0 && ctx.Err() == nil {
					recheck[0](i)
				}
				if err := readRange(i, 1); err != nil {
					if ctx.Err() != nil {
						return res, nil
					}
					return res, err
				}
			}
		}
	}
	return res, nil
}
