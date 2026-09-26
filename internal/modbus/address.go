package modbus

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Area 是数据区。
type Area byte

const (
	AreaNone             Area = iota // 原始 Offset，数据区由功能码决定
	AreaCoils                        // 0x
	AreaDiscreteInputs               // 1x
	AreaInputRegisters               // 3x
	AreaHoldingRegisters             // 4x
)

// Prefix 返回 0x / 1x / 3x / 4x 前缀。
func (a Area) Prefix() string {
	switch a {
	case AreaCoils:
		return "0x"
	case AreaDiscreteInputs:
		return "1x"
	case AreaInputRegisters:
		return "3x"
	case AreaHoldingRegisters:
		return "4x"
	}
	return ""
}

// ReadFunction 返回读取该数据区的功能码。
func (a Area) ReadFunction() FunctionCode {
	switch a {
	case AreaCoils:
		return FuncReadCoils
	case AreaDiscreteInputs:
		return FuncReadDiscreteInputs
	case AreaInputRegisters:
		return FuncReadInputRegisters
	}
	return FuncReadHoldingRegisters
}

// Writable 表示该数据区可写（1x、3x 只读）。
func (a Area) Writable() bool { return a == AreaCoils || a == AreaHoldingRegisters || a == AreaNone }

// AreaOf 返回功能码访问的数据区。
func AreaOf(f FunctionCode) Area {
	switch f {
	case FuncReadCoils, FuncWriteSingleCoil, FuncWriteMultipleCoils:
		return AreaCoils
	case FuncReadDiscreteInputs:
		return AreaDiscreteInputs
	case FuncReadInputRegisters:
		return AreaInputRegisters
	}
	return AreaHoldingRegisters
}

// AddressCandidate 是地址输入的一种解释。
type AddressCandidate struct {
	Offset uint16 // 协议地址（0–65535）
	Area   Area   // AreaNone 表示原始 Offset
	Label  string // 给用户看的解释
}

// ErrAddress 表示地址输入无法解析。
var ErrAddress = errors.New("modbus: 地址无法识别")

// ParseAddress 解析用户输入的地址：346、0x015A、40347、400347、4x0347、30001 等。
// 输入有歧义时返回多个候选，由界面让用户选择，不做猜测（设计文档 6.3）。
func ParseAddress(s string) ([]AddressCandidate, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch {
	case s == "":
		return nil, fmt.Errorf("%w：地址为空", ErrAddress)
	case strings.HasPrefix(lower, "0x"):
		v, err := strconv.ParseUint(s[2:], 16, 16)
		if err != nil {
			return nil, fmt.Errorf("%w：十六进制地址应在 0x0000–0xFFFF", ErrAddress)
		}
		return []AddressCandidate{{Offset: uint16(v), Label: "十六进制 Offset"}}, nil
	case len(lower) > 2 && (lower[:2] == "1x" || lower[:2] == "3x" || lower[:2] == "4x"):
		n, err := strconv.ParseUint(s[2:], 10, 32)
		if err != nil || n < 1 || n > 65536 {
			return nil, fmt.Errorf("%w：%s 写法应为 1–65536", ErrAddress, lower[:2])
		}
		area := map[string]Area{"1x": AreaDiscreteInputs, "3x": AreaInputRegisters, "4x": AreaHoldingRegisters}[lower[:2]]
		return []AddressCandidate{{Offset: uint16(n - 1), Area: area, Label: lower[:2] + " 写法（1-Based）"}}, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("%w：支持 346、0x015A、40347、400347、4x0347", ErrAddress)
	}
	six := []struct {
		base uint64
		area Area
	}{{400001, AreaHoldingRegisters}, {300001, AreaInputRegisters}, {100001, AreaDiscreteInputs}}
	for _, r := range six {
		if n >= r.base && n <= r.base+65535 {
			return []AddressCandidate{{Offset: uint16(n - r.base), Area: r.area, Label: "6 位 " + r.area.Prefix() + " 地址"}}, nil
		}
	}
	var out []AddressCandidate
	five := []struct {
		base uint64
		area Area
	}{{40001, AreaHoldingRegisters}, {30001, AreaInputRegisters}, {10001, AreaDiscreteInputs}}
	for _, r := range five {
		if n >= r.base && n <= r.base+9998 {
			out = append(out, AddressCandidate{Offset: uint16(n - r.base), Area: r.area, Label: r.area.Prefix() + " 地址"})
		}
	}
	if n <= 65535 {
		out = append(out, AddressCandidate{Offset: uint16(n), Label: "原始 Offset（0-Based）"})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w：%d 超出 0–65535", ErrAddress, n)
	}
	return out, nil
}

// Reference 返回 40001 风格的地址（9999 以上用 6 位）。
func Reference(area Area, offset uint16) string {
	base5, base6 := 40001, 400001
	switch area {
	case AreaCoils:
		base5, base6 = 1, 1
	case AreaDiscreteInputs:
		base5, base6 = 10001, 100001
	case AreaInputRegisters:
		base5, base6 = 30001, 300001
	}
	if int(offset) <= 9998 {
		return strconv.Itoa(base5 + int(offset))
	}
	return strconv.Itoa(base6 + int(offset))
}

// DescribeAddress 返回地址的全部写法，例如“Offset 346 · HEX 0x015A · 1-Based 347 · 40347”。
func DescribeAddress(area Area, offset uint16) string {
	if area == AreaNone {
		area = AreaHoldingRegisters
	}
	return fmt.Sprintf("Offset %d · HEX 0x%04X · 1-Based %d · %s", offset, offset, int(offset)+1, Reference(area, offset))
}
