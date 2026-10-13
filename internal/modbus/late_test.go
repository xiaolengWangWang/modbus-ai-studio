package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowServer 是 Modbus TCP 回环服务器：第 txID 条请求推迟 delay(txID) 后回一个寄存器，各条响应互不等待。
// net.Pipe 没有缓冲，模拟不了“帧到了、但没人读”，这里用真的 TCP 连接。
func slowServer(t *testing.T, opts Options, delay func(txID uint16) time.Duration) (*Client, func() []Packet) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
		for {
			head := make([]byte, 7)
			if _, err := io.ReadFull(conn, head); err != nil {
				return
			}
			pdu := make([]byte, int(binary.BigEndian.Uint16(head[4:]))-1)
			if _, err := io.ReadFull(conn, pdu); err != nil {
				return
			}
			id := binary.BigEndian.Uint16(head)
			adu := EncodeADU(ModeTCP, head[6], id, []byte{byte(FuncReadHoldingRegisters), 2, 0, byte(id)})
			time.AfterFunc(delay(id), func() { conn.Write(adu) })
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var mu sync.Mutex
	var pkts []Packet
	opts.Mode = ModeTCP
	opts.Observer = ObserverFunc(func(p Packet) { mu.Lock(); pkts = append(pkts, p); mu.Unlock() })
	return NewClient(conn, opts), func() []Packet { mu.Lock(); defer mu.Unlock(); return append([]Packet(nil), pkts...) }
}

func lateOf(t *testing.T, pkts []Packet) *LateError {
	t.Helper()
	for _, p := range pkts {
		if p.Status == StatusLate {
			var le *LateError
			if !errors.As(p.Err, &le) {
				t.Fatalf("晚到帧应说明比超时晚了多久：%v", p.Err)
			}
			return le
		}
	}
	t.Fatalf("没有晚到帧：%v", statuses(pkts))
	return nil
}

var read1 = Request{Slave: 1, Function: FuncReadHoldingRegisters, Quantity: 1}

// 保护间隔内到达：用时就是这帧到达的时刻，晚了多久是确定的。
func TestLateResponseInGuard(t *testing.T) {
	c, pkts := slowServer(t, Options{Timeout: 100 * time.Millisecond, Guard: 400 * time.Millisecond}, func(uint16) time.Duration { return 180 * time.Millisecond })
	if _, err := c.Do(context.Background(), read1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("应超时：%v", err)
	}
	le := lateOf(t, pkts())
	if le.Timeout != 100*time.Millisecond || le.Elapsed < 170*time.Millisecond || le.Elapsed > 400*time.Millisecond || le.Max != le.Elapsed {
		t.Fatalf("用时应约 180 ms，且是确定值：%+v", le)
	}
	if s := le.Error(); !strings.HasPrefix(s, "比超时晚 ") || !strings.Contains(s, "超时 100 ms，响应用了 ") || strings.Contains(s, "范围") ||
		!strings.Contains(s, "发出的请求 #000001：00 01 00 00 00 06 01 03 00 00 00 01") {
		t.Errorf("说明应有晚了多久和回的是哪条请求：%s", s)
	}
	if tx := pkts()[0]; le.Request.RequestID != 1 || !le.Request.Time.Equal(tx.Time) {
		t.Errorf("回的是第 1 条请求，发送时间和发送行一致：%+v，发送行 %v", le.Request, tx.Time)
	}
}

// 等下一条请求时才到：按事务号找回它属于哪条请求，晚了多久也是确定的。
func TestLateResponseDuringNextRequest(t *testing.T) {
	c, pkts := slowServer(t, Options{Timeout: 100 * time.Millisecond, Guard: 20 * time.Millisecond}, func(id uint16) time.Duration {
		if id == 1 {
			return 300 * time.Millisecond
		}
		return 350 * time.Millisecond // 第二条请求的响应排在第一条的晚到响应之后
	})
	if _, err := c.Do(context.Background(), read1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("应超时：%v", err)
	}
	next := read1
	next.Timeout = 2 * time.Second
	resp, err := c.Do(context.Background(), next)
	if err != nil || resp.Registers[0] != 2 {
		t.Fatalf("第二条请求应拿到自己的响应：%v %v", resp, err)
	}
	le := lateOf(t, pkts())
	if le.Timeout != 100*time.Millisecond || le.Elapsed < 290*time.Millisecond || le.Elapsed > 600*time.Millisecond || le.Max != le.Elapsed {
		t.Fatalf("用时应约 300 ms，按第一条请求的超时算：%+v", le)
	}
	if le.Request.RequestID != 1 {
		t.Errorf("等第 2 条请求时到的，回的是第 1 条：%+v", le.Request)
	}
}

// 两次请求之间到达、下一条请求发送前才读到：只能确定用时的范围。
func TestLateResponseBetweenRequests(t *testing.T) {
	c, pkts := slowServer(t, Options{Timeout: 100 * time.Millisecond, Guard: 20 * time.Millisecond}, func(id uint16) time.Duration {
		if id == 1 {
			return 200 * time.Millisecond
		}
		return 0
	})
	start := time.Now()
	if _, err := c.Do(context.Background(), read1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("应超时：%v", err)
	}
	time.Sleep(400*time.Millisecond - time.Since(start))
	if _, err := c.Do(context.Background(), read1); err != nil {
		t.Fatal(err)
	}
	le := lateOf(t, pkts())
	// 正常时在两次请求之间到达，只知道 120–400 ms 的范围；机器很忙时也可能落在保护间隔或下一条请求里，
	// 那时是确定值。不论哪种，范围都要包住真实用时（至少 200 ms），下限不小于超时
	if le.Elapsed < 100*time.Millisecond || le.Max < 200*time.Millisecond || le.Elapsed > le.Max {
		t.Fatalf("用时范围应包住 200 ms：%+v", le)
	}
	if s := le.Error(); strings.Contains(s, "只能确定范围") != (le.Max.Round(time.Millisecond) > le.Elapsed.Round(time.Millisecond)) {
		t.Errorf("只有范围时才说只能确定范围：%s", s)
	}
}

// 串口服务器 / DTU 转发、设备很慢：第 1 条请求 30 s 才回、第 2 条 10 s 才回，每 3 s 发一条（这里按 1/20 缩短）。
// 第 2 条的响应先到、第 1 条的后到，中间又发了好几条请求，仍按事务号各自对上自己的请求和用时。
func TestLateResponsesOutOfOrder(t *testing.T) {
	const interval = 150 * time.Millisecond
	slow := map[uint16]time.Duration{1: 1500 * time.Millisecond, 2: 500 * time.Millisecond}
	c, pkts := slowServer(t, Options{Timeout: 50 * time.Millisecond, Guard: 10 * time.Millisecond}, func(id uint16) time.Duration { return slow[id] })
	start := time.Now()
	for i := range 12 {
		time.Sleep(time.Until(start.Add(time.Duration(i) * interval)))
		c.Do(context.Background(), read1)
	}
	var got []*LateError
	for _, p := range pkts() {
		var le *LateError
		if p.Status == StatusLate && errors.As(p.Err, &le) {
			got = append(got, le)
		}
	}
	if len(got) != 2 || got[0].Request.RequestID != 2 || got[1].Request.RequestID != 1 {
		t.Fatalf("应先收到第 2 条、再收到第 1 条请求的晚到响应：%v", got)
	}
	for _, le := range got {
		want := slow[uint16(le.Request.RequestID)]
		if le.Max < want || le.Elapsed > le.Max || le.Elapsed > want+time.Second {
			t.Errorf("请求 #%d 用时应在 %v–%v 之间、包含 %v", le.Request.RequestID, le.Elapsed, le.Max, want)
		}
	}
	if got[0].Later < 2 || got[1].Later < 8 || !strings.Contains(got[1].Error(), "这期间又发了") {
		t.Errorf("应说明这期间又发了几条请求：%d、%d，%s", got[0].Later, got[1].Later, got[1].Error())
	}
}

// 网关把别的主站的响应也发到这个连接：事务号不是本连接发的，说明是别的主站的；连在一起收到的几帧拆开逐帧说明。
func TestForeignFramesAreSplitAndMarked(t *testing.T) {
	foreign := append(mustHex(t, "1C A6 00 00 00 05 0A 03 02 D8 DA"), mustHex(t, "01 4C 00 00 00 05 1E 03 02 00 58")...)
	c, pkts := echoServer(t, Options{Timeout: time.Second}, func(id uint16, unit byte, pdu []byte) [][]byte {
		own := EncodeADU(ModeTCP, unit, id, []byte{byte(FuncReadHoldingRegisters), 2, 0, 1})
		if id == 1 {
			return [][]byte{own, foreign} // 第 1 条请求结束后才到，下一条请求发送前读到
		}
		return [][]byte{own}
	})
	for range 2 {
		if _, err := c.Do(context.Background(), read1); err != nil {
			t.Fatal(err)
		}
	}
	var ids []uint16
	for _, p := range *pkts {
		if p.Status != StatusLate {
			continue
		}
		var fe *ForeignError
		if !errors.As(p.Err, &fe) || !strings.Contains(fe.Error(), "不是本连接最近发出的请求") {
			t.Fatalf("应说明不是本连接的请求：% X %v", p.Raw, p.Err)
		}
		ids = append(ids, fe.TxID)
	}
	if len(ids) != 2 || ids[0] != 0x1CA6 || ids[1] != 0x014C {
		t.Errorf("连在一起的两帧应拆开逐帧说明：%04X", ids)
	}
}

func TestLateErrorText(t *testing.T) {
	sent := time.Date(2026, 10, 13, 13, 5, 48, 504e6, time.Local)
	req := Packet{RequestID: 7, Time: sent, Mode: ModeTCP, Raw: mustHex(t, "00 07 00 00 00 06 03 03 00 9E 00 0A")}
	const of = "；回的是 13:05:48.504 发出的请求 #000007：00 07 00 00 00 06 03 03 00 9E 00 0A"
	for _, c := range []struct {
		e    LateError
		want string
	}{
		{LateError{Request: req, Timeout: time.Second, Elapsed: 1013 * time.Millisecond, Max: 1013 * time.Millisecond}, "比超时晚 13 ms：超时 1000 ms，响应用了 1013 ms" + of},
		{LateError{Request: req, Timeout: time.Second, Elapsed: time.Second + 300*time.Microsecond, Max: time.Second + 300*time.Microsecond}, "比超时晚不到 1 ms：超时 1000 ms，响应用了 1000 ms" + of},
		{LateError{Request: req, Timeout: time.Second, Elapsed: 1100 * time.Millisecond, Max: 2100 * time.Millisecond}, "比超时晚 100–1100 ms：超时 1000 ms，响应用了 1100–2100 ms（在两次请求之间到达，只能确定范围）" + of},
		{LateError{Request: req, Cancelled: true, Timeout: time.Second, Elapsed: 3611 * time.Millisecond, Max: 3611 * time.Millisecond}, "请求已取消（超时 1000 ms），响应用了 3611 ms" + of},
		{LateError{Request: req, Timeout: time.Second}, "超时 1000 ms" + of},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v\n得到 %s\n期望 %s", c.e, got, c.want)
		}
	}
}
