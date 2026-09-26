// Package modbus 实现 Modbus 协议核心：PDU 编解码、MBAP / RTU 分帧、
// 数据类型与字节序、地址解析，以及一问一答的主站客户端。
//
// 协议相关的计算全部在这里完成，界面和 AI 只使用这里的结果。
package modbus

// CRC16 计算 Modbus RTU 的 CRC-16（多项式 0xA001，初值 0xFFFF）。
// 发送时低字节在前。
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// AppendCRC 在帧尾追加 CRC（低字节在前）。
func AppendCRC(frame []byte) []byte {
	c := CRC16(frame)
	return append(frame, byte(c), byte(c>>8))
}

// CheckCRC 校验一帧带 CRC 的完整 RTU 报文。
func CheckCRC(frame []byte) bool {
	if len(frame) < 4 {
		return false
	}
	c := CRC16(frame[:len(frame)-2])
	return frame[len(frame)-2] == byte(c) && frame[len(frame)-1] == byte(c>>8)
}
