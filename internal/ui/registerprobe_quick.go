package ui

// showQuickResult 显示快速排查的结果，可以接着做完整检测：有点表时逐点独立验证全部点，否则逐个寄存器读这个范围。
func (ws *Workspace) showQuickResult(d readDef, q *quickResult, stopErr error, pr *probeRun) {
	out := quickText(d, q, stopErr)
	out.Text += "\n\n" + illegalNote
	result := newAIProbeResult(d, quickStates(d, q), stopErr)
	result.extra = quickEvidence(d, q)
	var actions []probeAction
	if pd, segs, ok := pointProbeDef(ws.points, readDef{Slave: d.Slave, Function: d.Function}); ok {
		actions = append(actions, probeAction{"继续逐点独立验证（全部点）", func() { ws.runRegisterProbeMode(nil, pd, probeEachPoint, segs) }})
	}
	actions = append(actions, probeAction{"继续逐个寄存器", func() { ws.runRegisterProbeMode(nil, d, probeSingle, nil) }})
	ws.probeResultDialog(nil, d, out, result, 0, 0, pr, actions...)
}
