package modbus

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DataType 是点位数据类型（V1 范围，设计文档 7.1）。
type DataType string

const (
	TypeInt16   DataType = "INT16"
	TypeUint16  DataType = "UINT16"
	TypeInt32   DataType = "INT32"
	TypeUint32  DataType = "UINT32"
	TypeFloat32 DataType = "FLOAT32"
)

// Registers 返回类型占用的寄存器数。
func (t DataType) Registers() int {
	switch t {
	case TypeInt32, TypeUint32, TypeFloat32:
		return 2
	}
	return 1
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
)

// Orders32 是 32 位类型的全部字节序。
var Orders32 = []ByteOrder{OrderABCD, OrderCDAB, OrderBADC, OrderDCBA}

// 线上第 i 个字节取大端表示的第 perm[i] 个字节。这些排列都是自逆的，编码和解码共用。
var perm = map[ByteOrder][]int{
	OrderAB:   {0, 1},
	OrderBA:   {1, 0},
	OrderABCD: {0, 1, 2, 3},
	OrderCDAB: {2, 3, 0, 1},
	OrderBADC: {1, 0, 3, 2},
	OrderDCBA: {3, 2, 1, 0},
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
	}
	return -math.MaxFloat32, math.MaxFloat32
}

// EncodeRaw 把原始值编码为寄存器。整型必须是整数且在类型范围内。
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
	if t != TypeFloat32 && raw != math.Trunc(raw) {
		return nil, fmt.Errorf("modbus: %s 只能写整数，得到 %v", t, raw)
	}
	var be []byte
	switch t {
	case TypeInt16:
		be = binary.BigEndian.AppendUint16(nil, uint16(int16(raw)))
	case TypeUint16:
		be = binary.BigEndian.AppendUint16(nil, uint16(raw))
	case TypeInt32:
		be = binary.BigEndian.AppendUint32(nil, uint32(int32(raw)))
	case TypeUint32:
		be = binary.BigEndian.AppendUint32(nil, uint32(raw))
	case TypeFloat32:
		be = binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(raw)))
	}
	return BytesToRegisters(permute(be, o)), nil
}

// DecodeRaw 按类型和字节序把寄存器解码为原始值。
func DecodeRaw(t DataType, o ByteOrder, regs []uint16) (float64, error) {
	if err := checkOrder(t, o); err != nil {
		return 0, err
	}
	if len(regs) != t.Registers() {
		return 0, fmt.Errorf("modbus: %s 需要 %d 个寄存器，得到 %d 个", t, t.Registers(), len(regs))
	}
	be := permute(RegistersToBytes(regs), o)
	switch t {
	case TypeInt16:
		return float64(int16(binary.BigEndian.Uint16(be))), nil
	case TypeUint16:
		return float64(binary.BigEndian.Uint16(be)), nil
	case TypeInt32:
		return float64(int32(binary.BigEndian.Uint32(be))), nil
	case TypeUint32:
		return float64(binary.BigEndian.Uint32(be)), nil
	}
	return float64(math.Float32frombits(binary.BigEndian.Uint32(be))), nil
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

// Engineering 把原始值换算为工程值。
func (s Scaling) Engineering(raw float64) float64 { return raw*s.factor() + s.Offset }

// Raw 把工程值反算为原始值（设计文档 7.4）。整型四舍五入，rounded 表示取整改变了数值，
// 界面应提示“实际将写入 …”。
func (s Scaling) Raw(t DataType, eng float64) (raw float64, rounded bool) {
	raw = (eng - s.Offset) / s.factor()
	if t == TypeFloat32 {
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
