package simulator

import (
	"encoding/binary"
	"sync/atomic"

	"modbus-ai-studio/internal/modbus"
)

// diagCounters 是 FC08 诊断计数器（Modbus 串行链路规范 6.8），模拟器按真实收发计数。
type diagCounters struct {
	bus, busErr, excs, served, noResp atomic.Uint32
}

func (d *diagCounters) clear() {
	for _, c := range []*atomic.Uint32{&d.bus, &d.busErr, &d.excs, &d.served, &d.noResp} {
		c.Store(0)
	}
}

// deviceID 是 FC43 / MEI 0E 读设备标识返回的对象。
var deviceID = []struct {
	id  byte
	val string
}{
	{0x00, "Modbus AI Studio"}, // VendorName
	{0x01, "HEAT-STATION-SIM"}, // ProductCode
	{0x02, "1.0"},              // MajorMinorRevision
	{0x04, "换热站模拟器"},           // ProductName
	{0x05, "HS-1000"},          // ModelName
}

func u16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

// processExtra 处理结构化请求之外的功能码：07、08、0B、11、16、17、2B。handled 为 false 时交给常规流程；
// resp 为 nil 表示不应答。
func (s *Server) processExtra(pdu []byte) (resp []byte, handled bool) {
	fc := modbus.FunctionCode(pdu[0])
	exc := func(code modbus.ExceptionCode) ([]byte, bool) { return modbus.ExceptionPDU(fc, code), true }
	h := modbus.AreaHoldingRegisters
	switch fc {
	case modbus.FuncReadExceptionStatus:
		return []byte{byte(fc), 0x00}, true
	case modbus.FuncDiagnostics:
		if len(pdu) < 3 {
			return exc(modbus.ExceptionIllegalDataValue)
		}
		sub := u16(pdu[1:])
		counters := map[uint16]*atomic.Uint32{0x0B: &s.diag.bus, 0x0C: &s.diag.busErr, 0x0D: &s.diag.excs, 0x0E: &s.diag.served, 0x0F: &s.diag.noResp}
		switch {
		case sub == 0x00 || sub == 0x01: // 回送请求数据；重启通信
			return pdu, true
		case sub == 0x0A: // 计数器和诊断寄存器清零
			s.diag.clear()
			return pdu, true
		case sub == 0x02 || (sub >= 0x10 && sub <= 0x12): // 诊断寄存器、NAK、忙、字符溢出：模拟器里都为 0
			return []byte{byte(fc), pdu[1], pdu[2], 0, 0}, true
		case counters[sub] != nil:
			return binary.BigEndian.AppendUint16([]byte{byte(fc), pdu[1], pdu[2]}, uint16(counters[sub].Load())), true
		}
		return exc(modbus.ExceptionIllegalFunction) // 包括 04 只听模式，模拟器不支持
	case modbus.FuncGetCommEventCounter:
		return binary.BigEndian.AppendUint16([]byte{byte(fc), 0, 0}, uint16(s.diag.served.Load())), true
	case modbus.FuncReportServerID:
		data := append([]byte("MAS-SIM"), 0xFF) // 从站 ID + 运行指示 FF（运行中）
		return append([]byte{byte(fc), byte(len(data))}, data...), true
	case modbus.FuncMaskWriteRegister:
		if len(pdu) != 7 {
			return exc(modbus.ExceptionIllegalDataValue)
		}
		addr, and, or := u16(pdu[1:]), u16(pdu[3:]), u16(pdu[5:])
		regs, ok := s.Store.Registers(h, addr, 1)
		if !ok {
			return exc(modbus.ExceptionIllegalDataAddress)
		}
		s.Store.SetRegisters(h, addr, []uint16{regs[0]&and | or&^and})
		return pdu, true
	case modbus.FuncReadWriteMultipleRegs:
		if len(pdu) < 10 || len(pdu) != 10+int(pdu[9]) || int(pdu[9]) != 2*int(u16(pdu[7:])) {
			return exc(modbus.ExceptionIllegalDataValue)
		}
		ra, rq, wa := u16(pdu[1:]), int(u16(pdu[3:])), u16(pdu[5:])
		if rq < 1 || rq > 125 {
			return exc(modbus.ExceptionIllegalDataValue)
		}
		if !s.Store.SetRegisters(h, wa, modbus.BytesToRegisters(pdu[10:])) { // 规范要求先写后读
			return exc(modbus.ExceptionIllegalDataAddress)
		}
		regs, ok := s.Store.Registers(h, ra, rq)
		if !ok {
			return exc(modbus.ExceptionIllegalDataAddress)
		}
		return append([]byte{byte(fc), byte(2 * rq)}, modbus.RegistersToBytes(regs)...), true
	case modbus.FuncEncapsulatedInterface:
		if len(pdu) != 4 || pdu[1] != 0x0E {
			return exc(modbus.ExceptionIllegalFunction)
		}
		code, from := pdu[2], pdu[3]
		out := []byte{byte(fc), 0x0E, code, 0x82, 0x00, 0x00, 0} // 一致性等级 82：常规标识，支持流式和单个读取
		for _, o := range deviceID {
			basic := o.id <= 0x02
			if (code == 0x01 && !basic) || (code == 0x04 && o.id != from) || o.id < from || code < 0x01 || code > 0x04 {
				continue
			}
			out = append(append(out, o.id, byte(len(o.val))), o.val...)
			out[6]++
		}
		if out[6] == 0 {
			return exc(modbus.ExceptionIllegalDataAddress)
		}
		return out, true
	}
	return nil, false
}
