package diagnose

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// 晚到帧的应答延迟按它回的那条请求记，不按收到它时正在等的请求；只知道范围时取下限。别的主站的响应不算晚到。
func TestEvidenceUsesLateRequest(t *testing.T) {
	var ev Evidence
	owner := modbus.Packet{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 100}
	rx := modbus.Packet{Dir: modbus.DirRX, Status: modbus.StatusLate, Slave: 9, Function: modbus.FuncReadHoldingRegisters, Address: 500}
	rx.Err = &modbus.LateError{Request: owner, Timeout: time.Second, Elapsed: 2500 * time.Millisecond, Max: 3 * time.Second}
	ev.Observe(rx)
	rx.Err = &modbus.LateError{Request: modbus.Packet{Slave: 2, Function: modbus.FuncReadHoldingRegisters}, Cancelled: true, Timeout: time.Second, Elapsed: 9 * time.Second, Max: 9 * time.Second}
	ev.Observe(rx)
	rx.Err = &modbus.ForeignError{TxID: 0x1CA6}
	ev.Observe(rx)
	if d := ev.DelayOf(1, modbus.FuncReadHoldingRegisters, 100); d != 2500*time.Millisecond {
		t.Errorf("按回的那条请求记，取下限：%v", d)
	}
	if ev.DelayOf(9, modbus.FuncReadHoldingRegisters, 500) != 0 || ev.DelayOf(2, modbus.FuncReadHoldingRegisters, 0) != 0 {
		t.Error("不应记到正在等的请求上；取消的请求不算超时")
	}
	if ev.Late() != 2 || ev.Foreign() != 1 || ev.OKSeen() {
		t.Errorf("晚到 %d、别的主站的响应 %d、正常响应 %v", ev.Late(), ev.Foreign(), ev.OKSeen())
	}
	ev.Reset()
	if ev.Late() != 0 || ev.DelayOf(1, modbus.FuncReadHoldingRegisters, 100) != 0 {
		t.Error("重新连接后清空")
	}
}

func TestSuggestTimeout(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{1100 * time.Millisecond: 2 * time.Second, 100 * time.Millisecond: 500 * time.Millisecond, 1013 * time.Millisecond: 2 * time.Second} {
		if got := SuggestTimeout(in); got != want {
			t.Errorf("%v → %v，期望 %v", in, got, want)
		}
	}
}

func TestAnalyze(t *testing.T) {
	read := Request{Slave: 3, Function: modbus.FuncReadHoldingRegisters, Start: 600, Qty: 4, Scan: 5 * time.Second}
	late := &Evidence{}
	late.Observe(modbus.Packet{Status: modbus.StatusLate, Err: &modbus.LateError{Request: modbus.Packet{Slave: 3, Function: modbus.FuncReadHoldingRegisters, Address: 600},
		Timeout: time.Second, Elapsed: 1013 * time.Millisecond, Max: 1013 * time.Millisecond}})
	foreign := &Evidence{}
	foreign.Observe(modbus.Packet{Status: modbus.StatusLate, Err: &modbus.ForeignError{TxID: 1}})
	tcp := Context{Mode: modbus.ModeTCP, Timeout: time.Second}
	for name, c := range map[string]struct {
		req    Request
		ctx    Context
		err    error
		action Action
		has    string
	}{
		"晚到响应建议调大超时":       {read, Context{Mode: modbus.ModeTCP, Timeout: time.Second, Evidence: late}, modbus.ErrTimeout, SetTimeout, "约 1013 ms 才到"},
		"别的主站的响应不建议调超时":    {read, Context{Mode: modbus.ModeTCP, Timeout: time.Second, Evidence: foreign}, modbus.ErrTimeout, None, "串口服务器 / DTU"},
		"TCP 一直超时建议识别协议":   {read, tcp, modbus.ErrTimeout, DetectProtocol, "RTU over TCP"},
		"模拟器不建议识别协议":       {read, Context{Mode: modbus.ModeTCP, Timeout: time.Second, Simulator: true}, modbus.ErrTimeout, None, "RTU over TCP"},
		"串口一直超时建议扫串口参数":    {read, Context{Mode: modbus.ModeRTU, Timeout: time.Second}, modbus.ErrTimeout, ScanSerial, "波特率"},
		"异常 02 建议逐个探测":     {read, tcp, &modbus.ExceptionError{Function: modbus.FuncReadHoldingRegisters, Code: modbus.ExceptionIllegalDataAddress}, ProbeRange, "40601–40604"},
		"FC04 不支持建议 FC03":  {Request{Function: modbus.FuncReadInputRegisters}, tcp, &modbus.ExceptionError{Function: modbus.FuncReadInputRegisters, Code: modbus.ExceptionIllegalFunction}, UseFC03, "FC03"},
		"设备忙、不知道扫描周期时只说原因": {Request{Function: modbus.FuncReadHoldingRegisters}, tcp, &modbus.ExceptionError{Code: modbus.ExceptionSlaveDeviceBusy}, None, ""},
		"连接断开建议重连":         {read, tcp, fmt.Errorf("%w：EOF", modbus.ErrConnection), Reconnect, "重新连接"},
	} {
		r := Analyze(c.req, c.ctx, c.err)
		if r.Action != c.action || !strings.Contains(r.Text+r.Hint, c.has) || (r.Action != None) != (r.Label != "") {
			t.Errorf("%s：%+v", name, r)
		}
		if c.action == SetTimeout && (r.Timeout != 2*time.Second || r.Label != "超时改为 2000 ms") {
			t.Errorf("%s：建议的超时 %v，%s", name, r.Timeout, r.Label)
		}
	}
}
