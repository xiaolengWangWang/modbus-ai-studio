// Package diagnose 分析读取出错的原因并给出处理建议，桌面版、Web 版和 modbus-cli 共用一套规则。
// 规则都是确定性的：只看错误类型、连接方式和本次连接里观测到的通信证据，不靠猜测。
package diagnose

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// Evidence 是一次连接期间积累的通信证据。Observe 在收发所在的 goroutine 里调用，其他方法可以在任何 goroutine 读。
type Evidence struct {
	okAt    atomic.Int64 // 最近一次正常响应（UnixNano），0 表示本次连接还没有
	late    atomic.Int64 // 晚到响应次数，不含别的主站的响应
	foreign atomic.Int64 // 别的主站的响应（Modbus TCP 事务号不是本连接发出的）

	mu        sync.Mutex
	lateDelay map[key]time.Duration // 每种请求观测到的最大应答延迟，来自找得到是哪条超时请求的晚到响应
}

// key 标识一种请求：站号、功能码、起始地址。
type key struct {
	slave byte
	fn    modbus.FunctionCode
	addr  uint16
}

// Reset 在重新连接时清空证据。
func (e *Evidence) Reset() {
	for _, v := range []*atomic.Int64{&e.okAt, &e.late, &e.foreign} {
		v.Store(0)
	}
	e.mu.Lock()
	e.lateDelay = nil
	e.mu.Unlock()
}

// Observe 记下一条收发记录。
func (e *Evidence) Observe(p modbus.Packet) {
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
			k := key{r.Slave, r.Function, r.Address}
			e.mu.Lock()
			if e.lateDelay == nil {
				e.lateDelay = map[key]time.Duration{}
			}
			e.lateDelay[k] = max(e.lateDelay[k], le.Elapsed)
			e.mu.Unlock()
		}
	}
}

// DelayOf 是这种请求观测到的最大应答延迟，没有晚到响应时为 0。
func (e *Evidence) DelayOf(slave byte, fn modbus.FunctionCode, addr uint16) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lateDelay[key{slave, fn, addr}]
}

// Late 是晚到响应的次数，不含别的主站的响应。
func (e *Evidence) Late() int64 { return e.late.Load() }

// Foreign 是别的主站的响应的帧数。
func (e *Evidence) Foreign() int64 { return e.foreign.Load() }

// OKSeen 表示本次连接上有过正常响应（含异常响应：协议和站号是对的）。
func (e *Evidence) OKSeen() bool { return e.okAt.Load() != 0 }
