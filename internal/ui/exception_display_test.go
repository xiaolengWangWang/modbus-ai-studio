package ui

import (
	"fmt"
	"strings"
	"testing"

	"modbus-ai-studio/internal/modbus"
)

func TestExceptionSummaryAndPacketFieldsIncludeChineseMeaning(t *testing.T) {
	for _, tc := range []struct {
		code    byte
		chinese string
	}{
		{0x01, "非法功能"},
		{0x02, "非法数据地址"},
		{0x03, "非法数据值"},
		{0x04, "从站设备故障"},
		{0x05, "请求已确认"},
		{0x06, "从站设备忙"},
		{0x08, "存储器奇偶校验错误"},
		{0x0A, "网关路径不可用"},
		{0x0B, "网关目标设备未响应"},
		{0x7F, "未知异常"},
	} {
		t.Run(fmt.Sprintf("%02X", tc.code), func(t *testing.T) {
			err := fmt.Errorf("read failed: %w", &modbus.ExceptionError{Function: modbus.FuncReadHoldingRegisters, Code: modbus.ExceptionCode(tc.code)})
			want := fmt.Sprintf("%02X %s", tc.code, tc.chinese)
			if summary := errSummary(err); !strings.Contains(summary, "异常 "+want) {
				t.Errorf("wrapped exception summary must retain the code and explain it in Chinese: %s", summary)
			}
			for _, mode := range []modbus.Mode{modbus.ModeTCP, modbus.ModeRTUOverTCP, modbus.ModeRTU, modbus.ModeASCIIOverTCP, modbus.ModeASCII} {
				t.Run(string(mode), func(t *testing.T) {
					packet := modbus.Packet{Mode: mode, Dir: modbus.DirRX, Raw: modbus.EncodeADU(mode, 1, 7, []byte{0x83, tc.code}), Status: modbus.StatusException}
					found := false
					for _, row := range describePacket(packet, nil) {
						if row.Name == "异常码" {
							found = true
							if row.Hex != fmt.Sprintf("%02X", tc.code) || !strings.Contains(row.Meaning, want) {
								t.Errorf("packet field must keep the raw exception byte and its Chinese meaning: %+v", row)
							}
						}
					}
					if !found {
						t.Fatal("exception response must expose an exception-code field")
					}
				})
			}
		})
	}
}
