package simulator

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// AddrRange 是保持寄存器地址段。
type AddrRange struct {
	Start uint16
	Count uint16
}

// Overlaps 判断与 [start, start+n) 是否有交集。
func (r AddrRange) Overlaps(start uint16, n int) bool {
	return int(start) < int(r.Start)+int(r.Count) && int(r.Start) < int(start)+n
}

// SlowRange 让某个地址段应答变慢，用来制造超时和晚到响应。
type SlowRange struct {
	AddrRange
	Delay time.Duration
}

// ExceptionRange 让某个地址段固定返回异常码。
type ExceptionRange struct {
	AddrRange
	Code modbus.ExceptionCode
}

// RevertRange 让写入在 After 之后被改回原值，模拟 PLC 程序覆盖。
type RevertRange struct {
	AddrRange
	After time.Duration
}

// Faults 是故障注入配置，对应设计文档 9.2。零值表示无故障。
type Faults struct {
	Delay         time.Duration    // 固定响应延迟
	Jitter        time.Duration    // 随机附加延迟上限
	DropRate      float64          // 不响应的比例，0–1
	CRCRate       float64          // RTU：响应 CRC 错误的比例
	WrongTxIDRate float64          // TCP：返回错误 Transaction ID 的比例
	SplitWrite    bool             // 响应拆成两个 TCP 包发送
	Slow          []SlowRange      // 慢应答地址段
	Exceptions    []ExceptionRange // 固定返回异常的地址段
	IgnoreWrites  []AddrRange      // 写入返回成功但值不变
	RevertWrites  []RevertRange    // 写入后被改回原值
}

// Server 是模拟从站。
type Server struct {
	Mode  modbus.Mode // ModeTCP 或 ModeRTUOverTCP
	Slave byte        // 只响应这个 Slave ID；0 表示响应任意 ID
	Store *Store

	mu     sync.Mutex
	faults Faults
	rnd    *rand.Rand
	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// NewServer 创建模拟从站。
func NewServer(mode modbus.Mode, slave byte, store *Store) *Server {
	return &Server{Mode: mode, Slave: slave, Store: store, rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
		conns: map[net.Conn]struct{}{}}
}

// SetFaults 在运行中替换故障配置。
func (s *Server) SetFaults(f Faults) {
	s.mu.Lock()
	s.faults = f
	s.mu.Unlock()
}

func (s *Server) snapshot() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults
}

func (s *Server) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rnd.Float64() < p
}

// Serve 在 ln 上接受连接，直到 Close。
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// Close 停止监听并断开全部连接。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

var errSkip = errors.New("skip")

// handle 逐条处理请求。一个连接上的请求按顺序处理，和真实设备一样一问一答。
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		txID, slave, pdu, err := s.readRequest(conn, r)
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return
		}
		if s.Slave != 0 && slave != s.Slave {
			continue // 不是本站地址，不响应
		}
		f := s.snapshot()
		var resp []byte
		req, perr := modbus.ParseRequestPDU(slave, pdu)
		if ex, ok := modbus.AsException(perr); ok {
			resp = modbus.ExceptionPDU(ex.Function, ex.Code)
		} else if perr != nil {
			continue
		} else {
			resp = s.process(req, f)
		}
		if s.chance(f.DropRate) {
			continue
		}
		delay := f.Delay
		if f.Jitter > 0 {
			s.mu.Lock()
			delay += time.Duration(s.rnd.Int63n(int64(f.Jitter)))
			s.mu.Unlock()
		}
		for _, sr := range f.Slow {
			if sr.Overlaps(req.Address, req.Count()) {
				delay += sr.Delay
			}
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		out := s.encode(txID, slave, resp, f)
		if f.SplitWrite && len(out) > 2 {
			if _, err := conn.Write(out[:len(out)/2]); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
			out = out[len(out)/2:]
		}
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

// readRequest 读取一条请求。格式不对的字节按真实设备的做法忽略（返回 errSkip）。
func (s *Server) readRequest(conn net.Conn, r *bufio.Reader) (uint16, byte, []byte, error) {
	if s.Mode == modbus.ModeTCP {
		head := make([]byte, 7)
		if _, err := io.ReadFull(r, head); err != nil {
			return 0, 0, nil, err
		}
		length := int(binary.BigEndian.Uint16(head[4:]))
		if binary.BigEndian.Uint16(head[2:]) != 0 || length < 1 || length > 254 {
			return 0, 0, nil, errors.New("MBAP 头非法") // 数据流已错位，断开连接
		}
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(r, pdu); err != nil {
			return 0, 0, nil, err
		}
		if len(pdu) == 0 {
			return 0, 0, nil, errSkip
		}
		return binary.BigEndian.Uint16(head), head[6], pdu, nil
	}
	head, err := r.Peek(2)
	if err != nil {
		return 0, 0, nil, err
	}
	n := modbus.RTURequestLength(head)
	if n == 0 {
		if head, err = r.Peek(7); err != nil {
			return 0, 0, nil, err
		}
		n = modbus.RTURequestLength(head)
	}
	if n < 0 || n > 256 {
		// 长度无法确定：丢掉当前已到达的字节，等下一帧
		r.Discard(r.Buffered())
		return 0, 0, nil, errSkip
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return 0, 0, nil, err
	}
	if !modbus.CheckCRC(frame) {
		r.Discard(r.Buffered())
		return 0, 0, nil, errSkip
	}
	return 0, frame[0], frame[1 : n-2], nil
}

func (s *Server) process(req modbus.Request, f Faults) []byte {
	n := req.Count()
	for _, er := range f.Exceptions {
		if er.Overlaps(req.Address, n) {
			return modbus.ExceptionPDU(req.Function, er.Code)
		}
	}
	area := modbus.AreaOf(req.Function)
	illegal := modbus.ExceptionPDU(req.Function, modbus.ExceptionIllegalDataAddress)
	switch req.Function {
	case modbus.FuncReadHoldingRegisters, modbus.FuncReadInputRegisters:
		regs, ok := s.Store.Registers(area, req.Address, n)
		if !ok {
			return illegal
		}
		return modbus.EncodeResponsePDU(req, regs, nil)
	case modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs:
		bits, ok := s.Store.Bits(area, req.Address, n)
		if !ok {
			return illegal
		}
		return modbus.EncodeResponsePDU(req, nil, bits)
	case modbus.FuncWriteSingleCoil, modbus.FuncWriteMultipleCoils:
		if !s.Store.SetBits(modbus.AreaCoils, req.Address, req.Bits) {
			return illegal
		}
	case modbus.FuncWriteSingleRegister, modbus.FuncWriteMultipleRegisters:
		old, ok := s.Store.Registers(modbus.AreaHoldingRegisters, req.Address, n)
		if !ok {
			return illegal
		}
		for _, ig := range f.IgnoreWrites {
			if ig.Overlaps(req.Address, n) {
				return modbus.EncodeResponsePDU(req, nil, nil) // 回成功，但不写入
			}
		}
		s.Store.SetRegisters(modbus.AreaHoldingRegisters, req.Address, req.Values)
		for _, rv := range f.RevertWrites {
			if rv.Overlaps(req.Address, n) {
				addr := req.Address
				time.AfterFunc(rv.After, func() { s.Store.SetRegisters(modbus.AreaHoldingRegisters, addr, old) })
			}
		}
	}
	return modbus.EncodeResponsePDU(req, nil, nil)
}

func (s *Server) encode(txID uint16, slave byte, pdu []byte, f Faults) []byte {
	if s.Mode == modbus.ModeTCP {
		if s.chance(f.WrongTxIDRate) {
			txID++
		}
		return modbus.EncodeADU(modbus.ModeTCP, slave, txID, pdu)
	}
	out := modbus.EncodeADU(s.Mode, slave, 0, pdu)
	if s.chance(f.CRCRate) {
		out[len(out)-1] ^= 0xFF
	}
	return out
}
