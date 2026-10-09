package ui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"syscall"

	"modbus-ai-studio/internal/modbus"
)

// closeKind 是 TCP 连接断开的方式。从读写错误判断，相当于抓包看到的 FIN、RST 或重传超时，
// 分得清是设备主动断开、被强制复位，还是网络中断。
type closeKind int

const (
	closeUnknown     closeKind = iota
	closeFIN                   // 读到 EOF：设备正常关闭了连接
	closeRST                   // 连接被复位
	closeAborted               // 本机重传一直没有确认，中止了连接
	closeUnreachable           // 网络不可达
)

// closeKindOf 按读写错误判断连接是怎么断的。Windows 的错误码（WSAECONNRESET 10054 等）和 syscall 里的
// 同名常量不相等，单独判断。
func closeKindOf(err error) closeKind {
	var errno syscall.Errno
	switch {
	case err == nil:
		return closeUnknown
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return closeFIN
	case errors.Is(err, syscall.ECONNRESET):
		return closeRST
	case errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.ETIMEDOUT):
		return closeAborted
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return closeUnreachable
	case runtime.GOOS == "windows" && errors.As(err, &errno):
		switch errno {
		case 10054: // WSAECONNRESET
			return closeRST
		case 10053, 10060: // WSAECONNABORTED、WSAETIMEDOUT
			return closeAborted
		case 10050, 10051, 10065: // WSAENETDOWN、WSAENETUNREACH、WSAEHOSTUNREACH
			return closeUnreachable
		}
	}
	return closeUnknown
}

// label 是写在通信报文和日志里的一句话；判断不出时为空。
func (k closeKind) label() string {
	return [...]string{"", "设备关闭了连接（收到 FIN）", "连接被复位（收到 RST）", "本机重传没有回应，中止了连接", "网络不可达"}[k]
}

// explain 说明这种断开方式意味着什么。
func (k closeKind) explain() string {
	switch k {
	case closeFIN:
		return "FIN 是正常关闭：设备（或串口服务器、网关）主动断开了这条连接，常见原因是空闲超时、连接数限制、不接受这条请求，或设备正常重启。"
	case closeRST:
		return "RST 是强制复位：设备的 Modbus 服务重启或出错、端口被关掉，或中间的防火墙、网关、NAT 清掉了这条连接。"
	case closeAborted:
		return "本机发出的数据一直没有收到确认，TCP 重传超时后中止了连接：网线、交换机、无线网桥中断，或设备掉电，不是设备主动断开。"
	case closeUnreachable:
		return "本机到设备的网络不可达：网卡断开、网线拔了，或 IP 地址、路由改了。"
	}
	return ""
}

// connAddrs 是 TCP 连接两端的地址，例如“本机 192.168.1.5:51028 → 192.168.1.10:502”，可以和设备侧、
// 防火墙的日志或抓包对上；串口返回空。
func connAddrs(t modbus.Transport) string {
	c, ok := t.(net.Conn)
	if !ok {
		return ""
	}
	return fmt.Sprintf("本机 %s → %s", c.LocalAddr(), c.RemoteAddr())
}
