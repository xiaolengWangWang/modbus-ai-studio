package ui

import (
	"fyne.io/fyne/v2/test"
	"modbus-ai-studio/internal/modbus"
	"strings"
	"testing"
	"time"
)

func TestAIPacketExchangeDoesNotCrossConnectionsOrGuessLateFrames(t *testing.T) {
	tx := modbus.Packet{ConnectionID: "a", RequestID: 1, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 10, Count: 2, Dir: modbus.DirTX, Status: modbus.StatusSent}
	rx := tx
	rx.Dir = modbus.DirRX
	rx.Status = modbus.StatusTimeout
	other := tx
	other.ConnectionID = "b"
	late := tx
	late.Dir = modbus.DirRX
	late.Status = modbus.StatusLate
	packets := []modbus.Packet{other, tx, rx, late}
	got := relatedAIPackets(rx, packets)
	if len(got) != 1 || got[0].Dir != modbus.DirTX || got[0].ConnectionID != "a" {
		t.Fatalf("incorrect exchange: %+v", got)
	}
	if len(relatedAIPackets(late, packets)) != 0 {
		t.Fatal("late frame guessed from active request metadata")
	}
	legacy := rx
	legacy.ConnectionID = ""
	if len(relatedAIPackets(legacy, packets)) != 0 {
		t.Fatal("legacy packet guessed a connection")
	}
	v := aiPacket(late, false)
	for _, key := range []string{"request_group", "address", "quantity", "request_id"} {
		if _, ok := v[key]; ok {
			t.Errorf("late frame retained unverified %s", key)
		}
	}
}

func TestAIRefreshPacketCollectsNewResponseWithoutChangingSelection(t *testing.T) {
	ws := openAIWS(t, false)
	locked(func() {
		tx := modbus.Packet{Time: time.Now(), ConnectionID: "a", RequestID: 1, Mode: modbus.ModeTCP, Dir: modbus.DirTX, Status: modbus.StatusSent}
		ws.ring.push(tx)
		ws.openAI(&tx)
		rx := tx
		rx.Dir = modbus.DirRX
		rx.Status = modbus.StatusSuccess
		ws.ring.push(rx)
		test.Tap(ws.ai.refreshEvidence)
		if len(ws.ai.snapshot.Evidence) != 2 || ws.ai.target.packet.Dir != modbus.DirTX {
			t.Error("refresh omitted new response or changed selection")
		}
		rx.Status = modbus.StatusLate
		s := ws.aiSnapshotFor(aiTarget{packet: &rx}, false, false)
		if strings.Contains(s.Source, "Slave") || strings.Contains(s.Source, "地址") {
			t.Error("late snapshot source asserted unverified ownership")
		}
	})
}
