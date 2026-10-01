// Package control 实现写入与控制验证：写入后多次回读，判断控制是否真实生效（设计文档第 11 章）。
// 收到正常写响应不等于控制成功，只有回读值在容差内匹配目标才算 PASS。
package control

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Target 描述一个控制点：按采集配置回读，按写配置写入。
type Target struct {
	Slave      byte
	Area       modbus.Area // AreaCoils 表示线圈（值为 0 / 1）；其他值都按保持寄存器处理
	Address    uint16
	Type       modbus.DataType
	ReadOrder  modbus.ByteOrder // 采集字节序，回读按它解码
	WriteOrder modbus.ByteOrder // 写入字节序，为空时与采集一致
	Scaling    modbus.Scaling
	Function   modbus.FunctionCode // FC06 或 FC16（线圈为 FC05 或 FC15）；为 0 时单个寄存器 / 线圈用 FC06 / FC05，否则 FC16
}

func (t Target) coil() bool { return t.Area == modbus.AreaCoils }

// EncodeOrder 是写入时实际使用的字节序。
func (t Target) EncodeOrder() modbus.ByteOrder {
	if t.WriteOrder == "" {
		return t.ReadOrder
	}
	return t.WriteOrder
}

// Result 是控制验证结论，取值与 SQLite control_tests.result 一致。
type Result string

const (
	ResultPass          Result = "PASS"
	ResultNotApplied    Result = "NOT_APPLIED"    // 回读一直是原值
	ResultOverwritten   Result = "OVERWRITTEN"    // 先变成目标值，后被改回
	ResultMismatch      Result = "MISMATCH"       // 变了但不等于目标
	ResultUnknown       Result = "UNKNOWN"        // 写入超时，且无法从回读判断
	ResultProtocolError Result = "PROTOCOL_ERROR" // 设备拒绝或读原值失败
)

// DefaultSchedule 是默认回读时刻：写入后 0 ms、200 ms、1000 ms。
var DefaultSchedule = []time.Duration{0, 200 * time.Millisecond, time.Second}

// Readback 是一次回读结果。
type Readback struct {
	At        time.Duration
	Registers []uint16
	Value     float64 // 工程值
	Err       error
}

// Report 是一次控制验证的完整记录。
type Report struct {
	Original     float64
	OriginalRegs []uint16
	Target       float64  // 写入寄存器对应的工程值
	Written      []uint16 // 按写入字节序编码的寄存器
	WriteErr     error
	Readbacks    []Readback
	Result       Result
	Hint         string // 确定性诊断给出的原因，例如字节序不一致
}

// Encode 把工程值编码为要写入的寄存器（编码预览同样调用它）。
// 线圈只接受 0 或 1，编码为一个值为 0 / 1 的“寄存器”。
func Encode(t Target, value float64) (regs []uint16, rounded bool, err error) {
	if t.coil() {
		if value != 0 && value != 1 {
			return nil, false, fmt.Errorf("线圈只能写 0（OFF）或 1（ON），得到 %v", value)
		}
		return []uint16{uint16(value)}, false, nil
	}
	raw, rounded := t.Scaling.Raw(t.Type, value)
	regs, err = modbus.EncodeRaw(t.Type, t.EncodeOrder(), raw)
	return regs, rounded, err
}

// EncodeText 把输入的工程值编码为寄存器，value 是输入的数值。64 位整型且没有换算（倍率 1）时按整数精确解析：
// float64 只有 53 位精度，超过 2^53 的值经过 float64 会丢掉低位。这种情况下也接受 0x 开头的十六进制原始值。
func EncodeText(t Target, s string) (regs []uint16, value float64, rounded bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, 0, false, errors.New("请输入数值")
	}
	if (t.Type == modbus.TypeInt64 || t.Type == modbus.TypeUint64) && t.Scaling.Identity() && !t.coil() {
		if regs, err = modbus.ParseRaw(t.Type, t.EncodeOrder(), s); err != nil {
			return nil, 0, false, err
		}
		value, err = modbus.DecodeRaw(t.Type, t.EncodeOrder(), regs)
		return regs, value, false, err
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, 0, false, fmt.Errorf("“%s”不是数字", s)
	}
	regs, rounded, err = Encode(t, v)
	return regs, v, rounded, err
}

// WriteRequest 返回写入请求，界面用它展示完整请求报文。
func WriteRequest(t Target, regs []uint16) modbus.Request {
	if t.coil() {
		bits := make([]bool, len(regs))
		for i, r := range regs {
			bits[i] = r != 0
		}
		fc := t.Function
		if fc != modbus.FuncWriteMultipleCoils {
			fc = modbus.FuncWriteSingleCoil
		}
		return modbus.Request{Slave: t.Slave, Function: fc, Address: t.Address, Bits: bits}
	}
	fc := t.Function
	if fc == 0 {
		fc = modbus.FuncWriteMultipleRegisters
		if len(regs) == 1 {
			fc = modbus.FuncWriteSingleRegister
		}
	}
	return modbus.Request{Slave: t.Slave, Function: fc, Address: t.Address, Values: regs}
}

func (t Target) read(ctx context.Context, c *modbus.Client) ([]uint16, float64, error) {
	if t.coil() {
		resp, err := c.Do(ctx, modbus.Request{Slave: t.Slave, Function: modbus.FuncReadCoils, Address: t.Address, Quantity: 1})
		if err != nil {
			return nil, 0, err
		}
		if resp.Bits[0] {
			return []uint16{1}, 1, nil
		}
		return []uint16{0}, 0, nil
	}
	resp, err := c.Do(ctx, modbus.Request{Slave: t.Slave, Function: modbus.FuncReadHoldingRegisters,
		Address: t.Address, Quantity: uint16(t.Type.Registers())})
	if err != nil {
		return nil, 0, err
	}
	raw, err := modbus.DecodeRaw(t.Type, t.ReadOrder, resp.Registers)
	return resp.Registers, t.Scaling.Engineering(raw), err
}

func (t Target) write(ctx context.Context, c *modbus.Client, regs []uint16) error {
	req := WriteRequest(t, regs)
	if req.Function == modbus.FuncWriteSingleRegister && len(regs) > 1 {
		// 两次 FC06 不是原子操作，调用方应已提示用户（设计文档 11.7）
		for i, r := range regs {
			one := req
			one.Address, one.Values = t.Address+uint16(i), []uint16{r}
			if _, err := c.Do(ctx, one); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := c.Do(ctx, req)
	return err
}

// same 判断按 o 解码的回读寄存器是否等于 want（按 wantOrder 编码）。整型和线圈比较原始整数：
// 64 位整型超出 float64 的 53 位精度，比较工程值会把只差低位的两个值当成相等。
// 浮点比较工程值，允许相对 1e-6 的误差。
func (t Target) same(got []uint16, o modbus.ByteOrder, want []uint16, wantOrder modbus.ByteOrder) bool {
	if t.coil() {
		return len(got) == 1 && len(want) == 1 && (got[0] != 0) == (want[0] != 0)
	}
	if !t.Type.Float() {
		a, err1 := modbus.Bits(t.Type, o, got)
		b, err2 := modbus.Bits(t.Type, wantOrder, want)
		return err1 == nil && err2 == nil && a == b
	}
	a, err1 := modbus.DecodeRaw(t.Type, o, got)
	b, err2 := modbus.DecodeRaw(t.Type, wantOrder, want)
	return err1 == nil && err2 == nil && math.Abs(a-b) <= math.Max(math.Abs(b)*1e-6, 1e-6)
}

// Verify 读原值、写入 regs（Encode / EncodeText 的结果）、按 schedule 多次回读并判定（设计文档 11.4、11.5）。
// 不自动恢复原值。
func Verify(ctx context.Context, c *modbus.Client, t Target, regs []uint16, schedule []time.Duration) Report {
	if len(schedule) == 0 {
		schedule = DefaultSchedule
	}
	rep := Report{Written: regs}
	if len(regs) == 0 {
		rep.Result, rep.Hint = ResultProtocolError, "没有要写入的值"
		return rep
	}
	if t.coil() {
		rep.Target = float64(min(regs[0], 1))
	} else if raw, err := modbus.DecodeRaw(t.Type, t.EncodeOrder(), regs); err == nil {
		rep.Target = t.Scaling.Engineering(raw)
	} else {
		rep.Result, rep.Hint = ResultProtocolError, err.Error()
		return rep
	}
	orig, origVal, err := t.read(ctx, c)
	if err != nil {
		rep.Result, rep.Hint = ResultProtocolError, "读取原值失败："+err.Error()
		return rep
	}
	rep.OriginalRegs, rep.Original = orig, origVal
	rep.WriteErr = t.write(ctx, c, rep.Written)
	ack := false
	if ex, ok := modbus.AsException(rep.WriteErr); ok {
		// 05 Acknowledge 表示设备已接受、还在处理，不是拒绝：照常回读，以回读结果为准
		if ex.Code != modbus.ExceptionAcknowledge {
			rep.Result, rep.Hint = ResultProtocolError, fmt.Sprintf("%v。%s", ex, ex.Code.Tip())
			return rep
		}
		ack = true
	}
	if rep.WriteErr != nil && !ack && !errors.Is(rep.WriteErr, modbus.ErrTimeout) && !errors.Is(rep.WriteErr, modbus.ErrCRC) && !errors.Is(rep.WriteErr, modbus.ErrLRC) {
		rep.Result, rep.Hint = ResultProtocolError, rep.WriteErr.Error()
		return rep
	}

	start := time.Now()
	for _, at := range schedule {
		if d := at - time.Since(start); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				rep.Result, rep.Hint = ResultUnknown, "已取消"
				return rep
			}
		}
		rb := Readback{At: at}
		rb.Registers, rb.Value, rb.Err = t.read(ctx, c)
		rep.Readbacks = append(rep.Readbacks, rb)
	}
	rep.Result, rep.Hint = classify(t, rep)
	if ack && rep.Result != ResultPass {
		rep.Hint = "设备回了 05 Acknowledge（已接受、处理中），回读时可能还没处理完，稍后再读一次确认。" + rep.Hint
	}
	return rep
}

func classify(t Target, rep Report) (Result, string) {
	isTarget := func(rb Readback) bool {
		return rb.Err == nil && t.same(rb.Registers, t.ReadOrder, rep.Written, t.EncodeOrder())
	}
	isOriginal := func(rb Readback) bool {
		return rb.Err == nil && t.same(rb.Registers, t.ReadOrder, rep.OriginalRegs, t.ReadOrder)
	}
	match, back := 0, 0
	for _, rb := range rep.Readbacks {
		if isTarget(rb) {
			match++
		}
		if isOriginal(rb) {
			back++
		}
	}
	n := len(rep.Readbacks)
	first, last := rep.Readbacks[0], rep.Readbacks[n-1]
	switch {
	case match == n:
		return ResultPass, ""
	case back == n:
		return ResultNotApplied, "回读一直是原值。可能原因：写入地址不对、点位只读、设备处于本地模式或有联锁条件"
	case isTarget(first) && isOriginal(last):
		return ResultOverwritten, "写入已生效但随后被改回，疑似 PLC 程序或设备逻辑覆盖了该值"
	case last.Err != nil && rep.WriteErr != nil:
		return ResultUnknown, "写入超时且回读失败，无法判断设备是否执行"
	}
	if last.Err != nil || t.coil() {
		return ResultMismatch, "疑似数据类型、Scale 或地址配置错误"
	}
	// 值变了但不等于目标：尝试其他字节序，找出是不是写入字节序与采集不一致
	if t.Type.Registers() > 1 {
		for _, o := range t.Type.Orders() {
			if o != t.ReadOrder && t.same(last.Registers, o, rep.Written, t.EncodeOrder()) {
				return ResultMismatch, fmt.Sprintf("按 %s 解码回读值正好等于目标：写入用了 %s，采集用 %s，两者不一致", o, t.EncodeOrder(), t.ReadOrder)
			}
		}
	}
	// 64 位整型只差低位：设备内部用 double 保存，超过 2^53 的值存不下全部位数
	if t.Type == modbus.TypeInt64 || t.Type == modbus.TypeUint64 {
		got, _ := modbus.Bits(t.Type, t.ReadOrder, last.Registers)
		want, _ := modbus.Bits(t.Type, t.EncodeOrder(), rep.Written)
		asDouble := func(u uint64) float64 {
			if t.Type == modbus.TypeInt64 {
				return float64(int64(u))
			}
			return float64(u)
		}
		if asDouble(got) == asDouble(want) {
			return ResultMismatch, "回读只有低位与目标不同：设备可能用 double（53 位精度）保存 64 位整数，超过 2^53 的值低位会丢失"
		}
	}
	return ResultMismatch, "疑似数据类型、Scale 或地址配置错误"
}

// Restore 按读到的原始寄存器逐位写回原值。
func Restore(ctx context.Context, c *modbus.Client, t Target, original []uint16) error {
	t.WriteOrder = t.ReadOrder
	return t.write(ctx, c, original)
}
