package modbus

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Direction 是报文方向。
type Direction string

const (
	DirTX Direction = "TX"
	DirRX Direction = "RX"
)

// Status 是报文状态，取值与 SQLite packets.status 字段一致（设计文档 14.3）。
type Status string

const (
	StatusSent            Status = "SENT"
	StatusSuccess         Status = "SUCCESS"
	StatusTimeout         Status = "TIMEOUT"
	StatusException       Status = "EXCEPTION"
	StatusCRCError        Status = "CRC_ERROR"
	StatusConnectionError Status = "CONNECTION_ERROR"
	StatusParseError      Status = "PARSE_ERROR"
	StatusCancelled       Status = "CANCELLED"
	StatusLate            Status = "LATE_RESPONSE"
	StatusUnexpected      Status = "UNEXPECTED"
	StatusNonstandard     Status = "NONSTANDARD" // 属于本次请求的非标准响应，见 NonstandardError
)

// Packet 是一条收发记录。同一请求的 TX 和结果行共用 RequestID；
// RX 行的地址和数量取自配对的请求，因为 FC03 等响应本身不含地址。
type Packet struct {
	Time         time.Time
	RequestID    uint64
	ConnectionID string // non-identifying connection generation; empty in legacy recordings
	Dir          Direction
	Mode         Mode
	Slave        byte
	TxID         uint16 // 仅 Modbus TCP
	Function     FunctionCode
	Address      uint16
	Count        uint16
	Raw          []byte // 超时等没有收到字节的结果行为空
	Status       Status
	RTT          time.Duration // 仅结果行
	Err          error
}

// Observer 接收全部收发记录。Packet Recorder 通过它同时分发给界面和 SQLite。
// OnPacket 在请求所在的 goroutine 里同步调用，实现方不要阻塞。
type Observer interface {
	OnPacket(Packet)
}

// ObserverFunc 让普通函数实现 Observer。
type ObserverFunc func(Packet)

func (f ObserverFunc) OnPacket(p Packet) { f(p) }

// Options 是客户端参数。
type Options struct {
	Mode        Mode
	Timeout     time.Duration // 响应超时，默认 1000 ms
	Guard       time.Duration // 超时后的保护间隔，默认串口 RTU 50 ms、TCP 类 100 ms
	CharGap     time.Duration // 功能码未知或需要重新对齐时的字符间隔，默认 20 ms
	ReadRetries int           // 读请求超时或 CRC 错误后的重试次数；写请求从不重试
	DiscardEcho bool          // 丢弃 RS485 转换器回显的发送帧
	Observer    Observer
}

// DefaultOptions 返回设计文档中的默认参数：超时 1000 ms，读重试 1 次，写不重试。
func DefaultOptions(mode Mode) Options {
	return Options{Mode: mode, Timeout: time.Second, ReadRetries: 1}
}

// Client 是 Modbus 主站。同一时刻只有一个未完成请求（设计文档 5.5），
// 响应必须通过严格校验才与请求匹配，校验不过的字节只记录、不采用。
type Client struct {
	mu           sync.Mutex
	t            Transport
	r            frameReader
	opts         Options
	timeout      atomic.Int64 // 响应超时（纳秒），可在运行中修改
	txID         uint16
	reqID        uint64
	connectionID string
	sent         []sentReq // 最近发出的请求，晚到帧据此找回回的是哪条请求（late.go）
}

// NewClient 在已建立的连接上创建客户端。零值参数使用默认值（ReadRetries 除外）。
func NewClient(t Transport, opts Options) *Client {
	if opts.Mode == "" {
		opts.Mode = ModeTCP
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Second
	}
	if opts.Guard <= 0 {
		opts.Guard = 100 * time.Millisecond
		if opts.Mode.Serial() {
			opts.Guard = 50 * time.Millisecond
		}
	}
	if opts.CharGap <= 0 {
		opts.CharGap = 20 * time.Millisecond
	}
	c := &Client{t: t, r: frameReader{t: t}, opts: opts, connectionID: rand.Text()}
	c.timeout.Store(int64(opts.Timeout))
	return c
}

// SetTimeout 修改响应超时，从下一条请求起生效。不等待正在进行的请求，可以在界面线程直接调用。
func (c *Client) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout.Store(int64(d))
	}
}

// Timeout 返回当前响应超时。
func (c *Client) Timeout() time.Duration { return time.Duration(c.timeout.Load()) }

// Mode 返回连接模式。
func (c *Client) Mode() Mode { return c.opts.Mode }

// Close 关闭底层连接。
func (c *Client) Close() error { return c.t.Close() }

// Do 发送请求并等待匹配的响应。读请求在超时或 CRC 错误后按 ReadRetries 重试；
// 写请求不重试，因为超时不代表设备没有执行（设计文档 5.7）。
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	attempts := 1
	if req.Function.IsRead() && !req.NoRetry {
		attempts += c.opts.ReadRetries
	}
	var err error
	for i := 0; i < attempts; i++ {
		var resp *Response
		resp, err = c.roundTrip(ctx, req)
		if err == nil || !(errors.Is(err, ErrTimeout) || errors.Is(err, ErrCRC) || errors.Is(err, ErrLRC)) || ctx.Err() != nil {
			return resp, err
		}
	}
	return nil, err
}

func (c *Client) emit(p Packet) {
	if c.opts.Observer != nil {
		if p.Time.IsZero() { // 发送行自带时间，晚到帧说明里的发送时间和它一致
			p.Time = time.Now()
		}
		p.ConnectionID = c.connectionID
		c.opts.Observer.OnPacket(p)
	}
}

func (c *Client) roundTrip(ctx context.Context, req Request) (*Response, error) {
	pdu, err := req.PDU()
	if err != nil {
		return nil, err
	}
	base := Packet{Slave: req.Slave, Function: req.Function, Address: req.Address, Count: uint16(req.Count())}
	var resp *Response
	_, err = c.exchange(ctx, base, pdu, req.Timeout, func(p []byte) error {
		var err error
		resp, err = ParseResponsePDU(req, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	if resp == nil { // RTU 广播
		resp = &Response{Function: req.Function}
	}
	return resp, nil
}

// DoRaw 发送任意 PDU（功能码 + 数据），返回第一帧通过 Transaction ID（TCP）或 Slave（RTU）校验、
// 且功能码与请求相同的响应 PDU，内容不做功能码级校验。用于调试 FC08、FC2B 等不常用功能码。
// 不重试；异常响应返回 *ExceptionError，同时返回异常 PDU。
func (c *Client) DoRaw(ctx context.Context, slave byte, pdu []byte) ([]byte, error) {
	if len(pdu) == 0 || len(pdu) > 253 {
		return nil, fmt.Errorf("%w：PDU 长度应为 1–253 字节", ErrInvalidRequest)
	}
	base := Packet{Slave: slave, Function: FunctionCode(pdu[0])}
	if len(pdu) >= 5 {
		base.Address = uint16(pdu[1])<<8 | uint16(pdu[2])
	}
	return c.exchange(ctx, base, pdu, 0, func(p []byte) error {
		switch {
		case p[0] == pdu[0]|0x80 && len(p) != 2:
			return fmt.Errorf("%w：异常响应应为 2 字节，收到 %d 字节", ErrMismatch, len(p))
		case p[0] == pdu[0]|0x80:
			return &ExceptionError{Function: FunctionCode(pdu[0]), Code: ExceptionCode(p[1])}
		case p[0] != pdu[0]:
			return fmt.Errorf("%w：功能码 %02X，期望 %02X", ErrMismatch, p[0], pdu[0])
		}
		return nil
	})
}

// exchange 发送一帧并等待匹配的响应，返回响应 PDU。parse 决定响应内容是否属于本次请求：
// 返回 *ExceptionError 表示异常响应（结束），其他错误表示对不上（记录后继续等待）。
func (c *Client) exchange(ctx context.Context, base Packet, pdu []byte, timeout time.Duration, parse func([]byte) error) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.txID++
	c.reqID++
	base.RequestID, base.Mode = c.reqID, c.opts.Mode
	if c.opts.Mode.mbap() {
		base.TxID = c.txID
	}
	result := func(raw []byte, st Status, err error) {
		p := base
		p.Dir, p.Raw, p.Status, p.Err = DirRX, raw, st, err
		c.emit(p)
	}

	c.r.ctx = ctx
	stop := context.AfterFunc(ctx, func() { _ = c.t.SetReadDeadline(time.Now()) })
	defer stop()

	// 发送前清掉上一次遗留的字节，避免被当成本次响应。它们在上一次读连接之后才到（请求读到自己的响应就结束，
	// 之后到的字节留在连接里）；上一次请求里已经读进缓冲的，说不清什么时候到的
	from := c.r.last
	if len(c.r.buf) > 0 {
		from = time.Time{}
	}
	if stale := c.r.drain(time.Millisecond, time.Now().Add(2*time.Millisecond)); len(stale) > 0 {
		c.late(stale, from, time.Now(), result)
	}

	adu := EncodeADU(c.opts.Mode, base.Slave, c.txID, pdu)
	start := time.Now()
	if _, err := c.t.Write(adu); err != nil {
		p := base
		p.Dir, p.Raw, p.Status, p.Err = DirTX, adu, StatusConnectionError, err
		c.emit(p)
		return nil, fmt.Errorf("%w：%v", ErrConnection, err)
	}
	tx := base
	tx.Dir, tx.Raw, tx.Status, tx.Time = DirTX, adu, StatusSent, time.Now()
	c.emit(tx)

	// RTU 广播（Slave 0）没有响应，等待转向延迟即可
	if base.Slave == 0 && !c.opts.Mode.mbap() {
		time.Sleep(c.opts.Guard)
		return nil, nil
	}

	if timeout <= 0 {
		timeout = c.Timeout()
	}
	deadline := start.Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.sent = append(c.sent[max(0, len(c.sent)-maxSent+1):], sentReq{tx: tx, start: start, timeout: deadline.Sub(start)})
	end := func(st Status) { c.sent[len(c.sent)-1].ended = st }
	if c.opts.DiscardEcho && !c.opts.Mode.mbap() {
		c.r.skipEcho(adu, deadline)
	}

	// 事务号对上、内容却对不上的帧数（仅 Modbus TCP）。超时时据此说明设备其实回过话，不是完全没有应答
	rejected := 0
	for {
		f, err := c.r.readFrame(c.opts.Mode, deadline, c.opts.CharGap)
		switch {
		case err == nil:
		case errors.Is(err, ErrCRC), errors.Is(err, ErrLRC):
			result(f.Raw, StatusCRCError, err)
			return nil, err
		case errors.Is(err, ErrFraming):
			result(f.Raw, StatusParseError, err)
			continue
		case ctx.Err() != nil:
			result(nil, StatusCancelled, ctx.Err())
			end(StatusCancelled)
			return nil, ctx.Err()
		case isTimeout(err):
			err = ErrTimeout
			if rejected > 0 {
				err = mismatchTimeout(rejected)
			}
			result(nil, StatusTimeout, err)
			end(StatusTimeout)
			c.afterTimeout(result)
			return nil, err
		default:
			result(nil, StatusConnectionError, err)
			return nil, fmt.Errorf("%w：%v", ErrConnection, err)
		}

		// Modbus TCP 靠 Transaction ID 关联；Unit ID 不少设备不按规范回显，不作为判断依据
		if c.opts.Mode.mbap() && f.TxID != c.txID {
			now := time.Now()
			result(f.Raw, StatusLate, c.lateError(f.Raw, now, now))
			continue
		}
		if !c.opts.Mode.mbap() && f.Slave != base.Slave {
			result(f.Raw, StatusUnexpected, nil)
			continue
		}
		if c.opts.Mode.mbap() {
			rejected++ // 下面结束等待的分支都会返回，留下的只有对不上的帧
		}
		if len(f.PDU) == 0 {
			result(f.Raw, StatusParseError, ErrMalformed)
			continue
		}
		err = parse(f.PDU)
		if ex, ok := AsException(err); ok {
			p := base
			p.Dir, p.Raw, p.Status, p.Err, p.RTT = DirRX, f.Raw, StatusException, ex, time.Since(start)
			c.emit(p)
			return f.PDU, ex
		}
		if ns, ok := AsNonstandard(err); ok {
			// 事务号、Unit ID 都对上，才能确定这帧回的就是本次请求，结束等待；否则只记录
			if c.opts.Mode.mbap() && f.Slave == base.Slave {
				p := base
				p.Dir, p.Raw, p.Status, p.Err, p.RTT = DirRX, f.Raw, StatusNonstandard, ns, time.Since(start)
				c.emit(p)
				return f.PDU, ns
			}
			why := "RTU / ASCII 没有事务号"
			if c.opts.Mode.mbap() {
				why = fmt.Sprintf("Unit ID %d 与请求的 %d 不同", f.Slave, base.Slave)
			}
			err = fmt.Errorf("%w：功能码 %02X 回显了请求的地址和数量，但%s，不能确定属于本次请求", ErrMismatch, f.PDU[0], why)
		}
		if err != nil {
			result(f.Raw, StatusUnexpected, err)
			continue
		}
		p := base
		p.Dir, p.Raw, p.Status, p.RTT = DirRX, f.Raw, StatusSuccess, time.Since(start)
		c.emit(p)
		return f.PDU, nil
	}
}

// mismatchTimeout 是超时前收到过事务号相同、内容对不上的帧：errors.Is 同时认 ErrTimeout 和 ErrMismatch。
type mismatchTimeout int

func (n mismatchTimeout) Error() string {
	return fmt.Sprintf("modbus: 响应超时：期间收到 %d 帧事务号相同、但内容与请求对不上的响应，已丢弃", int(n))
}

func (mismatchTimeout) Is(target error) bool { return target == ErrTimeout || target == ErrMismatch }

// afterTimeout 在超时后等待保护间隔并清空接收缓冲。保护间隔内收到的字节记为晚到响应，
// 绝不当作下一条请求的响应（设计文档 5.5）。
// 超时前已经收到半帧的，说不清什么时候到的，只说明回的是哪条请求。
func (c *Client) afterTimeout(result func([]byte, Status, error)) {
	until := time.Now().Add(c.opts.Guard)
	partial := len(c.r.buf) > 0
	if c.r.fill(1, until) == nil {
		at := time.Now()
		if partial {
			at = time.Time{}
		}
		c.late(c.r.drain(c.opts.Guard, until), at, at, result)
	}
	time.Sleep(time.Until(until))
}
