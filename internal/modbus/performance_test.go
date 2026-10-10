package modbus

import (
	"slices"
	"strings"
	"testing"
)

// Keep results observable so benchmarks include the returned buffers.
var (
	codecBits            uint64
	codecBytes           []byte
	codecRegisters       []uint16
	codecInterpretations []Interpretation
	codecOrder           ByteOrder
	codecSuggested       bool
	codecLRC             byte
)

type codecCase struct {
	name      string
	run       func() error
	maxAllocs float64
}

func codecCases() []codecCase {
	regs := []uint16{0x0102, 0x0304, 0x0506, 0x0709}
	pair := []uint16{0, 0x4170}
	small := []byte{0x11, 0x03, 0, 0x6B, 0, 3}
	large := make([]byte, 254)
	for i := range large {
		large[i] = byte(i)
	}
	ascii := EncodeASCII(large)
	return []codecCase{
		{"Bits64", func() error {
			var err error
			codecBits, err = Bits(TypeUint64, OrderHGFEDCBA, regs)
			return err
		}, 0},
		{"Encode64", func() error {
			var err error
			codecRegisters, err = EncodeRaw(TypeUint64, OrderHGFEDCBA, 1234567)
			return err
		}, 1},
		{"ByteOrderFor", func() error {
			codecOrder = OrderCDAB.For(TypeUint64)
			return nil
		}, 0},
		{"Interpret32", func() error {
			codecInterpretations = Interpret32(pair)
			return nil
		}, 1},
		{"SuggestByteOrder", func() error {
			codecOrder, codecSuggested = SuggestByteOrder(OrderABCD, pair)
			return nil
		}, 0},
		{"EncodeASCIISmall", func() error {
			codecBytes = EncodeASCII(small)
			return nil
		}, 1},
		{"EncodeASCIIMax", func() error {
			codecBytes = EncodeASCII(large)
			return nil
		}, 1},
		{"ParseASCIIMax", func() error {
			var err error
			codecBytes, codecLRC, err = ParseASCII(ascii)
			return err
		}, 1},
		{"EncodeRTUSmall", func() error {
			codecBytes = EncodeADU(ModeRTU, small[0], 7, small[1:])
			return nil
		}, 1},
		{"EncodeRTUResponse", func() error {
			codecBytes = EncodeADU(ModeRTU, 1, 7, []byte{3, 4, 0, 0, 0x41, 0x70})
			return nil
		}, 1},
		{"EncodeRTUMax", func() error {
			codecBytes = EncodeADU(ModeRTU, large[0], 7, large[1:])
			return nil
		}, 1},
		{"EncodeTCPMax", func() error {
			codecBytes = EncodeADU(ModeTCP, large[0], 7, large[1:])
			return nil
		}, 1},
	}
}

func TestCodecAllocationBudgets(t *testing.T) {
	for _, tc := range codecCases() {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			allocs := testing.AllocsPerRun(100, func() { err = tc.run() })
			if err != nil {
				t.Fatal(err)
			}
			if allocs > tc.maxAllocs {
				t.Errorf("%.0f allocations per call, want at most %.0f", allocs, tc.maxAllocs)
			}
		})
	}
}

func BenchmarkCodec(b *testing.B) {
	for _, tc := range codecCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := tc.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestIntegerByteOrderPatterns(t *testing.T) {
	for _, tc := range []struct {
		dataType DataType
		order    ByteOrder
		value    uint64
		regs     []uint16
	}{
		{TypeUint16, OrderAB, 0x12AB, []uint16{0x12AB}},
		{TypeUint16, OrderBA, 0x12AB, []uint16{0xAB12}},
		{TypeUint32, OrderABCD, 0x1234ABCD, []uint16{0x1234, 0xABCD}},
		{TypeUint32, OrderCDAB, 0x1234ABCD, []uint16{0xABCD, 0x1234}},
		{TypeUint32, OrderBADC, 0x1234ABCD, []uint16{0x3412, 0xCDAB}},
		{TypeUint32, OrderDCBA, 0x1234ABCD, []uint16{0xCDAB, 0x3412}},
		{TypeUint64, OrderABCDEFGH, 0x0123456789ABCDEF, []uint16{0x0123, 0x4567, 0x89AB, 0xCDEF}},
		{TypeUint64, OrderGHEFCDAB, 0x0123456789ABCDEF, []uint16{0xCDEF, 0x89AB, 0x4567, 0x0123}},
		{TypeUint64, OrderBADCFEHG, 0x0123456789ABCDEF, []uint16{0x2301, 0x6745, 0xAB89, 0xEFCD}},
		{TypeUint64, OrderHGFEDCBA, 0x0123456789ABCDEF, []uint16{0xEFCD, 0xAB89, 0x6745, 0x2301}},
	} {
		t.Run(string(tc.order), func(t *testing.T) {
			regs := fromBits(tc.dataType, tc.order, tc.value)
			if !slices.Equal(regs, tc.regs) {
				t.Fatalf("encoded %04X, want %04X", regs, tc.regs)
			}
			got, err := Bits(tc.dataType, tc.order, tc.regs)
			if err != nil || got != tc.value {
				t.Fatalf("decoded %X (%v), want %X", got, err, tc.value)
			}
		})
	}
}

func TestSuggestByteOrderWithoutUniqueCandidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order ByteOrder
		regs  []uint16
	}{
		{"ambiguous", OrderABCD, []uint16{0, 0x4042}},
		{"no plausible order", OrderABCD, []uint16{0, 1}},
		{"missing register", OrderABCD, []uint16{0}},
		{"unknown current order", "XYZ", []uint16{0, 0x4170}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if order, ok := SuggestByteOrder(tc.order, tc.regs); ok || order != "" {
				t.Fatalf("suggested %s (%v) without a unique valid candidate", order, ok)
			}
		})
	}
}

func TestASCIIMaxFrame(t *testing.T) {
	data := make([]byte, 254)
	for i := range data {
		data[i] = byte(i)
	}
	raw := EncodeASCII(data)
	if len(raw) != maxASCIIFrame || strings.ContainsAny(string(raw), "abcdef") || string(raw[len(raw)-4:]) != "7D\r\n" {
		t.Fatalf("invalid maximum-length ASCII frame: %q", raw)
	}
	for _, frame := range [][]byte{raw, []byte(strings.ToLower(string(raw)))} {
		got, lrc, err := ParseASCII(frame)
		if err != nil || lrc != 0x7D || !slices.Equal(got, data) {
			t.Fatalf("decoded %X, LRC %X (%v)", got, lrc, err)
		}
		got[0] = 0xFF
		if string(frame[1:3]) != "00" || data[0] != 0 {
			t.Fatal("decoded data must not alias the frame or encoding input")
		}
	}
	raw[len(raw)-4], raw[len(raw)-3] = '0', '0'
	got, lrc, err := ParseASCII(raw)
	if err != ErrLRC || lrc != 0 || !slices.Equal(got, data) {
		t.Fatalf("invalid checksum must preserve decoded data: %X, LRC %X (%v)", got, lrc, err)
	}
}
