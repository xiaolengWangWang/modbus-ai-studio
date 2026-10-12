package ui

import (
	"context"
	"errors"
	"time"
)

type aiProbeResult struct {
	def     readDef
	states  []int8
	checks  []pointCheck   // 逐点独立验证时每个点的整段 / 单独读取结果
	extra   map[string]any // 其他检测方式的细节，例如快速排查的抽样和边界对照
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
	names := map[int8]string{1: "readable_individually", 2: "batch_returned_unverified", -1: "illegal_address", -2: "inconclusive", -3: "no_response", -4: "nonstandard_response", 0: "untested"}
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
	out := map[string]any{"time": p.at.Format(time.RFC3339Nano), "slave": p.def.Slave, "function": byte(p.def.Function), "start_offset": p.def.Start, "quantity": p.def.Qty, "counts": counts, "ranges": spans, "stopped_reason": p.stopped}
	if len(p.checks) > 0 {
		// 点名不发送，只发地址、类型和两次读取的结果
		var contra []map[string]any
		for _, c := range p.checks {
			if c.contradicts() {
				contra = append(contra, map[string]any{"offset": c.Offset, "type": string(c.Type), "registers": c.regs(),
					"single_state": names[c.single], "batch_words": c.words, "batch_all_zero": c.allZero()})
			}
		}
		out["mode"] = "each_point_independent"
		out["ranges_meaning"] = "ranges 是每个点按自身宽度单独读取的结果，不在点表里的地址为 untested"
		out["points"] = len(p.checks)
		out["batch_returned_but_single_failed"] = contra
		out["note_points"] = "先按点表把地址相连的点整段读一次，再每个点单独读一次。batch_returned_but_single_failed 是整段读取有返回、该点单独读取失败的点，batch_words 是整段读取在该点位置返回的原始字；全为 0 只提示设备可能给无数据地址补 0，不能据此断定具体配置原因。"
	}
	for k, v := range p.extra {
		out[k] = v
	}
	out["note"] = "readable_individually 是该地址或点单独读取有正常响应；batch_returned_unverified 只是所在的批量读取有返回，不能推断每个地址单独可读，返回的 0 也不能当作真实值或未配置的证据。非法地址仅表示该功能码读取被拒绝；nonstandard_response 是设备回了属于该请求、但既非正常数据也非标准异常的帧（如功能码 89 原样回显请求），第二个字节不是异常码；未响应可能是超时或网关异常0B，不代表地址不存在；无法判断和未检测不是非法地址。"
	return out
}
