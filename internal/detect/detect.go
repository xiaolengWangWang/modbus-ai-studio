// Package detect 实现协议自动识别：只知道 IP 和端口时，判断设备是 Modbus TCP、RTU over TCP 还是 ASCII over TCP（设计文档 5.8）。
package detect

import (
	"context"
	"errors"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Dialer 建立一条新连接。每次探测都重新连接，避免上一次探测的残留字节让网关数据流错位。
type Dialer func(ctx context.Context) (modbus.Transport, error)

// Options 是探测参数。
type Options struct {
	Slaves   []byte              // 默认依次尝试 1、255；不用 0，广播没有响应
	Address  uint16              // 探测地址，默认 0
	Function modbus.FunctionCode // 只允许 FC03 / FC04，默认 FC03
	Timeout  time.Duration       // 每次探测的超时，默认 1000 ms
	Observer modbus.Observer     // 探测报文同样要记录，operation 记为 DETECT
}

// Attempt 是一次探测的记录，未识别时用来向用户解释看到了什么。
type Attempt struct {
	Mode  modbus.Mode
	Slave byte
	Err   error // nil 表示收到正常响应
}

// Result 是识别结果。
type Result struct {
	Mode        modbus.Mode // 识别出的协议
	Slave       byte
	ByException bool // 靠异常响应确认（异常响应同样说明协议是对的）
	Attempts    []Attempt
}

// ErrUnknown 表示两种格式都没有得到可解析的响应。
var ErrUnknown = errors.New("detect: 未识别出协议")

// Detect 依次发 MBAP、RTU、ASCII 格式的请求，每次都重新连接，逐个尝试 Slave ID。
func Detect(ctx context.Context, dial Dialer, opts Options) (Result, error) {
	if len(opts.Slaves) == 0 {
		opts.Slaves = []byte{1, 255}
	}
	if opts.Function == 0 {
		opts.Function = modbus.FuncReadHoldingRegisters
	}
	if opts.Function != modbus.FuncReadHoldingRegisters && opts.Function != modbus.FuncReadInputRegisters {
		return Result{}, errors.New("detect: 探测只允许使用 FC03 或 FC04")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Second
	}
	var res Result
	for _, slave := range opts.Slaves {
		for _, mode := range []modbus.Mode{modbus.ModeTCP, modbus.ModeRTUOverTCP, modbus.ModeASCIIOverTCP} {
			err := probe(ctx, dial, mode, slave, opts)
			res.Attempts = append(res.Attempts, Attempt{Mode: mode, Slave: slave, Err: err})
			_, isExc := modbus.AsException(err)
			if err == nil || isExc {
				res.Mode, res.Slave, res.ByException = mode, slave, isExc
				return res, nil
			}
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
		}
	}
	return res, ErrUnknown
}

func probe(ctx context.Context, dial Dialer, mode modbus.Mode, slave byte, opts Options) error {
	t, err := dial(ctx)
	if err != nil {
		return err
	}
	defer t.Close()
	c := modbus.NewClient(t, modbus.Options{Mode: mode, Timeout: opts.Timeout, Guard: time.Millisecond, Observer: opts.Observer})
	_, err = c.Do(ctx, modbus.Request{Slave: slave, Function: opts.Function, Address: opts.Address, Quantity: 1})
	return err
}
