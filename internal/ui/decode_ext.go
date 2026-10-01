package ui

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"modbus-ai-studio/internal/modbus"
)

// diagSubNames 是 FC08 诊断的子功能（Modbus 串行链路规范 6.8）。
var diagSubNames = map[int]string{
	0x00: "回送请求数据", 0x01: "重启通信", 0x02: "读诊断寄存器", 0x03: "修改 ASCII 结束符", 0x04: "进入只听模式",
	0x0A: "计数器和诊断寄存器清零", 0x0B: "总线报文计数", 0x0C: "总线通信错误计数（CRC / LRC）", 0x0D: "总线异常响应计数",
	0x0E: "本站报文计数", 0x0F: "本站无响应计数", 0x10: "本站 NAK 计数", 0x11: "本站忙计数", 0x12: "总线字符溢出计数",
	0x14: "清除溢出计数和标志",
}

// deviceObjectNames 是 FC43 / MEI 0E 读设备标识的标准对象。
var deviceObjectNames = map[byte]string{
	0x00: "厂商名称", 0x01: "产品代码", 0x02: "版本", 0x03: "厂商网址", 0x04: "产品名称", 0x05: "型号", 0x06: "应用名称",
}

func objectName(id byte) string {
	if n, ok := deviceObjectNames[id]; ok {
		return n
	}
	if id >= 0x80 {
		return fmt.Sprintf("私有对象 %02X", id)
	}
	return fmt.Sprintf("对象 %02X", id)
}

var readCodeNames = map[byte]string{0x01: "基本（厂商、产品代码、版本）", 0x02: "常规", 0x03: "扩展", 0x04: "单个对象"}

// printableText 把设备返回的字节按文字显示：合法 UTF-8 原样显示，不可见字符用点代替。
func printableText(b []byte) string {
	if !utf8.Valid(b) {
		return asciiText(b)
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '.'
	}, string(b))
}

// describeExtra 解析结构化读写之外的功能码：07、08、11、12、17、20–24、43。
func describeExtra(w *fieldWriter, p modbus.Packet, f modbus.FunctionCode, data []byte) {
	u16 := func(b []byte) int { return int(binary.BigEndian.Uint16(b)) }
	ref := func(off int) string { return modbus.Reference(modbus.AreaHoldingRegisters, uint16(off)) }
	req := p.Dir == modbus.DirTX
	short := func() { w.add("数据", data, "长度与功能码不符，按原样显示") }
	rest := func(name string, b []byte, meaning string) {
		if len(b) > 0 {
			w.add(name, b, "%s", meaning)
		}
	}
	switch f {
	case modbus.FuncReadExceptionStatus:
		switch {
		case req:
		case len(data) != 1:
			short()
		default:
			w.add("异常状态", data, "%08b（8 个状态位，含义由设备定义）", data[0])
		}
	case modbus.FuncDiagnostics:
		if len(data) < 2 {
			short()
			return
		}
		sub := u16(data)
		name, ok := diagSubNames[sub]
		if !ok {
			name = "设备自定义子功能"
		}
		w.add("子功能", data[:2], "%02X %s", sub, name)
		d := data[2:]
		switch {
		case sub == 0x00 && req:
			rest("数据", d, "要求原样回送")
		case sub == 0x00:
			rest("数据", d, "回送的数据，应与请求一致")
		case sub == 0x04 && req:
			rest("数据", d, "设备进入只听模式后不再应答任何请求，要发子功能 01 才能恢复")
		case !req && sub >= 0x0B && sub <= 0x12 && len(d) == 2:
			w.add("计数", d, "%d", u16(d))
		case !req && sub == 0x02 && len(d) == 2:
			w.add("诊断寄存器", d, "%016b", u16(d))
		default:
			rest("数据", d, "")
		}
	case modbus.FuncGetCommEventCounter, modbus.FuncGetCommEventLog:
		if req {
			return
		}
		if f == modbus.FuncGetCommEventLog {
			if len(data) < 7 || len(data) != 1+int(data[0]) {
				short()
				return
			}
			w.add("字节数", data[:1], "%d", data[0])
			data = data[1:]
		} else if len(data) != 4 {
			short()
			return
		}
		st := "空闲"
		if u16(data) == 0xFFFF {
			st = "忙（上一条命令还在处理）"
		}
		w.add("状态", data[:2], "%s", st)
		w.add("事件计数", data[2:4], "%d（成功完成的报文数）", u16(data[2:]))
		if f == modbus.FuncGetCommEventLog {
			w.add("报文计数", data[4:6], "%d", u16(data[4:]))
			rest("事件", data[6:], "最新的在前，每字节一个事件")
		}
	case modbus.FuncReportServerID:
		switch {
		case req:
		case len(data) < 2 || len(data) != 1+int(data[0]):
			short()
		default:
			w.add("字节数", data[:1], "%d", data[0])
			id, run := data[1:len(data)-1], data[len(data)-1]
			rest("从站 ID", id, printableText(id))
			state := map[byte]string{0xFF: "运行", 0x00: "停止"}[run]
			if state == "" {
				state = "非标准值"
			}
			w.add("运行指示", []byte{run}, "%s", state)
		}
	case modbus.FuncMaskWriteRegister:
		if len(data) != 6 {
			short()
			return
		}
		and, or := u16(data[2:]), u16(data[4:])
		w.add("地址", data[:2], "Offset %d（%s）", u16(data), ref(u16(data)))
		w.add("AND 掩码", data[2:4], "%016b", and)
		w.add("OR 掩码", data[4:6], "%016b", or)
		if req {
			w.rows = append(w.rows, decodeRow{Name: "结果", Meaning: fmt.Sprintf("新值 =（当前值 AND %04X）OR（%04X AND NOT %04X）：AND 为 0 的位改成 OR 的对应位", and, or, and)})
		} else {
			w.rows = append(w.rows, decodeRow{Name: "说明", Meaning: "回显，与请求一致表示已执行"})
		}
	case modbus.FuncReadWriteMultipleRegs:
		if req {
			if len(data) < 9 || len(data) != 9+int(data[8]) {
				short()
				return
			}
			w.add("读起始地址", data[0:2], "Offset %d（%s）", u16(data), ref(u16(data)))
			w.add("读数量", data[2:4], "%d", u16(data[2:]))
			w.add("写起始地址", data[4:6], "Offset %d（%s），设备先写后读", u16(data[4:]), ref(u16(data[4:])))
			w.add("写数量", data[6:8], "%d", u16(data[6:]))
			w.add("字节数", data[8:9], "%d", data[8])
			describeRegisters(w, modbus.AreaHoldingRegisters, u16(data[4:]), data[9:])
			return
		}
		if len(data) < 1 || len(data) != 1+int(data[0]) {
			short()
			return
		}
		w.add("字节数", data[:1], "%d", data[0])
		describeRegisters(w, modbus.AreaHoldingRegisters, int(p.Address), data[1:])
	case modbus.FuncReadFIFOQueue:
		switch {
		case req && len(data) == 2:
			w.add("FIFO 地址", data, "Offset %d（%s）", u16(data), ref(u16(data)))
		case !req && len(data) >= 4 && len(data) == 2+u16(data):
			w.add("字节数", data[:2], "%d", u16(data))
			w.add("FIFO 个数", data[2:4], "%d（最多 31 个）", u16(data[2:]))
			var vals []string
			for i := 4; i+1 < len(data); i += 2 {
				vals = append(vals, fmt.Sprint(u16(data[i:])))
			}
			rest("数据", data[4:], strings.Join(vals, "  "))
		default:
			short()
		}
	case modbus.FuncEncapsulatedInterface:
		describeDeviceID(w, req, data, short)
	case modbus.FuncReadFileRecord, modbus.FuncWriteFileRecord:
		rest("数据", data, "文件记录：字节数 + 子请求（参考类型 06、文件号、记录号、长度），按原样显示")
	default:
		rest("数据", data, "")
	}
}

// describeDeviceID 解析 FC43 / MEI 0E 读设备标识：厂商、产品代码、版本、型号等。
func describeDeviceID(w *fieldWriter, req bool, data []byte, short func()) {
	if len(data) < 1 {
		short()
		return
	}
	if data[0] != 0x0E {
		w.add("MEI 类型", data[:1], "%02X（本工具只解析 0E 读设备标识）", data[0])
		if len(data) > 1 {
			w.add("数据", data[1:], "")
		}
		return
	}
	w.add("MEI 类型", data[:1], "0E 读设备标识")
	if req {
		if len(data) != 3 {
			short()
			return
		}
		w.add("读取方式", data[1:2], "%s", readCodeNames[data[1]])
		w.add("对象 ID", data[2:3], "%s（从这个对象开始读）", objectName(data[2]))
		return
	}
	if len(data) < 6 {
		short()
		return
	}
	level := map[byte]string{1: "基本", 2: "常规", 3: "扩展"}[data[2]&0x7F]
	if data[2]&0x80 != 0 {
		level += "，支持流式和单个读取"
	} else {
		level += "，只支持流式读取"
	}
	more := "没有了"
	if data[3] == 0xFF {
		more = "还有对象，下次从“下一个对象”继续读"
	}
	w.add("读取方式", data[1:2], "%s", readCodeNames[data[1]])
	w.add("一致性等级", data[2:3], "%s", level)
	w.add("后续", data[3:4], "%s", more)
	w.add("下一个对象", data[4:5], "%s", objectName(data[4]))
	w.add("对象个数", data[5:6], "%d", data[5])
	objs := data[6:]
	for i := 0; i < int(data[5]); i++ {
		if len(objs) < 2 || len(objs) < 2+int(objs[1]) {
			w.add("数据", objs, "对象长度与数据不符，按原样显示")
			return
		}
		n := int(objs[1])
		w.add(objectName(objs[0]), objs[:2+n], "%s", printableText(objs[2:2+n]))
		objs = objs[2+n:]
	}
}
