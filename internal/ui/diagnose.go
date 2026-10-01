package ui

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// evidence 是一次连接期间积累的通信证据，供错误自动分析使用。收发回调在请求所在的
// goroutine 里更新，界面线程读取。
type evidence struct {
	okAt atomic.Int64 // 最近一次正常响应（UnixNano），0 表示本次连接还没有
	late atomic.Int64 // 晚到响应次数

	mu        sync.Mutex
	txTime    map[uint64]time.Time // RequestID → 发送时刻，只保留最近几十条
	lateDelay map[reqKey]int64     // 每种请求观测到的最大应答延迟（ms），来自超时后保护间隔内到达的晚到响应
}

// reqKey 标识一种请求，晚到响应按它归到对应的读取窗口。
type reqKey struct {
	slave byte
	fn    modbus.FunctionCode
	addr  uint16
}

func (e *evidence) reset() {
	for _, v := range []*atomic.Int64{&e.okAt, &e.late} {
		v.Store(0)
	}
	e.mu.Lock()
	e.txTime, e.lateDelay = nil, nil
	e.mu.Unlock()
}

func (e *evidence) delayOf(k reqKey) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lateDelay[k]
}

func (e *evidence) observe(p modbus.Packet) {
	switch {
	case p.Dir == modbus.DirTX:
		e.mu.Lock()
		if e.txTime == nil || len(e.txTime) > 64 {
			e.txTime = map[uint64]time.Time{}
		}
		e.txTime[p.RequestID] = p.Time
		e.mu.Unlock()
	case p.Status == modbus.StatusSuccess || p.Status == modbus.StatusException:
		e.okAt.Store(p.Time.UnixNano())
	case p.Status == modbus.StatusLate:
		e.late.Add(1)
		e.mu.Lock()
		if sent, ok := e.txTime[p.RequestID]; ok {
			k := reqKey{p.Slave, p.Function, p.Address}
			if e.lateDelay == nil {
				e.lateDelay = map[reqKey]int64{}
			}
			e.lateDelay[k] = max(e.lateDelay[k], p.Time.Sub(sent).Milliseconds())
		}
		e.mu.Unlock()
	}
}

// diagnosis 是读取窗口错误行下面的自动分析：错误本身、可能原因，以及可选的一键处理。
type diagnosis struct {
	Text   string
	Hint   string
	Action string // 按钮文字，空表示没有一键处理
	Do     func()
}

// suggestTimeout 按观测到的应答延迟给出超时建议：延迟的 1.5 倍，向上取整到 500 ms。
func suggestTimeout(delayMS int64) int64 {
	v := (delayMS*3/2 + 499) / 500 * 500
	return max(v, 500)
}

// diagnose 分析读取窗口的错误。规则都是确定性的，只根据错误类型、连接模式和本次连接的通信证据判断。
func (ws *Workspace) diagnose(w *readWindow, err error) diagnosis {
	d := w.def
	s := ws.session
	if s == nil {
		return diagnosis{Text: "未连接"}
	}
	ev := &ws.evidence
	if ex, ok := modbus.AsException(err); ok {
		dg := diagnosis{Text: fmt.Sprintf("异常 %02X：%s", byte(ex.Code), ex.Code.Name())}
		switch ex.Code {
		case modbus.ExceptionIllegalFunction:
			dg.Hint = fmt.Sprintf("设备不支持功能码 %s。", d.Function)
			switch d.Function {
			case modbus.FuncReadInputRegisters:
				dg.Hint += "不少设备把数据都放在保持寄存器里，试试 FC03。"
				dg.Action, dg.Do = "改用 FC03", func() { ws.redefine(w, func(d *readDef) { d.Function = modbus.FuncReadHoldingRegisters }) }
			case modbus.FuncReadHoldingRegisters:
				dg.Hint += "试试 FC04（输入寄存器）。"
				dg.Action, dg.Do = "改用 FC04", func() { ws.redefine(w, func(d *readDef) { d.Function = modbus.FuncReadInputRegisters }) }
			default:
				dg.Hint += ex.Code.Tip()
			}
		case modbus.ExceptionIllegalDataAddress:
			dg.Hint = fmt.Sprintf("%s 中有设备未定义的地址：① 地址可能差 1（手册按 1 起始、协议按 0 起始），试试起始地址 ±1；② 读取范围跨过了未定义的区段。",
				refSpan(d.area(), d.Start, d.Qty))
			dg.Action, dg.Do = "逐个探测可读地址", func() { ws.probeRange(w) }
		case modbus.ExceptionIllegalDataValue:
			dg.Hint = fmt.Sprintf("数量 %d 可能超出设备单次读取上限（不少设备只允许 32 或 64 个）。", d.Qty)
			if d.Qty > 1 {
				dg.Action, dg.Do = "数量减半", func() { ws.redefine(w, func(d *readDef) { d.Qty = max(1, d.Qty/2) }) }
			}
		case modbus.ExceptionSlaveDeviceBusy:
			dg.Hint = fmt.Sprintf("%s。当前扫描周期 %d ms。", ex.Code.Tip(), d.Scan.Milliseconds())
			dg.Action, dg.Do = "扫描周期加倍", func() { ws.redefine(w, func(d *readDef) { d.Scan *= 2 }) }
		case modbus.ExceptionGatewayPathUnavailable:
			dg.Hint = fmt.Sprintf("网关没有配置到 Slave %d 的路由，检查网关里的从站地址映射。", d.Slave)
		case modbus.ExceptionGatewayTargetFailed:
			dg.Hint = fmt.Sprintf("网关收到了请求，但串口侧的 Slave %d 没有应答：检查 Slave ID、串口侧波特率 / 校验位、485 接线。", d.Slave)
		default:
			dg.Hint = ex.Code.Tip()
		}
		return dg
	}

	timeout := ws.timeout.Milliseconds()
	switch {
	case errors.Is(err, modbus.ErrTimeout):
		dg := diagnosis{Text: fmt.Sprintf("超时：%d ms 内无响应", timeout)}
		switch delay := ev.delayOf(reqKey{d.Slave, d.Function, d.Start}); {
		case delay >= timeout:
			sug := suggestTimeout(delay)
			dg.Hint = fmt.Sprintf("设备有应答，但约 %d ms 才到，超过了超时 %d ms（报文里记为晚到响应）。", delay, timeout)
			dg.Action, dg.Do = fmt.Sprintf("超时改为 %d ms", sug), func() { ws.setTimeout(time.Duration(sug) * time.Millisecond) }
		case ev.okAt.Load() != 0:
			dg.Hint = fmt.Sprintf("同一连接上有正常响应，链路和协议没问题。检查 Slave %d 是否存在，或该地址段是否需要更长时间才应答。", d.Slave)
			dg.Action, dg.Do = "扫描从站地址", ws.scanSlavesDialog
		case s.mode == modbus.ModeTCP:
			dg.Hint = "连接已建立但设备一直没有应答：① 设备可能是 RTU over TCP（串口服务器、透传模块常见）；② Unit ID 要填网关后面设备的站号。"
			dg.Action, dg.Do = "识别协议", ws.detectProtocol
		case s.mode == modbus.ModeRTUOverTCP:
			dg.Hint = fmt.Sprintf("设备一直没有应答：① 设备可能是 Modbus TCP；② 串口服务器的波特率、校验位要与设备一致；③ 检查 Slave ID %d。", d.Slave)
			dg.Action, dg.Do = "识别协议", ws.detectProtocol
		default:
			dg.Hint = fmt.Sprintf("设备一直没有应答：① 波特率 / 校验位 / 停止位与设备一致吗；② Slave ID %d 对吗；③ A、B 线是否接反、终端电阻；④ 总线上是否还有别的主站。", d.Slave)
			dg.Action, dg.Do = "扫描串口参数", ws.scanSerialDialog
		}
		if s.sim != nil && dg.Action == "识别协议" { // 内置模拟器的协议是确定的
			dg.Action, dg.Do = "", nil
		}
		return dg
	case errors.Is(err, modbus.ErrCRC), errors.Is(err, modbus.ErrLRC):
		check := "CRC"
		if s.mode.IsASCII() {
			check = "LRC"
		}
		return diagnosis{Text: check + " 错误：响应校验失败",
			Hint: "波特率 / 校验位不一致或线路干扰。偶发可以忽略（读请求会自动重试 1 次），频繁出现请检查接线和屏蔽。"}
	case errors.Is(err, modbus.ErrConnection):
		return diagnosis{Text: "连接错误：" + err.Error(), Hint: "连接已断开。检查网线、设备是否重启，然后重新连接。",
			Action: "重新连接", Do: ws.reconnect}
	}
	return diagnosis{Text: err.Error()}
}

// suggestFloatOrder 检查 FLOAT32 / FLOAT64 显示：当前字节序下多数非零值不合理、而恰好有一种字节序全部合理时，
// 返回那种字节序。这是确定性判断，不依赖 AI。
func suggestFloatOrder(dt modbus.DataType, regs []uint16, cur modbus.ByteOrder) (modbus.ByteOrder, bool) {
	var pairs [][]uint16
	n := dt.Registers()
	for i := 0; i+n <= len(regs); i += n {
		for _, r := range regs[i : i+n] {
			if r != 0 {
				pairs = append(pairs, regs[i:i+n])
				break
			}
		}
	}
	if len(pairs) == 0 {
		return "", false
	}
	good := func(o modbus.ByteOrder) int {
		n := 0
		for _, p := range pairs {
			if v, err := modbus.DecodeRaw(dt, o, p); err == nil && isPlausible(dt, v) {
				n++
			}
		}
		return n
	}
	if good(cur)*2 > len(pairs) {
		return "", false
	}
	var found []modbus.ByteOrder
	for _, o := range dt.Orders() {
		if o != cur && good(o) == len(pairs) {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
