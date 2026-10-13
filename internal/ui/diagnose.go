package ui

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// evidence 是一次连接期间积累的通信证据，供错误自动分析使用。收发回调在请求所在的
// goroutine 里更新，界面线程读取。
type evidence struct {
	okAt    atomic.Int64 // 最近一次正常响应（UnixNano），0 表示本次连接还没有
	late    atomic.Int64 // 晚到响应次数，不含别的主站的响应
	foreign atomic.Int64 // 别的主站的响应（Modbus TCP 事务号不是本连接发出的）

	mu        sync.Mutex
	lateDelay map[reqKey]int64 // 每种请求观测到的最大应答延迟（ms），来自找得到是哪条超时请求的晚到响应
}

// reqKey 标识一种请求，晚到响应按它归到对应的读取窗口。
type reqKey struct {
	slave byte
	fn    modbus.FunctionCode
	addr  uint16
}

func (e *evidence) reset() {
	for _, v := range []*atomic.Int64{&e.okAt, &e.late, &e.foreign} {
		v.Store(0)
	}
	e.mu.Lock()
	e.lateDelay = nil
	e.mu.Unlock()
}

func (e *evidence) delayOf(k reqKey) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lateDelay[k]
}

func (e *evidence) observe(p modbus.Packet) {
	var le *modbus.LateError
	switch {
	case p.Status == modbus.StatusSuccess || p.Status == modbus.StatusException || p.Status == modbus.StatusNonstandard:
		e.okAt.Store(p.Time.UnixNano())
	case p.Status != modbus.StatusLate:
	case errors.As(p.Err, new(*modbus.ForeignError)):
		e.foreign.Add(1)
	default:
		e.late.Add(1)
		// 晚到帧常在等下一条请求时才到，记录行上的请求不一定是它的，按客户端找回的那条请求记；
		// 在两次请求之间到达的只知道范围，取下限：至少用了这么久。上限可能接近整个扫描周期，按它建议会调得过大；
		// 按下限调大后还超时，下一次晚到会再给出更大的建议
		if errors.As(p.Err, &le) && !le.Cancelled && le.Elapsed > 0 {
			r := le.Request
			k := reqKey{r.Slave, r.Function, r.Address}
			e.mu.Lock()
			if e.lateDelay == nil {
				e.lateDelay = map[reqKey]int64{}
			}
			e.lateDelay[k] = max(e.lateDelay[k], le.Elapsed.Milliseconds())
			e.mu.Unlock()
		}
	}
}

// foreignAdvice 是收到别的主站的响应时的处理建议。
const foreignAdvice = "停掉其他主站，或在串口服务器 / DTU 里关掉多主机共享、多中心转发（串口收到的数据发给所有连接）后再试。"

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
	// 收到了属于本次请求的帧，先说收到了什么，再谈超时一类的解释；不建议减数量、换功能码或扫站号
	if ns, ok := modbus.AsNonstandard(err); ok {
		return diagnosis{Text: fmt.Sprintf("非标准响应：功能码 %02X，原样回显了请求的地址和数量", ns.PDU[0]),
			Hint: fmt.Sprintf("设备收到了请求并作答，但回的不是正常数据，也不是标准异常（%s 的标准异常应为 %02X），没有寄存器数据；后面的字节是地址，不是异常码。"+
				"已知 MCGS 数据转发接口读到没有转发数据的地址时这样回，但仅凭这帧不能确定原因。用“检测寄存器”逐点核对哪些点读不通，再到设备的转发通道里核对变量连接。",
				d.Function, byte(d.Function)|0x80),
			Action: "检测这个范围", Do: func() { ws.probeRange(w) }}
	}
	if errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch) {
		dg := diagnosis{Text: "超时：期间收到过事务号对应、但内容与请求不符的响应",
			Hint: "设备在应答，只是回的内容和请求对不上（见报文记录里的“不匹配”行，例如 Unit ID、功能码、字节数或回显不符），没有收到正常数据。这不是链路不通，先看那几帧回了什么。"}
		if n := ev.foreign.Load(); n > 0 {
			dg.Hint += fmt.Sprintf("这条连接上还收到了 %d 帧别的主站的响应：中间的串口服务器 / DTU 在几个主站之间转发，把别人的响应配上了本程序的事务号。%s", n, foreignAdvice)
		}
		return dg
	}
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
		case ev.foreign.Load() > 0:
			dg.Hint = fmt.Sprintf("这条连接上收到了 %d 帧别的主站的响应（事务号不是本程序发出的，报文里标为“非本连接的响应”）：中间的串口服务器 / DTU 把几个主站的通信都转给了这个连接，"+
				"本程序的请求没等到自己的响应。这不是设备回得慢，调大超时也收不到。%s", ev.foreign.Load(), foreignAdvice)
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

// suggestPointOrder 检查按点表显示的浮点点：当前字节序下多数不合理、而恰好有一种字节序全部合理时，
// 返回当前和建议的字节序（都用 32 位写法）。窗口里的浮点点字节序不一致时不判断。
func suggestPointOrder(pts pointTable, d readDef, regs []uint16) (cur, sug modbus.ByteOrder, ok bool) {
	type sample struct {
		t    modbus.DataType
		regs []uint16
	}
	var ss []sample
	for i := range regs {
		p, found := pts.Get(d.area(), d.Start+uint16(i))
		if !found || !p.Type.Float() || i+p.Regs() > len(regs) || !slices.ContainsFunc(regs[i:i+p.Regs()], func(r uint16) bool { return r != 0 }) {
			continue
		}
		o := p.Order.For(modbus.TypeFloat32)
		if cur != "" && o != cur {
			return "", "", false
		}
		cur = o
		ss = append(ss, sample{p.Type, regs[i : i+p.Regs()]})
	}
	good := func(o modbus.ByteOrder) int {
		n := 0
		for _, s := range ss {
			if v, err := modbus.DecodeRaw(s.t, o.For(s.t), s.regs); err == nil && isPlausible(s.t, v) {
				n++
			}
		}
		return n
	}
	if len(ss) == 0 || good(cur)*2 > len(ss) {
		return "", "", false
	}
	var found []modbus.ByteOrder
	for _, o := range modbus.Orders32 {
		if o != cur && good(o) == len(ss) {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		return "", "", false
	}
	return cur, found[0], true
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
