package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestParseResponsePDUStrictExceptionAndEcho(t *testing.T) {
	read := Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 8, Quantity: 2}
	if _, err := ParseResponsePDU(read, mustHex(t, "83 02 00 08")); !errors.Is(err, ErrMismatch) {
		t.Errorf("异常码后面还有字节，不能当作标准异常 02：%v", err)
	} else if _, ok := AsException(err); ok {
		t.Errorf("畸形异常帧不能返回 ExceptionError：%v", err)
	}
	ns, ok := AsNonstandard(func() error { _, e := ParseResponsePDU(read, mustHex(t, "89 00 08 00 02")); return e }())
	if !ok || ns.Function != FuncReadHoldingRegisters || hexOf(ns.PDU) != "89 00 08 00 02" {
		t.Fatalf("89 原样回显地址和数量应判为非标准响应，并保留原 PDU：%v", ns)
	}
	if _, ok := AsException(ns); ok {
		t.Error("89 回显不能当作异常，后面的 00 不是异常码")
	}
	input := Request{Slave: 1, Function: FuncReadInputRegisters, Address: 800, Quantity: 50}
	if _, ok := AsNonstandard(func() error { _, e := ParseResponsePDU(input, mustHex(t, "89 03 20 00 32")); return e }()); !ok {
		t.Error("FC04 的同形回显也应识别")
	}
	for name, pdu := range map[string]string{
		"数量不同": "89 00 08 00 01", "地址不同": "89 00 09 00 02", "多一个字节": "89 00 08 00 02 00", "其他功能码": "8A 00 08 00 02",
	} {
		_, err := ParseResponsePDU(read, mustHex(t, pdu))
		if _, ok := AsNonstandard(err); ok || !errors.Is(err, ErrMismatch) {
			t.Errorf("%s的帧不能当作本次请求的回显：%v", name, err)
		}
	}
	coils := Request{Slave: 1, Function: FuncReadCoils, Address: 8, Quantity: 2}
	if _, err := ParseResponsePDU(coils, mustHex(t, "89 00 08 00 02")); !errors.Is(err, ErrMismatch) {
		t.Errorf("只认读寄存器的回显：%v", err)
	}
}

// echoServer 在 net.Pipe 的另一端按 reply 回应 Modbus TCP 请求，reply 返回要写出的各段字节。
func echoServer(t *testing.T, opts Options, reply func(txID uint16, unit byte, pdu []byte) [][]byte) (*Client, *[]Packet) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		for {
			head := make([]byte, 7)
			if _, err := io.ReadFull(b, head); err != nil {
				return
			}
			pdu := make([]byte, int(binary.BigEndian.Uint16(head[4:]))-1)
			if _, err := io.ReadFull(b, pdu); err != nil {
				return
			}
			for _, part := range reply(binary.BigEndian.Uint16(head), head[6], pdu) {
				if _, err := b.Write(part); err != nil {
					return
				}
			}
		}
	}()
	var mu sync.Mutex
	var pkts []Packet
	opts.Mode = ModeTCP
	opts.Observer = ObserverFunc(func(p Packet) { mu.Lock(); pkts = append(pkts, p); mu.Unlock() })
	return NewClient(a, opts), &pkts
}

func echoOf(pdu []byte) []byte { return append([]byte{EchoFunction}, pdu[1:]...) }

func statuses(pkts []Packet) []Status {
	var out []Status
	for _, p := range pkts {
		out = append(out, p.Status)
	}
	return out
}

func TestClientEndsRequestOnMatchingEcho(t *testing.T) {
	c, pkts := echoServer(t, Options{Timeout: time.Second, ReadRetries: 1}, func(id uint16, unit byte, pdu []byte) [][]byte {
		adu := EncodeADU(ModeTCP, unit, id, echoOf(pdu))
		return [][]byte{adu[:5], adu[5:]} // 拆成两段到达
	})
	start := time.Now()
	_, err := c.Do(context.Background(), Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 8, Quantity: 2})
	ns, ok := AsNonstandard(err)
	if !ok || errors.Is(err, ErrTimeout) {
		t.Fatalf("事务号和 Unit ID 都对上的 89 回显应直接结束请求：%v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("不应等到超时：%v", time.Since(start))
	}
	got := *pkts
	if len(got) != 2 || got[1].Status != StatusNonstandard || hexOf(got[1].Raw) != "00 01 00 00 00 06 01 89 00 08 00 02" || got[1].Err != ns {
		t.Fatalf("应记 1 条请求、1 条非标准响应，原帧保留，且不重试：%v", statuses(got))
	}
}

func TestClientKeepsWaitingOnUncertainEcho(t *testing.T) {
	read := Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 8, Quantity: 2}
	for _, tc := range []struct {
		name     string
		frame    func(id uint16, unit byte, pdu []byte) []byte
		status   Status
		mismatch bool // 超时错误是否说明收到过事务号对上的帧
	}{
		{"Unit ID 不同", func(id uint16, unit byte, pdu []byte) []byte { return EncodeADU(ModeTCP, unit+1, id, echoOf(pdu)) }, StatusUnexpected, true},
		{"事务号不同", func(id uint16, unit byte, pdu []byte) []byte { return EncodeADU(ModeTCP, unit, id+1, echoOf(pdu)) }, StatusLate, false},
		{"数量不同", func(id uint16, unit byte, pdu []byte) []byte {
			return EncodeADU(ModeTCP, unit, id, []byte{EchoFunction, 0, 8, 0, 1})
		}, StatusUnexpected, true},
		{"异常后多出字节", func(id uint16, unit byte, pdu []byte) []byte {
			return EncodeADU(ModeTCP, unit, id, []byte{0x83, 0x02, 0x00, 0x08})
		}, StatusUnexpected, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, pkts := echoServer(t, Options{Timeout: 100 * time.Millisecond, Guard: 10 * time.Millisecond}, func(id uint16, unit byte, pdu []byte) [][]byte {
				return [][]byte{tc.frame(id, unit, pdu)}
			})
			_, err := c.Do(context.Background(), read)
			if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrMismatch) != tc.mismatch {
				t.Fatalf("不能确定属于本次请求的帧只记录、继续等到超时，超时说明是否收到过对上事务号的帧：%v", err)
			}
			if _, ok := AsNonstandard(err); ok {
				t.Fatal("不能当作本次请求的非标准响应")
			}
			if _, ok := AsException(err); ok {
				t.Fatal("不能当作标准异常")
			}
			if got := statuses(*pkts); len(got) < 3 || got[1] != tc.status || got[2] != StatusTimeout {
				t.Fatalf("记录的状态 %v，期望第二条为 %s、随后超时", got, tc.status)
			}
		})
	}
}

// 同一个 TCP 包里先到一帧对不上的回显、再到正常响应：丢弃前者，采用后者。
func TestClientAcceptsValidResponseAfterUncertainEcho(t *testing.T) {
	c, pkts := echoServer(t, Options{Timeout: time.Second}, func(id uint16, unit byte, pdu []byte) [][]byte {
		bad := EncodeADU(ModeTCP, unit+1, id, echoOf(pdu))
		good := EncodeADU(ModeTCP, unit, id, []byte{0x03, 0x04, 0x00, 0x01, 0x00, 0x02})
		return [][]byte{append(bad, good...)}
	})
	resp, err := c.Do(context.Background(), Request{Slave: 1, Function: FuncReadHoldingRegisters, Address: 8, Quantity: 2})
	if err != nil || resp.Registers[1] != 2 {
		t.Fatalf("随后到达的正常响应应被采用：%v %v", resp, err)
	}
	if got := statuses(*pkts); len(got) != 3 || got[1] != StatusUnexpected || got[2] != StatusSuccess {
		t.Fatalf("记录的状态 %v", got)
	}
}
