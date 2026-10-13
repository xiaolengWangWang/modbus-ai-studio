package ui

import (
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
	"modbus-ai-studio/internal/simulator"
)

// 点表的解析和查找在 internal/points，桌面版、Web 版和 modbus-cli 共用；界面代码沿用这里的短名字。
type (
	point       = points.Point
	pointTable  = points.Table
	ptKey       = points.Key
	pointImport = points.Import
)

const (
	typeString   = points.TypeString
	maxStringLen = points.MaxStringLen
)

var (
	newPointTable   = points.NewTable
	parsePointsFile = points.ParseFile
	parsePoints     = points.Parse
	decodeString    = points.DecodeString
	printableText   = points.Printable
	asciiText       = points.ASCII
)

// demoPoints 是换热站示例点表，与内置模拟器、设计文档 13.5 一致。
func demoPoints() pointTable {
	var ps []point
	for _, s := range simulator.HeatStationPoints {
		p := point{Area: modbus.AreaHoldingRegisters, Offset: s.Offset, Name: s.Name, Type: s.Type, Order: s.Order,
			Scale: s.Scale, Unit: s.Unit, RW: s.RW, Enum: simulator.HeatStationEnums[s.Offset]}
		if lim, ok := simulator.HeatStationLimits[s.Offset]; ok {
			p.Min, p.Max = &lim[0], &lim[1]
		}
		ps = append(ps, p)
	}
	return newPointTable(ps)
}
