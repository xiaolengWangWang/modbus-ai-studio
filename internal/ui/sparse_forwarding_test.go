package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
)

// 本文件的用例对应 MCGS 稀疏转发排查方案的验收用例（T01–T14），设备数据都是合成的。

// scriptedProbe 按 reply 回应每条请求，记录发出的地址和数量。
type scriptedProbe struct {
	reply func(addr uint16, n int) ([]uint16, error)
	sent  []string
}

func (s *scriptedProbe) Mode() modbus.Mode { return modbus.ModeTCP }

func (s *scriptedProbe) Do(_ context.Context, req modbus.Request) (*modbus.Response, error) {
	s.sent = append(s.sent, fmt.Sprintf("%d:%d", req.Address, req.Quantity))
	regs, err := s.reply(req.Address, int(req.Quantity))
	if err != nil {
		return nil, err
	}
	return &modbus.Response{Function: req.Function, Registers: regs}, nil
}

func echoErr(addr uint16, n int) error {
	return &modbus.NonstandardError{Function: modbus.FuncReadHoldingRegisters, PDU: []byte{modbus.EchoFunction, byte(addr >> 8), byte(addr), 0, byte(n)}}
}

func floatPoints(offsets ...uint16) pointTable {
	var ps []point
	for _, off := range offsets {
		ps = append(ps, point{Area: modbus.AreaHoldingRegisters, Offset: off, Name: fmt.Sprintf("点%d", off), Type: modbus.TypeFloat32})
	}
	return newPointTable(ps)
}

// T02 / T12：整段读取全部有返回，按点表分段不能说成每个点都可读；逐点独立验证找出整段返回 0、单独却读不通的点。
func TestEachPointFindsPointsHiddenBySparseForwarding(t *testing.T) {
	pts := floatPoints(0, 2, 4, 6, 8, 10)
	d, segs, ok := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	if !ok || len(segs) != 1 {
		t.Fatalf("六个相连的点应合成一段：%+v", segs)
	}
	faults := simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 0, Count: 2}, {Start: 8, Count: 2}}}

	res, err := probeUnits(context.Background(), registerProbeClient(t, modbus.ModeTCP, faults, nil, 16), d, probePoints, segs, func(int) {})
	if err != nil || fmt.Sprint(res) != "[2 2 2 2 2 2 2 2 2 2 2 2]" {
		t.Fatalf("整段有返回只能记为批量有返回：%v %v", res, err)
	}
	_, summary, bad := pointProbeText(d, res, segs)
	if bad != 0 || !strings.Contains(summary, "可读 0") || !strings.Contains(summary, "整段有返回 6（未逐点验证）") {
		t.Fatalf("按点表分段不能把整段返回说成 6 个点可读：%s", summary)
	}

	var sent []string
	c := registerProbeClient(t, modbus.ModeTCP, faults, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			sent = append(sent, fmt.Sprintf("%d:%d", p.Address, p.Count))
		}
	}), 16)
	segs[0].State = 0
	var progress []int
	checks, err := checkEachPoint(context.Background(), c, d, segs, func(n int) { progress = append(progress, n) })
	if err != nil || len(checks) != 6 {
		t.Fatalf("逐点独立验证应读完全部点：%v", err)
	}
	if fmt.Sprint(sent) != "[0:12 0:2 2:2 4:2 6:2 8:2 10:2]" || fmt.Sprint(progress) != "[1 2 3 4 5 6 7]" {
		t.Fatalf("应先整段读一次，再每个点按宽度单独读一次：%v，进度 %v", sent, progress)
	}
	for _, ck := range checks {
		forwarded := ck.Offset == 0 || ck.Offset == 8
		if ck.Batch != 1 || len(ck.Words) != 2 || forwarded != (ck.Single == 1) || !forwarded && ck.Single != -4 {
			t.Errorf("点 %d：整段 %d %v，单独 %d", ck.Offset, ck.Batch, ck.Words, ck.Single)
		}
	}
	text, summary, bad := eachPointText(d, checks, segs)
	for _, want := range []string{"整段读取：有返回 1 段，读不通 0 段", "可读 2", "非标准响应 4", "整段有返回、单独读不通（4 个点）",
		"40003 点2（FLOAT32）：单独读取非标准响应；整段返回 0000 0000", "其中 4 个点整段返回全为 0", "不能确定是未建通道、变量未连接还是其他配置原因"} {
		if !strings.Contains(text, want) {
			t.Errorf("结果缺少 %q：\n%s", want, text)
		}
	}
	if bad != 4 || !strings.Contains(summary, "整段有返回 1 / 1 段") || !strings.Contains(summary, "读不通 4（其中整段有返回 4）") {
		t.Errorf("摘要：%s", summary)
	}
	for _, wrong := range []string{"非法数据值", "6 个点可读", "全部点单独读取都有正常响应"} {
		if strings.Contains(text, wrong) {
			t.Errorf("结果不应出现 %q：\n%s", wrong, text)
		}
	}

	result := newAIProbeResult(d, pointCheckStates(d, checks), nil)
	result.checks = checks
	b, _ := json.Marshal(result.data())
	evidence := string(b)
	for _, want := range []string{`"mode":"each_point_independent"`, `"single_state":"nonstandard_response"`, `"batch_all_zero":true`, `"nonstandard_response":8`} {
		if !strings.Contains(evidence, want) {
			t.Errorf("AI 证据缺少 %s：%s", want, evidence)
		}
	}
	if strings.Contains(evidence, "点2") {
		t.Error("AI 证据不能带点名")
	}
}

// T01：正常设备，大量真实零值。逐点都读通时不报空位，也不把零值当成问题。
func TestEachPointAcceptsGenuineZeros(t *testing.T) {
	pts := floatPoints(0, 2, 4)
	d, segs, _ := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	checks, err := checkEachPoint(context.Background(), registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, nil, 8), d, segs, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	text, summary, bad := eachPointText(d, checks, segs)
	if bad != 0 || !strings.Contains(summary, "可读 3") || !strings.Contains(text, "全部点单独读取都有正常响应") ||
		strings.Contains(text, "补 0") || strings.Contains(text, "单独读不通") {
		t.Fatalf("真实零值不能报成空位：%s\n%s", summary, text)
	}
}

// T10：单独读取是标准异常、整段返回非零值，仍列为不一致，但不说成补零。
func TestEachPointContradictionWithNonZeroBatch(t *testing.T) {
	pts := newPointTable([]point{
		{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "前", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 1, Name: "中", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "后", Type: modbus.TypeUint16},
	})
	d, segs, _ := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	c := &scriptedProbe{reply: func(addr uint16, n int) ([]uint16, error) {
		if n == 3 {
			return []uint16{5, 7, 9}, nil
		}
		if addr == 1 {
			return nil, &modbus.ExceptionError{Function: modbus.FuncReadHoldingRegisters, Code: modbus.ExceptionIllegalDataAddress}
		}
		return []uint16{1}, nil
	}}
	checks, err := checkEachPoint(context.Background(), c, d, segs, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	text, _, bad := eachPointText(d, checks, segs)
	if bad != 1 || !strings.Contains(text, "40002 中（UINT16）：单独读取非法地址（异常 02）；整段返回 0007") ||
		!strings.Contains(text, "其中 1 个点整段返回有非零值：不是补 0") || strings.Contains(text, "设备可能给没有数据的地址补 0") ||
		strings.Contains(text, "非标准响应：设备回了") {
		t.Fatalf("标准异常与非零整段返回：\n%s", text)
	}
}

// T09：全部超时，控制点也读不通：不判定为缺通道。
func TestEachPointAllTimeoutsAreInconclusive(t *testing.T) {
	d, segs, _ := pointProbeDef(floatPoints(0, 2), readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	c := &scriptedProbe{reply: func(uint16, int) ([]uint16, error) { return nil, modbus.ErrTimeout }}
	checks, err := checkEachPoint(context.Background(), c, d, segs, func(int) {})
	if err != nil || len(c.sent) != 3 {
		t.Fatalf("TCP 超时应继续读完：%v %v", c.sent, err)
	}
	text, summary, _ := eachPointText(d, checks, segs)
	if !strings.Contains(text, "没有一个点单独读通，缺少可读的对照") || strings.Contains(text, "整段有返回、单独读不通") ||
		strings.Contains(text, "补 0") || !strings.Contains(summary, "整段有返回 0 / 1 段") {
		t.Fatalf("纯超时不能归为稀疏转发：%s\n%s", summary, text)
	}
}

// T11：双字点只认双字请求的结果，单字证据另列：一个字转发、一个字没转发时，双字读通，单字各不相同。
func TestMultiRegisterPointJudgedByItsOwnRequest(t *testing.T) {
	faults := simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 1, Count: 1}}}
	d, segs, _ := pointProbeDef(floatPoints(0), readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	checks, err := checkEachPoint(context.Background(), registerProbeClient(t, modbus.ModeTCP, faults, nil, 4), d, segs, func(int) {})
	if err != nil || len(checks) != 1 || checks[0].Single != 1 {
		t.Fatalf("双字请求读通，点就记为可读：%+v %v", checks, err)
	}
	words, err := probeUnits(context.Background(), registerProbeClient(t, modbus.ModeTCP, faults, nil, 4), d, probeSingle, nil, func(int) {})
	if err != nil || fmt.Sprint(words) != "[-4 1]" {
		t.Fatalf("逐个寄存器另列单字结果，不能由双字结果推断：%v %v", words, err)
	}
}

// T14：中途停止，未检测的点不算读通。
func TestEachPointCancellationLeavesPointsUntested(t *testing.T) {
	d, segs, _ := pointProbeDef(floatPoints(0, 2, 4), readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &scriptedProbe{reply: func(addr uint16, n int) ([]uint16, error) { return make([]uint16, n), nil }}
	checks, err := checkEachPoint(ctx, c, d, segs, func(n int) {
		if n == 2 {
			cancel()
		}
	})
	if err != nil || len(c.sent) != 2 {
		t.Fatalf("停止后不再发请求：%v %v", c.sent, err)
	}
	text, summary, _ := eachPointText(d, checks, segs)
	if !strings.Contains(summary, "可读 1") || !strings.Contains(summary, "未检测 2") || strings.Contains(text, "全部点单独读取都有正常响应") {
		t.Fatalf("停止后的结果：%s\n%s", summary, text)
	}
}

// 从检测对话框选“逐点独立验证”，结果窗口给出两种读取的对照。
func TestEachPointModeFromDialog(t *testing.T) {
	ws := independentProbeWorkspace(t, simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 0, Count: 2}}})
	locked(func() {
		ws.setPoints(floatPoints(0, 2))
		ws.registerProbeDialog()
		top := ws.win.Canvas().Overlays().Top()
		selects := findSelects(top)
		selects[1].SetSelected(probeModeNames[probeEachPoint])
		if !strings.Contains(overlayText(ws), "共 3 次请求") {
			t.Errorf("说明应给出请求数：%s", overlayText(ws))
		}
		test.Tap(findButtons(top, "开始检测")[0])
	})
	waitFor(t, 5*time.Second, "逐点独立验证完成", func() bool { return !ws.probeRunning && strings.Contains(overlayText(ws), "逐点独立验证") })
	locked(func() {
		report := overlayText(ws)
		for _, want := range []string{"整段有返回 1 / 1 段", "可读 1", "读不通 1（其中整段有返回 1）", "40003 点2（FLOAT32）：单独读取非标准响应"} {
			if !strings.Contains(report, want) {
				t.Errorf("结果缺少 %q：%s", want, report)
			}
		}
	})
	snapshotPNG(t, ws.win, "register-probe-each-point.png")
}

// T04：读窗口收到 89 回显时，诊断说明收到了什么，不给减数量、改功能码、扫站号一类的建议。
func TestNonstandardEchoDiagnosis(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	var w *readWindow
	locked(func() {
		ws.proto.SetSelected(protoTCP) // 只有带事务号的 Modbus TCP 才能确定回显属于本次请求
		d := defaultDef()
		d.Start, d.Qty = 8, 2
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "connection", func() bool { return ws.session != nil })
	locked(func() {
		ws.session.sim.SetFaults(simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 0, Count: 2}}})
	})
	waitFor(t, 5*time.Second, "非标准响应诊断", func() bool { return strings.Contains(w.errLbl.Text, "非标准响应") })
	locked(func() {
		if w.actionBtn.Text != "检测这个范围" {
			t.Errorf("应提供检测，而不是 %q", w.actionBtn.Text)
		}
		for _, wrong := range []string{"数量减半", "改用 FC04", "扫描从站", "非法数据值", "没有应答"} {
			if strings.Contains(w.hintLbl.Text+w.actionBtn.Text, wrong) {
				t.Errorf("诊断不应出现 %q：%s", wrong, w.hintLbl.Text)
			}
		}
	})
	tap(ws.connBtn)
}

// 日志里的 89 回显交给 AI 时标明是非标准回显，不带异常码；畸形的 83 帧标为未知，不当作异常 02。
func TestAILogEvidenceClassifiesNonstandardEcho(t *testing.T) {
	ws := openAIWS(t, false)
	tx := []byte{0, 1, 0, 0, 0, 6, 1, 3, 0x03, 0x20, 0, 0x32}
	for _, tc := range []struct {
		name, kind string
		rx         []byte
	}{
		{"echo", "nonstandard_request_echo", []byte{0, 1, 0, 0, 0, 6, 1, 0x89, 0x03, 0x20, 0, 0x32}},
		{"exception-with-extra-bytes", "", []byte{0, 1, 0, 0, 0, 5, 1, 0x83, 2, 0x03, 0x20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locked(func() {
				e := logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventReadFail, TX: tx, RX: tc.rx}, mode: modbus.ModeTCP}
				for _, ev := range ws.aiSnapshotFor(aiTarget{event: &e}, false, false).Evidence {
					if ev.Kind != "log_result" {
						continue
					}
					var v map[string]any
					json.Unmarshal(ev.Data, &v)
					if _, ok := v["exception_code"]; ok {
						t.Errorf("不能带异常码：%v", v)
					}
					if tc.kind != "" && (v["frame_kind"] != tc.kind || v["response_function"] != float64(0x89)) || tc.kind == "" && v["metadata_valid"] != false {
						t.Errorf("分类不对：%v", v)
					}
				}
			})
		})
	}
	if kind := aiErrorKind(echoErr(8, 2)); kind != "NONSTANDARD_REQUEST_ECHO" {
		t.Errorf("错误种类：%s", kind)
	}
}

// T04–T06：报文解析按原请求功能码和长度判断，89 回显和带多余字节的 83 都不把第二个字节当异常码。
func TestDescribeNonstandardResponses(t *testing.T) {
	rx := func(status modbus.Status, addr, count uint16, pdu ...byte) []decodeRow {
		return describePacket(modbus.Packet{Mode: modbus.ModeTCP, Dir: modbus.DirRX, Function: modbus.FuncReadHoldingRegisters,
			Address: addr, Count: count, Status: status, Raw: modbus.EncodeADU(modbus.ModeTCP, 1, 14, pdu)}, nil)
	}
	text := func(rows []decodeRow) string { return rowsText(rows) }
	field := func(rows []decodeRow, name string) *decodeRow {
		for i := range rows {
			if rows[i].Name == name {
				return &rows[i]
			}
		}
		return nil
	}
	echo := rx(modbus.StatusNonstandard, 800, 50, 0x89, 0x03, 0x20, 0x00, 0x32)
	if field(echo, "异常码") != nil || strings.Contains(text(echo), "非法数据值") {
		t.Errorf("89 回显后面的 03 是地址高字节，不是异常码：\n%s", text(echo))
	}
	if f := field(echo, "回显地址"); f == nil || f.Hex != "03 20" || !strings.Contains(f.Meaning, "与请求相同") {
		t.Errorf("应列出回显的地址：\n%s", text(echo))
	}
	if !strings.Contains(text(echo), "两者都不是") || !strings.Contains(text(echo), "属于本次请求") {
		t.Errorf("应先说明不是正常响应也不是标准异常：\n%s", text(echo))
	}
	if extra := rx(modbus.StatusUnexpected, 8, 2, 0x83, 0x02, 0x00, 0x08); field(extra, "异常码") != nil || !strings.Contains(text(extra), "不按异常码解读") {
		t.Errorf("83 02 后还有字节，不能当标准异常：\n%s", text(extra))
	}
	if std := rx(modbus.StatusException, 8, 2, 0x83, 0x02); field(std, "异常码") == nil || !strings.Contains(text(std), "非法数据地址") {
		t.Errorf("标准异常照常解析：\n%s", text(std))
	}
	// 晚到帧不一定属于记录行上的请求，不按那条请求判断回显，但也不当异常码
	if late := rx(modbus.StatusLate, 8, 2, 0x89, 0x00, 0x08, 0x00, 0x02); field(late, "异常码") != nil || field(late, "回显地址") != nil {
		t.Errorf("晚到帧：\n%s", text(late))
	}
}
