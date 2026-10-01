package ui

import (
	"context"
	"net"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

func TestScanSlaves(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 5, simulator.HeatStation())
	srv.SetFaults(simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 0, Count: 1}, Code: modbus.ExceptionIllegalDataAddress}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeRTUOverTCP, Guard: 10 * time.Millisecond})
	defer c.Close()
	hits, err := scanSlaves(context.Background(), c, 3, 7, 60*time.Millisecond, func(int, []slaveHit) {})
	if err != nil || hitIDs(hits) != "5" || hits[0].err == nil {
		t.Fatalf("应只找到 Slave 5（异常响应也算在线）：%v %v", hits, err)
	}
}

// 串口参数扫描：设备是 19200 8E1，前面 5 种组合没有应答，第 6 种找到。
func TestScanSerial(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	live, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(live)
	silent, _ := net.Listen("tcp", "127.0.0.1:0") // 接受连接但从不应答，相当于参数不对
	t.Cleanup(func() { srv.Close(); silent.Close() })
	go func() {
		for {
			if _, err := silent.Accept(); err != nil {
				return
			}
		}
	}()
	var tried []string
	open := func(cfg transport.SerialConfig) (modbus.Transport, error) {
		addr := silent.Addr().String()
		if cfg.BaudRate == 19200 && cfg.Parity == "E" && cfg.StopBits == 1 && cfg.DataBits == 8 {
			addr = live.Addr().String()
		}
		return net.Dial("tcp", addr)
	}
	mode, baud, format, err := scanSerial(context.Background(), modbus.ModeRTU, open, "COM9", 1, 80*time.Millisecond, nil,
		func(_, _ int, try string) { tried = append(tried, try) })
	if err != nil || mode != modbus.ModeRTU || baud != 19200 || format != "8E1" || len(tried) != 6 {
		t.Fatalf("找到 %s %d %s（%v），试了 %v", mode, baud, format, err, tried)
	}
}

func TestDiagCounters(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Quantity: 1})
	}
	counts, err := readDiagCounters(context.Background(), c, 1)
	if err != nil || len(counts) != 8 || counts[0].sub != 0x0B || counts[0].n != 4 || counts[3].sub != 0x0E || counts[3].n != 7 {
		t.Fatalf("诊断计数器 %+v %v", counts, err)
	}
}
