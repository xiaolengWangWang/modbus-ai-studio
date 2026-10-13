package points

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ASCII 把字节按 ASCII 字符显示：0 显示为 ·，其他不可见字符显示为 .。
func ASCII(b []byte) string {
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

// Printable 把设备返回的字节按文字显示：合法 UTF-8 原样显示，不可见字符用点代替。
func Printable(b []byte) string {
	if !utf8.Valid(b) {
		return ASCII(b)
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '.'
	}, string(b))
}
