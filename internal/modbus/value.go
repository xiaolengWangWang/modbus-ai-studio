package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DataType 是点位数据类型（设计文档 7.1，另加 64 位）。
type DataType string

const (
	TypeInt16   DataType = "INT16"
	TypeUint16  DataType = "UINT16"
	TypeInt32   DataType = "INT32"
	TypeUint32  DataType = "UINT32"
	TypeFloat32 DataType = "FLOAT32"
	TypeInt64   DataType = "INT64"
	TypeUint64  DataType = "UINT64"
	TypeFloat64 DataType = "FLOAT64"
)

// Registers 返回类型占用的寄存器数。
func (t DataType) Registers() int {
	switch t {
	case TypeInt32, TypeUint32, TypeFloat32:
		return 2
	case TypeInt64, TypeUint64, TypeFloat64:
		return 4
	}
	return 1
}

// Float 表示浮点类型。
func (t DataType) Float() bool { return t == TypeFloat32 || t == TypeFloat64 }

func (t DataType) signed() bool { return t == TypeInt16 || t == TypeInt32 || t == TypeInt64 }

// Orders 返回类型可用的全部字节序。
func (t DataType) Orders() []ByteOrder {
	switch t.Registers() {
	case 2:
		return Orders32
	case 4:
		return Orders64
	}
	return []ByteOrder{OrderAB, OrderBA}
}

// ByteOrder 是字节序。字母 A 表示大端时的最高字节。
type ByteOrder string

const (
	OrderAB   ByteOrder = "AB"   // 16 位标准大端
	OrderBA   ByteOrder = "BA"   // 16 位字节交换
	OrderABCD ByteOrder = "ABCD" // 32 位标准大端
	OrderCDAB ByteOrder = "CDAB" // 字交换
	OrderBADC ByteOrder = "BADC" // 字内字节交换
	OrderDCBA ByteOrder = "DCBA" // 完全小端

	OrderABCDEFGH ByteOrder = "ABCDEFGH" // 64 位标准大端
	OrderGHEFCDAB ByteOrder = "GHEFCDAB" // 字顺序反转（低字在前）
	OrderBADCFEHG ByteOrder = "BADCFEHG" // 字内字节交换
	OrderHGFEDCBA ByteOrder = "HGFEDCBA" // 完全小端
)

// Orders32 是 32 位类型的全部字节序。
var Orders32 = []ByteOrder{OrderABCD, OrderCDAB, OrderBADC, OrderDCBA}

// Orders64 是 64 位类型的全部字节序，与 Orders32 一一对应。
var Orders64 = []ByteOrder{OrderABCDEFGH, OrderGHEFCDAB, OrderBADCFEHG, OrderHGFEDCBA}

// orderFamilies 是同一种字节序在 16 / 32 / 64 位下的写法：标准大端、字内字节交换、字顺序反转、完全小端。
var orderFamilies = [][3]ByteOrder{
	{OrderAB, OrderABCD, OrderABCDEFGH},
	{OrderBA, OrderBADC, OrderBADCFEHG},
	{OrderAB, OrderCDAB, OrderGHEFCDAB},
	{OrderBA, OrderDCBA, OrderHGFEDCBA},
}

// For 把字节序换成类型 t 宽度下的同一种写法，例如 INT64 用 CDAB（低字在前）得到 GHEFCDAB。
// 设备手册常只写 32 位的字节序，64 位按同样的规则排列。不认识的字节序原样返回，由编码解码报错。
func (o ByteOrder) For(t DataType) ByteOrder {
	w := map[int]int{1: 0, 2: 1, 4: 2}[t.Registers()]
	for _, f := range orderFamilies {
		for _, x := range f {
			if x == o {
				return f[w]
			}
		}
	}
	return o
}

// 线上第 i 个字节取大端表示的第 perm[i] 个字节。这些排列都是自逆的，编码和解码共用。
var perm = map[ByteOrder][]int{
	OrderAB:   {0, 1},
	OrderBA:   {1, 0},
	OrderABCD: {0, 1, 2, 3},
	OrderCDAB: {2, 3, 0, 1},
	OrderBADC: {1, 0, 3, 2},
	OrderDCBA: {3, 2, 1, 0},

	OrderABCDEFGH: {0, 1, 2, 3, 4, 5, 6, 7},
	OrderGHEFCDAB: {6, 7, 4, 5, 2, 3, 0, 1},
	OrderBADCFEHG: {1, 0, 3, 2, 5, 4, 7, 6},
	OrderHGFEDCBA: {7, 6, 5, 4, 3, 2, 1, 0},
}

func checkOrder(t DataType, o ByteOrder) error {
	p, ok := perm[o]
	if !ok || len(p) != t.Registers()*2 {
		return fmt.Errorf("modbus: %s 不能使用字节序 %s", t, o)
	}
	return nil
}

func permute(b []byte, o ByteOrder) []byte {
	p := perm[o]
	out := make([]byte, len(p))
	for i, j := range p {
		out[i] = b[j]
	}
	return out
}

// RegistersToBytes 把寄存器按线上顺序展开成字节。
func RegistersToBytes(regs []uint16) []byte {
	out := make([]byte, 0, len(regs)*2)
	for _, r := range regs {
		out = binary.BigEndian.AppendUint16(out, r)
	}
	return out
}

// BytesToRegisters 把线上字节合成寄存器。
func BytesToRegisters(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return out
}

// Bits 按字节序把寄存器拼成整数（大端含义），1、2、4 个寄存器分别得到 16、32、64 位。
// 64 位整型要用它（或 FormatInt）取精确值：DecodeRaw 返回 float64，超过 2^53 的值会丢掉低位。
func Bits(t DataType, o ByteOrder, regs []uint16) (uint64, error) {
	if err := checkOrder(t, o); err != nil {
		return 0, err
	}
	if len(regs) != t.Registers() {
		return 0, fmt.Errorf("modbus: %s 需要 %d 个寄存器，得到 %d 个", t, t.Registers(), len(regs))
	}
	var u uint64
	for _, b := range permute(RegistersToBytes(regs), o) {
		u = u<<8 | uint64(b)
	}
	return u, nil
}

func fromBits(t DataType, o ByteOrder, u uint64) []uint16 {
	be := make([]byte, t.Registers()*2)
	for i := len(be) - 1; i >= 0; i-- {
		be[i] = byte(u)
		u >>= 8
	}
	return BytesToRegisters(permute(be, o))
}

// typeRange 返回 float64 能表示的取值范围。64 位整型的上限取 2^63 / 2^64 之下最近的 float64，
// 否则 2^63 这样的值会通过检查、转换时溢出。
func typeRange(t DataType) (float64, float64) {
	switch t {
	case TypeInt16:
		return math.MinInt16, math.MaxInt16
	case TypeUint16:
		return 0, math.MaxUint16
	case TypeInt32:
		return math.MinInt32, math.MaxInt32
	case TypeUint32:
		return 0, math.MaxUint32
	case TypeInt64:
		return math.MinInt64, math.Nextafter(1<<63, 0)
	case TypeUint64:
		return 0, math.Nextafter(1<<64, 0)
	case TypeFloat64:
		return -math.MaxFloat64, math.MaxFloat64
	}
	return -math.MaxFloat32, math.MaxFloat32
}

// EncodeRaw 把原始值编码为寄存器。整型必须是整数且在类型范围内。
// 64 位整型超过 2^53 时 float64 已经丢了低位，要精确写入用 ParseRaw。
func EncodeRaw(t DataType, o ByteOrder, raw float64) ([]uint16, error) {
	if err := checkOrder(t, o); err != nil {
		return nil, err
	}
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return nil, fmt.Errorf("modbus: %s 不能写入 %v", t, raw)
	}
	lo, hi := typeRange(t)
	if raw < lo || raw > hi {
		return nil, fmt.Errorf("modbus: %v 超出 %s 范围 %v…%v", raw, t, lo, hi)
	}
	if !t.Float() && raw != math.Trunc(raw) {
		return nil, fmt.Errorf("modbus: %s 只能写整数，得到 %v", t, raw)
	}
	var u uint64
	switch {
	case t == TypeFloat32:
		u = uint64(math.Float32bits(float32(raw)))
	case t == TypeFloat64:
		u = math.Float64bits(raw)
	case t.signed():
		u = uint64(int64(raw)) // 补码，fromBits 只取低 16 / 32 位
	default:
		u = uint64(raw)
	}
	return fromBits(t, o, u), nil
}

// DecodeRaw 按类型和字节序把寄存器解码为原始值。
func DecodeRaw(t DataType, o ByteOrder, regs []uint16) (float64, error) {
	u, err := Bits(t, o, regs)
	if err != nil {
		return 0, err
	}
	switch t {
	case TypeInt16:
		return float64(int16(u)), nil
	case TypeInt32:
		return float64(int32(u)), nil
	case TypeInt64:
		return float64(int64(u)), nil
	case TypeFloat32:
		return float64(math.Float32frombits(uint32(u))), nil
	case TypeFloat64:
		return math.Float64frombits(u), nil
	}
	return float64(u), nil
}

// FormatInt 精确显示整型的值，64 位整型也不丢位。
func FormatInt(t DataType, o ByteOrder, regs []uint16) (string, error) {
	if t.Float() {
		return "", fmt.Errorf("modbus: %s 不是整型", t)
	}
	u, err := Bits(t, o, regs)
	if err != nil {
		return "", err
	}
	switch t {
	case TypeInt16:
		return strconv.FormatInt(int64(int16(u)), 10), nil
	case TypeInt32:
		return strconv.FormatInt(int64(int32(u)), 10), nil
	case TypeInt64:
		return strconv.FormatInt(int64(u), 10), nil
	}
	return strconv.FormatUint(u, 10), nil
}

// ParseRaw 把输入的原始值编码为寄存器。整型按整数精确解析，64 位整型不经过 float64，不会丢低位；
// 也接受 15.0、1e3 这种写法。0x 开头的十六进制表示原始字节（大端含义），浮点也可以这样写，
// 例如 FLOAT32 的 0x41700000 就是 15.0。
func ParseRaw(t DataType, o ByteOrder, s string) ([]uint16, error) {
	if err := checkOrder(t, o); err != nil {
		return nil, err
	}
	s = strings.TrimSpace(s)
	bits := t.Registers() * 16
	if h, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
		u, err := strconv.ParseUint(h, 16, bits)
		if err != nil {
			return nil, fmt.Errorf("“%s”不是 %d 位以内的十六进制数", s, bits)
		}
		return fromBits(t, o, u), nil
	}
	if !t.Float() {
		var u uint64
		var err error
		if t.signed() {
			var v int64
			v, err = strconv.ParseInt(s, 10, bits)
			u = uint64(v)
		} else {
			u, err = strconv.ParseUint(s, 10, bits)
		}
		if err == nil {
			return fromBits(t, o, u), nil
		}
		if errors.Is(err, strconv.ErrRange) || (!t.signed() && strings.HasPrefix(s, "-")) {
			lo, hi := "0", strconv.FormatUint(1<<bits-1, 10)
			if t.signed() {
				lo, hi = strconv.FormatInt(-1<<(bits-1), 10), strconv.FormatInt(1<<(bits-1)-1, 10)
			}
			return nil, fmt.Errorf("%s 超出 %s 范围 %s…%s", s, t, lo, hi)
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("“%s”不是数字", s)
	}
	return EncodeRaw(t, o, v)
}

// Scaling 是工程值换算：工程值 = 原始值 × Scale + Offset。Scale 为 0 时按 1 处理。
type Scaling struct {
	Scale  float64
	Offset float64
}

func (s Scaling) factor() float64 {
	if s.Scale == 0 {
		return 1
	}
	return s.Scale
}

// Identity 表示不做换算（倍率 1、偏移 0），原始值就是工程值。
func (s Scaling) Identity() bool { return s.factor() == 1 && s.Offset == 0 }

// Engineering 把原始值换算为工程值。
func (s Scaling) Engineering(raw float64) float64 { return raw*s.factor() + s.Offset }

// Raw 把工程值反算为原始值（设计文档 7.4）。整型四舍五入，rounded 表示取整改变了数值，
// 界面应提示“实际将写入 …”。
func (s Scaling) Raw(t DataType, eng float64) (raw float64, rounded bool) {
	raw = (eng - s.Offset) / s.factor()
	if t.Float() {
		return raw, false
	}
	r := math.Round(raw)
	return r, math.Abs(r-raw) > 1e-9
}

// PlausibleFloat32 判断 FLOAT32 解码结果是否像真实数据：有限，且为 0 或绝对值在 1e-4 到 1e7 之间。
// 非规格化数和异常量级通常意味着字节序不对。
func PlausibleFloat32(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	a := math.Abs(v)
	return a == 0 || (a >= 1e-4 && a < 1e7)
}

// PlausibleFloat64 与 PlausibleFloat32 相同，量级放宽到 1e-6 到 1e15：字节序错了的 FLOAT64
// 指数位被打乱，通常解出 1e-300、1e200 这种量级。
func PlausibleFloat64(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	a := math.Abs(v)
	return a == 0 || (a >= 1e-6 && a < 1e15)
}

// Interpretation 是同一组寄存器在某种字节序下的解读（多解释视图，设计文档 7.3）。
type Interpretation struct {
	Order     ByteOrder
	Float32   float64
	Int32     int32
	Uint32    uint32
	Plausible bool // FLOAT32 结果是否合理
}

// Interpret32 返回两个寄存器在四种字节序下的解读。
func Interpret32(regs []uint16) []Interpretation {
	out := make([]Interpretation, 0, len(Orders32))
	for _, o := range Orders32 {
		f, _ := DecodeRaw(TypeFloat32, o, regs)
		u, _ := DecodeRaw(TypeUint32, o, regs)
		out = append(out, Interpretation{Order: o, Float32: f, Int32: int32(uint32(u)), Uint32: uint32(u), Plausible: PlausibleFloat32(f)})
	}
	return out
}

// SuggestByteOrder 在当前字节序解出的 FLOAT32 不合理、且恰好只有另一种字节序解出合理值时，
// 返回那个字节序。这是确定性判断，不依赖 AI。
func SuggestByteOrder(current ByteOrder, regs []uint16) (ByteOrder, bool) {
	if len(regs) != 2 {
		return "", false
	}
	if v, err := DecodeRaw(TypeFloat32, current, regs); err != nil || PlausibleFloat32(v) {
		return "", false
	}
	var found []ByteOrder
	for _, in := range Interpret32(regs) {
		if in.Order != current && in.Plausible {
			found = append(found, in.Order)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
