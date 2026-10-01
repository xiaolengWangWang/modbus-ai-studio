package modbus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// FunctionCode 是 Modbus 功能码。
type FunctionCode byte

const (
	FuncReadCoils              FunctionCode = 0x01
	FuncReadDiscreteInputs     FunctionCode = 0x02
	FuncReadHoldingRegisters   FunctionCode = 0x03
	FuncReadInputRegisters     FunctionCode = 0x04
	FuncWriteSingleCoil        FunctionCode = 0x05
	FuncWriteSingleRegister    FunctionCode = 0x06
	FuncWriteMultipleCoils     FunctionCode = 0x0F
	FuncWriteMultipleRegisters FunctionCode = 0x10

	// 以下功能码不做结构化请求，由自定义请求（Client.DoRaw）发送，报文解析和模拟器支持
	FuncReadExceptionStatus   FunctionCode = 0x07 // 仅串口
	FuncDiagnostics           FunctionCode = 0x08 // 仅串口，带子功能码
	FuncGetCommEventCounter   FunctionCode = 0x0B // 仅串口
	FuncGetCommEventLog       FunctionCode = 0x0C // 仅串口
	FuncReportServerID        FunctionCode = 0x11 // 仅串口
	FuncReadFileRecord        FunctionCode = 0x14
	FuncWriteFileRecord       FunctionCode = 0x15
	FuncMaskWriteRegister     FunctionCode = 0x16
	FuncReadWriteMultipleRegs FunctionCode = 0x17
	FuncReadFIFOQueue         FunctionCode = 0x18
	FuncEncapsulatedInterface FunctionCode = 0x2B // MEI 0E：读设备标识
)

// 单次请求的数量上限（Modbus 规范）。超过上限的读取由上层拆分。
const (
	MaxReadBits       = 2000
	MaxReadRegisters  = 125
	MaxWriteBits      = 1968
	MaxWriteRegisters = 123
)

// 功能码名称前面的数字是十进制功能码，和 Modbus 规范、Modbus Poll 一致（0x10 写作 16）。
var funcNames = map[FunctionCode]string{
	FuncReadCoils:              "01 读线圈",
	FuncReadDiscreteInputs:     "02 读离散输入",
	FuncReadHoldingRegisters:   "03 读保持寄存器",
	FuncReadInputRegisters:     "04 读输入寄存器",
	FuncWriteSingleCoil:        "05 写单个线圈",
	FuncWriteSingleRegister:    "06 写单个寄存器",
	FuncReadExceptionStatus:    "07 读异常状态",
	FuncDiagnostics:            "08 诊断",
	FuncGetCommEventCounter:    "11 读通信事件计数",
	FuncGetCommEventLog:        "12 读通信事件记录",
	FuncWriteMultipleCoils:     "15 写多个线圈",
	FuncWriteMultipleRegisters: "16 写多个寄存器",
	FuncReportServerID:         "17 报告从站 ID",
	FuncReadFileRecord:         "20 读文件记录",
	FuncWriteFileRecord:        "21 写文件记录",
	FuncMaskWriteRegister:      "22 掩码写寄存器",
	FuncReadWriteMultipleRegs:  "23 读写多个寄存器",
	FuncReadFIFOQueue:          "24 读 FIFO 队列",
	FuncEncapsulatedInterface:  "43 封装接口（读设备标识）",
}

func (f FunctionCode) String() string {
	if s, ok := funcNames[f]; ok {
		return s
	}
	return fmt.Sprintf("功能码 %d（0x%02X）", byte(f), byte(f))
}

// IsRead 表示读功能码（01–04）。
func (f FunctionCode) IsRead() bool { return f >= FuncReadCoils && f <= FuncReadInputRegisters }

// IsWrite 表示写功能码（05、06、15、16）。
func (f FunctionCode) IsWrite() bool {
	return f == FuncWriteSingleCoil || f == FuncWriteSingleRegister ||
		f == FuncWriteMultipleCoils || f == FuncWriteMultipleRegisters
}

var (
	// ErrInvalidRequest 表示请求参数不合法，未发送。
	ErrInvalidRequest = errors.New("modbus: 请求参数无效")
	// ErrMismatch 表示响应与请求对不上（功能码、字节数或回显不符）。
	// 客户端丢弃这类响应并继续等待，不会把它当成本次请求的结果。
	ErrMismatch = errors.New("modbus: 响应与请求不匹配")
	// ErrMalformed 表示报文长度或结构不合法。
	ErrMalformed = errors.New("modbus: 报文格式错误")
)

// Request 描述一次主站请求。
type Request struct {
	Slave    byte
	Function FunctionCode
	Address  uint16
	Quantity uint16        // 读请求的数量；写请求由 Values / Bits 决定
	Values   []uint16      // FC06、FC16 的寄存器值
	Bits     []bool        // FC05、FC15 的线圈值
	Timeout  time.Duration // 本次请求的响应超时，0 表示用客户端的超时；扫描从站时用较短的值
}

// Count 返回请求涉及的寄存器或线圈个数。
func (r Request) Count() int {
	switch r.Function {
	case FuncWriteSingleCoil, FuncWriteSingleRegister:
		return 1
	case FuncWriteMultipleCoils:
		return len(r.Bits)
	case FuncWriteMultipleRegisters:
		return len(r.Values)
	}
	return int(r.Quantity)
}

// Validate 检查功能码、数量上限和地址范围。
func (r Request) Validate() error {
	limit := 0
	switch r.Function {
	case FuncReadCoils, FuncReadDiscreteInputs:
		limit = MaxReadBits
	case FuncReadHoldingRegisters, FuncReadInputRegisters:
		limit = MaxReadRegisters
	case FuncWriteSingleCoil:
		if len(r.Bits) != 1 {
			return fmt.Errorf("%w：FC05 需要 1 个线圈值", ErrInvalidRequest)
		}
	case FuncWriteSingleRegister:
		if len(r.Values) != 1 {
			return fmt.Errorf("%w：FC06 需要 1 个寄存器值", ErrInvalidRequest)
		}
	case FuncWriteMultipleCoils:
		limit = MaxWriteBits
	case FuncWriteMultipleRegisters:
		limit = MaxWriteRegisters
	default:
		return fmt.Errorf("%w：不支持的功能码 %02X", ErrInvalidRequest, byte(r.Function))
	}
	n := r.Count()
	if limit > 0 && (n < 1 || n > limit) {
		return fmt.Errorf("%w：%s 数量 %d 超出 1–%d", ErrInvalidRequest, r.Function, n, limit)
	}
	if int(r.Address)+n > 0x10000 {
		return fmt.Errorf("%w：地址 %d 加数量 %d 超出 65535", ErrInvalidRequest, r.Address, n)
	}
	return nil
}

// PDU 编码请求 PDU（功能码 + 数据）。
func (r Request) PDU() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	p := []byte{byte(r.Function), byte(r.Address >> 8), byte(r.Address)}
	switch r.Function {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters:
		p = binary.BigEndian.AppendUint16(p, r.Quantity)
	case FuncWriteSingleCoil:
		v := uint16(0x0000)
		if r.Bits[0] {
			v = 0xFF00
		}
		p = binary.BigEndian.AppendUint16(p, v)
	case FuncWriteSingleRegister:
		p = binary.BigEndian.AppendUint16(p, r.Values[0])
	case FuncWriteMultipleCoils:
		packed := packBits(r.Bits)
		p = binary.BigEndian.AppendUint16(p, uint16(len(r.Bits)))
		p = append(p, byte(len(packed)))
		p = append(p, packed...)
	case FuncWriteMultipleRegisters:
		p = binary.BigEndian.AppendUint16(p, uint16(len(r.Values)))
		p = append(p, byte(len(r.Values)*2))
		for _, v := range r.Values {
			p = binary.BigEndian.AppendUint16(p, v)
		}
	}
	return p, nil
}

// Response 是解析后的正常响应。
type Response struct {
	Function  FunctionCode
	Registers []uint16
	Bits      []bool
}

// ParseResponsePDU 按请求严格校验并解析响应 PDU。
// 异常响应返回 *ExceptionError；对不上的响应返回 ErrMismatch。
func ParseResponsePDU(req Request, pdu []byte) (*Response, error) {
	if len(pdu) < 2 {
		return nil, fmt.Errorf("%w：PDU 只有 %d 字节", ErrMalformed, len(pdu))
	}
	fc := pdu[0]
	if fc == byte(req.Function)|0x80 {
		return nil, &ExceptionError{Function: req.Function, Code: ExceptionCode(pdu[1])}
	}
	if fc != byte(req.Function) {
		return nil, fmt.Errorf("%w：功能码 %02X，期望 %02X", ErrMismatch, fc, byte(req.Function))
	}
	switch req.Function {
	case FuncReadHoldingRegisters, FuncReadInputRegisters:
		bc := int(pdu[1])
		if bc != int(req.Quantity)*2 || len(pdu) != 2+bc {
			return nil, fmt.Errorf("%w：字节数 %d，期望 %d", ErrMismatch, bc, int(req.Quantity)*2)
		}
		regs := make([]uint16, req.Quantity)
		for i := range regs {
			regs[i] = binary.BigEndian.Uint16(pdu[2+2*i:])
		}
		return &Response{Function: req.Function, Registers: regs}, nil
	case FuncReadCoils, FuncReadDiscreteInputs:
		bc, want := int(pdu[1]), (int(req.Quantity)+7)/8
		if bc != want || len(pdu) != 2+bc {
			return nil, fmt.Errorf("%w：字节数 %d，期望 %d", ErrMismatch, bc, want)
		}
		return &Response{Function: req.Function, Bits: unpackBits(pdu[2:], int(req.Quantity))}, nil
	case FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegisters:
		reqPDU, err := req.PDU()
		if err != nil {
			return nil, err
		}
		// 写响应回显地址和数值（或数量），必须与请求完全一致
		if len(pdu) != 5 || !bytes.Equal(pdu[1:5], reqPDU[1:5]) {
			return nil, fmt.Errorf("%w：写响应回显与请求不一致", ErrMismatch)
		}
		return &Response{Function: req.Function}, nil
	}
	return nil, fmt.Errorf("%w：不支持的功能码 %02X", ErrInvalidRequest, byte(req.Function))
}

// ParseRequestPDU 解析从站收到的请求 PDU（模拟器使用）。
// 功能码不支持或数值非法时返回 *ExceptionError，调用方据此回异常响应。
func ParseRequestPDU(slave byte, pdu []byte) (Request, error) {
	if len(pdu) < 1 {
		return Request{}, ErrMalformed
	}
	req := Request{Slave: slave, Function: FunctionCode(pdu[0])}
	exc := func(code ExceptionCode) error { return &ExceptionError{Function: req.Function, Code: code} }
	switch req.Function {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters,
		FuncWriteSingleCoil, FuncWriteSingleRegister:
		if len(pdu) != 5 {
			return req, ErrMalformed
		}
		req.Address = binary.BigEndian.Uint16(pdu[1:])
		v := binary.BigEndian.Uint16(pdu[3:])
		switch req.Function {
		case FuncWriteSingleCoil:
			if v != 0xFF00 && v != 0x0000 {
				return req, exc(ExceptionIllegalDataValue)
			}
			req.Bits = []bool{v == 0xFF00}
		case FuncWriteSingleRegister:
			req.Values = []uint16{v}
		default:
			req.Quantity = v
		}
	case FuncWriteMultipleCoils, FuncWriteMultipleRegisters:
		if len(pdu) < 6 || len(pdu) != 6+int(pdu[5]) {
			return req, ErrMalformed
		}
		req.Address = binary.BigEndian.Uint16(pdu[1:])
		n := int(binary.BigEndian.Uint16(pdu[3:]))
		data := pdu[6:]
		if req.Function == FuncWriteMultipleRegisters {
			if len(data) != n*2 {
				return req, exc(ExceptionIllegalDataValue)
			}
			for i := 0; i < n; i++ {
				req.Values = append(req.Values, binary.BigEndian.Uint16(data[2*i:]))
			}
		} else {
			if len(data) != (n+7)/8 {
				return req, exc(ExceptionIllegalDataValue)
			}
			req.Bits = unpackBits(data, n)
		}
	default:
		return req, exc(ExceptionIllegalFunction)
	}
	if err := req.Validate(); err != nil {
		return req, exc(ExceptionIllegalDataValue)
	}
	return req, nil
}

// EncodeResponsePDU 生成正常响应 PDU（模拟器使用）。
func EncodeResponsePDU(req Request, regs []uint16, bits []bool) []byte {
	switch req.Function {
	case FuncReadHoldingRegisters, FuncReadInputRegisters:
		p := []byte{byte(req.Function), byte(len(regs) * 2)}
		for _, v := range regs {
			p = binary.BigEndian.AppendUint16(p, v)
		}
		return p
	case FuncReadCoils, FuncReadDiscreteInputs:
		packed := packBits(bits)
		return append([]byte{byte(req.Function), byte(len(packed))}, packed...)
	}
	reqPDU, _ := req.PDU()
	return reqPDU[:5]
}

func packBits(bits []bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

func unpackBits(data []byte, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = data[i/8]&(1<<(i%8)) != 0
	}
	return out
}
