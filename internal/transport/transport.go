// Package transport 提供 TCP 和串口两种底层连接。Modbus 编解码不关心具体是哪一种。
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// DialTCP 建立 TCP 连接，Modbus TCP 与 RTU over TCP 共用。
func DialTCP(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", address)
}

// SerialConfig 是串口参数。
type SerialConfig struct {
	Port     string // Windows COM3、Linux /dev/ttyUSB0、macOS /dev/cu.usbserial-xxx
	BaudRate int
	DataBits int    // 默认 8
	Parity   string // N、E、O，默认 N
	StopBits int    // 1 或 2，默认 1
}

// Serial 把 go.bug.st/serial 的“读超时”适配成 modbus.Transport 需要的“读截止时间”。
type Serial struct {
	port     serial.Port
	mu       sync.Mutex
	deadline time.Time
}

// OpenSerial 打开串口。
func OpenSerial(cfg SerialConfig) (*Serial, error) {
	mode := &serial.Mode{BaudRate: cfg.BaudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}
	if mode.BaudRate == 0 {
		mode.BaudRate = 9600
	}
	if cfg.DataBits != 0 {
		mode.DataBits = cfg.DataBits
	}
	switch strings.ToUpper(cfg.Parity) {
	case "", "N":
	case "E":
		mode.Parity = serial.EvenParity
	case "O":
		mode.Parity = serial.OddParity
	default:
		return nil, fmt.Errorf("transport: 不支持的校验位 %q", cfg.Parity)
	}
	switch cfg.StopBits {
	case 0, 1:
	case 2:
		mode.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("transport: 不支持的停止位 %d", cfg.StopBits)
	}
	p, err := serial.Open(cfg.Port, mode)
	if err != nil {
		return nil, explainSerialError(cfg.Port, err)
	}
	return &Serial{port: p}, nil
}

// Linux 用户不在 dialout 组时打开失败，直接给出处理办法（设计文档 3.2）。
func explainSerialError(port string, err error) error {
	var pe *serial.PortError
	if runtime.GOOS == "linux" && errors.As(err, &pe) && pe.Code() == serial.PermissionDenied {
		return fmt.Errorf("transport: 没有权限打开 %s，执行 sudo usermod -aG dialout $USER 后重新登录：%w", port, err)
	}
	return fmt.Errorf("transport: 打开 %s 失败：%w", port, err)
}

// SetReadDeadline 设置读截止时间；零值表示不超时。
func (s *Serial) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return nil
}

func (s *Serial) Read(p []byte) (int, error) {
	s.mu.Lock()
	deadline := s.deadline
	s.mu.Unlock()
	timeout := serial.NoTimeout
	if !deadline.IsZero() {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
	}
	if err := s.port.SetReadTimeout(timeout); err != nil {
		return 0, err
	}
	n, err := s.port.Read(p)
	if n == 0 && err == nil {
		return 0, os.ErrDeadlineExceeded
	}
	return n, err
}

func (s *Serial) Write(p []byte) (int, error) { return s.port.Write(p) }

// Close 关闭串口。
func (s *Serial) Close() error { return s.port.Close() }

// ListSerialPorts 列出本机串口，名称按平台原样返回。
func ListSerialPorts() ([]string, error) { return serial.GetPortsList() }
