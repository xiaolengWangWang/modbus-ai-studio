package ui

import (
	"fmt"
	"net"

	"modbus-ai-studio/internal/modbus"
)

// connAddrs 是 TCP 连接两端的地址，例如“本机 192.168.1.5:51028 → 192.168.1.10:502”，可以和设备侧、
// 防火墙的日志或抓包对上；串口返回空。
func connAddrs(t modbus.Transport) string {
	c, ok := t.(net.Conn)
	if !ok {
		return ""
	}
	return fmt.Sprintf("本机 %s → %s", c.LocalAddr(), c.RemoteAddr())
}
