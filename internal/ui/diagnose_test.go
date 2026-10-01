package ui

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// 读取定义换成 FLOAT32 ABCD 读温差设定：值不合理，自动建议改用 CDAB，一键修正。
func TestFloatOrderSuggestion(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order = 346, 4, kindFloat32, modbus.OrderABCD
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "建议改用 CDAB", func() bool { return hasData(w) && w.actionBtn.Text == "改用 CDAB" && w.actionBtn.Visible() })
	tap(w.actionBtn)
	waitFor(t, 5*time.Second, "改用 CDAB 后显示 15.0", func() bool {
		v, _ := w.valueText(0)
		return w.def.Order == modbus.OrderCDAB && v == "15.0"
	})
	tap(ws.connBtn)
}

// 读取范围越界：异常 02 给出原因，并提供逐个探测。
func TestIllegalAddressDiagnosis(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start = 995 // 模拟器只有 0–999
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "异常 02 诊断", func() bool {
		return strings.Contains(w.errLbl.Text, "异常 02") && w.actionBtn.Text == "逐个探测可读地址"
	})
	var hint string
	var s *session
	locked(func() { hint, s = w.hintLbl.Text, ws.session })
	if !strings.Contains(hint, "40996–41005") {
		t.Errorf("诊断应说明读取范围：%q", hint)
	}
	res, err := probe(context.Background(), s.client, w.def, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 1 1 1 1 -1 -1 -1 -1 -1]" {
		t.Errorf("探测结果 %v %v", res, err)
	}
	tap(ws.connBtn)
}

// 协议选错：设备是 RTU over TCP，却按 Modbus TCP 连接。诊断给出“识别协议”，一键识别后自动改协议并重新连接。
func TestDetectProtocolFromDiagnosis(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() { w = ws.addWindow(defaultDef()) })
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.target.SetText(ln.Addr().String())
		ws.proto.SetSelected(protoTCP)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "诊断给出识别协议", func() bool { return w.actionBtn.Text == "识别协议" && w.actionBtn.Visible() })
	tap(w.actionBtn)
	waitFor(t, 10*time.Second, "识别为 RTU over TCP 并重新连接", func() bool {
		return ws.proto.Selected == protoRTUTCP && ws.session != nil && hasData(w)
	})
}

// ASCII 模式下 LRC 错误要和 CRC 错误一样给出原因分析，不能只显示原始错误文字。
func TestLRCDiagnosis(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeASCIIOverTCP, 1, simulator.HeatStation())
	srv.SetFaults(simulator.Faults{CRCRate: 1})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.target.SetText(ln.Addr().String())
		ws.proto.SetSelected(protoASCIITCP)
		w = ws.addWindow(defaultDef())
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "LRC 错误的分析", func() bool {
		return w.errLbl.Text == "LRC 错误：响应校验失败" && strings.Contains(w.hintLbl.Text, "波特率")
	})
	tap(ws.connBtn)
}
