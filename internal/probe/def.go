// Package probe 检测一段寄存器里哪些地址或点读得通：自动定位、逐个寄存器、按点表分段、逐点独立验证、快速排查，
// 以及检测报告（Markdown 摘要、JSONL 明细、CSV 失败清单）。桌面版和 modbus-cli 共用。
package probe

import (
	"errors"
	"fmt"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
)

// Def 是检测的范围：站号、功能码、起始地址和数量。
type Def struct {
	Slave    byte
	Function modbus.FunctionCode
	Start    uint16
	Qty      int
}

func (d Def) area() modbus.Area { return modbus.AreaOf(d.Function) }

func (d Def) bits() bool {
	return d.Function == modbus.FuncReadCoils || d.Function == modbus.FuncReadDiscreteInputs
}

// maxQty 是一次最多读几个：线圈、离散输入 2000 个，寄存器 125 个。
func (d Def) maxQty() int {
	if d.bits() {
		return modbus.MaxReadBits
	}
	return modbus.MaxReadRegisters
}

type (
	point      = points.Point
	pointTable = points.Table
)

var refSpan = modbus.ReferenceSpan

// ErrUnstable 是检测时设备在读同一范围时反复断开连接：结果无法判断。
var ErrUnstable = errors.New("设备在读取该范围时反复断开连接，结果无法判断")

// Validate 检查检测范围：可以分好几批读，先检查整个范围，免得地址回绕。
func Validate(d Def) error {
	if d.Slave == 0 {
		return errors.New("Slave ID 应为 1–255（0 是广播，没有读响应）")
	}
	if d.Function < modbus.FuncReadCoils || d.Function > modbus.FuncReadInputRegisters {
		return errors.New("检测只支持 FC01、FC02、FC03、FC04")
	}
	if d.Qty < 1 || d.Qty > 65536-int(d.Start) {
		return fmt.Errorf("起始 Offset %d 的检测数量应为 1–%d，不能超出 65535", d.Start, 65536-int(d.Start))
	}
	return nil
}

func hexs(b []byte) string { return fmt.Sprintf("% X", b) }
