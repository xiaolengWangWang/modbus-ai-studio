// Package tests 用模拟器对协议引擎做集成测试，覆盖设计文档 9.2 的故障注入场景。
package tests

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

type recorder struct {
	mu   sync.Mutex
	pkts []modbus.Packet
}

func (r *recorder) OnPacket(p modbus.Packet) {
	r.mu.Lock()
	r.pkts = append(r.pkts, p)
	r.mu.Unlock()
}

func (r *recorder) count(st modbus.Status) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.pkts {
		if p.Status == st {
			n++
		}
	}
	return n
}

func (r *recorder) raw(dir modbus.Direction, st modbus.Status) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.pkts {
		if p.Dir == dir && p.Status == st {
			out = append(out, hexOf(p.Raw))
		}
	}
	return out
}

func hexOf(b []byte) string {
	s := strings.ToUpper(hex.EncodeToString(b))
	var parts []string
	for i := 0; i < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, " ")
}

// startServer 启动换热站模拟器，返回地址。
func startServer(t *testing.T, mode modbus.Mode, f simulator.Faults) (*simulator.Server, string) {
	t.Helper()
	srv := simulator.NewServer(mode, 1, simulator.HeatStation())
	srv.SetFaults(f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

func newClient(t *testing.T, addr string, opts modbus.Options) (*modbus.Client, *recorder) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	opts.Observer = rec
	c := modbus.NewClient(conn, opts)
	t.Cleanup(func() { c.Close() })
	return c, rec
}

func readRegs(t *testing.T, c *modbus.Client, addr, n uint16) ([]uint16, error) {
	t.Helper()
	resp, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: addr, Quantity: n})
	if err != nil {
		return nil, err
	}
	return resp.Registers, nil
}

func float(t *testing.T, regs []uint16) float64 {
	t.Helper()
	v, err := modbus.DecodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, regs)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func writeFloat(t *testing.T, c *modbus.Client, addr uint16, v float64) error {
	t.Helper()
	regs, err := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncWriteMultipleRegisters, Address: addr, Values: regs})
	return err
}

var modes = []modbus.Mode{modbus.ModeTCP, modbus.ModeRTUOverTCP, modbus.ModeASCIIOverTCP}

// 模拟器初值读出的报文必须与设计文档 13.3 的快照逐字节一致。
func TestHeatStationSnapshotMatchesDocument(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	regs, err := readRegs(t, c, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	const wantTX = "01 03 00 00 00 14 45 C5"
	const wantRX = "01 03 28 CC CD 42 34 00 00 42 00 00 84 00 01 00 00 42 12 15 56 44 0C D6 87 00 12 B4 3F 00 96 10 9A 02 8D 00 3E 00 2D 00 00 00 00 A9 BC"
	if got := rec.raw(modbus.DirTX, modbus.StatusSent); len(got) != 1 || got[0] != wantTX {
		t.Errorf("TX 与文档不一致：%v", got)
	}
	if got := rec.raw(modbus.DirRX, modbus.StatusSuccess); len(got) != 1 || got[0] != wantRX {
		t.Errorf("RX 与文档不一致：%v", got)
	}
	supply, ret := float(t, regs[0:2]), float(t, regs[2:4])
	dt := modbus.Scaling{Scale: 0.1}.Engineering(float64(regs[4]))
	power := float(t, regs[8:10])
	if math.Abs(supply-45.2) > 1e-4 || ret != 32 || math.Abs(dt-13.2) > 1e-9 || math.Abs(power-1.163*36.5*13.2) > 1e-3 {
		t.Errorf("换热站数据不合理：供水 %v 回水 %v 温差 %v 热功率 %v", supply, ret, dt, power)
	}
}

func TestReadWriteBothModes(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			_, addr := startServer(t, mode, simulator.Faults{})
			c, rec := newClient(t, addr, modbus.Options{Mode: mode})
			if err := writeFloat(t, c, 346, 16.0); err != nil {
				t.Fatal(err)
			}
			regs, err := readRegs(t, c, 346, 2)
			if err != nil || float(t, regs) != 16.0 {
				t.Fatalf("回读失败：%v %v", regs, err)
			}
			if mode == modbus.ModeRTUOverTCP {
				tx := rec.raw(modbus.DirTX, modbus.StatusSent)
				rx := rec.raw(modbus.DirRX, modbus.StatusSuccess)
				if tx[0] != "01 10 01 5A 00 02 04 00 00 41 80 4A 8C" || rx[0] != "01 10 01 5A 00 02 60 27" || rx[1] != "01 03 04 00 00 41 80 CB C3" {
					t.Errorf("报文与设计文档 13.6 不一致：TX %v RX %v", tx, rx)
				}
			}
		})
	}
}

func TestExceptionIllegalAddress(t *testing.T) {
	for _, mode := range modes {
		_, addr := startServer(t, mode, simulator.Faults{})
		c, rec := newClient(t, addr, modbus.Options{Mode: mode})
		_, err := readRegs(t, c, 995, 10)
		ex, ok := modbus.AsException(err)
		if !ok || ex.Code != modbus.ExceptionIllegalDataAddress || rec.count(modbus.StatusException) != 1 {
			t.Errorf("%s：应返回异常 02，得到 %v", mode, err)
		}
	}
}

func TestTimeoutAndReadRetry(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{DropRate: 1})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP, Timeout: 150 * time.Millisecond, Guard: 10 * time.Millisecond, ReadRetries: 1})
	start := time.Now()
	if _, err := readRegs(t, c, 0, 2); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("应超时，得到 %v", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("读请求应重试一次，总耗时 %v", el)
	}
	if rec.count(modbus.StatusSent) != 2 || rec.count(modbus.StatusTimeout) != 2 {
		t.Errorf("应有 2 次发送和 2 次超时，得到 %d / %d", rec.count(modbus.StatusSent), rec.count(modbus.StatusTimeout))
	}
	// 写请求不重试：超时不代表设备没有执行
	if err := writeFloat(t, c, 346, 16.0); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("写应超时，得到 %v", err)
	}
	if rec.count(modbus.StatusSent) != 3 {
		t.Errorf("写请求不应重试，发送次数 %d", rec.count(modbus.StatusSent))
	}
}

// RTU 没有 Transaction ID。慢应答在保护间隔内到达时必须记为晚到响应，下一条请求读到的仍是自己的数据。
func TestLateResponseNotMismatchedRTU(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 600, Count: 4}, Delay: 250 * time.Millisecond}}})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP, Timeout: 200 * time.Millisecond, Guard: 100 * time.Millisecond})
	if _, err := readRegs(t, c, 600, 4); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("应超时，得到 %v", err)
	}
	if late := rec.raw(modbus.DirRX, modbus.StatusLate); len(late) != 1 || late[0] != "01 03 08 00 00 C0 60 02 AA 00 01 E4 87" {
		t.Fatalf("晚到响应应被记录且与文档一致，得到 %v", late)
	}
	regs, err := readRegs(t, c, 0, 2)
	if err != nil || math.Abs(float(t, regs)-45.2) > 1e-4 {
		t.Fatalf("下一条请求读到的不是自己的数据：%v %v", regs, err)
	}
}

// Modbus TCP 靠 Transaction ID 区分：旧请求的响应在新请求等待期间到达，被丢弃后继续等到正确响应。
func TestLateResponseDiscardedByTxID(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 600, Count: 4}, Delay: 250 * time.Millisecond}}})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP, Timeout: 200 * time.Millisecond, Guard: 5 * time.Millisecond})
	if _, err := readRegs(t, c, 600, 4); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("应超时，得到 %v", err)
	}
	regs, err := readRegs(t, c, 0, 2)
	if err != nil || math.Abs(float(t, regs)-45.2) > 1e-4 {
		t.Fatalf("第二次读取失败：%v %v", regs, err)
	}
	if rec.count(modbus.StatusLate) != 1 {
		t.Errorf("旧 Transaction ID 的响应应记为晚到响应，得到 %d 条", rec.count(modbus.StatusLate))
	}
}

func TestWrongTransactionIDTimesOut(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{WrongTxIDRate: 1})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP, Timeout: 150 * time.Millisecond, Guard: 10 * time.Millisecond})
	if _, err := readRegs(t, c, 0, 2); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("Transaction ID 不符的响应不能被采用，得到 %v", err)
	}
	if rec.count(modbus.StatusLate) != 1 {
		t.Errorf("应记录 1 条不匹配的响应，得到 %d", rec.count(modbus.StatusLate))
	}
}

func TestCRCError(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{CRCRate: 1})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP, Timeout: 200 * time.Millisecond, ReadRetries: 1})
	if _, err := readRegs(t, c, 0, 2); !errors.Is(err, modbus.ErrCRC) {
		t.Fatalf("应返回 CRC 错误，得到 %v", err)
	}
	if rec.count(modbus.StatusCRCError) != 2 {
		t.Errorf("读请求 CRC 错误应重试一次，CRC 错误记录 %d 条", rec.count(modbus.StatusCRCError))
	}
}

func TestSplitResponse(t *testing.T) {
	for _, mode := range modes {
		_, addr := startServer(t, mode, simulator.Faults{SplitWrite: true})
		c, _ := newClient(t, addr, modbus.Options{Mode: mode})
		if regs, err := readRegs(t, c, 0, 20); err != nil || len(regs) != 20 {
			t.Errorf("%s：拆包到达的响应应能拼回，得到 %v", mode, err)
		}
	}
}

func TestExceptionFault(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 346, Count: 2}, Code: modbus.ExceptionSlaveDeviceFailure}}})
	c, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP})
	if ex, ok := modbus.AsException(func() error { _, e := readRegs(t, c, 346, 2); return e }()); !ok || ex.Code != modbus.ExceptionSlaveDeviceFailure {
		t.Errorf("应返回异常 04，得到 %v", ex)
	}
}

// 写入返回成功但值不变：协议层成功，回读仍是原值（控制验证应判“未生效”）。
func TestIgnoredWrite(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{IgnoreWrites: []simulator.AddrRange{{Start: 346, Count: 2}}})
	c, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP})
	if err := writeFloat(t, c, 346, 16.0); err != nil {
		t.Fatalf("写响应应为成功，得到 %v", err)
	}
	if regs, _ := readRegs(t, c, 346, 2); float(t, regs) != 15.0 {
		t.Errorf("值应保持 15.0，得到 %v", float(t, regs))
	}
}

// 写入后被改回：先回读到目标值，稍后变回原值（控制验证应判“被覆盖”）。
func TestRevertedWrite(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{RevertWrites: []simulator.RevertRange{{AddrRange: simulator.AddrRange{Start: 346, Count: 2}, After: 150 * time.Millisecond}}})
	c, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP})
	if err := writeFloat(t, c, 346, 16.0); err != nil {
		t.Fatal(err)
	}
	if regs, _ := readRegs(t, c, 346, 2); float(t, regs) != 16.0 {
		t.Errorf("写后立即回读应为 16.0，得到 %v", float(t, regs))
	}
	time.Sleep(300 * time.Millisecond)
	if regs, _ := readRegs(t, c, 346, 2); float(t, regs) != 15.0 {
		t.Errorf("被改回后应为 15.0，得到 %v", float(t, regs))
	}
}

// echoConn 模拟会回显发送帧的 RS485 转换器。
type echoConn struct {
	net.Conn
	mu      sync.Mutex
	pending []byte
}

func (e *echoConn) Write(p []byte) (int, error) {
	e.mu.Lock()
	e.pending = append(e.pending, p...)
	e.mu.Unlock()
	return e.Conn.Write(p)
}

func (e *echoConn) Read(p []byte) (int, error) {
	e.mu.Lock()
	if len(e.pending) > 0 {
		n := copy(p, e.pending)
		e.pending = e.pending[n:]
		e.mu.Unlock()
		return n, nil
	}
	e.mu.Unlock()
	return e.Conn.Read(p)
}

func TestDiscardEcho(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	for _, discard := range []bool{false, true} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c := modbus.NewClient(&echoConn{Conn: conn}, modbus.Options{Mode: modbus.ModeRTUOverTCP, Timeout: 300 * time.Millisecond, DiscardEcho: discard})
		_, err = readRegs(t, c, 0, 20)
		c.Close()
		if discard && err != nil {
			t.Errorf("开启丢弃回显后应读取成功，得到 %v", err)
		}
		if !discard && err == nil {
			t.Error("有回显且未开启丢弃时，回显不能被当成响应")
		}
	}
}

func TestContextCancel(t *testing.T) {
	_, addr := startServer(t, modbus.ModeTCP, simulator.Faults{DropRate: 1})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeTCP, Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Do(ctx, modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Quantity: 2})
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Errorf("取消后应尽快返回，得到 %v，耗时 %v", err, time.Since(start))
	}
	if rec.count(modbus.StatusCancelled) != 1 {
		t.Error("应记录 CANCELLED")
	}
}

// 多个调用方并发请求时，同一时刻只有一个未完成请求，每条 TX 后紧跟自己的结果。
func TestConcurrentCallsSerialized(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := readRegs(t, c, uint16(i*2), 2); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := 0; i+1 < len(rec.pkts); i += 2 {
		tx, rx := rec.pkts[i], rec.pkts[i+1]
		if tx.Dir != modbus.DirTX || rx.Dir != modbus.DirRX || tx.RequestID != rx.RequestID {
			t.Fatalf("第 %d 条报文顺序错乱：%+v / %+v", i, tx, rx)
		}
	}
}

func dialer(addr string) detect.Dialer {
	return func(ctx context.Context) (modbus.Transport, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestDetect(t *testing.T) {
	for _, mode := range modes {
		_, addr := startServer(t, mode, simulator.Faults{})
		res, err := detect.Detect(context.Background(), dialer(addr), detect.Options{Timeout: 200 * time.Millisecond})
		if err != nil || res.Mode != mode || res.Slave != 1 || res.ByException {
			t.Errorf("%s：识别结果 %+v %v", mode, res, err)
		}
	}
	// 探测地址不存在时收到异常 02，同样能确认协议
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{})
	res, err := detect.Detect(context.Background(), dialer(addr), detect.Options{Address: 5000, Timeout: 200 * time.Millisecond})
	if err != nil || res.Mode != modbus.ModeRTUOverTCP || !res.ByException {
		t.Errorf("异常响应也应确认协议：%+v %v", res, err)
	}
	// 始终不响应：三种格式、两个 Slave 共 6 次尝试后报告未识别
	_, addr = startServer(t, modbus.ModeTCP, simulator.Faults{DropRate: 1})
	res, err = detect.Detect(context.Background(), dialer(addr), detect.Options{Timeout: 100 * time.Millisecond})
	if !errors.Is(err, detect.ErrUnknown) || len(res.Attempts) != 6 {
		t.Errorf("应报告未识别并列出 6 次尝试：%+v %v", res, err)
	}
}

// 自定义请求：任意 PDU 原样发送，响应只按事务号 / Slave 和功能码关联，不按功能码校验内容。
func TestDoRaw(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			_, addr := startServer(t, mode, simulator.Faults{})
			c, rec := newClient(t, addr, modbus.Options{Mode: mode})
			resp, err := c.DoRaw(context.Background(), 1, []byte{0x03, 0x01, 0x5A, 0x00, 0x02})
			if err != nil || hexOf(resp) != "03 04 00 00 41 70" {
				t.Fatalf("读温差设定：响应 %s，错误 %v", hexOf(resp), err)
			}
			// 用户自定义功能码 65 模拟器不支持，应返回异常 01，同时返回异常 PDU
			resp, err = c.DoRaw(context.Background(), 1, []byte{0x41, 0x00})
			ex, ok := modbus.AsException(err)
			if !ok || ex.Code != modbus.ExceptionIllegalFunction || hexOf(resp) != "C1 01" {
				t.Fatalf("FC65：响应 %s，错误 %v", hexOf(resp), err)
			}
			if n := rec.count(modbus.StatusException); n != 1 {
				t.Errorf("应记录 1 条异常响应，得到 %d", n)
			}
			if _, err := c.DoRaw(context.Background(), 1, nil); !errors.Is(err, modbus.ErrInvalidRequest) {
				t.Errorf("空 PDU 应拒绝发送，得到 %v", err)
			}
		})
	}
}

// 运行中修改超时：从下一条请求起生效，不需要重新连接。
func TestSetTimeoutAtRuntime(t *testing.T) {
	_, addr := startServer(t, modbus.ModeRTUOverTCP, simulator.Faults{Slow: []simulator.SlowRange{{AddrRange: simulator.AddrRange{Start: 600, Count: 4}, Delay: 300 * time.Millisecond}}})
	c, _ := newClient(t, addr, modbus.Options{Mode: modbus.ModeRTUOverTCP, Timeout: 150 * time.Millisecond, Guard: 20 * time.Millisecond})
	if _, err := readRegs(t, c, 600, 4); !errors.Is(err, modbus.ErrTimeout) {
		t.Fatalf("超时 150 ms 应超时，得到 %v", err)
	}
	time.Sleep(300 * time.Millisecond) // 让晚到响应落地并在下一次请求前被清掉
	c.SetTimeout(time.Second)
	if c.Timeout() != time.Second {
		t.Fatalf("Timeout() = %v", c.Timeout())
	}
	if _, err := readRegs(t, c, 600, 4); err != nil {
		t.Fatalf("超时改为 1000 ms 后应成功，得到 %v", err)
	}
}

// 新增功能码：模拟器按规范应答，三种模式都走得通（FC08、FC43 没有长度字段，RTU 靠字符间隔分帧）。
func TestExtraFunctionCodes(t *testing.T) {
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			_, addr := startServer(t, mode, simulator.Faults{})
			c, _ := newClient(t, addr, modbus.Options{Mode: mode})
			do := func(pdu ...byte) string {
				t.Helper()
				resp, err := c.DoRaw(context.Background(), 1, pdu)
				if err != nil {
					t.Fatalf("% X：%v", pdu, err)
				}
				return hexOf(resp)
			}
			cases := []struct{ name, got, want string }{
				{"FC07 读异常状态", do(0x07), "07 00"},
				{"FC08 回送", do(0x08, 0x00, 0x00, 0x12, 0x34), "08 00 00 12 34"},
				{"FC11 报告从站 ID", do(0x11), "11 08 4D 41 53 2D 53 49 4D FF"},
				// 40352 阀门手动开度原值 0x028A：(0x028A AND 0x00F2) OR (0x0025 AND NOT 0x00F2) = 0x0087
				{"FC22 掩码写", do(0x16, 0x01, 0x5F, 0x00, 0xF2, 0x00, 0x25), "16 01 5F 00 F2 00 25"},
				{"FC03 回读掩码写结果", do(0x03, 0x01, 0x5F, 0x00, 0x01), "03 02 00 87"},
				// 先把 40352 写成 0x0290，再读 40351–40352
				{"FC23 读写多个", do(0x17, 0x01, 0x5E, 0x00, 0x02, 0x01, 0x5F, 0x00, 0x01, 0x02, 0x02, 0x90), "17 04 13 88 02 90"},
				{"FC08 服务器报文计数", do(0x08, 0x00, 0x0E), "08 00 0E 00 07"},
			}
			for _, c := range cases {
				if c.got != c.want {
					t.Errorf("%s：%s，期望 %s", c.name, c.got, c.want)
				}
			}
			if id := do(0x2B, 0x0E, 0x01, 0x00); !strings.HasPrefix(id, "2B 0E 01 82 00 00 03 00 10") {
				t.Errorf("FC43 读设备标识：%s", id)
			}
			if _, err := c.DoRaw(context.Background(), 1, []byte{0x08, 0x00, 0x04, 0x00, 0x00}); err == nil {
				t.Error("FC08 只听模式模拟器不支持，应回异常")
			}
		})
	}
}

// ASCII 模式下的 LRC 错误和 RTU 的 CRC 错误一样：读请求重试，报文记为 CRC_ERROR。
func TestASCIILRCError(t *testing.T) {
	_, addr := startServer(t, modbus.ModeASCIIOverTCP, simulator.Faults{CRCRate: 1})
	c, rec := newClient(t, addr, modbus.Options{Mode: modbus.ModeASCIIOverTCP, ReadRetries: 1})
	if _, err := readRegs(t, c, 346, 2); !errors.Is(err, modbus.ErrLRC) {
		t.Fatalf("应报 LRC 错误，得到 %v", err)
	}
	if n := rec.count(modbus.StatusCRCError); n != 2 {
		t.Errorf("读请求应重试 1 次，共 2 条校验错误，得到 %d", n)
	}
}
