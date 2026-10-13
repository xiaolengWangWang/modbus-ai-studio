package diagnose

import (
	"errors"
	"fmt"

	"modbus-ai-studio/internal/modbus"
)

// errSummary 是错误的短说明，用在识别结果、探测结果等列表里。
func Summary(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return "异常 " + ex.Code.Description()
	}
	if ns, ok := modbus.AsNonstandard(err); ok {
		return fmt.Sprintf("非标准响应（功能码 %02X，回显请求）", ns.PDU[0])
	}
	switch {
	case errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch):
		return "超时（收到过内容不符的响应）"
	case errors.Is(err, modbus.ErrTimeout):
		return "超时"
	case errors.Is(err, modbus.ErrCRC):
		return "CRC 错误"
	case errors.Is(err, modbus.ErrConnection):
		return "连接错误"
	}
	return err.Error()
}

// Kind 是错误的类别代码，写进检测明细（JSONL）和给 AI 的证据：EXCEPTION_02、TIMEOUT、CRC_ERROR 等。
func Kind(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("EXCEPTION_%02X", byte(ex.Code))
	}
	if _, ok := modbus.AsNonstandard(err); ok {
		return "NONSTANDARD_REQUEST_ECHO"
	}
	if errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch) {
		return "TIMEOUT_AFTER_MISMATCHED_FRAME"
	}
	for _, v := range []struct {
		err  error
		kind string
	}{{modbus.ErrTimeout, "TIMEOUT"}, {modbus.ErrCRC, "CRC_ERROR"}, {modbus.ErrLRC, "LRC_ERROR"}, {modbus.ErrConnection, "CONNECTION_ERROR"}} {
		if errors.Is(err, v.err) {
			return v.kind
		}
	}
	return "OTHER_ERROR"
}
