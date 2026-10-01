package ui

import (
	"errors"
	"testing"

	"modbus-ai-studio/internal/modbus"
)

func TestValueFormats(t *testing.T) {
	cases := []struct {
		k    valueKind
		in   string
		want float64
	}{
		{kindSigned, "-5", -5}, {kindHex, "015A", 346}, {kindHex, "0x015a", 346}, {kindBinary, "0000 0001 0101 1010", 346},
		{kindUnsigned, "0x10", 16}, {kindFloat32, "15.5", 15.5},
	}
	for _, c := range cases {
		if v, err := parseValue(c.k, c.in); err != nil || v != c.want {
			t.Errorf("parseValue(%s, %q) = %v, %v", c.k, c.in, v, err)
		}
	}
	if _, err := parseValue(kindSigned, "abc"); err == nil {
		t.Error("非数字应报错")
	}
	if got := formatReg(kindBinary, 0x015A); got != "0000 0001 0101 1010" {
		t.Errorf("Binary %q", got)
	}
	if got := formatReg(kindSigned, 0xFFFB); got != "-5" {
		t.Errorf("Signed %q", got)
	}
	if got, ok := formatWide(kindFloat32, modbus.OrderCDAB, []uint16{0x0000, 0x4170}); got != "15.0" || !ok {
		t.Errorf("FLOAT32 CDAB %q %v", got, ok)
	}
	for _, c := range []struct {
		k    valueKind
		o    modbus.ByteOrder
		regs []uint16
		want string
		ok   bool
	}{
		{kindInt64, modbus.OrderABCD, []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFC18}, "-1000", true},
		{kindUint64, modbus.OrderCDAB, []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, "18446744073709551615", true},
		{kindFloat64, modbus.OrderCDAB, []uint16{0, 0, 0, 0x402E}, "15.0", true},
		{kindFloat64, modbus.OrderABCD, []uint16{0, 0, 0, 0x402E}, "8.1175e-320", false},
		{kindUint32, modbus.OrderABCD, []uint16{0xFFFF, 0xFFFF}, "4294967295", true},
	} {
		if got, ok := formatWide(c.k, c.o, c.regs); got != c.want || ok != c.ok {
			t.Errorf("%s %s %04X：%q %v，期望 %q %v", c.k, c.o, c.regs, got, ok, c.want, c.ok)
		}
	}
	if o, ok := suggestFloatOrder(modbus.TypeFloat64, []uint16{0, 0, 0, 0x402E}, modbus.OrderABCDEFGH); !ok || o != modbus.OrderGHEFCDAB {
		t.Errorf("FLOAT64 字节序建议 %v %v", o, ok)
	}
	if o, ok := suggestFloatOrder(modbus.TypeFloat32, []uint16{0x0000, 0x4170, 0x0000, 0x4234}, modbus.OrderABCD); !ok || o != modbus.OrderCDAB {
		t.Errorf("字节序建议 %v %v", o, ok)
	}
	if _, ok := suggestFloatOrder(modbus.TypeFloat32, []uint16{0x4170, 0x0000}, modbus.OrderABCD); ok {
		t.Error("值合理时不应给建议")
	}
	if b, err := parseHex("01 03,0x00 005A"); err != nil || hexs(b) != "01 03 00 00 5A" {
		t.Errorf("parseHex %s %v", hexs(b), err)
	}
	if _, err := parseHex("1 03"); err == nil {
		t.Error("奇数位应报错")
	}
	if suggestTimeout(1100) != 2000 || suggestTimeout(100) != 500 {
		t.Errorf("超时建议 %d %d", suggestTimeout(1100), suggestTimeout(100))
	}
	if errSummary(errors.New("x")) != "x" {
		t.Error("errSummary")
	}
}
