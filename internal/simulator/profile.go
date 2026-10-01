package simulator

import (
	"context"
	"math"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Point 是示例点表中的一个点。
type Point struct {
	Offset uint16
	Name   string
	Type   modbus.DataType
	Order  modbus.ByteOrder
	Scale  float64
	Unit   string
	RW     bool
}

// HeatStationPoints 是设计文档 13.5 的换热站 PLC 示例点表，32 位数据统一 CDAB。
var HeatStationPoints = []Point{
	{0, "二次供水温度", modbus.TypeFloat32, modbus.OrderCDAB, 1, "℃", false},
	{2, "二次回水温度", modbus.TypeFloat32, modbus.OrderCDAB, 1, "℃", false},
	{4, "二次温差", modbus.TypeInt16, modbus.OrderAB, 0.1, "℃", false},
	{5, "状态字", modbus.TypeUint16, modbus.OrderAB, 1, "", false},
	{6, "瞬时流量", modbus.TypeFloat32, modbus.OrderCDAB, 1, "m³/h", false},
	{8, "热功率", modbus.TypeFloat32, modbus.OrderCDAB, 1, "kW", false},
	{10, "累计热量", modbus.TypeUint32, modbus.OrderCDAB, 1, "kWh", false},
	{12, "累计流量", modbus.TypeUint32, modbus.OrderCDAB, 0.01, "m³", false},
	{14, "循环泵频率", modbus.TypeUint16, modbus.OrderAB, 0.01, "Hz", false},
	{15, "调节阀开度", modbus.TypeUint16, modbus.OrderAB, 0.1, "%", false},
	{16, "一次供水压力", modbus.TypeInt16, modbus.OrderAB, 0.01, "MPa", false},
	{17, "二次供水压力", modbus.TypeInt16, modbus.OrderAB, 0.01, "MPa", false},
	{18, "补水泵状态", modbus.TypeUint16, modbus.OrderAB, 1, "", false},
	{19, "故障代码", modbus.TypeUint16, modbus.OrderAB, 1, "", false},
	{346, "温差设定", modbus.TypeFloat32, modbus.OrderCDAB, 1, "℃", true},
	{348, "供水温度设定", modbus.TypeFloat32, modbus.OrderCDAB, 1, "℃", true},
	{350, "泵频率上限", modbus.TypeUint16, modbus.OrderAB, 0.01, "Hz", true},
	{351, "阀门手动开度", modbus.TypeUint16, modbus.OrderAB, 0.1, "%", true},
	{352, "运行模式", modbus.TypeUint16, modbus.OrderAB, 1, "", true},
	{353, "控制权", modbus.TypeUint16, modbus.OrderAB, 1, "", false},
	{600, "室外温度", modbus.TypeFloat32, modbus.OrderCDAB, 1, "℃", false},
	{602, "室外湿度", modbus.TypeUint16, modbus.OrderAB, 0.1, "%RH", false},
	{603, "气象站通讯", modbus.TypeUint16, modbus.OrderAB, 1, "", false},
}

// HeatStationEnums 是状态类点的取值含义。
var HeatStationEnums = map[uint16]map[int]string{
	18:  {0: "停止", 1: "运行"}, // 补水泵状态
	19:  {0: "无故障"},         // 故障代码
	352: {0: "手动", 1: "自动"}, // 运行模式
	353: {0: "本地", 1: "远程"}, // 控制权
	603: {0: "中断", 1: "正常"}, // 气象站通讯
}

// HeatStationLimits 是可写点允许的工程值范围（点表 min / max）。
var HeatStationLimits = map[uint16][2]float64{
	346: {5, 25},  // 温差设定 ℃
	348: {30, 70}, // 供水温度设定 ℃
	350: {20, 50}, // 泵频率上限 Hz
	351: {0, 100}, // 阀门手动开度 %
	352: {0, 1},   // 运行模式 0 手动 / 1 自动
}

const (
	heatSupply     = 45.2
	heatReturn     = 32.0
	heatDeltaT     = 13.2 // 45.2 − 32.0
	heatFlow       = 36.5
	heatEnergy     = 1234567 // kWh
	heatVolume     = 9876543 // 0.01 m³
	waterPowerCoef = 1.163   // kW = 1.163 × m³/h × K（水）
)

// HeatStation 按示例点表初始化数据，地址 0–999 有效，超出返回异常 02。
// 初值与设计文档 13.3 的报文快照一致。
func HeatStation() *Store {
	st := NewStore(1000)
	raw := map[uint16]float64{
		0: heatSupply, 2: heatReturn, 4: 132, 5: 0x0001, 6: heatFlow, 8: waterPowerCoef * heatFlow * heatDeltaT,
		10: heatEnergy, 12: heatVolume, 14: 4250, 15: 653, 16: 62, 17: 45, 18: 0, 19: 0,
		346: 15.0, 348: 45.0, 350: 5000, 351: 650, 352: 1, 353: 1,
		600: -3.5, 602: 682, 603: 1,
	}
	for _, p := range HeatStationPoints {
		_ = st.SetValue(modbus.AreaHoldingRegisters, p.Offset, p.Type, p.Order, raw[p.Offset])
	}
	return st
}

// RunHeatStation 每秒更新瞬时流量、热功率和两个累计量，直到 ctx 结束。
func RunHeatStation(ctx context.Context, st *Store) {
	start := time.Now()
	energy, volume := 0.0, 0.0
	last := start
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			dt := now.Sub(last).Seconds()
			last = now
			flow := math.Round((heatFlow+0.4*math.Sin(now.Sub(start).Seconds()/4))*100) / 100
			power := waterPowerCoef * flow * heatDeltaT
			energy += power * dt / 3600
			volume += flow * dt / 3600 * 100
			h := modbus.AreaHoldingRegisters
			_ = st.SetValue(h, 6, modbus.TypeFloat32, modbus.OrderCDAB, flow)
			_ = st.SetValue(h, 8, modbus.TypeFloat32, modbus.OrderCDAB, power)
			_ = st.SetValue(h, 10, modbus.TypeUint32, modbus.OrderCDAB, heatEnergy+math.Floor(energy))
			_ = st.SetValue(h, 12, modbus.TypeUint32, modbus.OrderCDAB, heatVolume+math.Floor(volume))
		}
	}
}
