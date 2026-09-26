package modbus

import (
	"encoding/hex"
	"errors"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hexOf(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{v}))
	}
	return strings.Join(parts, " ")
}

// 设计文档中出现的报文，CRC 必须全部正确。
func TestCRCDocumentFrames(t *testing.T) {
	frames := []string{
		"01 03 00 00 00 01 84 0A",
		"01 03 00 00 00 0A C5 CD",
		"01 03 00 00 00 14 45 C5",
		"01 03 01 5A 00 02 E5 E4",
		"01 03 04 00 00 41 70 CB 87",
		"01 10 01 5A 00 02 04 00 00 41 80 4A 8C",
		"01 10 01 5A 00 02 60 27",
		"01 03 04 00 00 41 80 CB C3",
		"01 03 02 58 00 04 C4 62",
		"01 03 08 00 00 C0 60 02 AA 00 01 E4 87",
		"01 03 28 CC CD 42 34 00 00 42 00 00 84 00 01 00 00 42 12 15 56 44 0C D6 87 00 12 B4 3F 00 96 10 9A 02 8D 00 3E 00 2D 00 00 00 00 A9 BC",
	}
	for _, f := range frames {
		if !CheckCRC(mustHex(t, f)) {
			t.Errorf("CRC 不正确：%s", f)
		}
	}
	// 效果图中的错误 CRC 必须被识别出来
	if CheckCRC(mustHex(t, "01 03 00 00 00 14 C4 0E")) {
		t.Error("错误的 CRC 被当成正确")
	}
}

func TestRequestPDUAndADU(t *testing.T) {
	cases := []struct {
		req  Request
		mode Mode
		want string
	}{
		{Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 0, Quantity: 20}, ModeRTU, "01 03 00 00 00 14 45 C5"},
		{Request{Slave: 1, Function: FuncWriteMultipleRegisters, Address: 346, Values: []uint16{0x0000, 0x4180}}, ModeRTU,
			"01 10 01 5A 00 02 04 00 00 41 80 4A 8C"},
		{Request{Slave: 1, Function: FuncWriteSingleCoil, Address: 5, Bits: []bool{true}}, ModeRTU, "01 05 00 05 FF 00 9C 3B"},
		{Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 346, Quantity: 2}, ModeTCP, "00 07 00 00 00 06 01 03 01 5A 00 02"},
	}
	for _, c := range cases {
		pdu, err := c.req.PDU()
		if err != nil {
			t.Fatal(err)
		}
		got := hexOf(EncodeADU(c.mode, c.req.Slave, 7, pdu))
		if got != c.want {
			t.Errorf("%v：得到 %s，期望 %s", c.req.Function, got, c.want)
		}
	}
}

func TestRequestValidate(t *testing.T) {
	bad := []Request{
		{Function: FuncReadHoldingRegisters, Quantity: 0},
		{Function: FuncReadHoldingRegisters, Quantity: 126},
		{Function: FuncWriteMultipleRegisters, Values: make([]uint16, 124)},
		{Function: FuncReadHoldingRegisters, Address: 65535, Quantity: 2},
		{Function: 0x2B, Quantity: 1},
	}
	for _, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v 应被拒绝，得到 %v", r, err)
		}
	}
}

func TestParseResponsePDU(t *testing.T) {
	read := Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 346, Quantity: 2}
	resp, err := ParseResponsePDU(read, mustHex(t, "03 04 00 00 41 70"))
	if err != nil || resp.Registers[1] != 0x4170 {
		t.Fatalf("正常响应解析失败：%v %v", resp, err)
	}
	if _, err := ParseResponsePDU(read, mustHex(t, "03 02 00 00")); !errors.Is(err, ErrMismatch) {
		t.Errorf("字节数不符应判为不匹配，得到 %v", err)
	}
	if _, err := ParseResponsePDU(read, mustHex(t, "04 04 00 00 41 70")); !errors.Is(err, ErrMismatch) {
		t.Errorf("功能码不符应判为不匹配，得到 %v", err)
	}
	ex, ok := AsException(func() error { _, e := ParseResponsePDU(read, mustHex(t, "83 02")); return e }())
	if !ok || ex.Code != ExceptionIllegalDataAddress {
		t.Errorf("异常响应解析失败：%v", ex)
	}
	write := Request{Slave: 1, Function: FuncWriteSingleRegister, Address: 351, Values: []uint16{650}}
	if _, err := ParseResponsePDU(write, mustHex(t, "06 01 5F 02 8A")); err != nil {
		t.Errorf("FC06 回显解析失败：%v", err)
	}
	if _, err := ParseResponsePDU(write, mustHex(t, "06 01 5F 02 8B")); !errors.Is(err, ErrMismatch) {
		t.Errorf("FC06 回显数值不同应判为不匹配，得到 %v", err)
	}
}

func TestParseRequestPDU(t *testing.T) {
	req, err := ParseRequestPDU(1, mustHex(t, "10 01 5A 00 02 04 00 00 41 80"))
	if err != nil || req.Address != 346 || len(req.Values) != 2 || req.Values[1] != 0x4180 {
		t.Fatalf("FC16 请求解析失败：%+v %v", req, err)
	}
	if _, err := ParseRequestPDU(1, mustHex(t, "05 00 01 12 34")); err == nil {
		t.Error("FC05 非法数值应返回异常 03")
	}
	if ex, ok := AsException(func() error { _, e := ParseRequestPDU(1, mustHex(t, "2B 0E 01 00")); return e }()); !ok || ex.Code != ExceptionIllegalFunction {
		t.Error("未知功能码应返回异常 01")
	}
}

func TestEncodeDecodeByteOrders(t *testing.T) {
	cases := []struct {
		t    DataType
		o    ByteOrder
		raw  float64
		regs []uint16
	}{
		{TypeFloat32, OrderCDAB, 15.0, []uint16{0x0000, 0x4170}},
		{TypeFloat32, OrderCDAB, 16.0, []uint16{0x0000, 0x4180}},
		{TypeFloat32, OrderABCD, 15.0, []uint16{0x4170, 0x0000}},
		{TypeFloat32, OrderBADC, 15.0, []uint16{0x7041, 0x0000}},
		{TypeFloat32, OrderDCBA, 15.0, []uint16{0x0000, 0x7041}},
		{TypeFloat32, OrderCDAB, -3.5, []uint16{0x0000, 0xC060}},
		{TypeInt32, OrderABCD, -1000, []uint16{0xFFFF, 0xFC18}},
		{TypeUint32, OrderCDAB, 1234567, []uint16{0xD687, 0x0012}},
		{TypeUint32, OrderCDAB, 9876543, []uint16{0xB43F, 0x0096}},
		{TypeInt16, OrderAB, -123, []uint16{0xFF85}},
		{TypeUint16, OrderBA, 0x1234, []uint16{0x3412}},
	}
	for _, c := range cases {
		regs, err := EncodeRaw(c.t, c.o, c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(regs) != len(c.regs) || regs[0] != c.regs[0] || (len(regs) > 1 && regs[1] != c.regs[1]) {
			t.Errorf("%s %s %v：编码得到 %04X，期望 %04X", c.t, c.o, c.raw, regs, c.regs)
		}
		v, err := DecodeRaw(c.t, c.o, c.regs)
		if err != nil || v != c.raw {
			t.Errorf("%s %s：解码得到 %v，期望 %v（%v）", c.t, c.o, v, c.raw, err)
		}
	}
	if _, err := EncodeRaw(TypeInt16, OrderAB, 40000); err == nil {
		t.Error("超出 INT16 范围应报错")
	}
	if _, err := EncodeRaw(TypeUint16, OrderAB, 1.5); err == nil {
		t.Error("整型写入小数应报错")
	}
	if _, err := EncodeRaw(TypeFloat32, OrderAB, 1); err == nil {
		t.Error("FLOAT32 不能用 16 位字节序")
	}
}

// 设计文档 13.3：寄存器 0x0000 0x4170 在四种字节序下的解读。
func TestInterpretAndSuggest(t *testing.T) {
	regs := []uint16{0x0000, 0x4170}
	want := map[ByteOrder]struct {
		f  float64
		u  uint32
		ok bool
	}{
		OrderABCD: {2.3474551874369336e-41, 16752, false},
		OrderCDAB: {15.0, 1097859072, true},
		OrderBADC: {4.026911396930227e-41, 28737, false},
		OrderDCBA: {2.389224275820784e+29, 1883308032, false},
	}
	for _, in := range Interpret32(regs) {
		w := want[in.Order]
		if math.Abs(in.Float32-w.f) > math.Abs(w.f)*1e-6 || in.Uint32 != w.u || in.Plausible != w.ok {
			t.Errorf("%s：得到 %v %d %v", in.Order, in.Float32, in.Uint32, in.Plausible)
		}
	}
	if o, ok := SuggestByteOrder(OrderABCD, regs); !ok || o != OrderCDAB {
		t.Errorf("应建议 CDAB，得到 %s %v", o, ok)
	}
	if _, ok := SuggestByteOrder(OrderCDAB, regs); ok {
		t.Error("当前字节序合理时不应给建议")
	}
}

func TestScaling(t *testing.T) {
	s := Scaling{Scale: 0.1}
	if raw, rounded := s.Raw(TypeInt16, 12.34); raw != 123 || !rounded {
		t.Errorf("12.34 / 0.1 应取整为 123 并提示，得到 %v %v", raw, rounded)
	}
	if raw, rounded := s.Raw(TypeInt16, 16.0); raw != 160 || rounded {
		t.Errorf("16.0 / 0.1 应为 160 且不提示，得到 %v %v", raw, rounded)
	}
	if v := (Scaling{Scale: 0.01}).Engineering(4250); math.Abs(v-42.5) > 1e-9 {
		t.Errorf("4250 × 0.01 应为 42.5，得到 %v", v)
	}
}

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		want []AddressCandidate
	}{
		{"346", []AddressCandidate{{346, AreaNone, "原始 Offset（0-Based）"}}},
		{"0x015A", []AddressCandidate{{346, AreaNone, "十六进制 Offset"}}},
		{"40347", []AddressCandidate{{346, AreaHoldingRegisters, "4x 地址"}, {40347, AreaNone, "原始 Offset（0-Based）"}}},
		{"400347", []AddressCandidate{{346, AreaHoldingRegisters, "6 位 4x 地址"}}},
		{"4x0347", []AddressCandidate{{346, AreaHoldingRegisters, "4x 写法（1-Based）"}}},
		{"30001", []AddressCandidate{{0, AreaInputRegisters, "3x 地址"}, {30001, AreaNone, "原始 Offset（0-Based）"}}},
	}
	for _, c := range cases {
		got, err := ParseAddress(c.in)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("%s：得到 %+v %v", c.in, got, err)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s 第 %d 个解释：得到 %+v，期望 %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
	for _, in := range []string{"", "abc", "70000", "0x10000", "4x0"} {
		if _, err := ParseAddress(in); !errors.Is(err, ErrAddress) {
			t.Errorf("%q 应报错，得到 %v", in, err)
		}
	}
	if got := DescribeAddress(AreaHoldingRegisters, 346); got != "Offset 346 · HEX 0x015A · 1-Based 347 · 40347" {
		t.Errorf("DescribeAddress 得到 %s", got)
	}
}

// 响应被拆成多段到达时，按长度分帧仍能拼出完整一帧。
func TestReadFrameSplitAcrossReads(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	frame := mustHex(t, "01 03 04 00 00 41 70 CB 87")
	go func() {
		for _, part := range [][]byte{frame[:1], frame[1:4], frame[4:]} {
			b.Write(part)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	r := frameReader{t: a}
	f, err := r.readFrame(ModeRTUOverTCP, time.Now().Add(time.Second), 20*time.Millisecond)
	if err != nil || hexOf(f.PDU) != "03 04 00 00 41 70" {
		t.Fatalf("分段到达的帧解析失败：%v %s", err, hexOf(f.Raw))
	}
}

func TestRTUResponseLength(t *testing.T) {
	cases := map[string]int{"01": 0, "01 03": 0, "01 03 28": 45, "01 83": 5, "01 10": 8, "01 06": 8, "01 2B": -1}
	for in, want := range cases {
		if got := RTUResponseLength(mustHex(t, in)); got != want {
			t.Errorf("%s：得到 %d，期望 %d", in, got, want)
		}
	}
}
