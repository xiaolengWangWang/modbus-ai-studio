// Package slavescan 搜索总线上在应答的站号（Slave ID），桌面版、Web 版和 modbus-cli 共用。
//
// 每个站号依次试几次读取，有一次应答就算找到：先读用户给的地址（设备上肯定有的点），再读 40001（FC03）、
// 30001（FC04）、线圈 1（FC01）。有的设备收到不存在的地址时不回异常、直接不应答，只读 40001 会漏掉它。
// 正常响应、异常响应、89 回显都说明这个站号有设备；网关替不在线的设备回的异常 0A、0B 不算。
package slavescan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Client 发一个请求、等它的响应。*modbus.Client 和检测用的连接都可以。
type Client interface {
	Do(ctx context.Context, req modbus.Request) (*modbus.Response, error)
}

// Probe 是试探站号用的一次读取：读 Address 起 1 个寄存器或线圈。
type Probe struct {
	Function modbus.FunctionCode
	Address  uint16 // 协议地址，0 起
}

func (p Probe) String() string {
	ref := modbus.Reference(modbus.AreaOf(p.Function), p.Address)
	if p.Function == modbus.FuncReadCoils {
		ref = "线圈 " + ref // 线圈的写法就是 1 起的编号，单写数字看不出是线圈
	}
	return fmt.Sprintf("%s（FC%02d）", ref, byte(p.Function))
}

// defaults 是每个站号都要试的读取。
var defaults = []Probe{
	{modbus.FuncReadHoldingRegisters, 0},
	{modbus.FuncReadInputRegisters, 0},
	{modbus.FuncReadCoils, 0},
}

// Probes 是每个站号依次试的读取：extra（用户给的地址）在前，再接 40001、30001、线圈 1，去掉重复的。
func Probes(extra ...Probe) []Probe {
	var out []Probe
	for _, p := range append(extra, defaults...) {
		dup := false
		for _, q := range out {
			dup = dup || q == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// ParseProbe 把“40101”“30005”“10003”这类地址写法换成试探用的读取，按数据区选功能码；
// 原始 Offset（如 346、0x0064）按保持寄存器（FC03）读。
func ParseProbe(s string) (Probe, error) {
	cands, err := modbus.ParseAddress(s)
	if err != nil {
		return Probe{}, err
	}
	c := cands[0]
	return Probe{c.Area.ReadFunction(), c.Offset}, nil
}

// Hit 是找到的一个站号。
type Hit struct {
	ID    int
	Probe Probe         // 哪次读取得到的应答
	Err   error         // nil 是正常响应；否则是异常响应或 89 回显，同样说明设备在线
	RTT   time.Duration // 这次读取从发出到收到应答
}

// Note 说明这个站号是怎么应答的。
func (h Hit) Note() string {
	if e, ok := modbus.AsException(h.Err); ok {
		return fmt.Sprintf("读 %s 回异常 %s：设备在线，只是这个地址不可读", h.Probe, e.Code.Description())
	}
	if h.Err != nil {
		return fmt.Sprintf("读 %s 收到 89 回显（非标准响应）：设备在线", h.Probe)
	}
	return fmt.Sprintf("读 %s 正常响应", h.Probe)
}

// answered 判断一次读取有没有得到这个站号上设备的应答。
func answered(err error) bool {
	if err == nil {
		return true
	}
	if e, ok := modbus.AsException(err); ok {
		// 网关替后面不在线的设备回的，不说明有设备
		return e.Code != modbus.ExceptionGatewayPathUnavailable && e.Code != modbus.ExceptionGatewayTargetFailed
	}
	_, echo := modbus.AsNonstandard(err)
	return echo
}

// Scan 逐个搜索 from–to 的站号，每个站号依次试 probes，每次读取最多等 timeout。每搜完一个站号调用一次 progress。
// 连接断开时停下，返回已找到的站号和错误；ctx 取消时返回已找到的站号。
func Scan(ctx context.Context, c Client, from, to int, timeout time.Duration, probes []Probe, progress func(id int, hits []Hit)) ([]Hit, error) {
	if len(probes) == 0 {
		probes = Probes()
	}
	var hits []Hit
	for id := from; id <= to && ctx.Err() == nil; id++ {
		for _, p := range probes {
			start := time.Now()
			_, err := c.Do(ctx, modbus.Request{Slave: byte(id), Function: p.Function, Address: p.Address, Quantity: 1, Timeout: timeout, NoRetry: true})
			if ctx.Err() != nil {
				return hits, nil
			}
			if answered(err) {
				hits = append(hits, Hit{ID: id, Probe: p, Err: err, RTT: time.Since(start)})
				break
			}
			if errors.Is(err, modbus.ErrConnection) {
				return hits, err
			}
		}
		if progress != nil {
			progress(id, hits)
		}
	}
	return hits, nil
}

// Validate 检查搜索范围和每次读取的等待时间，错误说明给用户看。
func Validate(from, to int, timeout time.Duration) error {
	switch {
	case from < 1 || to > 255 || from > to:
		return errors.New("站号范围应在 1–255 之间，起始不大于结束")
	case timeout < 20*time.Millisecond || timeout > 10*time.Second:
		return errors.New("每次读取的等待时间应为 20–10000 ms")
	}
	return nil
}

// Worst 是最坏情况下（都不应答）要多久，开始前告诉用户。
func Worst(from, to int, timeout time.Duration, probes int) time.Duration {
	return time.Duration(to-from+1) * time.Duration(probes) * timeout
}
