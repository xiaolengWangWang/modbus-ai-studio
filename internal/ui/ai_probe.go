package ui

import (
	"context"
	"errors"
	"time"
)

type aiProbeResult struct {
	def     readDef
	states  []int8
	stopped string
	at      time.Time
}

func newAIProbeResult(d readDef, states []int8, err error) *aiProbeResult {
	p := &aiProbeResult{def: d, states: append([]int8(nil), states...), at: time.Now()}
	if errors.Is(err, context.Canceled) {
		p.stopped = "cancelled"
	} else if err != nil {
		p.stopped = aiErrorKind(err)
	}
	return p
}

func (p *aiProbeResult) data() map[string]any {
	names := map[int8]string{1: "readable", -1: "illegal_address", -2: "inconclusive", -3: "no_response", 0: "untested"}
	counts := map[string]int{}
	var spans []map[string]any
	for i := 0; i < p.def.Qty; {
		state := int8(0)
		if i < len(p.states) {
			state = p.states[i]
		}
		j := i + 1
		for j < p.def.Qty {
			next := int8(0)
			if j < len(p.states) {
				next = p.states[j]
			}
			if next != state {
				break
			}
			j++
		}
		name, ok := names[state]
		if !ok {
			name = "inconclusive"
		}
		counts[name] += j - i
		spans = append(spans, map[string]any{"start_offset": int(p.def.Start) + i, "quantity": j - i, "state": name})
		i = j
	}
	return map[string]any{"time": p.at.Format(time.RFC3339Nano), "slave": p.def.Slave, "function": byte(p.def.Function), "start_offset": p.def.Start, "quantity": p.def.Qty, "counts": counts, "ranges": spans, "stopped_reason": p.stopped, "note": "非法地址仅表示该功能码读取被拒绝；未响应可能是超时或网关异常0B，不代表地址不存在；无法判断和未检测不是非法地址。"}
}
