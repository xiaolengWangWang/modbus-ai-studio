package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// sparseClient 是模拟 MCGS 稀疏转发的从站：values 写进保持寄存器，只有 forwarded 里的地址有转发数据。
func sparseClient(t *testing.T, size int, forwarded []simulator.AddrRange, values map[uint16]uint16, observer modbus.Observer) *modbus.Client {
	t.Helper()
	store := simulator.NewStore(size)
	for a, v := range values {
		store.SetRegisters(modbus.AreaHoldingRegisters, a, []uint16{v})
	}
	srv := simulator.NewServer(modbus.ModeTCP, 1, store)
	srv.SetFaults(simulator.Faults{Forwarded: forwarded})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeTCP, Timeout: 250 * time.Millisecond, Observer: observer})
	t.Cleanup(func() { c.Close() })
	return c
}

func sentRequests(sent *[]string) modbus.Observer {
	return modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			*sent = append(*sent, fmt.Sprintf("%d:%d", p.Address, p.Count))
		}
	})
}

// T02 / T03：批量读取全部有返回，快速排查从全 0 段抽查到单独读不通的点；边界对照起点不变、只多一个已单独读通的地址，两轮一致。
func TestQuickCheckFindsContradictionAndBoundary(t *testing.T) {
	var sent []string
	c := sparseClient(t, 64, []simulator.AddrRange{{Start: 0, Count: 8}, {Start: 34, Count: 2}},
		map[uint16]uint16{0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 6, 6: 7, 7: 8, 34: 0x4170, 35: 1}, sentRequests(&sent))
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 40}
	q, err := quickCheckRange(context.Background(), c, d, pointTable{}, quickBudget, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[0:40 8:1 34:1 8:26 8:27 8:26 8:27 36:1]"; fmt.Sprint(sent) != want {
		t.Fatalf("请求顺序 %v，期望 %s：先批量，再抽查全 0 段首个地址，读紧挨着的控制点，边界对照 A B A B，再抽下一段", sent, want)
	}
	if q.runs != 2 || len(q.candidates) != 2 || q.control == nil || q.control.state != 1 || q.boundary == nil || !q.boundary.consistent() || !q.boundary.bZero || q.used != 7 || q.exhausted {
		t.Fatalf("结果：runs=%d candidates=%+v control=%+v boundary=%+v used=%d", q.runs, q.candidates, q.control, q.boundary, q.used)
	}
	out := quickText(d, q, nil)
	for _, want := range []string{"已发现批量读取与单独读取不一致", "抽查的 2 个候选中 2 个单独读不通", "两轮结果一致", "B 在 A 范围内返回的全是 0",
		"A 40009–40034（26 个寄存器）", "B 40009–40035（27 个寄存器）", "这是抽样结果，不代表其他点", "验证请求：7 / 16"} {
		if !strings.Contains(out.text, want) {
			t.Errorf("结果缺少 %q：\n%s", want, out.text)
		}
	}
	if len(out.csvRows) != 2 || !strings.Contains(out.summary, "单独读不通 2") {
		t.Errorf("失败清单 %v，摘要 %s", out.csvRows, out.summary)
	}
	ev := quickEvidence(d, q)
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), `"consistent":true`) || !strings.Contains(string(b), `"single_state":"nonstandard_response"`) {
		t.Errorf("AI 证据：%s", b)
	}
}

// 有点表时按点的宽度抽查，边界对照按点对齐。
func TestQuickCheckUsesPointWidths(t *testing.T) {
	var sent []string
	c := sparseClient(t, 16, []simulator.AddrRange{{Start: 6, Count: 2}}, map[uint16]uint16{6: 0x4170, 7: 1}, sentRequests(&sent))
	pts := floatPoints(0, 2, 4, 6, 8)
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Start: 0, Qty: 10}
	q, err := quickCheckRange(context.Background(), c, d, pts, quickBudget, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := "[0:10 0:2 6:2 0:6 0:8 0:6 0:8 8:2]"; fmt.Sprint(sent) != want {
		t.Fatalf("请求 %v，期望 %s", sent, want)
	}
	if out := quickText(d, q, nil); !strings.Contains(out.text, "40001 点0（FLOAT32）：单独读取非标准响应") {
		t.Errorf("应按点列出：\n%s", out.text)
	}
}

// T13：抽查没发现问题，只能说抽样范围内没有不一致，不说全表正常。
func TestQuickCheckWithoutFindingsOnlyClaimsSample(t *testing.T) {
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{}, nil, 16)
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 16}
	q, err := quickCheckRange(context.Background(), c, d, pointTable{}, quickBudget, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	out := quickText(d, q, nil)
	if !strings.Contains(out.text, "抽查的 1 个候选单独读取都正常，抽样范围内没有发现不一致；不代表全表正常") || strings.Contains(out.text, "已发现") || out.warn {
		t.Fatalf("真实零值不能报问题，也不能说全表正常：\n%s", out.text)
	}
}

// T09：候选点读不通，控制点也读不通：不判定为补零。
func TestQuickCheckWithoutControlIsInconclusive(t *testing.T) {
	c := &scriptedProbe{reply: func(addr uint16, n int) ([]uint16, error) {
		if n > 1 {
			return []uint16{0, 0, 0, 5, 0, 0}, nil
		}
		return nil, modbus.ErrTimeout
	}}
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 6}
	q, _ := quickCheckRange(context.Background(), c, d, pointTable{}, quickBudget, func(int) {})
	out := quickText(d, q, nil)
	if !strings.Contains(out.text, "没有读通的控制点：链路或设备状态不确定，不判定为补零") || strings.Contains(out.text, "已发现批量读取与单独读取不一致") {
		t.Fatalf("控制点读不通：\n%s", out.text)
	}
}

// 预算用完时停下，写明覆盖未完成。
func TestQuickCheckStopsAtBudget(t *testing.T) {
	var sent []string
	c := sparseClient(t, 64, []simulator.AddrRange{{Start: 34, Count: 1}}, map[uint16]uint16{34: 9}, sentRequests(&sent))
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 40}
	q, _ := quickCheckRange(context.Background(), c, d, pointTable{}, 3, func(int) {})
	if q.used > 3 || !q.exhausted || q.boundary != nil {
		t.Fatalf("预算 3 不够做边界对照：used=%d exhausted=%v %v", q.used, q.exhausted, sent)
	}
	if out := quickText(d, q, nil); !strings.Contains(out.text, "覆盖未完成") || !strings.Contains(out.summary, "覆盖未完成") {
		t.Errorf("应写明覆盖未完成：%s\n%s", out.summary, out.text)
	}
}

// T18：导出报告：报告正文、JSONL 明细、CSV 失败清单三个文件，行数与统计一致，JSONL 带原始帧。
func TestExportProbeReport(t *testing.T) {
	run := &probeRun{mode: probeEachPoint, timeout: 250 * time.Millisecond, started: time.Now(), log: newProbeLog(), target: "Modbus TCP 127.0.0.1:502"}
	pts := floatPoints(0, 2, 4)
	d, segs, _ := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	run.def, run.points = d, pts.list()
	raw := sparseClient(t, 8, []simulator.AddrRange{{Start: 0, Count: 2}}, map[uint16]uint16{0: 0x4170}, modbus.ObserverFunc(run.log.frame))
	checks, err := checkEachPoint(context.Background(), loggedProbe{probeClient: raw, log: run.log}, d, segs, func(int) {})
	if err != nil {
		t.Fatal(err)
	}
	run.finished = time.Now()
	out := eachPointOutput(d, checks, segs, nil)
	path := filepath.Join(t.TempDir(), "检测报告.md")
	files, err := exportProbeReport(context.Background(), run, out, "", path)
	if err != nil || len(files) != 3 {
		t.Fatalf("导出：%v %v", files, err)
	}
	md, _ := os.ReadFile(path)
	for _, want := range []string{"实际设备地址未提供", "Modbus TCP 127.0.0.1:502", "SHA-256", "| 批量读取 | 1 | 1 | 0 | 0 |", "| 逐点单独读取 | 3 | 1 | 2 | 0 |",
		"| 逐个寄存器 | 0 | 本次未执行", "(<检测报告.jsonl>)", "(<检测报告-失败清单.csv>)", "共发出 4 条请求", "整段有返回、单独读不通（2 个点）"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("报告缺少 %q：\n%s", want, md)
		}
	}
	f, _ := os.Open(files[1])
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []jsonlEntry
	for sc.Scan() {
		var e jsonlEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, e)
	}
	if len(lines) != 4 || lines[0].Phase != phaseBatch || lines[1].Phase != phasePoint || lines[2].Result != "NONSTANDARD_REQUEST_ECHO" {
		t.Fatalf("JSONL：%+v", lines)
	}
	if fr := lines[2].Frames; len(fr) != 2 || fr[0].Direction != "TX" || !strings.HasSuffix(fr[1].Hex, "89 00 02 00 02") {
		t.Errorf("JSONL 应带这条请求的原始帧：%+v", fr)
	}
	c, _ := os.ReadFile(files[2])
	if !bytes.HasPrefix(c, []byte("\xEF\xBB\xBF")) {
		t.Error("CSV 应带 UTF-8 BOM，Excel 打开中文不乱码")
	}
	records, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(c, []byte("\xEF\xBB\xBF")))).ReadAll()
	if err != nil || len(records) != 3 || records[1][2] != "点2" || records[1][8] != "是" {
		t.Errorf("CSV：%v %v", records, err)
	}

	// 写不进去时报错，不留下半截文件
	bad := filepath.Join(t.TempDir(), "没有这个文件夹", "报告.md")
	if _, err := exportProbeReport(context.Background(), run, out, "", bad); err == nil {
		t.Error("写不进去应报错")
	}
	if entries, _ := os.ReadDir(filepath.Dir(filepath.Dir(bad))); len(entries) != 0 {
		t.Errorf("失败时不应留下文件：%v", entries)
	}
}

// 从检测对话框选快速排查：结果提供导出报告和继续完整检测。
func TestQuickModeFromDialogOffersExportAndFullCheck(t *testing.T) {
	ws := independentProbeWorkspace(t, simulator.Faults{Forwarded: []simulator.AddrRange{{Start: 4, Count: 2}}})
	locked(func() {
		ws.setPoints(floatPoints(0, 2, 4))
		ws.registerProbeDialog()
		top := ws.win.Canvas().Overlays().Top()
		findSelects(top)[1].SetSelected(probeModeNames[probeQuick])
		entries := probeEntries(top)
		entries[1].SetText("0")
		entries[2].SetText("6")
		if !strings.Contains(overlayText(ws), "按点表里的 3 个点") {
			t.Errorf("说明：%s", overlayText(ws))
		}
		test.Tap(findButtons(top, "开始检测")[0])
	})
	waitFor(t, 5*time.Second, "快速排查完成", func() bool { return !ws.probeRunning && strings.Contains(overlayText(ws), "快速排查：") })
	locked(func() {
		report := overlayText(ws)
		for _, want := range []string{"[导出报告]", "[继续逐点独立验证（全部点）]", "[继续逐个寄存器]", "单独读不通 1"} {
			if !strings.Contains(report, want) {
				t.Errorf("结果缺少 %q：%s", want, report)
			}
		}
		top := ws.win.Canvas().Overlays().Top()
		test.Tap(findButtons(top, "继续逐点独立验证（全部点）")[0])
	})
	waitFor(t, 5*time.Second, "接着做逐点独立验证", func() bool {
		return !ws.probeRunning && strings.Contains(overlayText(ws), "逐点独立验证：3 个点")
	})
}
