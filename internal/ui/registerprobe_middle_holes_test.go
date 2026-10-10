package ui

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

func middleNetworkProbeClient(t *testing.T, faults simulator.Faults, size int) *networkProbeClient {
	t.Helper()
	srv := simulator.NewServer(modbus.ModeTCP, 1, simulator.NewStore(size))
	srv.SetFaults(faults)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c := &networkProbeClient{
		cfg:     connConfig{mode: modbus.ModeTCP, target: ln.Addr().String(), timeout: 50 * time.Millisecond},
		waiting: func(bool) {},
	}
	t.Cleanup(c.close)
	return c
}

// A protocol exception at one middle address must not hide an illegal address
// farther along the range, or prevent the healthy final addresses being read.
func TestRegisterProbeContinuesAfterMiddlePointException(t *testing.T) {
	for _, code := range []modbus.ExceptionCode{
		modbus.ExceptionIllegalFunction,
		modbus.ExceptionIllegalDataValue,
		modbus.ExceptionSlaveDeviceFailure,
		modbus.ExceptionSlaveDeviceBusy,
	} {
		t.Run(code.Name(), func(t *testing.T) {
			c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Exceptions: []simulator.ExceptionRange{
				{AddrRange: simulator.AddrRange{Start: 2, Count: 1}, Code: code},
				{AddrRange: simulator.AddrRange{Start: 6, Count: 1}, Code: modbus.ExceptionIllegalDataAddress},
			}}, nil, 9)
			d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 9}
			res, err := probe(context.Background(), c, d, func(int) {})
			if err != nil || fmt.Sprint(res) != "[1 1 -2 1 1 1 -1 1 1]" {
				t.Fatalf("must classify both middle failures and finish reading healthy neighbours: states %v, error %v", res, err)
			}
		})
	}
}

// A hole inside a FLOAT32 point makes that complete point unreadable. Testing
// must preserve the point width and continue to the healthy following points.
func TestPointProbeReportsHoleInsideMultiRegisterPoint(t *testing.T) {
	pts := newPointTable([]point{
		{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "前状态", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 1, Name: "中间温度", Type: modbus.TypeFloat32},
		{Area: modbus.AreaHoldingRegisters, Offset: 3, Name: "后状态", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 4, Name: "累计量", Type: modbus.TypeFloat64},
	})
	d, segs, ok := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	if !ok {
		t.Fatal("holding-register points must be available for detection")
	}
	var readWidths []string
	c := registerProbeClient(t, modbus.ModeTCP, simulator.Faults{Exceptions: []simulator.ExceptionRange{
		{AddrRange: simulator.AddrRange{Start: 2, Count: 1}, Code: modbus.ExceptionIllegalDataAddress},
	}}, modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Dir == modbus.DirTX {
			readWidths = append(readWidths, fmt.Sprintf("%d:%d", p.Address, p.Count))
		}
	}), 8)
	res, err := probeUnits(context.Background(), c, d, probePoints, segs, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 -1 -1 1 1 1 1 1]" {
		t.Fatalf("must identify the complete affected point and read both sides of the hole: states %v, error %v", res, err)
	}
	if fmt.Sprint(readWidths) != "[0:8 0:1 1:2 3:1 4:4]" {
		t.Fatalf("must try the segment then read each point using its complete width: %v", readWidths)
	}
	text, summary, bad := pointProbeText(d, res, segs)
	if bad != 1 || !strings.Contains(summary, "可读 3") || !strings.Contains(summary, "未检测 0") ||
		!strings.Contains(text, "40002 中间温度（FLOAT32）：非法地址") ||
		strings.Contains(text, "后状态（UINT16）：非法地址") || strings.Contains(text, "累计量（FLOAT64）：非法地址") {
		t.Fatalf("result must name only the affected point, with all following points tested: %s\n%s", summary, text)
	}
}

// A slow middle point can keep the server busy after a failed batch. Healthy
// neighbours must be verified after that backlog, rather than reported as holes.
func TestPointProbeRechecksHealthyNeighboursAfterMiddleTimeout(t *testing.T) {
	pts := newPointTable([]point{
		{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "前点一", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 1, Name: "前点二", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "中间慢点", Type: modbus.TypeUint16},
		{Area: modbus.AreaHoldingRegisters, Offset: 3, Name: "后点", Type: modbus.TypeUint16},
	})
	d, segs, ok := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
	if !ok {
		t.Fatal("holding-register points must be available for detection")
	}
	c := middleNetworkProbeClient(t, simulator.Faults{Slow: []simulator.SlowRange{
		{AddrRange: simulator.AddrRange{Start: 2, Count: 1}, Delay: 250 * time.Millisecond},
	}}, 4)
	res, err := probeUnits(context.Background(), c, d, probePoints, segs, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 1 -3 1]" {
		t.Fatalf("only the middle slow point must remain unresponsive after verifying healthy neighbours: states %v, error %v", res, err)
	}
	text, summary, bad := pointProbeText(d, res, segs)
	if bad != 1 || !strings.Contains(summary, "可读 3") || !strings.Contains(text, "40003 中间慢点（UINT16）：未响应") ||
		strings.Contains(text, "前点一（UINT16）：未响应") || strings.Contains(text, "后点（UINT16）：未响应") {
		t.Fatalf("result must name the actual middle slow point without healthy neighbours: %s\n%s", summary, text)
	}
}

func TestRegisterProbeClearsMiddleTimeoutBacklogBeforeFinalVerdict(t *testing.T) {
	c := middleNetworkProbeClient(t, simulator.Faults{Slow: []simulator.SlowRange{
		{AddrRange: simulator.AddrRange{Start: 2, Count: 1}, Delay: 250 * time.Millisecond},
	}}, 4)
	d := readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 4}
	res, err := probe(context.Background(), c, d, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 1 -3 1]" {
		t.Fatalf("automatic detection must not retain a timeout caused only by an earlier slow request: states %v, error %v", res, err)
	}
}
