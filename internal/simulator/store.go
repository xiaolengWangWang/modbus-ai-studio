// Package simulator 是 Slave 模拟器：Modbus TCP / RTU over TCP 从站，带故障注入。
// 它是协议引擎、控制验证和诊断的自动化测试台（设计文档第 9 章）。
package simulator

import (
	"sync"

	"modbus-ai-studio/internal/modbus"
)

// Store 保存从站的四个数据区。超出容量的地址读写返回异常 02。
type Store struct {
	mu       sync.RWMutex
	coils    []bool
	discrete []bool
	input    []uint16
	holding  []uint16
}

// NewStore 创建每个数据区 size 个地址的存储。
func NewStore(size int) *Store {
	return &Store{
		coils:    make([]bool, size),
		discrete: make([]bool, size),
		input:    make([]uint16, size),
		holding:  make([]uint16, size),
	}
}

func (s *Store) regs(area modbus.Area) []uint16 {
	if area == modbus.AreaInputRegisters {
		return s.input
	}
	return s.holding
}

func (s *Store) bits(area modbus.Area) []bool {
	if area == modbus.AreaDiscreteInputs {
		return s.discrete
	}
	return s.coils
}

func inRange(size int, addr uint16, n int) bool { return n > 0 && int(addr)+n <= size }

// Registers 读寄存器；越界返回 false。
func (s *Store) Registers(area modbus.Area, addr uint16, n int) ([]uint16, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.regs(area)
	if !inRange(len(r), addr, n) {
		return nil, false
	}
	return append([]uint16(nil), r[addr:int(addr)+n]...), true
}

// SetRegisters 写寄存器；越界返回 false。
func (s *Store) SetRegisters(area modbus.Area, addr uint16, vals []uint16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.regs(area)
	if !inRange(len(r), addr, len(vals)) {
		return false
	}
	copy(r[addr:], vals)
	return true
}

// Bits 读线圈或离散输入；越界返回 false。
func (s *Store) Bits(area modbus.Area, addr uint16, n int) ([]bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.bits(area)
	if !inRange(len(b), addr, n) {
		return nil, false
	}
	return append([]bool(nil), b[addr:int(addr)+n]...), true
}

// SetBits 写线圈或离散输入；越界返回 false。
func (s *Store) SetBits(area modbus.Area, addr uint16, vals []bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bits(area)
	if !inRange(len(b), addr, len(vals)) {
		return false
	}
	copy(b[addr:], vals)
	return true
}

// SetValue 按类型和字节序写入一个原始值，用来构造测试数据。
func (s *Store) SetValue(area modbus.Area, addr uint16, t modbus.DataType, o modbus.ByteOrder, raw float64) error {
	regs, err := modbus.EncodeRaw(t, o, raw)
	if err != nil {
		return err
	}
	s.SetRegisters(area, addr, regs)
	return nil
}
