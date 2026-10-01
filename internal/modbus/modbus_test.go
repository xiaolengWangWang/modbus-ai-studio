package modbus

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
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
		{TypeFloat64, OrderABCDEFGH, 15.0, []uint16{0x402E, 0, 0, 0}},
		{TypeFloat64, OrderGHEFCDAB, 15.0, []uint16{0, 0, 0, 0x402E}},
		{TypeFloat64, OrderBADCFEHG, 15.0, []uint16{0x2E40, 0, 0, 0}},
		{TypeFloat64, OrderHGFEDCBA, 15.0, []uint16{0, 0, 0, 0x2E40}},
		{TypeInt64, OrderABCDEFGH, -2, []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFE}},
		{TypeUint64, OrderGHEFCDAB, 0x0001000200030004, []uint16{0x0004, 0x0003, 0x0002, 0x0001}},
	}
	for _, c := range cases {
		regs, err := EncodeRaw(c.t, c.o, c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(regs, c.regs) {
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
	if _, err := EncodeRaw(TypeInt64, OrderABCD, 1); err == nil {
		t.Error("INT64 不能用 32 位字节序")
	}
	if _, err := EncodeRaw(TypeInt64, OrderABCDEFGH, 1<<63); err == nil {
		t.Error("2^63 超出 INT64 范围应报错，不能溢出成负数")
	}
}

// 64 位整型超过 2^53 时 float64 存不下全部位数：FormatInt / ParseRaw 按整数精确处理。
func TestInt64Exact(t *testing.T) {
	regs := []uint16{0x0102, 0x0304, 0x0506, 0x0709}
	if s, err := FormatInt(TypeUint64, OrderABCDEFGH, regs); err != nil || s != "72623859790382857" {
		t.Errorf("UINT64 得到 %s %v", s, err)
	}
	if v, _ := DecodeRaw(TypeUint64, OrderABCDEFGH, regs); uint64(v) == 72623859790382857 {
		t.Error("前提：float64 表示不了这个值")
	}
	cases := []struct {
		t    DataType
		o    ByteOrder
		in   string
		regs []uint16
		text string // FormatInt 的结果，空表示与 in 相同
	}{
		{TypeUint64, OrderABCDEFGH, "72623859790382857", regs, ""},
		{TypeUint64, OrderGHEFCDAB, "72623859790382857", []uint16{0x0709, 0x0506, 0x0304, 0x0102}, ""},
		{TypeUint64, OrderABCDEFGH, "18446744073709551615", []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, ""},
		{TypeInt64, OrderABCDEFGH, "-9223372036854775808", []uint16{0x8000, 0, 0, 0}, ""},
		{TypeInt64, OrderABCDEFGH, "0xFFFFFFFFFFFFFFFF", []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, "-1"},
		{TypeInt64, OrderABCDEFGH, "15.0", []uint16{0, 0, 0, 15}, "15"},
		{TypeInt32, OrderCDAB, "-1000", []uint16{0xFC18, 0xFFFF}, ""},
	}
	for _, c := range cases {
		got, err := ParseRaw(c.t, c.o, c.in)
		if err != nil || !slices.Equal(got, c.regs) {
			t.Errorf("%s %s “%s”：得到 %04X %v，期望 %04X", c.t, c.o, c.in, got, err, c.regs)
			continue
		}
		want := c.text
		if want == "" {
			want = c.in
		}
		if s, _ := FormatInt(c.t, c.o, got); s != want {
			t.Errorf("%s “%s”：显示为 %s，期望 %s", c.t, c.in, s, want)
		}
	}
	if v, err := ParseRaw(TypeFloat32, OrderABCD, "0x41700000"); err != nil || !slices.Equal(v, []uint16{0x4170, 0}) {
		t.Errorf("FLOAT32 十六进制原始值：%04X %v", v, err)
	}
	for _, c := range []struct {
		t  DataType
		in string
	}{
		{TypeInt64, "9223372036854775808"}, {TypeUint64, "18446744073709551616"}, {TypeUint64, "-1"},
		{TypeInt64, "1.5"}, {TypeInt64, "abc"}, {TypeUint64, "0x10000000000000000"},
	} {
		if _, err := ParseRaw(c.t, OrderABCDEFGH, c.in); err == nil {
			t.Errorf("%s “%s” 应报错", c.t, c.in)
		}
	}
	if _, err := ParseRaw(TypeUint64, OrderABCDEFGH, "-1"); err == nil || !strings.Contains(err.Error(), "0…18446744073709551615") {
		t.Errorf("超出范围应给出范围：%v", err)
	}
}

func TestByteOrderFor(t *testing.T) {
	cases := []struct {
		o    ByteOrder
		t    DataType
		want ByteOrder
	}{
		{OrderCDAB, TypeInt64, OrderGHEFCDAB}, {OrderABCD, TypeFloat64, OrderABCDEFGH},
		{OrderBADC, TypeUint64, OrderBADCFEHG}, {OrderDCBA, TypeInt64, OrderHGFEDCBA},
		{OrderGHEFCDAB, TypeFloat32, OrderCDAB}, {OrderAB, TypeUint64, OrderABCDEFGH},
		{OrderBA, TypeInt32, OrderBADC}, {OrderHGFEDCBA, TypeInt16, OrderBA},
		{OrderGHEFCDAB, TypeInt64, OrderGHEFCDAB}, {"XYZ", TypeInt64, "XYZ"},
	}
	for _, c := range cases {
		if got := c.o.For(c.t); got != c.want {
			t.Errorf("%s 用于 %s 得到 %s，期望 %s", c.o, c.t, got, c.want)
		}
	}
	// 同一种字节序在 32 位和 64 位下排列规则一致：FLOAT64 15.0 的高 32 位与 FLOAT32 的排列方式相同
	for i, o := range Orders32 {
		r32, _ := EncodeRaw(TypeUint32, o, 0x01020304)
		r64, _ := EncodeRaw(TypeUint64, Orders64[i], 0x0102030405060708)
		b32, b64 := RegistersToBytes(r32), RegistersToBytes(r64)
		if o == OrderCDAB || o == OrderDCBA { // 低字在前：高 32 位在后半
			b64 = b64[4:]
		} else {
			b64 = b64[:4]
		}
		if string(b32) != string(b64) {
			t.Errorf("%s 与 %s 排列不一致：% X / % X", o, Orders64[i], b32, b64)
		}
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

// Modbus ASCII：规范里的经典例子 Slave 17 读 40108 起 3 个寄存器，LRC 为 7E。
func TestASCIIFrames(t *testing.T) {
	req := EncodeADU(ModeASCII, 0x11, 0, []byte{0x03, 0x00, 0x6B, 0x00, 0x03})
	if string(req) != ":1103006B00037E\r\n" {
		t.Fatalf("ASCII 请求 %q", req)
	}
	data, lrc, err := ParseASCII([]byte(":1103006b00037e\r\n")) // 小写也接受
	if err != nil || lrc != 0x7E || data[0] != 0x11 || len(data) != 6 {
		t.Fatalf("解析 %X %X %v", data, lrc, err)
	}
	if _, _, err := ParseASCII([]byte(":1103006B00037F\r\n")); err != ErrLRC {
		t.Errorf("LRC 错误应报 ErrLRC，得到 %v", err)
	}
	for _, bad := range []string{"1103006B00037E\r\n", ":1103006B00037E", ":11030\r\n", ":11G3006B00037E\r\n"} {
		if _, _, err := ParseASCII([]byte(bad)); err != ErrFraming {
			t.Errorf("%q 应报格式错误，得到 %v", bad, err)
		}
	}
	// 帧前的噪声和被新冒号打断的半截帧都丢弃，后面完整的帧照常读出
	r := frameReader{t: &fakeConn{data: []byte("xx:0103\r:010302000AF0\r\n")}}
	var got []string
	for i := 0; i < 3; i++ {
		f, err := r.readASCII(time.Now().Add(time.Second))
		got = append(got, fmt.Sprintf("%q %v", f.Raw, err))
	}
	want := []string{`"xx" ` + ErrFraming.Error(), `":0103\r" ` + ErrFraming.Error(), `":010302000AF0\r\n" <nil>`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("分帧\n得到 %v\n期望 %v", got, want)
	}
}

func TestNewFunctionCodeLengths(t *testing.T) {
	cases := []struct {
		head []byte
		resp bool
		want int
	}{
		{[]byte{1, 0x07}, true, 5},
		{[]byte{1, 0x0B}, true, 8},
		{[]byte{1, 0x16}, true, 10},
		{[]byte{1, 0x11, 9}, true, 14},
		{[]byte{1, 0x17, 4}, true, 9},
		{[]byte{1, 0x18, 0, 6}, true, 12},
		{[]byte{1, 0x18, 0}, true, 0},
		{[]byte{1, 0x08}, true, -1},
		{[]byte{1, 0x17, 0, 0, 0, 2, 0, 0, 0, 1, 2}, false, 15},
		{[]byte{1, 0x11}, false, 4},
	}
	for _, c := range cases {
		got := RTURequestLength(c.head)
		if c.resp {
			got = RTUResponseLength(c.head)
		}
		if got != c.want {
			t.Errorf("% X（响应 %v）长度 %d，期望 %d", c.head, c.resp, got, c.want)
		}
	}
	if FuncReadWriteMultipleRegs.String() != "23 读写多个寄存器" || FunctionCode(0x41).String() != "功能码 65（0x41）" {
		t.Error("功能码名称")
	}
}

// fakeConn 一次给出全部数据，读完后按超时处理。
type fakeConn struct{ data []byte }

func (c *fakeConn) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}
func (c *fakeConn) Write(p []byte) (int, error)     { return len(p), nil }
func (c *fakeConn) Close() error                    { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error { return nil }
