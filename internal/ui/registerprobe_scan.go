package ui

import (
	"context"
	"errors"
	"fmt"

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
	probeAuto   probeMode = iota // 批量读验证正常段，异常段定位到单个地址
	probeSingle                  // 逐个寄存器：每个地址单独读一次，找出全部读不通的地址
	probePoints                  // 按点表分段：地址相连的点一段读一次，读不通的段逐点再读
)

var probeModeNames = []string{"自动定位（批量读，异常段逐个定位）", "逐个寄存器", "按点表分段"}

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
	for _, p := range pts.list() {
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
		n := p.regs()
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

// probeRead 读一段并归类：可读 1，异常 02（非法地址）-1，超时或网关 0B -3，其他异常码 -2。
// 连接错误、校验错误和 RTU / ASCII 超时返回错误，检测停止：RTU / ASCII 没有事务编号，超时后接着发，
// 会把迟到的响应算到别的地址上。
func probeRead(ctx context.Context, c probeClient, d readDef, start, n int) (int8, error) {
	_, err := c.Do(ctx, modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start + uint16(start), Quantity: uint16(n)})
	ex, isEx := modbus.AsException(err)
	span := refSpan(d.area(), d.Start+uint16(start), n)
	switch {
	case err == nil:
		return 1, nil
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case errors.Is(err, errProbeUnstable):
		return -2, nil
	case errors.Is(err, modbus.ErrTimeout) && c.Mode() != modbus.ModeTCP:
		return -2, fmt.Errorf("%s 请求超时；RTU/ASCII 无事务编号，已停止以免将迟到响应归到其他地址。请增大超时并重新连接后检测", span)
	case isEx && ex.Code == modbus.ExceptionIllegalDataAddress:
		return -1, nil
	case errors.Is(err, modbus.ErrTimeout), isEx && ex.Code == modbus.ExceptionGatewayTargetFailed:
		return -3, nil
	case isEx:
		return -2, nil
	}
	return -2, fmt.Errorf("%s %s", span, errSummary(err))
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
	read := func(start, n int) (int8, error) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		state, err := probeRead(ctx, c, d, start, n)
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
			if _, err := read(i, 1); err != nil {
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
		state, err := probeRead(ctx, c, d, s.start, s.n)
		s.state = state
		// 整段失败不能证明每个点都失败。需要逐点复核的段保持未检测，
		// 只有点的读取完成后才记录结论和进度，停止时不沿用整段异常。
		if state == 1 || len(s.points) == 1 || err != nil {
			mark(s.start, s.n, state)
		}
		if err != nil {
			return stop(err)
		}
		if state == 1 || len(s.points) == 1 {
			continue
		}
		for _, p := range s.points {
			if _, err := read(int(p.Offset)-int(d.Start), p.regs()); err != nil {
				return stop(err)
			}
		}
	}
	return res, nil
}

// probe uses successful batch reads to verify whole ranges, bisects illegal
// address responses, and falls back to single reads for other batch failures.
// 1 可读，-1 单地址异常 02，-2 其他错误，-3 超时/网关 0B，0 未检测。
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
		_, err := c.Do(ctx, modbus.Request{Slave: d.Slave, Function: d.Function, Address: d.Start + uint16(start), Quantity: uint16(n)})
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			mark(start, n, 1)
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
		case errors.Is(err, modbus.ErrTimeout) || isEx && ex.Code == modbus.ExceptionGatewayTargetFailed:
			mark(start, n, -3)
		case errors.Is(err, errProbeUnstable):
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
