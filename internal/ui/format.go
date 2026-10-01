package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"

	"modbus-ai-studio/internal/modbus"
)

// formatEng 格式化工程值：FLOAT32 保留两位小数（整数显示为 45.0 这种形式），
// 整型按 Scale 的小数位数显示（132 × 0.1 → 13.2）。
func formatEng(p point, v float64) string {
	if p.Type == modbus.TypeFloat32 {
		return formatFloat(v)
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

// formatFloat 保留两位小数；整数显示为 45.0；量级异常时用科学计数法，避免一长串数字。
func formatFloat(v float64) string {
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

// compactTheme 缩小内边距和行距，让通信报文和解析面板一屏显示更多行（接近 Modbus Poll 的密度）。
type compactTheme struct{ fyne.Theme }

func (t compactTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInnerPadding:
		return 2
	case theme.SizeNameLineSpacing:
		return 2
	}
	return t.Theme.Size(n)
}

func compact(obj fyne.CanvasObject) *container.ThemeOverride {
	return container.NewThemeOverride(obj, compactTheme{theme.DefaultTheme()})
}

// formatRTT 显示响应时间：10 ms 以下保留一位小数，本机模拟器这类亚毫秒响应不会显示成 0。
func formatRTT(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 10 {
		return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
}

func scaling(p point) modbus.Scaling { return modbus.Scaling{Scale: p.Scale} }

// decodePoint 把点的寄存器解码为工程值，plausible 表示 FLOAT32 结果是否合理。
func decodePoint(p point, regs []uint16) (v float64, plausible bool, err error) {
	raw, err := modbus.DecodeRaw(p.Type, p.Order, regs)
	if err != nil {
		return 0, false, err
	}
	plausible = p.Type != modbus.TypeFloat32 || modbus.PlausibleFloat32(raw)
	return scaling(p).Engineering(raw), plausible, nil
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

// asciiText 把字节按 ASCII 字符显示：0 显示为 ·，其他不可见字符显示为 .。
func asciiText(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c == 0:
			out = append(out, "·"...)
		case c < 0x20 || c > 0x7E:
			out = append(out, '.')
		default:
			out = append(out, c)
		}
	}
	return string(out)
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
func refSpan(area modbus.Area, off uint16, n int) string {
	a := modbus.Reference(area, off)
	if n <= 1 {
		return a
	}
	return a + "–" + modbus.Reference(area, off+uint16(n-1))
}

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
	kindASCII    valueKind = "ASCII" // 每个寄存器两个字符，高字节在前
)

var valueKinds = []valueKind{kindPoint, kindSigned, kindUnsigned, kindHex, kindBinary, kindASCII, kindInt32, kindUint32, kindFloat32}

func kindNames() []string {
	out := make([]string, len(valueKinds))
	for i, k := range valueKinds {
		out[i] = string(k)
	}
	return out
}

// wide 表示 32 位格式：两个寄存器显示一个值，值显示在第一个寄存器上。
func (k valueKind) wide() bool { return k == kindInt32 || k == kindUint32 || k == kindFloat32 }

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
	}
	return modbus.TypeUint16
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

// formatWide 按 32 位格式显示两个寄存器，plausible 表示 FLOAT32 结果是否合理。
func formatWide(k valueKind, o modbus.ByteOrder, regs []uint16) (string, bool) {
	v, err := modbus.DecodeRaw(k.dataType(), o, regs)
	if err != nil {
		return err.Error(), false
	}
	if k == kindFloat32 {
		return formatFloat(v), modbus.PlausibleFloat32(v)
	}
	return strconv.FormatFloat(v, 'f', 0, 64), true
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

var statusText = map[modbus.Status]string{
	modbus.StatusTimeout:         "超时",
	modbus.StatusException:       "异常",
	modbus.StatusCRCError:        "CRC 错误，已丢弃",
	modbus.StatusLate:            "晚到响应，已丢弃",
	modbus.StatusUnexpected:      "不匹配，已丢弃",
	modbus.StatusCancelled:       "已取消",
	modbus.StatusConnectionError: "连接错误",
	modbus.StatusParseError:      "格式错误，已丢弃",
}
