package transport

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
)

// CloseKind 是 TCP 连接断开的方式。从读写错误判断，相当于抓包看到的 FIN、RST 或重传超时，
// 分得清是设备主动断开、被强制复位，还是网络中断。桌面版和 Web 版共用。
type CloseKind int

const (
	CloseUnknown     CloseKind = iota
	CloseFIN                   // 读到 EOF：设备正常关闭了连接
	CloseRST                   // 连接被复位
	CloseAborted               // 本机重传一直没有确认，中止了连接
	CloseUnreachable           // 网络不可达
)

// CloseKindOf 按读写错误判断连接是怎么断的。Windows 的错误码（WSAECONNRESET 10054 等）和 syscall 里的
// 同名常量不相等，单独判断。
func CloseKindOf(err error) CloseKind {
	var errno syscall.Errno
	switch {
	case err == nil:
		return CloseUnknown
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return CloseFIN
	case errors.Is(err, syscall.ECONNRESET):
		return CloseRST
	case errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.ETIMEDOUT):
		return CloseAborted
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return CloseUnreachable
	case runtime.GOOS == "windows" && errors.As(err, &errno):
		switch errno {
		case 10054: // WSAECONNRESET
			return CloseRST
		case 10053, 10060: // WSAECONNABORTED、WSAETIMEDOUT
			return CloseAborted
		case 10050, 10051, 10065: // WSAENETDOWN、WSAENETUNREACH、WSAEHOSTUNREACH
			return CloseUnreachable
		}
	}
	return CloseUnknown
}

// Label 是写在通信报文和日志里的一句话；判断不出时为空。
func (k CloseKind) Label() string {
	return [...]string{"", "设备关闭了连接（收到 FIN）", "连接被复位（收到 RST）", "本机重传没有回应，中止了连接", "网络不可达"}[k]
}

// Explain 说明这种断开方式意味着什么。
func (k CloseKind) Explain() string {
	switch k {
	case CloseFIN:
		return "FIN 是正常关闭：设备（或串口服务器、网关）主动断开了这条连接，常见原因是空闲超时、连接数限制、不接受这条请求，或设备正常重启。"
	case CloseRST:
		return "RST 是强制复位：设备的 Modbus 服务重启或出错、端口被关掉，或中间的防火墙、网关、NAT 清掉了这条连接。"
	case CloseAborted:
		return "本机发出的数据一直没有收到确认，TCP 重传超时后中止了连接：网线、交换机、无线网桥中断，或设备掉电，不是设备主动断开。"
	case CloseUnreachable:
		return "本机到设备的网络不可达：网卡断开、网线拔了，或 IP 地址、路由改了。"
	}
	return ""
}

// DialErrText 把建立 TCP 连接失败说成现场能处理的话：连接被拒绝、超时分别说明；其他错误原样返回。
func DialErrText(err error) string {
	switch {
	case isConnRefused(err):
		return "连接被拒绝：设备的 Modbus 服务没开，或端口不对"
	case errors.Is(err, os.ErrDeadlineExceeded), isNetTimeout(err):
		return "连接超时：设备不在线或网络不通"
	}
	return err.Error()
}

// isConnRefused 判断连接被拒绝。Windows 上的错误码是 WSAECONNREFUSED（10061），和 syscall.ECONNREFUSED 不相等。
func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &errno) && errno == 10061
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
