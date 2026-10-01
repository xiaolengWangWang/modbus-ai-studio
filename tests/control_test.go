package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/control"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// 温差设定 40347：FLOAT32 CDAB，原值 15.0。
var setpoint = control.Target{Slave: 1, Address: 346, Type: modbus.TypeFloat32, ReadOrder: modbus.OrderCDAB}

var fastSchedule = []time.Duration{0, 50 * time.Millisecond, 300 * time.Millisecond}

func TestControlVerifyResults(t *testing.T) {
	cases := []struct {
		name   string
		faults simulator.Faults
		target control.Target
		want   control.Result
		hint   string
	}{
		{"正常生效", simulator.Faults{}, setpoint, control.ResultPass, ""},
		{"写入返回成功但值不变", simulator.Faults{IgnoreWrites: []simulator.AddrRange{{Start: 346, Count: 2}}}, setpoint, control.ResultNotApplied, "原值"},
		{"写入后被改回", simulator.Faults{RevertWrites: []simulator.RevertRange{{AddrRange: simulator.AddrRange{Start: 346, Count: 2}, After: 150 * time.Millisecond}}},
			setpoint, control.ResultOverwritten, "改回"},
		{"写入字节序与采集不一致", simulator.Faults{}, func() control.Target { t := setpoint; t.WriteOrder = modbus.OrderABCD; return t }(),
			control.ResultMismatch, "写入用了 ABCD，采集用 CDAB"},
		{"设备拒绝", simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 346, Count: 2}, Code: modbus.ExceptionIllegalDataValue}}},
			setpoint, control.ResultProtocolError, "Illegal Data Value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, addr := startServer(t, modbus.ModeTCP, c.faults)
			cl, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP})
			rep := control.Verify(context.Background(), cl, c.target, 16.0, fastSchedule)
			if rep.Result != c.want || !strings.Contains(rep.Hint, c.hint) {
				t.Fatalf("结论 %s（%s），期望 %s（含“%s”）", rep.Result, rep.Hint, c.want, c.hint)
			}
			if c.want != control.ResultProtocolError && rep.Original != 15.0 {
				t.Errorf("原值应为 15.0，得到 %v", rep.Original)
			}
		})
	}
}

func TestControlRestore(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	cl, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	rep := control.Verify(context.Background(), cl, setpoint, 16.0, fastSchedule)
	if rep.Result != control.ResultPass {
		t.Fatalf("应为 PASS，得到 %s %s", rep.Result, rep.Hint)
	}
	if err := control.Restore(context.Background(), cl, setpoint, rep.OriginalRegs); err != nil {
		t.Fatal(err)
	}
	if regs, _ := readRegs(t, cl, 346, 2); float(t, regs) != 15.0 {
		t.Errorf("恢复后应为 15.0，得到 %v", float(t, regs))
	}
}

func TestControlScaledInteger(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{})
	cl, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP})
	valve := control.Target{Slave: 1, Address: 351, Type: modbus.TypeUint16, ReadOrder: modbus.OrderAB, Scaling: modbus.Scaling{Scale: 0.1}}
	rep := control.Verify(context.Background(), cl, valve, 70.0, fastSchedule)
	if rep.Result != control.ResultPass || rep.Written[0] != 700 || rep.Original != 65.0 {
		t.Fatalf("阀门开度 65.0 → 70.0 应 PASS 且写入原始值 700，得到 %s %v %v", rep.Result, rep.Written, rep.Original)
	}
}

// 线圈写入验证：FC05 写、FC01 回读；恢复原值同样走 FC05。
func TestControlCoil(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	cl, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	coil := control.Target{Slave: 1, Area: modbus.AreaCoils, Address: 5}
	rep := control.Verify(context.Background(), cl, coil, 1, fastSchedule)
	if rep.Result != control.ResultPass || rep.Original != 0 {
		t.Fatalf("结论 %s（%s），原值 %v", rep.Result, rep.Hint, rep.Original)
	}
	if got := rec.raw(modbus.DirTX, modbus.StatusSent)[1]; got != hexOf(modbus.AppendCRC([]byte{1, 5, 0, 5, 0xFF, 0})) {
		t.Errorf("写线圈报文 %s", got)
	}
	if err := control.Restore(context.Background(), cl, coil, rep.OriginalRegs); err != nil {
		t.Fatal(err)
	}
	resp, err := cl.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadCoils, Address: 5, Quantity: 1})
	if err != nil || resp.Bits[0] {
		t.Fatalf("恢复后应为 OFF：%v %v", resp, err)
	}
	if _, _, err := control.Encode(coil, 2); err == nil {
		t.Error("线圈写 2 应报错")
	}
}
