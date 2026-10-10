package modbus

import (
	"fmt"
	"strings"
	"testing"
)

func TestExceptionErrorIncludesChineseMeaningAndOriginalCode(t *testing.T) {
	for _, tc := range []struct {
		code          ExceptionCode
		chinese, name string
	}{
		{0x01, "非法功能", "Illegal Function"},
		{0x02, "非法数据地址", "Illegal Data Address"},
		{0x03, "非法数据值", "Illegal Data Value"},
		{0x04, "从站设备故障", "Slave Device Failure"},
		{0x05, "请求已确认", "Acknowledge"},
		{0x06, "从站设备忙", "Slave Device Busy"},
		{0x08, "存储器奇偶校验错误", "Memory Parity Error"},
		{0x0A, "网关路径不可用", "Gateway Path Unavailable"},
		{0x0B, "网关目标设备未响应", "Gateway Target Device Failed to Respond"},
		{0x7F, "未知异常", "Unknown Exception"},
	} {
		t.Run(fmt.Sprintf("%02X", byte(tc.code)), func(t *testing.T) {
			request := Request{Slave: 1, Function: FuncReadHoldingRegisters, Quantity: 1}
			_, err := ParseResponsePDU(request, []byte{0x83, byte(tc.code)})
			ex, ok := AsException(err)
			if !ok || ex.Code != tc.code {
				t.Fatalf("response must preserve its original exception code: %v", err)
			}
			want := fmt.Sprintf("异常 %02X %s", byte(tc.code), tc.chinese)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("decoded exception must display its code with the Chinese meaning %q: %s", want, err)
			}
			if tc.code.Name() != tc.name {
				t.Errorf("canonical English name must stay compatible: got %q, want %q", tc.code.Name(), tc.name)
			}
		})
	}
}
