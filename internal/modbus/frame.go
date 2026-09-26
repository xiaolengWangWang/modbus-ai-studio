package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"time"
)

// Mode 是连接模式，由“编解码器 + 传输”组合得到（设计文档第 4 章）。
// 取值与 SQLite packets.protocol 字段一致。
type Mode string

const (
	ModeTCP        Mode = "MODBUS_TCP"   // MBAP + TCP
	ModeRTU        Mode = "MODBUS_RTU"   // RTU + 串口
	ModeRTUOverTCP Mode = "RTU_OVER_TCP" // RTU + TCP
)

func (m Mode) mbap() bool { return m == ModeTCP }

// Valid 表示受支持的模式。
func (m Mode) Valid() bool { return m == ModeTCP || m == ModeRTU || m == ModeRTUOverTCP }

// Transport 是底层字节通道：TCP 连接或串口。net.Conn 直接满足该接口。
type Transport interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	SetReadDeadline(t time.Time) error
}

var (
	// ErrTimeout 表示在超时时间内没有收到匹配的响应。
	ErrTimeout = errors.New("modbus: 响应超时")
	// ErrCRC 表示 RTU 响应 CRC 校验失败，响应已丢弃。
	ErrCRC = errors.New("modbus: CRC 校验失败")
	// ErrFraming 表示收到的字节无法组成合法报文（例如 MBAP 协议标识不为 0）。
	ErrFraming = errors.New("modbus: 报文分帧失败")
	// ErrConnection 表示连接已断开或读写出错。
	ErrConnection = errors.New("modbus: 连接错误")
)

// EncodeADU 把 PDU 打包成完整报文：Modbus TCP 加 MBAP 头，RTU 类加 Slave 和 CRC。
func EncodeADU(mode Mode, slave byte, txID uint16, pdu []byte) []byte {
	if mode.mbap() {
		adu := make([]byte, 7, 7+len(pdu))
		binary.BigEndian.PutUint16(adu[0:], txID)
		binary.BigEndian.PutUint16(adu[4:], uint16(len(pdu)+1))
		adu[6] = slave
		return append(adu, pdu...)
	}
	return AppendCRC(append([]byte{slave}, pdu...))
}

// RTUResponseLength 根据已收到的开头字节计算 RTU 响应的整帧长度（设计文档 5.4）。
// 返回 0 表示还需要更多字节才能判断；返回 -1 表示功能码未知，只能按字符间隔分帧。
func RTUResponseLength(head []byte) int {
	if len(head) < 2 {
		return 0
	}
	fc := head[1]
	if fc&0x80 != 0 {
		return 5
	}
	switch FunctionCode(fc) {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters:
		if len(head) < 3 {
			return 0
		}
		return 5 + int(head[2])
	case FuncWriteSingleCoil, FuncWriteSingleRegister, FuncWriteMultipleCoils, FuncWriteMultipleRegisters:
		return 8
	}
	return -1
}

// RTURequestLength 与 RTUResponseLength 相同，用于从站解析请求。
func RTURequestLength(head []byte) int {
	if len(head) < 2 {
		return 0
	}
	switch FunctionCode(head[1]) {
	case FuncReadCoils, FuncReadDiscreteInputs, FuncReadHoldingRegisters, FuncReadInputRegisters,
		FuncWriteSingleCoil, FuncWriteSingleRegister:
		return 8
	case FuncWriteMultipleCoils, FuncWriteMultipleRegisters:
		if len(head) < 7 {
			return 0
		}
		return 9 + int(head[6])
	}
	return -1
}

// Frame 是收到的一帧完整报文。
type Frame struct {
	Raw   []byte
	Slave byte
	TxID  uint16 // 仅 Modbus TCP
	PDU   []byte
}

// frameReader 在 Transport 之上做按长度分帧，未消费的字节留在 buf 里。
type frameReader struct {
	t   Transport
	ctx context.Context
	buf []byte
	tmp [512]byte
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// fill 一直读，直到缓冲区至少有 n 个字节，或到达 deadline。
func (r *frameReader) fill(n int, deadline time.Time) error {
	for len(r.buf) < n {
		if r.ctx != nil && r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		if err := r.t.SetReadDeadline(deadline); err != nil {
			return err
		}
		k, err := r.t.Read(r.tmp[:])
		r.buf = append(r.buf, r.tmp[:k]...)
		if err != nil {
			if len(r.buf) >= n {
				return nil
			}
			if r.ctx != nil && r.ctx.Err() != nil {
				return r.ctx.Err()
			}
			return err
		}
	}
	return nil
}

func (r *frameReader) take(n int) []byte {
	out := append([]byte(nil), r.buf[:n]...)
	r.buf = r.buf[n:]
	return out
}

// drain 取走缓冲区里的全部字节，并继续读，直到连续 gap 时间没有新字节或到达 until。
func (r *frameReader) drain(gap time.Duration, until time.Time) []byte {
	out := r.buf
	r.buf = nil
	for {
		d := time.Now().Add(gap)
		if d.After(until) {
			d = until
		}
		if !d.After(time.Now()) {
			return out
		}
		if err := r.t.SetReadDeadline(d); err != nil {
			return out
		}
		k, err := r.t.Read(r.tmp[:])
		out = append(out, r.tmp[:k]...)
		if err != nil {
			return out
		}
	}
}

// skipEcho 丢弃 RS485 转换器回显的发送帧。只有开头字节与发送帧完全相同才丢弃。
// 注意 FC05/FC06 的正常响应与请求完全相同，没有回显时不要开启该选项。
func (r *frameReader) skipEcho(echo []byte, deadline time.Time) {
	for i := range echo {
		if len(r.buf) <= i {
			if err := r.fill(i+1, deadline); err != nil {
				return
			}
		}
		if r.buf[i] != echo[i] {
			return
		}
	}
	r.take(len(echo))
}

// readFrame 按模式读取一帧：TCP 先读 7 字节 MBAP 再按 Length 读剩余；
// RTU 先读 Slave + 功能码，再按功能码算出整帧长度（设计文档 5.4）。
func (r *frameReader) readFrame(mode Mode, deadline time.Time, gap time.Duration) (Frame, error) {
	if mode.mbap() {
		if err := r.fill(7, deadline); err != nil {
			return Frame{}, err
		}
		length := int(binary.BigEndian.Uint16(r.buf[4:6]))
		if binary.BigEndian.Uint16(r.buf[2:4]) != 0 || length < 2 || length > 254 {
			return Frame{Raw: r.drain(gap, deadline)}, ErrFraming
		}
		if err := r.fill(6+length, deadline); err != nil {
			return Frame{}, err
		}
		raw := r.take(6 + length)
		return Frame{Raw: raw, TxID: binary.BigEndian.Uint16(raw[0:2]), Slave: raw[6], PDU: raw[7:]}, nil
	}
	if err := r.fill(2, deadline); err != nil {
		return Frame{}, err
	}
	n := RTUResponseLength(r.buf)
	if n == 0 {
		if err := r.fill(3, deadline); err != nil {
			return Frame{}, err
		}
		n = RTUResponseLength(r.buf)
	}
	var raw []byte
	if n < 0 {
		raw = r.drain(gap, deadline)
	} else {
		if err := r.fill(n, deadline); err != nil {
			return Frame{}, err
		}
		raw = r.take(n)
	}
	if !CheckCRC(raw) {
		// 同一帧剩下的字节一起丢弃，让下一帧重新对齐
		raw = append(raw, r.drain(gap, deadline)...)
		return Frame{Raw: raw}, ErrCRC
	}
	return Frame{Raw: raw, Slave: raw[0], PDU: raw[1 : len(raw)-2]}, nil
}
