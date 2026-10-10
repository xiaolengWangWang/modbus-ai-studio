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
	ExceptionMemoryParityError      ExceptionCode = 0x08
	ExceptionGatewayPathUnavailable ExceptionCode = 0x0A
	ExceptionGatewayTargetFailed    ExceptionCode = 0x0B
)

// 名称与排查建议见设计文档 5.10。
var exceptionInfo = map[ExceptionCode]struct{ name, chinese, tip string }{
	ExceptionIllegalFunction:        {"Illegal Function", "非法功能", "设备不支持该功能码，改用 FC03/FC04 或 FC06/FC16"},
	ExceptionIllegalDataAddress:     {"Illegal Data Address", "非法数据地址", "地址越界或 ±1 偏移；读取范围跨过了设备未定义的区段"},
	ExceptionIllegalDataValue:       {"Illegal Data Value", "非法数据值", "数量越限，或写入值超出设备允许范围"},
	ExceptionSlaveDeviceFailure:     {"Slave Device Failure", "从站设备故障", "设备内部错误，检查设备状态"},
	ExceptionAcknowledge:            {"Acknowledge", "请求已确认", "设备已接受请求但需要较长处理时间，稍后再读"},
	ExceptionSlaveDeviceBusy:        {"Slave Device Busy", "从站设备忙", "设备忙，降低轮询频率后重试"},
	ExceptionMemoryParityError:      {"Memory Parity Error", "存储器奇偶校验错误", "设备读扩展文件区（FC20 / FC21）时存储校验出错，检查设备存储"},
	ExceptionGatewayPathUnavailable: {"Gateway Path Unavailable", "网关路径不可用", "网关未配置到该 Slave 的路由"},
	ExceptionGatewayTargetFailed:    {"Gateway Target Device Failed to Respond", "网关目标设备未响应", "网关到串口设备这段不通：检查 Slave ID、波特率、485 接线"},
}

// Name 返回异常码的规范英文名称。
func (c ExceptionCode) Name() string {
	if info, ok := exceptionInfo[c]; ok {
		return info.name
	}
	return "Unknown Exception"
}

// ChineseName 返回异常码的中文含义。
func (c ExceptionCode) ChineseName() string {
	if info, ok := exceptionInfo[c]; ok {
		return info.chinese
	}
	return "未知异常"
}

// Description 保留原异常码，并统一显示中文含义和规范英文名。
func (c ExceptionCode) Description() string {
	return fmt.Sprintf("%02X %s（%s）", byte(c), c.ChineseName(), c.Name())
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
	return "modbus: 异常 " + e.Code.Description()
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
