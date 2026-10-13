package diagnose

import (
	"errors"
	"fmt"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Request 是出错的那次读取。
type Request struct {
	Slave    byte
	Function modbus.FunctionCode
	Start    uint16
	Qty      int
	Scan     time.Duration // 扫描周期，设备忙时建议加倍；不知道时为 0
}

// Context 是判断用的现场情况。
type Context struct {
	Mode      modbus.Mode
	Timeout   time.Duration
	Simulator bool      // 内置模拟器：协议是确定的，不建议识别协议
	Evidence  *Evidence // 本次连接的通信证据，可为空
}

// Action 是可以一键处理的动作，由调用方实现（桌面版是按钮，Web 版和 modbus-cli 写进建议里）。
type Action int

const (
	None           Action = iota
	SetTimeout            // 超时改为 Result.Timeout
	UseFC03               // 改用 FC03 读保持寄存器
	UseFC04               // 改用 FC04 读输入寄存器
	ProbeRange            // 检测这个范围、逐个探测可读地址
	HalveQty              // 数量减半
	DoubleScan            // 扫描周期加倍
	ScanSlaves            // 扫描从站地址
	DetectProtocol        // 识别协议
	ScanSerial            // 扫描串口参数
	Reconnect             // 重新连接
)

// Result 是分析结果。
type Result struct {
	Text    string        // 出了什么错
	Hint    string        // 可能的原因和处理方法
	Action  Action        // 一键处理，None 表示没有
	Label   string        // 一键处理的说法，例如“超时改为 2000 ms”
	Timeout time.Duration // Action 为 SetTimeout 时建议的超时
}

// ForeignAdvice 是收到别的主站的响应时的处理建议。
const ForeignAdvice = "停掉其他主站，或在串口服务器 / DTU 里关掉多主机共享、多中心转发（串口收到的数据发给所有连接）后再试。"

// SuggestTimeout 按观测到的应答延迟给出超时建议：延迟的 1.5 倍，向上取整到 500 ms。
func SuggestTimeout(delay time.Duration) time.Duration {
	ms := delay.Milliseconds()
	v := (ms*3/2 + 499) / 500 * 500
	return time.Duration(max(v, 500)) * time.Millisecond
}

// Analyze 分析一次读取错误。
func Analyze(req Request, c Context, err error) Result {
	ev := c.Evidence
	if ev == nil {
		ev = &Evidence{}
	}
	// 收到了属于本次请求的帧，先说收到了什么，再谈超时一类的解释；不建议减数量、换功能码或扫站号
	if ns, ok := modbus.AsNonstandard(err); ok {
		return Result{Text: fmt.Sprintf("非标准响应：功能码 %02X，原样回显了请求的地址和数量", ns.PDU[0]),
			Hint: fmt.Sprintf("设备收到了请求并作答，但回的不是正常数据，也不是标准异常（%s 的标准异常应为 %02X），没有寄存器数据；后面的字节是地址，不是异常码。"+
				"已知 MCGS 数据转发接口读到没有转发数据的地址时这样回，但仅凭这帧不能确定原因。用“检测寄存器”逐点核对哪些点读不通，再到设备的转发通道里核对变量连接。",
				req.Function, byte(req.Function)|0x80),
			Action: ProbeRange, Label: "检测这个范围"}
	}
	if errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch) {
		r := Result{Text: "超时：期间收到过事务号对应、但内容与请求不符的响应",
			Hint: "设备在应答，只是回的内容和请求对不上（见报文记录里的“不匹配”行，例如 Unit ID、功能码、字节数或回显不符），没有收到正常数据。这不是链路不通，先看那几帧回了什么。"}
		if n := ev.Foreign(); n > 0 {
			r.Hint += fmt.Sprintf("这条连接上还收到了 %d 帧别的主站的响应：中间的串口服务器 / DTU 在几个主站之间转发，把别人的响应配上了本程序的事务号。%s", n, ForeignAdvice)
		}
		return r
	}
	if ex, ok := modbus.AsException(err); ok {
		return exception(req, ex)
	}
	switch {
	case errors.Is(err, modbus.ErrTimeout):
		return timeout(req, c, ev)
	case errors.Is(err, modbus.ErrCRC), errors.Is(err, modbus.ErrLRC):
		check := "CRC"
		if c.Mode.IsASCII() {
			check = "LRC"
		}
		return Result{Text: check + " 错误：响应校验失败",
			Hint: "波特率 / 校验位不一致或线路干扰。偶发可以忽略（读请求会自动重试 1 次），频繁出现请检查接线和屏蔽。"}
	case errors.Is(err, modbus.ErrConnection):
		return Result{Text: "连接错误：" + err.Error(), Hint: "连接已断开。检查网线、设备是否重启，然后重新连接。", Action: Reconnect, Label: "重新连接"}
	}
	return Result{Text: err.Error()}
}

func exception(req Request, ex *modbus.ExceptionError) Result {
	r := Result{Text: "异常 " + ex.Code.Description()} // 例如 异常 02 非法数据地址（Illegal Data Address）
	switch ex.Code {
	case modbus.ExceptionIllegalFunction:
		r.Hint = fmt.Sprintf("设备不支持功能码 %s。", req.Function)
		switch req.Function {
		case modbus.FuncReadInputRegisters:
			r.Hint += "不少设备把数据都放在保持寄存器里，试试 FC03。"
			r.Action, r.Label = UseFC03, "改用 FC03"
		case modbus.FuncReadHoldingRegisters:
			r.Hint += "试试 FC04（输入寄存器）。"
			r.Action, r.Label = UseFC04, "改用 FC04"
		default:
			r.Hint += ex.Code.Tip()
		}
	case modbus.ExceptionIllegalDataAddress:
		r.Hint = fmt.Sprintf("%s 中有设备未定义的地址：① 地址可能差 1（手册按 1 起始、协议按 0 起始），试试起始地址 ±1；② 读取范围跨过了未定义的区段。",
			modbus.ReferenceSpan(modbus.AreaOf(req.Function), req.Start, req.Qty))
		r.Action, r.Label = ProbeRange, "逐个探测可读地址"
	case modbus.ExceptionIllegalDataValue:
		r.Hint = fmt.Sprintf("数量 %d 可能超出设备单次读取上限（不少设备只允许 32 或 64 个）。", req.Qty)
		if req.Qty > 1 {
			r.Action, r.Label = HalveQty, "数量减半"
		}
	case modbus.ExceptionSlaveDeviceBusy:
		r.Hint = ex.Code.Tip() + "。"
		if req.Scan > 0 {
			r.Hint = fmt.Sprintf("%s。当前扫描周期 %d ms。", ex.Code.Tip(), req.Scan.Milliseconds())
			r.Action, r.Label = DoubleScan, "扫描周期加倍"
		}
	case modbus.ExceptionGatewayPathUnavailable:
		r.Hint = fmt.Sprintf("网关没有配置到 Slave %d 的路由，检查网关里的从站地址映射。", req.Slave)
	case modbus.ExceptionGatewayTargetFailed:
		r.Hint = fmt.Sprintf("网关收到了请求，但串口侧的 Slave %d 没有应答：检查 Slave ID、串口侧波特率 / 校验位、485 接线。", req.Slave)
	default:
		r.Hint = ex.Code.Tip()
	}
	return r
}

func timeout(req Request, c Context, ev *Evidence) Result {
	ms := c.Timeout.Milliseconds()
	r := Result{Text: fmt.Sprintf("超时：%d ms 内无响应", ms)}
	switch delay := ev.DelayOf(req.Slave, req.Function, req.Start); {
	case delay > 0 && delay >= c.Timeout:
		r.Timeout = SuggestTimeout(delay)
		r.Hint = fmt.Sprintf("设备有应答，但约 %d ms 才到，超过了超时 %d ms（报文里记为晚到响应）。", delay.Milliseconds(), ms)
		r.Action, r.Label = SetTimeout, fmt.Sprintf("超时改为 %d ms", r.Timeout.Milliseconds())
	case ev.Foreign() > 0:
		r.Hint = fmt.Sprintf("这条连接上收到了 %d 帧别的主站的响应（事务号不是本程序发出的，报文里标为“非本连接的响应”）：中间的串口服务器 / DTU 把几个主站的通信都转给了这个连接，"+
			"本程序的请求没等到自己的响应。这不是设备回得慢，调大超时也收不到。%s", ev.Foreign(), ForeignAdvice)
	case ev.OKSeen():
		r.Hint = fmt.Sprintf("同一连接上有正常响应，链路和协议没问题。检查 Slave %d 是否存在，或该地址段是否需要更长时间才应答。", req.Slave)
		r.Action, r.Label = ScanSlaves, "扫描从站地址"
	case c.Mode == modbus.ModeTCP:
		r.Hint = "连接已建立但设备一直没有应答：① 设备可能是 RTU over TCP（串口服务器、透传模块常见）；② Unit ID 要填网关后面设备的站号。"
		r.Action, r.Label = DetectProtocol, "识别协议"
	case c.Mode == modbus.ModeRTUOverTCP:
		r.Hint = fmt.Sprintf("设备一直没有应答：① 设备可能是 Modbus TCP；② 串口服务器的波特率、校验位要与设备一致；③ 检查 Slave ID %d。", req.Slave)
		r.Action, r.Label = DetectProtocol, "识别协议"
	default:
		r.Hint = fmt.Sprintf("设备一直没有应答：① 波特率 / 校验位 / 停止位与设备一致吗；② Slave ID %d 对吗；③ A、B 线是否接反、终端电阻；④ 总线上是否还有别的主站。", req.Slave)
		r.Action, r.Label = ScanSerial, "扫描串口参数"
	}
	if c.Simulator && r.Action == DetectProtocol { // 内置模拟器的协议是确定的
		r.Action, r.Label = None, ""
	}
	return r
}
