package modbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
)

// Packet 是一条收发记录。同一请求的 TX 和结果行共用 RequestID；
// RX 行的地址和数量取自配对的请求，因为 FC03 等响应本身不含地址。
type Packet struct {
	Time      time.Time
	RequestID uint64
	Dir       Direction
	Mode      Mode
	Slave     byte
	TxID      uint16 // 仅 Modbus TCP
	Function  FunctionCode
	Address   uint16
	Count     uint16
	Raw       []byte // 超时等没有收到字节的结果行为空
	Status    Status
	RTT       time.Duration // 仅结果行
	Err       error
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
	mu    sync.Mutex
	t     Transport
	r     frameReader
	opts  Options
	txID  uint16
	reqID uint64
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
		if opts.Mode == ModeRTU {
			opts.Guard = 50 * time.Millisecond
		}
	}
	if opts.CharGap <= 0 {
		opts.CharGap = 20 * time.Millisecond
	}
	return &Client{t: t, r: frameReader{t: t}, opts: opts}
}

// Mode 返回连接模式。
func (c *Client) Mode() Mode { return c.opts.Mode }

// Close 关闭底层连接。
func (c *Client) Close() error { return c.t.Close() }

// Do 发送请求并等待匹配的响应。读请求在超时或 CRC 错误后按 ReadRetries 重试；
// 写请求不重试，因为超时不代表设备没有执行（设计文档 5.7）。
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	attempts := 1
	if req.Function.IsRead() {
		attempts += c.opts.ReadRetries
	}
	var err error
	for i := 0; i < attempts; i++ {
		var resp *Response
		resp, err = c.roundTrip(ctx, req)
		if err == nil || !(errors.Is(err, ErrTimeout) || errors.Is(err, ErrCRC)) || ctx.Err() != nil {
			return resp, err
		}
	}
	return nil, err
}

func (c *Client) emit(p Packet) {
	if c.opts.Observer != nil {
		p.Time = time.Now()
		c.opts.Observer.OnPacket(p)
	}
}

func (c *Client) roundTrip(ctx context.Context, req Request) (*Response, error) {
	pdu, err := req.PDU()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.txID++
	c.reqID++
	base := Packet{RequestID: c.reqID, Mode: c.opts.Mode, Slave: req.Slave, Function: req.Function,
		Address: req.Address, Count: uint16(req.Count())}
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

	// 发送前清掉上一次遗留的字节，避免被当成本次响应
	if stale := c.r.drain(time.Millisecond, time.Now().Add(2*time.Millisecond)); len(stale) > 0 {
		result(stale, StatusLate, nil)
	}

	adu := EncodeADU(c.opts.Mode, req.Slave, c.txID, pdu)
	start := time.Now()
	if _, err := c.t.Write(adu); err != nil {
		p := base
		p.Dir, p.Raw, p.Status, p.Err = DirTX, adu, StatusConnectionError, err
		c.emit(p)
		return nil, fmt.Errorf("%w：%v", ErrConnection, err)
	}
	tx := base
	tx.Dir, tx.Raw, tx.Status = DirTX, adu, StatusSent
	c.emit(tx)

	// RTU 广播（Slave 0）没有响应，等待转向延迟即可
	if req.Slave == 0 && !c.opts.Mode.mbap() {
		time.Sleep(c.opts.Guard)
		return &Response{Function: req.Function}, nil
	}

	deadline := start.Add(c.opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if c.opts.DiscardEcho && !c.opts.Mode.mbap() {
		c.r.skipEcho(adu, deadline)
	}

	for {
		f, err := c.r.readFrame(c.opts.Mode, deadline, c.opts.CharGap)
		switch {
		case err == nil:
		case errors.Is(err, ErrCRC):
			result(f.Raw, StatusCRCError, err)
			return nil, ErrCRC
		case errors.Is(err, ErrFraming):
			result(f.Raw, StatusParseError, err)
			continue
		case ctx.Err() != nil:
			result(nil, StatusCancelled, ctx.Err())
			return nil, ctx.Err()
		case isTimeout(err):
			result(nil, StatusTimeout, ErrTimeout)
			c.afterTimeout(result)
			return nil, ErrTimeout
		default:
			result(nil, StatusConnectionError, err)
			return nil, fmt.Errorf("%w：%v", ErrConnection, err)
		}

		// Modbus TCP 靠 Transaction ID 关联；Unit ID 不少设备不按规范回显，不作为判断依据
		if c.opts.Mode.mbap() && f.TxID != c.txID {
			result(f.Raw, StatusLate, nil)
			continue
		}
		if !c.opts.Mode.mbap() && f.Slave != req.Slave {
			result(f.Raw, StatusUnexpected, nil)
			continue
		}
		resp, err := ParseResponsePDU(req, f.PDU)
		if ex, ok := AsException(err); ok {
			p := base
			p.Dir, p.Raw, p.Status, p.Err, p.RTT = DirRX, f.Raw, StatusException, ex, time.Since(start)
			c.emit(p)
			return nil, ex
		}
		if err != nil {
			result(f.Raw, StatusUnexpected, err)
			continue
		}
		p := base
		p.Dir, p.Raw, p.Status, p.RTT = DirRX, f.Raw, StatusSuccess, time.Since(start)
		c.emit(p)
		return resp, nil
	}
}

// afterTimeout 在超时后等待保护间隔并清空接收缓冲。保护间隔内收到的字节记为晚到响应，
// 绝不当作下一条请求的响应（设计文档 5.5）。
func (c *Client) afterTimeout(result func([]byte, Status, error)) {
	until := time.Now().Add(c.opts.Guard)
	if late := c.r.drain(c.opts.Guard, until); len(late) > 0 {
		result(late, StatusLate, nil)
	}
	time.Sleep(time.Until(until))
}
