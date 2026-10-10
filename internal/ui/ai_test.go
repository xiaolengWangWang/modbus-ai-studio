package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modbus-ai-studio/internal/ai"
	"modbus-ai-studio/internal/modbus"
)

func TestAISnapshotPrivacyAndFrozenCopy(t *testing.T) {
	ws := openAIWS(t, true)
	locked(func() {
		w := ws.current()
		w.def.Name = "private-point-name"
		ws.target.SetText("private-host:502")
		w.mu.Lock()
		w.regs = []uint16{54321, 0, 65535}
		w.mu.Unlock()
		p := modbus.Packet{Time: time.Now(), Dir: modbus.DirRX, Slave: w.def.Slave, Function: w.def.Function, Address: w.def.Start, Count: uint16(w.def.Qty), Raw: []byte{0xDE, 0xAD, 0xBE, 0xEF}, Status: modbus.StatusSuccess, Err: fmt.Errorf("private-host key-private")}
		s := ws.aiSnapshot(&p, false, false)
		b, err := s.JSON()
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, secret := range []string{"private-host", "private-point-name", "key-private", "DE AD BE EF", "54321"} {
			if strings.Contains(text, secret) {
				t.Errorf("private data leaked: %s", secret)
			}
		}
		full := ws.aiSnapshot(&p, true, true)
		reading := ws.aiSnapshot(nil, false, true)
		readingBefore, _ := reading.JSON()
		before, err := full.JSON()
		if err != nil {
			t.Fatal(err)
		}
		p.Raw[0] = 0
		w.mu.Lock()
		w.regs[0] = 1
		w.mu.Unlock()
		after, _ := full.JSON()
		readingAfter, _ := reading.JSON()
		if string(before) != string(after) || string(readingBefore) != string(readingAfter) {
			t.Error("snapshot changed after capture")
		}
		if !strings.Contains(string(before), "DE AD BE EF") || !strings.Contains(string(readingBefore), "54321") {
			t.Error("explicit data inclusion missing")
		}
	})
}
func TestAISnapshotPreservesSelectionAndLocalEvidenceIDs(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		now := time.Now()
		for i := 0; i < 100; i++ {
			ws.ring.push(modbus.Packet{Time: now, Dir: modbus.DirTX, RequestID: 1, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: uint16(i), Count: 1})
		}
		old := modbus.Packet{Time: now.Add(-time.Hour), Dir: modbus.DirRX, RequestID: 1, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 999, Count: 1, Status: modbus.StatusLate}
		s := ws.aiSnapshot(&old, false, false)
		seen := map[string]bool{}
		packets := 0
		selected := false
		for _, e := range s.Evidence {
			if seen[e.ID] {
				t.Error("duplicate evidence ID")
			}
			seen[e.ID] = true
			if e.Kind == "packet" {
				packets++
			}
			if e.Kind == "selected_packet" {
				var v struct {
					Status      string `json:"status"`
					Association string `json:"association"`
				}
				if json.Unmarshal(e.Data, &v) != nil {
					t.Fatal("invalid selected packet")
				}
				selected = v.Status == string(modbus.StatusLate) && v.Association == "unknown_late_frame_owner"
			}
		}
		if !selected || packets > 63 {
			t.Errorf("selection lost or context unbounded: selected=%v packets=%d", selected, packets)
		}
	})
}

func TestAISnapshotKeepsOnlyMatchingReadRangeAndKnownLatency(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		w := ws.addWindow(defaultDef())
		now := time.Now()
		for _, p := range []modbus.Packet{
			{Time: now, Dir: modbus.DirTX, Mode: modbus.ModeTCP, Slave: w.def.Slave, Function: w.def.Function, Address: w.def.Start, Count: uint16(w.def.Qty)},
			{Time: now, Dir: modbus.DirRX, Mode: modbus.ModeTCP, Slave: w.def.Slave, Function: w.def.Function, Address: w.def.Start, Count: uint16(w.def.Qty), RTT: 350 * time.Microsecond},
			{Time: now, Dir: modbus.DirRX, Mode: modbus.ModeTCP, Slave: w.def.Slave, Function: w.def.Function, Address: w.def.Start, Count: uint16(w.def.Qty + 1)},
		} {
			ws.ring.push(p)
		}
		s := ws.aiSnapshot(nil, false, false)
		packets := 0
		for _, e := range s.Evidence {
			if e.Kind != "packet" {
				continue
			}
			packets++
			var v map[string]any
			if err := json.Unmarshal(e.Data, &v); err != nil {
				t.Fatal(err)
			}
			if v["quantity"] != float64(w.def.Qty) {
				t.Error("a different read range contaminated the snapshot")
			}
			if v["direction"] == string(modbus.DirTX) {
				if _, ok := v["rtt_ms"]; ok {
					t.Error("unknown send latency was presented as a measured zero")
				}
			} else if v["rtt_ms"] != 0.35 {
				t.Error("known response latency was lost")
			}
		}
		if packets != 2 {
			t.Errorf("expected the matching request and response, got %d packets", packets)
		}
	})
}

func TestAIToolRetryFailureAndCancellationKeepCompletedReport(t *testing.T) {
	ws := openAIWS(t, false)
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			fmt.Fprint(w, `{"model":"deepseek-flash","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"上次成功报告\",\"observations\":[],\"hypotheses\":[],\"next_checks\":[]}"}}]}`)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			close(started)
			<-release
		}
	}))
	defer srv.Close()
	defer close(release)
	var tool *aiTool
	locked(func() {
		ws.openAI(nil)
		tool = ws.ai
		tool.key.SetText("test-secret")
		tool.endpoint = srv.URL
		tool.start(false)
	})
	waitFor(t, 3*time.Second, "first report", func() bool { return !tool.busy })
	var report, export string
	locked(func() { report, export = tool.reportText, tool.exportText; tool.start(false) })
	waitFor(t, 3*time.Second, "retry failure", func() bool { return !tool.busy })
	locked(func() {
		if tool.reportText != report || tool.exportText != export || tool.copyBtn.Disabled() || tool.exportBtn.Disabled() {
			t.Error("a failed retry discarded the completed report")
		}
		tool.start(false)
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not start")
	}
	locked(func() {
		tool.cancelRequest()
		if tool.reportText != report || tool.exportText != export || tool.copyBtn.Disabled() || tool.exportBtn.Disabled() {
			t.Error("cancelling a retry discarded the completed report")
		}
		if !strings.Contains(tool.state.Text, "保留") {
			t.Error("cancel status did not explain that the previous report remains available")
		}
	})
}
func TestAIToolCancellationDiscardsOldResponse(t *testing.T) {
	ws := openAIWS(t, false)
	started := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"OLD RESULT\",\"observations\":[],\"hypotheses\":[],\"next_checks\":[]}"}}]}`)
		close(ended)
	}))
	defer srv.Close()
	var tool *aiTool
	locked(func() {
		ws.openAI(nil)
		tool = ws.ai
		tool.key.SetText("test-secret")
		tool.endpoint = srv.URL
		tool.start(false)
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("analysis did not start")
	}
	locked(func() {
		tool.cancelRequest()
		if tool.busy || tool.send.Disabled() {
			t.Error("cancellation did not restore controls")
		}
		tool.setOutput("NEW RESULT")
	})
	close(release)
	<-ended
	time.Sleep(100 * time.Millisecond)
	locked(func() {
		if tool.reportText != "NEW RESULT" {
			t.Error("old response replaced current result")
		}
		ws.openAI(nil)
		if ws.ai != tool {
			t.Error("duplicate assistant window")
		}
	})
}
func TestAIToolCloseCancelsPendingRequest(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		tool.busy = true
		tool.generation = 4
		cancelled := false
		tool.cancel = func() { cancelled = true }
		tool.win.Close()
		if !cancelled || ws.ai != nil || !tool.closed || tool.generation == 4 {
			t.Error("closing assistant did not cancel request")
		}
	})
}

func TestAIToolDisplaysReportAndMinimalConnectionTest(t *testing.T) {
	ws := openAIWS(t, false)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body.Messages[1].Content)
		fmt.Fprint(w, `{"model":"deepseek-flash","usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9},"choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"链路需要人工检查\",\"observations\":[{\"text\":\"未连接\",\"evidence_ids\":[\"E1\"]}],\"hypotheses\":[],\"next_checks\":[\"检查链路\"]}"}}]}`)
	}))
	defer srv.Close()
	var tool *aiTool
	locked(func() {
		ws.openAI(nil)
		tool = ws.ai
		tool.key.SetText("test-secret")
		tool.endpoint = srv.URL
		tool.question.SetText("private-device-question")
		tool.start(true)
	})
	waitFor(t, 3*time.Second, "AI connection test", func() bool { return !tool.busy })
	locked(func() {
		if !strings.Contains(tool.state.Text, "连接成功") || !strings.Contains(tool.state.Text, "合计 9") {
			t.Errorf("test result missing: %s", tool.state.Text)
		}
		tool.start(false)
	})
	waitFor(t, 3*time.Second, "AI report", func() bool { return !tool.busy })
	locked(func() {
		if !strings.Contains(tool.reportText, "链路需要人工检查") || !strings.Contains(tool.reportText, "[E1]") || !strings.Contains(tool.reportText, tool.snapshot.ID) {
			t.Error("report missing facts, citations or snapshot")
		}
		if strings.Contains(tool.reportText, "test-secret") {
			t.Error("report disclosed API key")
		}
		if tool.exportBtn.Disabled() || tool.copyBtn.Disabled() || !strings.Contains(tool.exportText, "发送时冻结证据") || !strings.Contains(tool.exportText, `"snapshot_id"`) || strings.Contains(tool.exportText, "test-secret") {
			t.Error("completed report did not enable safe standalone export")
		}
	})
	if len(requests) != 2 || strings.Contains(requests[0], "private-device-question") || strings.Contains(requests[0], "workspace_state") || !strings.Contains(requests[1], "private-device-question") {
		t.Errorf("test did not isolate synthetic evidence")
	}
}

func TestAIToolSettingsPersistOnlyEncryptedKey(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		tool.keyPath = filepath.Join(t.TempDir(), "deepseek.key")
		tool.key.SetText("ui-test-secret")
		tool.model.SetText("test-model")
		if ai.ProtectedStorage {
			tool.remember.SetChecked(true)
			tool.saveSettings()
			stored, err := ai.LoadKey(tool.keyPath)
			if err != nil || stored != "ui-test-secret" {
				t.Fatalf("encrypted save failed: %v", err)
			}
		}
		tool.remember.SetChecked(false)
		tool.saveSettings()
		stored, err := ai.LoadKey(tool.keyPath)
		if err != nil || stored != "" {
			t.Fatalf("session-only setting left persisted credential: %v", err)
		}
		if ws.app.Preferences().String("ai.deepseek.model") != "test-model" {
			t.Error("model setting not saved")
		}
	})
}

func TestAIToolNewSelectionInvalidatesPreviousReport(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		first := modbus.Packet{Time: time.Now(), Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 10, Count: 1}
		second := first
		second.Address = 20
		ws.openAI(&first)
		tool := ws.ai
		oldID := tool.snapshot.ID
		tool.setOutput("OLD REPORT FOR ADDRESS 10")
		ws.openAI(&second)
		if tool.snapshot.ID == oldID || strings.Contains(tool.reportText, "OLD REPORT") {
			t.Error("new evidence retained a previous object's report")
		}
		tool.setOutput("ANOTHER OLD REPORT")
		tool.includeValues.SetChecked(true)
		if strings.Contains(tool.reportText, "OLD REPORT") {
			t.Error("inclusion change retained previous report")
		}
	})
}
