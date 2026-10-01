package control

import (
	"strings"
	"testing"

	"modbus-ai-studio/internal/modbus"
)

// 设备用 double 保存 64 位整数时只回得来高 53 位：两个值换成 float64 相等，逐位比较才能发现。
func TestClassifyUint64LowBitsLost(t *testing.T) {
	tg := Target{Type: modbus.TypeUint64, ReadOrder: modbus.OrderABCDEFGH}
	written := []uint16{0x0102, 0x0304, 0x0506, 0x0708}
	lossy := []uint16{0x0102, 0x0304, 0x0506, 0x0700}
	if float64(0x0102030405060708) != float64(0x0102030405060700) {
		t.Fatal("前提：两个值的 float64 应相等")
	}
	rep := Report{Written: written, OriginalRegs: []uint16{0, 0, 0, 0}}
	for range 3 {
		rep.Readbacks = append(rep.Readbacks, Readback{Registers: lossy})
	}
	if res, hint := classify(tg, rep); res != ResultMismatch || !strings.Contains(hint, "double") {
		t.Fatalf("得到 %s（%s），期望 MISMATCH 并指出 double", res, hint)
	}
	for i := range rep.Readbacks {
		rep.Readbacks[i].Registers = written
	}
	if res, _ := classify(tg, rep); res != ResultPass {
		t.Fatalf("回读与写入一致应 PASS，得到 %s", res)
	}
}

// 64 位写入字节序与采集不一致：按另一种 64 位字节序解码正好等于目标。
func TestClassifyInt64OrderMismatch(t *testing.T) {
	tg := Target{Type: modbus.TypeInt64, ReadOrder: modbus.OrderGHEFCDAB, WriteOrder: modbus.OrderABCDEFGH}
	regs, _, _, err := EncodeText(tg, "-123456789012")
	if err != nil {
		t.Fatal(err)
	}
	rep := Report{Written: regs, OriginalRegs: []uint16{0, 0, 0, 0}}
	for range 3 {
		rep.Readbacks = append(rep.Readbacks, Readback{Registers: regs})
	}
	if res, hint := classify(tg, rep); res != ResultMismatch || !strings.Contains(hint, "写入用了 ABCDEFGH，采集用 GHEFCDAB") {
		t.Fatalf("得到 %s（%s）", res, hint)
	}
}
