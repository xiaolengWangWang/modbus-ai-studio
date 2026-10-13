package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// formatRTT 显示响应时间：10 ms 以下保留一位小数，本机模拟器这类亚毫秒响应不会显示成 0。
func formatRTT(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 10 {
		return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
}

// frameText 是报文的显示形式：ASCII 帧本身就是可读字符，原样显示并写出结尾的 CR LF；其他显示十六进制。
func frameText(mode modbus.Mode, raw []byte) string {
	body, crlf := strings.CutSuffix(string(raw), "\r\n")
	if !mode.IsASCII() || len(raw) == 0 || strings.IndexFunc(body, func(r rune) bool { return r < 0x20 || r > 0x7E }) >= 0 {
		return hexs(raw) // 不是可读字符（例如 ASCII 模式下收到了 RTU 二进制帧）
	}
	if crlf {
		body += " CRLF"
	}
	return body
}

func hexs(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

// parseHex 解析“01 03 00 00”“0103 0000”“0x01,0x03”等写法的十六进制字节串。
func parseHex(s string) ([]byte, error) {
	s = strings.NewReplacer("0x", "", "0X", "", ",", " ", ";", " ", "\n", " ", "\t", " ").Replace(s)
	var out []byte
	for _, f := range strings.Fields(s) {
		if len(f)%2 != 0 {
			return nil, fmt.Errorf("“%s”位数不是偶数，每个字节写两位十六进制", f)
		}
		for i := 0; i < len(f); i += 2 {
			v, err := strconv.ParseUint(f[i:i+2], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("“%s”不是十六进制", f[i:i+2])
			}
			out = append(out, byte(v))
		}
	}
	return out, nil
}

// refSpan 返回地址范围，例如 40001–40020。
var refSpan = modbus.ReferenceSpan

func addrSpan(off uint16, n int) string { return refSpan(modbus.AreaHoldingRegisters, off, n) }

// valueKind 是读取窗口的显示格式（Modbus Poll 的 Display 菜单）。
type valueKind string

const (
	kindPoint    valueKind = "工程值（点表）"
	kindSigned   valueKind = "Signed"
	kindUnsigned valueKind = "Unsigned"
	kindHex      valueKind = "Hex"
	kindBinary   valueKind = "Binary"
	kindInt32    valueKind = "INT32"
	kindUint32   valueKind = "UINT32"
	kindFloat32  valueKind = "FLOAT32"
	kindInt64    valueKind = "INT64"
	kindUint64   valueKind = "UINT64"
	kindFloat64  valueKind = "FLOAT64"
	kindASCII    valueKind = "ASCII" // 每个寄存器两个字符，高字节在前
)

var valueKinds = []valueKind{kindPoint, kindSigned, kindUnsigned, kindHex, kindBinary, kindASCII,
	kindInt32, kindUint32, kindFloat32, kindInt64, kindUint64, kindFloat64}

func kindNames() []string {
	out := make([]string, len(valueKinds))
	for i, k := range valueKinds {
		out[i] = string(k)
	}
	return out
}

// width 是一个值占的寄存器数：32 位格式 2 个、64 位格式 4 个，值显示在第一个寄存器上。
func (k valueKind) width() int { return k.dataType().Registers() }

// dataType 返回写入时使用的数据类型。
func (k valueKind) dataType() modbus.DataType {
	switch k {
	case kindSigned:
		return modbus.TypeInt16
	case kindInt32:
		return modbus.TypeInt32
	case kindUint32:
		return modbus.TypeUint32
	case kindFloat32:
		return modbus.TypeFloat32
	case kindInt64:
		return modbus.TypeInt64
	case kindUint64:
		return modbus.TypeUint64
	case kindFloat64:
		return modbus.TypeFloat64
	}
	return modbus.TypeUint16
}

// swap16 按 16 位格式的字节序取出寄存器的值：字节序是 BA（字节交换）时交换高低字节。
func swap16(o modbus.ByteOrder, r uint16) uint16 {
	if o.For(modbus.TypeUint16) == modbus.OrderBA {
		return r>>8 | r<<8
	}
	return r
}

// formatReg 按 16 位格式显示一个寄存器。
func formatReg(k valueKind, r uint16) string {
	switch k {
	case kindSigned:
		return strconv.Itoa(int(int16(r)))
	case kindHex:
		return fmt.Sprintf("0x%04X", r)
	case kindBinary:
		return fmt.Sprintf("%04b %04b %04b %04b", r>>12, r>>8&0xF, r>>4&0xF, r&0xF)
	case kindASCII:
		return asciiText([]byte{byte(r >> 8), byte(r)})
	}
	return strconv.Itoa(int(r))
}

// formatWide 按 32 / 64 位格式显示多个寄存器，plausible 表示浮点结果是否合理。o 可以写 32 位的字节序名，
// 64 位格式按同一种规则换算。整型精确显示，64 位不经过 float64。
func formatWide(k valueKind, o modbus.ByteOrder, regs []uint16) (string, bool) {
	dt := k.dataType()
	o = o.For(dt)
	if !dt.Float() {
		text, err := modbus.FormatInt(dt, o, regs)
		if err != nil {
			return err.Error(), false
		}
		return text, true
	}
	v, err := modbus.DecodeRaw(dt, o, regs)
	if err != nil {
		return err.Error(), false
	}
	return formatFloat(v), isPlausible(dt, v)
}

// parseValue 按显示格式解析用户输入：Hex 接受 0x1234 / 1234，Binary 接受 0b… 或 0/1 串（可带空格），
// 其余格式接受十进制，也接受 0x 前缀的十六进制。
func parseValue(k valueKind, s string) (float64, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch {
	case s == "":
		return 0, fmt.Errorf("请输入数值")
	case k == kindASCII:
		if len(s) > 2 || strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7E }) >= 0 {
			return 0, fmt.Errorf("ASCII 格式每个寄存器写 1–2 个英文字符，得到“%s”", s)
		}
		b := []byte(s + "\x00")[:2] // 一个字符时低字节补 0
		return float64(uint16(b[0])<<8 | uint16(b[1])), nil
	case k == kindBinary || strings.HasPrefix(lower, "0b"):
		b := strings.ReplaceAll(strings.TrimPrefix(lower, "0b"), " ", "")
		v, err := strconv.ParseUint(b, 2, 32)
		if err != nil {
			return 0, fmt.Errorf("“%s”不是二进制数", s)
		}
		return float64(v), nil
	case k == kindHex || strings.HasPrefix(lower, "0x"):
		v, err := strconv.ParseUint(strings.TrimPrefix(lower, "0x"), 16, 32)
		if err != nil {
			return 0, fmt.Errorf("“%s”不是十六进制数", s)
		}
		return float64(v), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("“%s”不是数字", s)
	}
	return v, nil
}
