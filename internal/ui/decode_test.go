package ui

import (
	"strings"
	"testing"

	"modbus-ai-studio/internal/modbus"
)

func TestDescribePacket(t *testing.T) {
	rx := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeRTUOverTCP, Function: modbus.FuncReadHoldingRegisters, Address: 346, Count: 2,
		Raw: modbus.AppendCRC([]byte{0x01, 0x03, 0x04, 0x00, 0x00, 0x41, 0x70}), Status: modbus.StatusSuccess, RequestID: 7}
	text := rowsText(describePacket(rx, demoPoints()))
	for _, want := range []string{"Slave ID = 1", "03 读保持寄存器", "字节数", "40347", "温差设定 15.0 ℃", "校验正确"} {
		if !strings.Contains(text, want) {
			t.Errorf("RTU 响应解析缺少 %q：\n%s", want, text)
		}
	}
	bad := rx
	bad.Raw = append([]byte(nil), rx.Raw...)
	bad.Raw[len(bad.Raw)-1] ^= 0xFF
	bad.Status = modbus.StatusCRCError
	if text := rowsText(describePacket(bad, demoPoints())); !strings.Contains(text, "校验错误，应为") || !strings.Contains(text, "校验（RTU 为 CRC") {
		t.Errorf("CRC 错误解析：\n%s", text)
	}
	ex := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeRTU, Raw: modbus.AppendCRC([]byte{0x01, 0x83, 0x02}), Status: modbus.StatusException}
	if text := rowsText(describePacket(ex, demoPoints())); !strings.Contains(text, "Illegal Data Address") || !strings.Contains(text, "±1") {
		t.Errorf("异常响应解析：\n%s", text)
	}
	tx := modbus.Packet{Dir: modbus.DirTX, Mode: modbus.ModeTCP, Raw: modbus.EncodeADU(modbus.ModeTCP, 1, 9, []byte{0x10, 0x01, 0x5A, 0x00, 0x02, 0x04, 0x00, 0x00, 0x41, 0x70})}
	text = rowsText(describePacket(tx, demoPoints()))
	for _, want := range []string{"Transaction ID = 9", "Unit ID = 1", "Offset 346（40347）", "40348", "16752"} {
		if !strings.Contains(text, want) {
			t.Errorf("MBAP 写请求解析缺少 %q：\n%s", want, text)
		}
	}
	coils := describePDUOnly(modbus.DirRX, []byte{0x01, 0x00, 0x00, 0x00, 0x0A}, []byte{0x01, 0x02, 0x05, 0x02}, nil)
	if text := rowsText(coils); !strings.Contains(text, "1=1  2=0  3=1") || !strings.Contains(text, "10=1") {
		t.Errorf("线圈响应解析：\n%s", text)
	}
}

func TestDescribeNewFunctions(t *testing.T) {
	d := func(dir modbus.Direction, req, pdu []byte) string {
		return rowsText(describePDUOnly(dir, req, pdu, nil))
	}
	devID := []byte{0x2B, 0x0E, 0x01, 0x82, 0x00, 0x00, 0x02, 0x00, 0x03, 'A', 'B', 'C', 0x01, 0x02, 'P', '1'}
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"FC43 读设备标识响应", d(modbus.DirRX, []byte{0x2B, 0x0E, 0x01, 0x00}, devID), []string{"读设备标识", "常规，支持流式和单个读取", "厂商名称", "ABC", "产品代码", "P1"}},
		{"FC43 请求", d(modbus.DirTX, nil, []byte{0x2B, 0x0E, 0x02, 0x00}), []string{"常规", "厂商名称"}},
		{"FC08 计数响应", d(modbus.DirRX, nil, []byte{0x08, 0x00, 0x0E, 0x00, 0x07}), []string{"本站报文计数", "计数", "7"}},
		{"FC08 只听模式请求", d(modbus.DirTX, nil, []byte{0x08, 0x00, 0x04, 0x00, 0x00}), []string{"进入只听模式", "不再应答"}},
		{"FC22 请求", d(modbus.DirTX, nil, []byte{0x16, 0x01, 0x5F, 0xFF, 0xF0, 0x00, 0x05}), []string{"22 掩码写寄存器", "40352", "AND 掩码", "1111111111110000", "结果"}},
		{"FC23 请求", d(modbus.DirTX, nil, []byte{0x17, 0x01, 0x5E, 0x00, 0x02, 0x01, 0x5F, 0x00, 0x01, 0x02, 0x02, 0x8A}), []string{"读起始地址", "40351", "写起始地址", "先写后读", "650"}},
		{"FC23 响应", d(modbus.DirRX, []byte{0x17, 0x01, 0x5E, 0x00, 0x02}, []byte{0x17, 0x04, 0x13, 0x88, 0x02, 0x8A}), []string{"40351", "5000", "40352", "650"}},
		{"FC17 报告从站 ID", d(modbus.DirRX, nil, []byte{0x11, 0x04, 'S', 'I', 'M', 0xFF}), []string{"从站 ID", "SIM", "运行"}},
		{"FC11 通信事件计数", d(modbus.DirRX, nil, []byte{0x0B, 0xFF, 0xFF, 0x00, 0x09}), []string{"忙", "9"}},
		{"FC07 异常状态", d(modbus.DirRX, nil, []byte{0x07, 0x05}), []string{"00000101"}},
		{"异常 08", d(modbus.DirRX, nil, []byte{0x94, 0x08}), []string{"20 读文件记录 的异常响应", "Memory Parity Error"}},
	}
	for _, c := range cases {
		for _, w := range c.want {
			if !strings.Contains(c.text, w) {
				t.Errorf("%s 缺少 %q：\n%s", c.name, w, c.text)
			}
		}
	}
}
