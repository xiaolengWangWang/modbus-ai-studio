package modbus

import (
	"bytes"
	"encoding/hex"
	"errors"
	"time"
)

// ErrLRC 表示 ASCII 响应 LRC 校验失败，响应已丢弃。和 CRC 错误一样，读请求会重试。
var ErrLRC = errors.New("modbus: LRC 校验失败")

// maxASCIIFrame 是 ASCII 帧的最大长度：冒号 + 2 ×（地址 + 253 字节 PDU + LRC）+ CR LF。
const maxASCIIFrame = 1 + 2*(1+253+1) + 2

// LRC 返回纵向冗余校验：全部字节相加取低 8 位后求补。
func LRC(b []byte) byte {
	var s byte
	for _, v := range b {
		s += v
	}
	return -s
}

// EncodeASCII 把“地址 + PDU”编成 Modbus ASCII 帧：冒号、大写十六进制字符、LRC、CR LF。
func EncodeASCII(data []byte) []byte {
	out := make([]byte, 0, 1+2*len(data)+4)
	out = append(out, ':')
	out = append(out, bytes.ToUpper(hex.AppendEncode(nil, data))...)
	out = append(out, bytes.ToUpper(hex.AppendEncode(nil, []byte{LRC(data)}))...)
	return append(out, '\r', '\n')
}

// ParseASCII 解析一帧 ASCII 报文，返回“地址 + PDU”和收到的 LRC。格式不对返回 ErrFraming；
// LRC 不对返回 ErrLRC，同时仍返回解出的数据，界面据此给出正确值。十六进制大小写都接受。
func ParseASCII(raw []byte) (data []byte, lrc byte, err error) {
	if len(raw) < 9 || raw[0] != ':' || !bytes.HasSuffix(raw, []byte("\r\n")) {
		return nil, 0, ErrFraming
	}
	b, err := hex.DecodeString(string(raw[1 : len(raw)-2]))
	if err != nil {
		return nil, 0, ErrFraming
	}
	data, lrc = b[:len(b)-1], b[len(b)-1]
	if LRC(data) != lrc {
		return data, lrc, ErrLRC
	}
	return data, lrc, nil
}

// readASCII 按冒号找帧头、按 CR LF 找帧尾（ASCII 模式不靠字符间隔分帧）。
// 帧头前的字节、半截被新冒号打断的帧都当作格式错误丢弃。
func (r *frameReader) readASCII(deadline time.Time) (Frame, error) {
	if err := r.fill(1, deadline); err != nil {
		return Frame{}, err
	}
	if i := bytes.IndexByte(r.buf, ':'); i != 0 {
		if i < 0 {
			i = len(r.buf)
		}
		return Frame{Raw: r.take(i)}, ErrFraming
	}
	for {
		if j := bytes.IndexByte(r.buf[1:], ':'); j >= 0 {
			if k := bytes.Index(r.buf, []byte("\r\n")); k < 0 || k > j+1 {
				return Frame{Raw: r.take(j + 1)}, ErrFraming
			}
		}
		if k := bytes.Index(r.buf, []byte("\r\n")); k >= 0 {
			raw := r.take(k + 2)
			data, _, err := ParseASCII(raw)
			if err != nil {
				return Frame{Raw: raw}, err
			}
			return Frame{Raw: raw, Slave: data[0], PDU: data[1:]}, nil
		}
		if len(r.buf) > maxASCIIFrame {
			return Frame{Raw: r.take(len(r.buf))}, ErrFraming
		}
		if err := r.fill(len(r.buf)+1, deadline); err != nil {
			return Frame{}, err
		}
	}
}
