package modbus

import (
	"errors"
	"fmt"
)

// ExceptionCode 是从站返回的异常码。
type ExceptionCode byte

const (
	ExceptionIllegalFunction        ExceptionCode = 0x01
	ExceptionIllegalDataAddress     ExceptionCode = 0x02
	ExceptionIllegalDataValue       ExceptionCode = 0x03
	ExceptionSlaveDeviceFailure     ExceptionCode = 0x04
	ExceptionAcknowledge            ExceptionCode = 0x05
	ExceptionSlaveDeviceBusy        ExceptionCode = 0x06
	ExceptionGatewayPathUnavailable ExceptionCode = 0x0A
	ExceptionGatewayTargetFailed    ExceptionCode = 0x0B
)

// 名称与排查建议见设计文档 5.10。
var exceptionInfo = map[ExceptionCode]struct{ name, tip string }{
	ExceptionIllegalFunction:        {"Illegal Function", "设备不支持该功能码，改用 FC03/FC04 或 FC06/FC16"},
	ExceptionIllegalDataAddress:     {"Illegal Data Address", "地址越界或 ±1 偏移；读取范围跨过了设备未定义的区段"},
	ExceptionIllegalDataValue:       {"Illegal Data Value", "数量越限，或写入值超出设备允许范围"},
	ExceptionSlaveDeviceFailure:     {"Slave Device Failure", "设备内部错误，检查设备状态"},
	ExceptionAcknowledge:            {"Acknowledge", "设备已接受请求但需要较长处理时间，稍后再读"},
	ExceptionSlaveDeviceBusy:        {"Slave Device Busy", "设备忙，降低轮询频率后重试"},
	ExceptionGatewayPathUnavailable: {"Gateway Path Unavailable", "网关未配置到该 Slave 的路由"},
	ExceptionGatewayTargetFailed:    {"Gateway Target Device Failed to Respond", "网关到串口设备这段不通：检查 Slave ID、波特率、485 接线"},
}

// Name 返回异常码的规范名称。
func (c ExceptionCode) Name() string {
	if info, ok := exceptionInfo[c]; ok {
		return info.name
	}
	return "Unknown Exception"
}

// Tip 返回排查建议。
func (c ExceptionCode) Tip() string {
	if info, ok := exceptionInfo[c]; ok {
		return info.tip
	}
	return "查阅设备手册中该异常码的含义"
}

// ExceptionError 表示从站返回了异常响应。
// 异常响应也说明协议和 Slave ID 是对的，只是请求内容被拒绝。
type ExceptionError struct {
	Function FunctionCode
	Code     ExceptionCode
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus: 异常 %02X %s", byte(e.Code), e.Code.Name())
}

// AsException 判断 err 是否为从站异常。
func AsException(err error) (*ExceptionError, bool) {
	var e *ExceptionError
	ok := errors.As(err, &e)
	return e, ok
}

// ExceptionPDU 生成异常响应 PDU（模拟器使用）。
func ExceptionPDU(f FunctionCode, code ExceptionCode) []byte {
	return []byte{byte(f) | 0x80, byte(code)}
}
