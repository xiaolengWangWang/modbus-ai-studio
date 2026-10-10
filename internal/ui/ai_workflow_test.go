package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
)

func TestAIFaultEntryBindsClickedWindowAndRegister(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		ws.loadDemo()
		first, other := ws.windows[0], ws.windows[1]
		ws.setCurrent(other)
		first.setDiagnosis(diagnosis{Text: "TIMEOUT"})
		first.aiBtn.OnTapped()
		tool := ws.ai
		if tool == nil {
			t.Fatal("fault entry did not open assistant")
		}
		if !strings.Contains(tool.source.Text, "窗口 1") {
			t.Fatal("fault entry bound to another current window")
		}
		ws.setCurrent(first)
		first.sel = 2
		ws.inspect.showRegister(first)
		ws.inspect.openAI()
		b, _ := tool.snapshot.JSON()
		if !strings.Contains(string(b), `"selected_register_offset": 2`) {
			t.Fatal("register inspector lost selected address")
		}
		ws.setCurrent(other)
		tool.includeValues.SetChecked(true)
		b, _ = tool.snapshot.JSON()
		if !strings.Contains(string(b), `"selected_register_offset": 2`) {
			t.Error("another current window changed the assistant's bound register")
		}
		var definition struct {
			Address uint16 `json:"address"`
		}
		for _, e := range tool.snapshot.Evidence {
			if e.Kind == "current_read_definition" {
				json.Unmarshal(e.Data, &definition)
			}
		}
		if definition.Address != 0 {
			t.Error("data inclusion changed to another current window")
		}
		ws.removeWindow(first)
		tool.capture()
		if !tool.send.Disabled() || !strings.Contains(tool.state.Text, "已关闭") {
			t.Error("closed source window silently borrowed another source")
		}
	})
}

func TestAILogEntryKeepsHistoricalEvidencePrivateAndFrozen(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		old := time.Now().Add(-time.Hour)
		tx := []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}
		rx := []byte{0, 1, 0, 0, 0, 3, 1, 0x83, 2}
		event := logEntry{Event: recorder.Event{Time: old, Kind: recorder.EventReadFail, Window: 7, Detail: "private-host:502 api-secret", Analysis: "point-private C:\\private\\manual", TX: tx, RX: rx}, mode: modbus.ModeTCP}
		ws.inspect.showLog(event)
		ws.inspect.openAI()
		tool := ws.ai
		if tool == nil {
			t.Fatal("log entry did not open assistant")
		}
		b, _ := tool.snapshot.JSON()
		text := string(b)
		for _, private := range []string{"private-host", "api-secret", "point-private", "manual", "00 01"} {
			if strings.Contains(text, private) {
				t.Errorf("private log content sent by default: %s", private)
			}
		}
		if !strings.Contains(text, `"exception_code": 2`) || !strings.Contains(text, `"address": 10`) {
			t.Error("log request/exception metadata missing")
		}
		for _, e := range tool.snapshot.Evidence {
			if e.Kind == "current_read_definition" || e.Kind == "packet" {
				t.Error("historical log borrowed current read evidence")
			}
		}
		tx[8] = 99
		rx[8] = 4
		tool.includeRaw.SetChecked(true)
		b, _ = tool.snapshot.JSON()
		text = string(b)
		if !strings.Contains(text, "00 01 00 00 00 06 01 03 00 0A 00 02") || !strings.Contains(text, "00 01 00 00 00 03 01 83 02") {
			t.Error("selected log bytes were not frozen")
		}
		if !strings.Contains(tool.source.Text, "日志") || !strings.Contains(tool.source.Text, "窗口 7") {
			t.Error("log source identity not visible")
		}
	})
}

func TestAISelectingLogStopsLiveRegisterInspectorRefresh(t *testing.T) {
	ws := openAIWS(t, true)
	locked(func() {
		w := ws.current()
		w.sel = 0
		ws.inspect.showRegister(w)
		e := logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventReadFail, Window: w.no}}
		ws.log.add(e)
		ws.log.list.Select(0)
		if ws.inspect.src != nil || ws.inspect.event == nil {
			t.Fatal("log selection left register inspector active")
		}
		ws.inspect.clear()
		if ws.inspect.event != nil {
			t.Error("cleared inspector kept old log target")
		}
	})
}

func TestAIKeyReadErrorAndCancellationRemainVisible(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	ws := openAIWS(t, false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		tool.keyPath = filepath.Join(t.TempDir(), "broken.key")
		if err := os.WriteFile(tool.keyPath, []byte("broken-ciphertext"), 0600); err != nil {
			t.Fatal(err)
		}
		tool.loadKey()
		tool.capture()
		if tool.key.Text != "" || !strings.Contains(tool.state.Text, "凭据") {
			t.Error("capture hid credential loading failure")
		}
		tool.setOutput("正在分析冻结证据…")
		tool.busy = true
		tool.testOnly = false
		tool.cancel = func() {}
		tool.cancelRequest()
		if strings.Contains(tool.reportText, "正在分析") || !strings.Contains(tool.reportText, "已取消") {
			t.Error("cancelled analysis still appears in progress")
		}
	})
}

type reportWriter struct {
	bytes.Buffer
	closed             bool
	writeErr, closeErr error
	short              bool
}

func (w *reportWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return len(p) - 1, nil
	}
	return w.Buffer.Write(p)
}
func (w *reportWriter) Close() error { w.closed = true; return w.closeErr }
func TestAIReportExportHandlesWriteAndCloseFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    reportWriter
		want error
	}{{name: "valid"}, {name: "short", w: reportWriter{short: true}, want: io.ErrShortWrite}, {name: "write", w: reportWriter{writeErr: io.ErrClosedPipe}, want: io.ErrClosedPipe}, {name: "close", w: reportWriter{closeErr: io.ErrUnexpectedEOF}, want: io.ErrUnexpectedEOF}} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeAIReport(&tc.w, "# 中文诊断报告\n\n证据 E1\n")
			if !errors.Is(err, tc.want) || !tc.w.closed {
				t.Errorf("export failed contract: err=%v closed=%v", err, tc.w.closed)
			}
			if tc.want == nil && tc.w.String() != "# 中文诊断报告\n\n证据 E1\n" {
				t.Error("report changed during export")
			}
		})
	}
}

func TestAIReportRenderingNeverCreatesRemoteContent(t *testing.T) {
	segments := aiReportSegments("# AI 诊断报告\n\n![remote](https://invalid.example/image)\n[link](https://invalid.example/)\n<script>bad()</script>")
	for _, segment := range segments {
		if _, ok := segment.(*widget.TextSegment); !ok {
			t.Fatal("report rendered model text as active remote content")
		}
	}
}

func TestAIHistoricalLogRejectsMismatchedOrCorruptFrames(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		for _, tc := range []struct {
			name   string
			mode   modbus.Mode
			tx, rx []byte
		}{
			{name: "oversized-exception", mode: modbus.ModeTCP, tx: []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}, rx: []byte{0, 1, 0, 0, 0, 4, 1, 0x83, 2, 0xFF}},
			{name: "wrong-transaction", mode: modbus.ModeTCP, tx: []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}, rx: []byte{0, 2, 0, 0, 0, 3, 1, 0x83, 2}},
			{name: "unknown-protocol", tx: []byte{1, 3, 0, 0, 0, 1, 0, 0}},
			{name: "bad-crc", mode: modbus.ModeRTU, tx: []byte{1, 3, 0, 0, 0, 1, 0, 0}},
		} {
			e := logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventReadFail, TX: tc.tx, RX: tc.rx}, mode: tc.mode}
			s := ws.aiSnapshotFor(aiTarget{event: &e}, false, false)
			b, _ := s.JSON()
			if strings.Contains(string(b), `"exception_code"`) || !strings.Contains(string(b), `"metadata_valid": false`) {
				t.Errorf("%s invented metadata for invalid frames", tc.name)
			}
		}
	})
}

func TestAIHistoricalLogOmitsUnrecordedPacketFacts(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		e := logEntry{Event: recorder.Event{Time: time.Now(), TX: []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}, RX: []byte{0, 1, 0, 0, 0, 3, 1, 0x83, 2}}, mode: modbus.ModeTCP}
		for _, ev := range ws.aiSnapshotFor(aiTarget{event: &e}, false, false).Evidence {
			if ev.Kind == "selected_log" {
				continue
			}
			var v map[string]any
			json.Unmarshal(ev.Data, &v)
			for _, key := range []string{"time", "request_id", "rtt_ms", "status"} {
				if _, exists := v[key]; exists {
					t.Errorf("historical %s invented %s", ev.Kind, key)
				}
			}
		}
	})
}

func TestAILiveLogPreservesSubMillisecondLatency(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		e := logEntry{Event: recorder.Event{Time: time.Now(), TX: []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}, RX: []byte{0, 1, 0, 0, 0, 3, 1, 0x83, 2}}, mode: modbus.ModeTCP}
		e.res = &modbus.Packet{Time: e.Time, Dir: modbus.DirRX, RTT: 350 * time.Microsecond, Status: modbus.StatusException}
		found := false
		for _, ev := range ws.aiSnapshotFor(aiTarget{event: &e}, false, false).Evidence {
			if ev.Kind != "log_result" {
				continue
			}
			found = true
			var v map[string]any
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				t.Fatal(err)
			}
			if v["rtt_ms"] != 0.35 {
				t.Errorf("live log truncated measured latency: %v", v["rtt_ms"])
			}
		}
		if !found {
			t.Error("live log response missing")
		}
	})
}

func TestAILiveCorruptResponseKeepsObservedErrorWithoutInventingFields(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		e := logEntry{Event: recorder.Event{Time: time.Now(), TX: []byte{0, 1, 0, 0, 0, 6, 1, 3, 0, 10, 0, 2}, RX: []byte{1, 3, 0, 0}}, mode: modbus.ModeTCP}
		e.res = &modbus.Packet{Time: e.Time, Dir: modbus.DirRX, Mode: e.mode, Slave: 1, Function: 3, Address: 10, Count: 2, Status: modbus.StatusCRCError, Err: modbus.ErrCRC, RequestID: 9, Raw: e.RX}
		for _, ev := range ws.aiSnapshotFor(aiTarget{event: &e}, false, false).Evidence {
			if ev.Kind != "log_result" {
				continue
			}
			var v map[string]any
			json.Unmarshal(ev.Data, &v)
			if v["metadata_valid"] != false || v["status"] != string(modbus.StatusCRCError) {
				t.Error("invalid response lost observed error or remained valid")
			}
			for _, key := range []string{"slave", "function", "address", "quantity", "exception_code"} {
				if _, exists := v[key]; exists {
					t.Errorf("invalid response invented %s", key)
				}
			}
			if _, exists := v["rtt_ms"]; exists {
				t.Error("unrecorded live latency was presented as zero")
			}
		}
	})
}

func TestAIRefreshUsesOriginatingInspector(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		in := newInspector(ws)
		in.showLog(logEntry{Event: recorder.Event{Time: time.Now(), Window: 7}})
		in.openAI()
		in.showLog(logEntry{Event: recorder.Event{Time: time.Now(), Window: 8}})
		ws.inspect.showPacket(modbus.Packet{Slave: 99})
		ws.ai.refresh.OnTapped()
		if !strings.Contains(ws.ai.source.Text, "窗口 8") {
			t.Error("historical refresh borrowed main-window selection")
		}
	})
}

func TestAIHistorySourceCannotBorrowLiveDataAfterClearOrClose(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		in := newInspector(ws)
		in.selectionOnly = true
		in.showLog(logEntry{Event: recorder.Event{Time: time.Now(), Window: 7}})
		in.openAI()
		tool := ws.ai
		in.clear()
		tool.refresh.OnTapped()
		if !tool.send.Disabled() || !strings.Contains(tool.state.Text, "没有选中") {
			t.Error("empty history selection borrowed live data")
		}
		in.showLog(logEntry{Event: recorder.Event{Time: time.Now(), Window: 8}})
		tool.refresh.OnTapped()
		if tool.send.Disabled() {
			t.Error("valid history selection did not recover")
		}
		in.closed = true
		tool.includeRaw.SetChecked(true)
		if !tool.send.Disabled() || !strings.Contains(tool.state.Text, "已关闭") {
			t.Error("closed history source remained refreshable")
		}
		ws.openAITarget(aiTarget{})
		if tool.send.Disabled() {
			t.Error("new main-window entry did not restore source")
		}
	})
}

func TestAINewEntryCancelsBusyAnalysisAndBindsNewTarget(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		ws.loadDemo()
		ws.openAITarget(aiTarget{read: ws.windows[0]})
		tool := ws.ai
		cancelled := false
		tool.busy = true
		tool.cancel = func() { cancelled = true }
		gen := tool.generation
		ws.windows[1].aiBtn.OnTapped()
		if !cancelled || tool.busy || tool.generation <= gen || !strings.Contains(tool.source.Text, "窗口 2") {
			t.Error("new explicit entry retained old busy analysis")
		}
	})
}

func TestAISelectedRegisterOutsideNewDefinitionRequiresRefresh(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		ws.loadDemo()
		w := ws.windows[0]
		ws.setCurrent(w)
		w.sel = 2
		ws.openAITarget(aiTarget{read: w})
		tool := ws.ai
		ws.redefine(w, func(d *readDef) { d.Start = 100 })
		tool.includeValues.SetChecked(true)
		if !tool.send.Disabled() || !strings.Contains(tool.state.Text, "范围") {
			t.Error("assistant paired an old selected register with a new unrelated range")
		}
	})
}
