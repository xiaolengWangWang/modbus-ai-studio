package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// 从读写错误判断断开方式：EOF 是 FIN，复位是 RST，重传超时、网络不可达是网络问题。Windows 的 WSA 错误码单独判断。
func TestCloseKindOf(t *testing.T) {
	op := func(err error) error {
		return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", err)}
	}
	cases := []struct {
		err  error
		want CloseKind
	}{
		{nil, CloseUnknown},
		{errors.New("其他错误"), CloseUnknown},
		{io.EOF, CloseFIN},
		{fmt.Errorf("读响应：%w", io.ErrUnexpectedEOF), CloseFIN},
		{op(syscall.ECONNRESET), CloseRST},
		{op(syscall.ECONNABORTED), CloseAborted},
		{op(syscall.ETIMEDOUT), CloseAborted},
		{op(syscall.ENETUNREACH), CloseUnreachable},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, []struct {
			err  error
			want CloseKind
		}{
			{op(syscall.Errno(10054)), CloseRST},
			{op(syscall.Errno(10053)), CloseAborted},
			{op(syscall.Errno(10060)), CloseAborted},
			{op(syscall.Errno(10051)), CloseUnreachable},
		}...)
	}
	for _, c := range cases {
		if got := CloseKindOf(c.err); got != c.want {
			t.Errorf("%v：得到 %d，期望 %d", c.err, got, c.want)
		}
	}
}

// 连接被拒绝要给出原因：Windows 上的错误码是 WSAECONNREFUSED（10061），和 syscall.ECONNREFUSED 不相等。
func TestDialErrTextConnRefused(t *testing.T) {
	refused := syscall.ECONNREFUSED
	if runtime.GOOS == "windows" {
		refused = syscall.Errno(10061)
	}
	err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", refused)}
	if got := DialErrText(err); !strings.Contains(got, "连接被拒绝") {
		t.Errorf("连接被拒绝没有识别出来：%s", got)
	}
}
