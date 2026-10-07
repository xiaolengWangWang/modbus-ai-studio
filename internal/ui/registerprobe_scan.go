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

// probe uses successful batch reads to verify whole ranges, bisects illegal
// address responses, and falls back to single reads for other batch failures.
// 1 可读，-1 单地址异常 02，-2 其他错误，-3 超时/网关 0B，0 未检测。
// TCP 单地址未响应继续检测；无事务编号的协议超时后停止，避免错认迟到响应。
func probe(ctx context.Context, c *modbus.Client, d readDef, progress func(int), recheck ...func(int)) ([]int8, error) {
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
