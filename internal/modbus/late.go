package modbus

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// 晚到帧：没等到响应就结束（超时或取消）的请求，之后才到的响应。客户端记着最近发出的请求，晚到帧按事务号
// （RTU / ASCII 没有事务号，按上一条请求）找回回的是哪条请求，在帧的 Err 里说明那条请求的发送时间、报文，
// 以及比超时晚了多久。Modbus TCP 收到事务号不是本连接发出的帧，说明是别的主站的响应，调大超时也收不到。

// sentReq 是最近发出的一条请求。
type sentReq struct {
	tx      Packet    // 发送行：发送时间、请求号、报文
	start   time.Time // 开始计时的时刻，和正常响应的 RTT 同一个起点
	timeout time.Duration
	ended   Status // 没等到响应就结束的为 StatusTimeout 或 StatusCancelled，之后到的响应才算晚到
}

// maxSent 是记住的请求条数。晚到几百条请求之后才到的响应，按“不是本连接发出的”处理。
const maxSent = 256

// LateError 说明晚到帧回的是哪条请求、比超时晚了多久。Max 大于 Elapsed 时帧在两次请求之间到达，只能确定用时的范围；
// 都为 0 时说不清什么时候到的。
type LateError struct {
	Request      Packet // 那条请求的发送行
	Later        int    // 发出那条请求之后、收到这帧之前又发了几条请求：设备慢、请求间隔短时，几条请求的响应会先后晚到，顺序也可能和请求不同
	Cancelled    bool   // 那条请求是被取消的，不是超时
	Timeout      time.Duration
	Elapsed, Max time.Duration // 从发出请求到收到这帧
}

func (e *LateError) Error() string {
	ms := func(d time.Duration) int64 { return d.Round(time.Millisecond).Milliseconds() }
	r := e.Request
	req := fmt.Sprintf("回的是 %s 发出的请求 #%06d：%s", r.Time.Format("15:04:05.000"), r.RequestID, frameString(r.Mode, r.Raw))
	if e.Later > 0 {
		req += fmt.Sprintf("（这期间又发了 %d 条请求）", e.Later)
	}
	took := fmt.Sprintf("响应用了 %d ms", ms(e.Elapsed))
	over := fmt.Sprintf(" %d ms", ms(e.Elapsed-e.Timeout))
	if e.Elapsed-e.Timeout < time.Millisecond {
		over = "不到 1 ms"
	}
	if ms(e.Max) > ms(e.Elapsed) {
		took = fmt.Sprintf("响应用了 %d–%d ms（在两次请求之间到达，只能确定范围）", ms(e.Elapsed), ms(e.Max))
		over = fmt.Sprintf(" %d–%d ms", ms(e.Elapsed-e.Timeout), ms(e.Max-e.Timeout))
	}
	switch {
	case e.Max == 0 && e.Cancelled:
		return "请求已取消；" + req
	case e.Max == 0:
		return fmt.Sprintf("超时 %d ms；%s", ms(e.Timeout), req)
	case e.Cancelled:
		return fmt.Sprintf("请求已取消（超时 %d ms），%s；%s", ms(e.Timeout), took, req)
	}
	return fmt.Sprintf("比超时晚%s：超时 %d ms，%s；%s", over, ms(e.Timeout), took, req)
}

// ForeignError 是 Modbus TCP 收到的帧，事务号不是本连接最近发出的请求：多半是网关把别的主站的响应也发到了这个连接。
type ForeignError struct{ TxID uint16 }

func (e *ForeignError) Error() string {
	return fmt.Sprintf("事务号 %04X 不是本连接最近发出的请求，多半是别的主站的", e.TxID)
}

func frameString(mode Mode, raw []byte) string {
	if mode.IsASCII() {
		return strings.TrimRight(string(raw), "\r\n")
	}
	return fmt.Sprintf("% X", raw)
}

// late 记下晚到的字节，它们在 from–to 之间到达。Modbus TCP 连在一起收到的几帧拆开，各自说明回的是哪条请求。
func (c *Client) late(raw []byte, from, to time.Time, result func([]byte, Status, error)) {
	frames := [][]byte{raw}
	if c.opts.Mode.mbap() {
		frames = splitMBAP(raw)
	}
	for _, f := range frames {
		result(f, StatusLate, c.lateError(f, from, to))
	}
}

// splitMBAP 把连在一起的 Modbus TCP 帧拆开，拆不开的部分作为最后一段。
func splitMBAP(raw []byte) [][]byte {
	var out [][]byte
	for len(raw) >= 8 && binary.BigEndian.Uint16(raw[2:]) == 0 {
		n := 6 + int(binary.BigEndian.Uint16(raw[4:]))
		if n < 8 || n > len(raw) {
			break
		}
		out = append(out, raw[:n])
		raw = raw[n:]
	}
	if len(raw) > 0 {
		out = append(out, raw)
	}
	return out
}

// lateError 找出晚到帧回的是哪条请求；from 为零表示说不清什么时候到的。找不到、或那条请求已经正常结束时返回 nil。
func (c *Client) lateError(raw []byte, from, to time.Time) error {
	var owner *sentReq
	later := 0
	if c.opts.Mode.mbap() {
		if len(raw) < 8 {
			return nil
		}
		id := binary.BigEndian.Uint16(raw)
		for i := len(c.sent) - 1; i >= 0 && owner == nil; i-- {
			if c.sent[i].tx.TxID == id {
				owner, later = &c.sent[i], len(c.sent)-1-i
			}
		}
		if owner == nil {
			return &ForeignError{TxID: id}
		}
		if FunctionCode(raw[7]&0x7F) != owner.tx.Function {
			return nil
		}
	} else if n := len(c.sent); n > 0 && c.sent[n-1].tx.RequestID+1 >= c.reqID {
		owner = &c.sent[n-1] // 只认上一条（超时后保护间隔内则是本条）：更早的说不清
	}
	if owner == nil || owner.ended == "" {
		return nil
	}
	e := &LateError{Request: owner.tx, Later: later, Cancelled: owner.ended == StatusCancelled, Timeout: owner.timeout}
	if !from.IsZero() {
		e.Elapsed, e.Max = from.Sub(owner.start), to.Sub(owner.start)
	}
	return e
}
