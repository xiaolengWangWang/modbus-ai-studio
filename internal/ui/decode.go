package ui

import (
	"encoding/binary"
	"fmt"
	"strings"

	"modbus-ai-studio/internal/modbus"
)

// statusExplain 解释报文状态，出现在报文解析的最后一行。
var statusExplain = map[modbus.Status]string{
	modbus.StatusTimeout:         "超时时间内没有收到匹配的响应。",
	modbus.StatusException:       "从站返回异常响应：协议和 Slave ID 是对的，只是请求内容被拒绝。",
	modbus.StatusCRCError:        "校验（RTU 为 CRC，ASCII 为 LRC）失败，整帧丢弃。常见原因：波特率 / 校验位不一致、线路干扰，或协议选错。",
	modbus.StatusLate:            "这帧在超时之后才到达，或是上一次通信遗留的字节，已丢弃，不会当作下一条请求的响应。",
	modbus.StatusUnexpected:      "这帧与请求对不上（Slave、功能码、字节数或回显不符），已丢弃并继续等待。",
	modbus.StatusCancelled:       "请求被取消（断开连接或停止轮询）。",
	modbus.StatusConnectionError: "连接错误：连接已断开或读写失败。",
	modbus.StatusParseError:      "收到的字节不是合法报文，已丢弃。Modbus TCP 下多半说明设备其实是 RTU over TCP。",
}

var modeName = map[modbus.Mode]string{
	modbus.ModeTCP:          "Modbus TCP",
	modbus.ModeRTUOverTCP:   "RTU over TCP",
	modbus.ModeRTU:          "Modbus RTU",
	modbus.ModeASCIIOverTCP: "ASCII over TCP",
	modbus.ModeASCII:        "Modbus ASCII",
}

// decodeRow 是报文解析的一行：字段名、原始字节、含义；Name 和 Hex 都为空时 Meaning 是整行说明。
type decodeRow struct{ Name, Hex, Meaning string }

type fieldWriter struct {
	rows []decodeRow
	pts  pointTable // 寄存器行顺带显示点名和工程值，可为空
}

func (w *fieldWriter) add(name string, raw []byte, meaning string, args ...any) {
	w.rows = append(w.rows, decodeRow{name, hexs(raw), fmt.Sprintf(meaning, args...)})
}

// addText 用于 ASCII 帧：第二列直接显示收到的字符。
func (w *fieldWriter) addText(name, text, meaning string, args ...any) {
	w.rows = append(w.rows, decodeRow{name, text, fmt.Sprintf(meaning, args...)})
}

func (w *fieldWriter) line(format string, args ...any) {
	w.rows = append(w.rows, decodeRow{Meaning: fmt.Sprintf(format, args...)})
}

// rowsText 把解析结果排成纯文本，用于复制。
func rowsText(rows []decodeRow) string {
	var b strings.Builder
	for _, r := range rows {
		if r.Name == "" && r.Hex == "" {
			b.WriteString(r.Meaning + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s%s%s\n", padWidth(r.Name, 12), padWidth(r.Hex, 18), r.Meaning)
	}
	return strings.TrimRight(b.String(), "\n")
}

// padWidth 按显示宽度补空格，中文字符按 2 列计。
func padWidth(s string, width int) string {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x2E80 {
			n++
		}
	}
	if n >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-n)
}

// describePacket 逐字段解释一条收发记录（报文解析面板）。响应本身不含地址，
// 地址和数量取自配对的请求（Packet 已带上）。
func describePacket(p modbus.Packet, pts pointTable) []decodeRow {
	w := fieldWriter{pts: pts}
	dir := "Rx 响应"
	if p.Dir == modbus.DirTX {
		dir = "Tx 请求"
	}
	w.line("%s #%06d · %s · %s · %d 字节", dir, p.RequestID, p.Time.Format("15:04:05.000"), modeName[p.Mode], len(p.Raw))
	if p.RTT > 0 {
		w.line("响应时间 %s", formatRTT(p.RTT))
	}
	w.line("")
	if len(p.Raw) == 0 {
		w.line("（没有收到数据）")
	} else {
		describeADU(&w, p)
	}
	if s, ok := statusExplain[p.Status]; ok {
		w.line("")
		w.line("说明：%s", s)
	}
	return w.rows
}

func describeADU(w *fieldWriter, p modbus.Packet) {
	raw := p.Raw
	var pdu []byte
	if p.Mode.IsASCII() {
		describeASCII(w, p)
		return
	}
	if p.Mode == modbus.ModeTCP {
		if len(raw) < 8 {
			w.add("数据", raw, "不足 8 字节，无法组成 MBAP 报文")
			return
		}
		length := int(binary.BigEndian.Uint16(raw[4:6]))
		w.add("事务号", raw[0:2], "Transaction ID = %d", binary.BigEndian.Uint16(raw[0:2]))
		proto := binary.BigEndian.Uint16(raw[2:4])
		note := "Modbus"
		if proto != 0 {
			note = "应为 0，不是 Modbus TCP 报文"
		}
		w.add("协议标识", raw[2:4], "%d（%s）", proto, note)
		note = "后续字节数"
		if length != len(raw)-6 {
			note = fmt.Sprintf("声明 %d，实际 %d，长度不符", length, len(raw)-6)
		}
		w.add("长度", raw[4:6], "%d（%s）", length, note)
		w.add("单元标识", raw[6:7], "Unit ID = %d", raw[6])
		pdu = raw[7:]
	} else {
		if len(raw) < 4 {
			w.add("数据", raw, "不足 4 字节，无法组成 RTU 报文")
			return
		}
		w.add("从站地址", raw[:1], "Slave ID = %d", raw[0])
		pdu = raw[1 : len(raw)-2]
	}
	describePDU(w, p, pdu)
	if p.Mode != modbus.ModeTCP {
		crc := raw[len(raw)-2:]
		want := modbus.CRC16(raw[:len(raw)-2])
		if modbus.CheckCRC(raw) {
			w.add("CRC", crc, "校验正确（低字节在前）")
		} else {
			w.add("CRC", crc, "校验错误，应为 %02X %02X", byte(want), byte(want>>8))
		}
	}
}

// describeASCII 解析 ASCII 帧：冒号、地址、PDU、LRC、CR LF。第二列显示原始字符，PDU 字段按字节显示。
func describeASCII(w *fieldWriter, p modbus.Packet) {
	raw := p.Raw
	data, lrc, err := modbus.ParseASCII(raw)
	if data == nil {
		w.addText("数据", frameText(p.Mode, raw), "%v：应以冒号开头、CR LF 结尾，中间是偶数个十六进制字符", err)
		return
	}
	w.addText("起始符", ":", "帧头")
	w.addText("从站地址", string(raw[1:3]), "Slave ID = %d", data[0])
	describePDU(w, p, data[1:])
	if err == nil {
		w.addText("LRC", string(raw[len(raw)-4:len(raw)-2]), "校验正确")
	} else {
		w.addText("LRC", string(raw[len(raw)-4:len(raw)-2]), "校验错误：收到 %02X，应为 %02X", lrc, modbus.LRC(data))
	}
	w.addText("结束符", "CR LF", "帧尾")
}

func describePDU(w *fieldWriter, p modbus.Packet, pdu []byte) {
	if len(pdu) == 0 {
		return
	}
	fc := pdu[0]
	if fc&0x80 != 0 {
		base := modbus.FunctionCode(fc & 0x7F)
		w.add("功能码", pdu[:1], "%02X = %s 的异常响应", fc, base)
		if len(pdu) >= 2 {
			code := modbus.ExceptionCode(pdu[1])
			w.add("异常码", pdu[1:2], "%s", code.Description())
			w.rows = append(w.rows, decodeRow{Name: "建议", Meaning: code.Tip()})
		}
		return
	}
	f := modbus.FunctionCode(fc)
	w.add("功能码", pdu[:1], "%s", f)
	data := pdu[1:]
	area := modbus.AreaOf(f)
	ref := func(off int) string { return modbus.Reference(area, uint16(off)) }
	u16 := func(b []byte) int { return int(binary.BigEndian.Uint16(b)) }
	short := func() { w.add("数据", data, "长度与功能码不符，按原样显示") }

	if p.Dir == modbus.DirTX {
		switch f {
		case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs, modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters:
			if len(data) != 4 {
				short()
				return
			}
			addr, n := u16(data[0:2]), u16(data[2:4])
			w.add("起始地址", data[0:2], "Offset %d（%s）", addr, ref(addr))
			w.add("数量", data[2:4], "%d（%s）", n, refSpan(area, uint16(addr), n))
		case modbus.FuncWriteSingleCoil:
			if len(data) != 4 {
				short()
				return
			}
			w.add("地址", data[0:2], "Offset %d（%s）", u16(data), ref(u16(data)))
			w.add("值", data[2:4], "%s", coilValue(u16(data[2:4])))
		case modbus.FuncWriteSingleRegister:
			if len(data) != 4 {
				short()
				return
			}
			w.add("地址", data[0:2], "Offset %d（%s）", u16(data), ref(u16(data)))
			v := uint16(u16(data[2:4]))
			w.add("值", data[2:4], "%d（Signed %d）", v, int16(v))
		case modbus.FuncWriteMultipleCoils, modbus.FuncWriteMultipleRegisters:
			if len(data) < 5 || len(data) != 5+int(data[4]) {
				short()
				return
			}
			addr, n := u16(data[0:2]), u16(data[2:4])
			w.add("起始地址", data[0:2], "Offset %d（%s）", addr, ref(addr))
			w.add("数量", data[2:4], "%d", n)
			w.add("字节数", data[4:5], "%d", data[4])
			if f == modbus.FuncWriteMultipleRegisters {
				describeRegisters(w, area, addr, data[5:])
			} else {
				describeBits(w, area, addr, n, data[5:])
			}
		default:
			describeExtra(w, p, f, data)
		}
		return
	}

	switch f {
	case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs, modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters:
		if len(data) < 1 || len(data) != 1+int(data[0]) {
			short()
			return
		}
		w.add("字节数", data[:1], "%d", data[0])
		if f == modbus.FuncReadHoldingRegisters || f == modbus.FuncReadInputRegisters {
			describeRegisters(w, area, int(p.Address), data[1:])
		} else {
			describeBits(w, area, int(p.Address), int(p.Count), data[1:])
		}
	case modbus.FuncWriteSingleCoil, modbus.FuncWriteSingleRegister, modbus.FuncWriteMultipleCoils, modbus.FuncWriteMultipleRegisters:
		if len(data) != 4 {
			short()
			return
		}
		w.add("地址", data[0:2], "Offset %d（%s）回显", u16(data), ref(u16(data)))
		switch f {
		case modbus.FuncWriteSingleCoil:
			w.add("值", data[2:4], "%s 回显", coilValue(u16(data[2:4])))
		case modbus.FuncWriteSingleRegister:
			w.add("值", data[2:4], "%d 回显", u16(data[2:4]))
		default:
			w.add("数量", data[2:4], "%d 回显", u16(data[2:4]))
		}
	default:
		describeExtra(w, p, f, data)
	}
}

func coilValue(v int) string {
	switch v {
	case 0xFF00:
		return "ON"
	case 0x0000:
		return "OFF"
	}
	return fmt.Sprintf("0x%04X 非法（只能是 FF00 或 0000）", v)
}

// describeRegisters 每个寄存器一行；点表里的点顺带显示工程值。
func describeRegisters(w *fieldWriter, area modbus.Area, addr int, b []byte) {
	if len(b)%2 != 0 {
		w.add("数据", b, "字节数不是偶数")
		return
	}
	for i := 0; i+1 < len(b); i += 2 {
		off := addr + i/2
		v := binary.BigEndian.Uint16(b[i:])
		meaning := fmt.Sprintf("%d", v)
		if int16(v) < 0 {
			meaning += fmt.Sprintf("（Signed %d）", int16(v))
		}
		if pt, ok := w.pts.get(area, uint16(off)); ok {
			n := pt.regs()
			if pt.Type == typeString && i+2*n <= len(b) {
				meaning += fmt.Sprintf(" · %s “%s”", pt.Name, decodeString(pt.Order, modbus.BytesToRegisters(b[i:i+2*n])))
			} else if i+2*n <= len(b) {
				if eng, _, err := decodePoint(pt, modbus.BytesToRegisters(b[i:i+2*n])); err == nil {
					meaning += fmt.Sprintf(" · %s %s %s", pt.Name, formatEng(pt, eng), pt.Unit)
				}
			}
		}
		w.add(modbus.Reference(area, uint16(off)), b[i:i+2], "%s", meaning)
	}
}

func describeBits(w *fieldWriter, area modbus.Area, addr, n int, b []byte) {
	var parts []string
	for i := 0; i < n && i/8 < len(b); i++ {
		v := 0
		if b[i/8]&(1<<(i%8)) != 0 {
			v = 1
		}
		parts = append(parts, fmt.Sprintf("%s=%d", modbus.Reference(area, uint16(addr+i)), v))
	}
	w.add("数据", b, "低位在前")
	for i := 0; i < len(parts); i += 8 {
		end := min(i+8, len(parts))
		w.rows = append(w.rows, decodeRow{Name: " ", Meaning: strings.Join(parts[i:end], "  ")})
	}
}

// describePDUOnly 解析自定义请求的请求或响应 PDU。响应里没有地址，地址和数量取自请求 PDU。
func describePDUOnly(dir modbus.Direction, reqPDU, pdu []byte, pts pointTable) []decodeRow {
	p := modbus.Packet{Dir: dir}
	if len(reqPDU) >= 5 {
		p.Address = binary.BigEndian.Uint16(reqPDU[1:3])
		p.Count = binary.BigEndian.Uint16(reqPDU[3:5])
	}
	w := fieldWriter{pts: pts}
	describePDU(&w, p, pdu)
	return w.rows
}
