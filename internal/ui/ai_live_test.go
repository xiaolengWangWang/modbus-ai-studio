//go:build deepseek_live

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	"modbus-ai-studio/internal/modbus"
)

// Explicitly opt in: this test makes three paid requests to the official API.
// It reads the existing key without saving settings, and sends synthetic data.
func TestDeepSeekLiveDiagnosticWorkflow(t *testing.T) {
	if os.Getenv("MODBUS_AI_LIVE_TEST") != "1" {
		t.Skip("set MODBUS_AI_LIVE_TEST=1 to test the official API")
	}
	ws := openWS(t, test.NewTempApp(t), false)
	var tool *aiTool
	locked(func() {
		ws.openAI(nil)
		tool = ws.ai
		if strings.TrimSpace(tool.key.Text) == "" {
			t.Fatal("DeepSeek credential unavailable; no request sent")
		}
		tool.start(true)
	})
	waitFor(t, 70*time.Second, "official DeepSeek connection test", func() bool { return !tool.busy })
	locked(func() {
		if !strings.HasPrefix(tool.state.Text, "连接成功") {
			t.Fatalf("Connection test failed: %s", tool.state.Text)
		}
		t.Log(tool.state.Text)
	})
	for _, tc := range []struct {
		name     string
		packet   modbus.Packet
		question string
	}{
		{"timeout", modbus.Packet{Mode: modbus.ModeTCP, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 0, Count: 2, Dir: modbus.DirRX, Status: modbus.StatusTimeout, Err: modbus.ErrTimeout, RTT: time.Second}, "这是合成测试：请根据证据解释超时的含义及局限，区分事实和假设，给出最多三项人工检查，不推测寄存器值或设备型号。"},
		{"illegal-address", modbus.Packet{Mode: modbus.ModeTCP, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 995, Count: 10, Dir: modbus.DirRX, Status: modbus.StatusException, Err: &modbus.ExceptionError{Function: modbus.FuncReadHoldingRegisters, Code: modbus.ExceptionIllegalDataAddress}, RTT: 350 * time.Microsecond}, "这是合成测试：请解释异常02能证明什么、不能证明什么，区分事实和假设，并给出最多三项人工检查。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.packet.Time = time.Now()
			locked(func() {
				ws.openAI(&tc.packet)
				tool.question.SetText(tc.question)
				tool.start(false)
			})
			waitFor(t, 70*time.Second, "official DeepSeek diagnostic report", func() bool { return !tool.busy })
			locked(func() {
				if !strings.HasPrefix(tool.state.Text, "分析完成") || tool.copyBtn.Disabled() || tool.exportBtn.Disabled() {
					t.Fatalf("Diagnostic workflow failed: %s", tool.state.Text)
				}
				if !strings.Contains(tool.reportText, "[E1]") || !strings.Contains(tool.exportText, tool.snapshot.ID) || strings.Contains(tool.exportText, tool.key.Text) {
					t.Fatal("Report must cite the synthetic evidence and exclude the key")
				}
				test.Tap(tool.copyBtn)
				if ws.app.Clipboard().Content() != tool.reportText {
					t.Fatal("Copy did not preserve the completed report")
				}
				if dir := os.Getenv("MODBUS_AI_LIVE_REPORT_DIR"); dir != "" {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal("Cannot create synthetic report directory")
					}
					if err := os.WriteFile(filepath.Join(dir, tc.name+".md"), []byte(tool.exportText), 0600); err != nil {
						t.Fatal("Cannot save synthetic diagnostic report")
					}
				}
				t.Log(tool.state.Text)
			})
		})
	}
}
