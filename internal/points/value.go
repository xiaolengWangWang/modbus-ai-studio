package points

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"modbus-ai-studio/internal/modbus"
)

// Scaling 是点的工程值换算：工程值 = 原始值 × Scale。
func (p Point) Scaling() modbus.Scaling { return modbus.Scaling{Scale: p.Scale} }

// Value 把点的寄存器解码为工程值，plausible 表示浮点结果像不像真实数据（字节序错了常常是天文数字）。
func (p Point) Value(regs []uint16) (v float64, plausible bool, err error) {
	raw, err := modbus.DecodeRaw(p.Type, p.Order, regs)
	if err != nil {
		return 0, false, err
	}
	return p.Scaling().Engineering(raw), Plausible(p.Type, raw), nil
}

// Text 是点的工程值文字（带枚举含义，不带单位）：64 位整型不经过 float64，按寄存器精确显示；
// STRING 解成文字。线圈、离散输入的点把 0 / 1 当成一个寄存器传进来。
func (p Point) Text(regs []uint16) (text string, plausible bool, err error) {
	switch {
	case p.Type == TypeString:
		return DecodeString(p.Order, regs), true, nil
	case ExactInt(p.Type, p.Scaling()):
		text, err = modbus.FormatInt(p.Type, p.Order, regs)
		return text, true, err
	}
	v, plausible, err := p.Value(regs)
	if err != nil {
		return "", false, err
	}
	return p.Format(v), plausible, nil
}

// Format 格式化工程值：浮点保留两位小数（整数显示为 45.0 这种形式），整型按 Scale 的小数位数显示
// （132 × 0.1 → 13.2），有枚举含义时跟在后面（0 停止）。
func (p Point) Format(v float64) string {
	if p.Type.Float() {
		return FormatFloat(v)
	}
	s := strconv.FormatFloat(p.Scale, 'f', -1, 64)
	decimals := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimals = len(s) - i - 1
	}
	out := strconv.FormatFloat(v, 'f', decimals, 64)
	if name, ok := p.Enum[int(v)]; ok && v == math.Trunc(v) {
		out += " " + name
	}
	return out
}

// FormatFloat 保留两位小数；整数显示为 45.0；量级异常时用科学计数法，避免一长串数字。
func FormatFloat(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	if a := math.Abs(v); a != 0 && (a < 1e-4 || a >= 1e9) {
		return strconv.FormatFloat(v, 'g', 6, 64)
	}
	r := math.Round(v*100) / 100
	if r == math.Trunc(r) {
		return strconv.FormatFloat(r, 'f', 1, 64)
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}

// Plausible 判断浮点值是否像真实数据，整型总是合理。
func Plausible(t modbus.DataType, v float64) bool {
	switch t {
	case modbus.TypeFloat32:
		return modbus.PlausibleFloat32(v)
	case modbus.TypeFloat64:
		return modbus.PlausibleFloat64(v)
	}
	return true
}

// ExactInt 表示按寄存器精确显示：64 位整型且没有换算。float64 只有 53 位精度，按工程值显示会丢掉低位。
func ExactInt(t modbus.DataType, s modbus.Scaling) bool {
	return (t == modbus.TypeInt64 || t == modbus.TypeUint64) && s.Identity()
}

// Range 是一次读取的范围：同一数据区里相近的点合成一段。
type Range struct {
	Area  modbus.Area
	Start uint16
	Count int
}

// Ranges 按地址把点合成读取范围：一次最多读 120 个地址，相隔 20 个地址以上另起一段。
// 桌面版按它建读取窗口，modbus-cli 和 Web 版按它读点表。
func Ranges(ps []Point) []Range {
	sorted := slices.Clone(ps)
	slices.SortFunc(sorted, func(a, b Point) int {
		if a.Area != b.Area {
			return int(a.Area) - int(b.Area)
		}
		return int(a.Offset) - int(b.Offset)
	})
	var out []Range
	for _, p := range sorted {
		end := int(p.Offset) + p.Regs()
		if n := len(out); n > 0 {
			r := &out[n-1]
			if r.Area == p.Area && end-int(r.Start) <= 120 && int(p.Offset)-(int(r.Start)+r.Count) <= 20 {
				r.Count = max(r.Count, end-int(r.Start))
				continue
			}
		}
		out = append(out, Range{p.Area, p.Offset, p.Regs()})
	}
	return out
}
