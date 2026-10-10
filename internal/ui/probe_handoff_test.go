package ui

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// A late error from the closed pre-detection client belongs to its old link.
func TestIndependentPointProbeIgnoresOldClientErrorAfterRestore(t *testing.T) {
	ws := independentProbeWorkspace(t, simulator.Faults{})
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty = 20, 1
		w = ws.addWindow(d)
		ws.connect()
	})
	waitFor(t, 3*time.Second, "main connection ready", func() bool { return ws.session != nil && hasData(w) })
	var oldClient *modbus.Client
	var primary *session
	locked(func() {
		primary, oldClient = ws.session, ws.session.client
		ws.runRegisterProbeMode(nil, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 1}, probeSingle, nil)
	})
	waitFor(t, 3*time.Second, "main connection restored", func() bool {
		return !ws.probeRunning && primary.lost == nil && primary.client != oldClient
	})
	_, err := oldClient.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 20, Quantity: 1})
	if !errors.Is(err, modbus.ErrConnection) {
		t.Fatalf("old detection client should already be closed: %v", err)
	}
	locked(func() {
		if primary.lost != nil || len(primary.losses) != 0 {
			t.Error("closed old client must not report a loss on the restored main connection")
		}
	})
}

// A timed-out detection response must never become the first normal poll's data.
// Both requests use FC03/quantity 1, so only connection/transaction isolation
// prevents the delayed value for address 0 from being accepted for address 20.
func TestIndependentPointProbeDoesNotAdoptLateResponseAfterRestore(t *testing.T) {
	const probeValue, mainValue uint16 = 1111, 2222
	store := simulator.NewStore(32)
	store.SetRegisters(modbus.AreaHoldingRegisters, 0, []uint16{probeValue})
	store.SetRegisters(modbus.AreaHoldingRegisters, 20, []uint16{mainValue})
	srv := simulator.NewServer(modbus.ModeTCP, 1, store)
	srv.SetFaults(simulator.Faults{Slow: []simulator.SlowRange{{
		AddrRange: simulator.AddrRange{Start: 0, Count: 1},
		Delay:     250 * time.Millisecond,
	}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	ws := openWS(t, test.NewTempApp(t), false)
	var mainWindow *readWindow
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.proto.SetSelected(protoTCP)
		ws.target.SetText(ln.Addr().String())
		ws.timeoutE.SetText("100")
		ws.setPoints(newPointTable([]point{{
			Area: modbus.AreaHoldingRegisters, Offset: 0,
			Type: modbus.TypeUint16, Name: "slow detection point",
		}}))
		d := defaultDef()
		d.Start, d.Qty, d.Scan = 20, 1, time.Second
		mainWindow = ws.addWindow(d)
		ws.connect()
	})
	waitFor(t, 3*time.Second, "normal poll reads address 20", func() bool {
		mainWindow.mu.Lock()
		defer mainWindow.mu.Unlock()
		return ws.session != nil && mainWindow.err == nil && len(mainWindow.regs) == 1 && mainWindow.regs[0] == mainValue
	})
	var txBefore int
	locked(func() {
		mainWindow.mu.Lock()
		txBefore = mainWindow.tx
		mainWindow.mu.Unlock()
		d, segs, _ := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
		ws.runRegisterProbeMode(nil, d, probePoints, segs)
	})
	waitFor(t, 5*time.Second, "point detection finishes", func() bool { return !ws.probeRunning })
	waitFor(t, 5*time.Second, "first restored normal poll completes", func() bool {
		mainWindow.mu.Lock()
		defer mainWindow.mu.Unlock()
		return mainWindow.tx > txBefore && mainWindow.err == nil
	})
	locked(func() {
		mainWindow.mu.Lock()
		if len(mainWindow.regs) != 1 || mainWindow.regs[0] != mainValue {
			t.Errorf("restored address 20 must contain %d; adopted delayed address 0 value: %v", mainValue, mainWindow.regs)
		}
		mainWindow.mu.Unlock()
		// Check every recorded success too, so a later correct poll cannot hide
		// a transient adoption of the old detection response.
		ws.ring.mu.Lock()
		defer ws.ring.mu.Unlock()
		for i := max(0, ws.ring.n-len(ws.ring.buf)); i < ws.ring.n; i++ {
			p := ws.ring.buf[i%len(ws.ring.buf)]
			if p.Dir == modbus.DirRX && p.Status == modbus.StatusSuccess && p.Address == 20 && len(p.Raw) == 11 {
				if value := binary.BigEndian.Uint16(p.Raw[9:]); value == probeValue {
					t.Errorf("normal poll accepted the delayed detection value %d (transaction %d)", value, p.TxID)
				}
			}
		}
	})
}
