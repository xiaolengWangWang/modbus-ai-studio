package recorder

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

func queryRecorder(t testing.TB) *Recorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queries.sqlite3")
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Recorder{db: db, root: path, path: path, sessions: make(map[int64]sessionRecord)}
	t.Cleanup(func() { r.db.Close() })
	return r
}

// Restricting history before aggregation must preserve counts, empty sessions,
// completed sessions and the ID tie-breaker for connections in one millisecond.
func TestRecentSessionsKeepCountsAndLimitSemantics(t *testing.T) {
	r := queryRecorder(t)
	_, err := r.db.Exec(`
		INSERT INTO sessions (id, started_at, ended_at, protocol, target, window) VALUES
			(10, 100, NULL, 'MODBUS_TCP', 'old', 1),
			(20, 200, 250, 'MODBUS_TCP', 'completed', 2),
			(30, 300, NULL, 'MODBUS_TCP', 'same-time', 3),
			(40, 300, NULL, 'MODBUS_TCP', 'empty', 4);
		INSERT INTO packets (session_id, time, request_id, direction, protocol, slave,
			tx_id, function, address, count, status, rtt_us) VALUES
			(20, 1, 1, 'TX', 'MODBUS_TCP', 1, 1, 3, 0, 1, 'SENT', 0),
			(20, 2, 1, 'RX', 'MODBUS_TCP', 1, 1, 3, 0, 1, 'TIMEOUT', 0),
			(30, 3, 2, 'TX', 'MODBUS_TCP', 1, 2, 3, 0, 1, 'SUCCESS', 0),
			(30, 4, 2, 'RX', 'MODBUS_TCP', 1, 2, 3, 0, 1, 'CRC_ERROR', 0),
			(30, 5, 3, 'RX', 'MODBUS_TCP', 1, 3, 3, 0, 1, 'TIMEOUT', 0);
		INSERT INTO events (session_id, time, kind, detail) VALUES
			(20, 1, 'DISCONNECT', 'ended'),
			(20, 2, 'READ_FAIL', 'failed'),
			(20, 3, 'CONNECT_FAIL', 'failed'),
			(30, 4, 'READ_FAIL', 'failed'),
			(30, 5, 'READ_OK', 'recovered');`)
	if err != nil {
		t.Fatal(err)
	}
	type summary struct {
		id                                   int64
		packets, errors, disconnects, faults int
		ended                                bool
	}
	all := []summary{{40, 0, 0, 0, 0, false}, {30, 3, 2, 0, 1, false}, {20, 2, 1, 1, 2, true}, {10, 0, 0, 0, 0, false}}
	for _, tc := range []struct {
		limit int
		want  []summary
	}{{0, nil}, {1, all[:1]}, {2, all[:2]}, {20, all}, {-1, all}} {
		t.Run(fmt.Sprintf("limit_%d", tc.limit), func(t *testing.T) {
			sessions, err := r.Sessions(tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			var got []summary
			for _, s := range sessions {
				got = append(got, summary{s.ID, s.Packets, s.Errors, s.Disconnects, s.Faults, !s.End.IsZero()})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("recent sessions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Deduplicating session lookups must still copy every session after rotation,
// including interleaved sessions in a batch and its request ordering.
func TestBatchKeepsInterleavedSessionsAcrossRotation(t *testing.T) {
	r := queryRecorder(t)
	first, err := r.StartSession(modbus.ModeTCP, "first:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.StartSession(modbus.ModeTCP, "second:502", 2)
	if err != nil {
		t.Fatal(err)
	}
	r.dbMu.Lock()
	err = r.rotateLocked()
	r.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	batch := make([]item, 12)
	for i := range batch {
		id := first
		if i%2 != 0 {
			id = second
		}
		batch[i] = item{id, modbus.Packet{Time: time.Unix(0, 0), RequestID: uint64(i + 1),
			Dir: modbus.DirTX, Mode: modbus.ModeTCP, Status: modbus.StatusSent, Raw: []byte{1, 3}}}
	}
	if err := r.insert(batch); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   int64
		want []uint64
	}{{first, []uint64{1, 3, 5, 7, 9, 11}}, {second, []uint64{2, 4, 6, 8, 10, 12}}} {
		packets, err := r.Packets(tc.id, 20)
		if err != nil {
			t.Fatal(err)
		}
		var got []uint64
		for _, p := range packets {
			got = append(got, p.RequestID)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("session %d request IDs = %v, want %v", tc.id, got, tc.want)
		}
	}
	rows, err := r.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("batch lost its session references")
	}
}

func seedQueryArchive(b *testing.B, r *Recorder, sessions, packets, events int) {
	b.Helper()
	tx, err := r.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	sessionInsert, err := tx.Prepare("INSERT INTO sessions (id, started_at, protocol, target, window) VALUES (?, ?, 'MODBUS_TCP', 'plc:502', 1)")
	if err != nil {
		b.Fatal(err)
	}
	defer sessionInsert.Close()
	packetInsert, err := tx.Prepare(`INSERT INTO packets (session_id, time, request_id, direction, protocol, slave,
		tx_id, function, address, count, raw, status, rtt_us) VALUES (?, ?, ?, 'RX', 'MODBUS_TCP', 1, 1, 3, 0, 1, ?, ?, 100)`)
	if err != nil {
		b.Fatal(err)
	}
	defer packetInsert.Close()
	eventInsert, err := tx.Prepare("INSERT INTO events (session_id, time, kind, detail) VALUES (?, ?, ?, 'test')")
	if err != nil {
		b.Fatal(err)
	}
	defer eventInsert.Close()
	for id := 1; id <= sessions; id++ {
		if _, err := sessionInsert.Exec(id, id); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < packets; i++ {
			status := "SUCCESS"
			if i%10 == 0 {
				status = "TIMEOUT"
			}
			if _, err := packetInsert.Exec(id, i, i, []byte{1, 3, 2, 0, 7}, status); err != nil {
				b.Fatal(err)
			}
		}
		for i := 0; i < events; i++ {
			kind := "READ_OK"
			if i%2 == 0 {
				kind = "READ_FAIL"
			}
			if _, err := eventInsert.Exec(id, i, kind); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkSessionsLargeArchive(b *testing.B) {
	for _, count := range []int{100, 2000} {
		b.Run(fmt.Sprintf("sessions_%d", count), func(b *testing.B) {
			r := queryRecorder(b)
			seedQueryArchive(b, r, count, 100, 10)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				sessions, err := r.Sessions(20)
				if err != nil || len(sessions) != 20 || sessions[0].ID != int64(count) || sessions[0].Packets != 100 || sessions[0].Errors != 10 || sessions[0].Faults != 5 {
					b.Fatalf("incomplete session summary: %+v, %v", sessions, err)
				}
			}
		})
	}
}

func BenchmarkInsertBatchSameSession(b *testing.B) {
	r := queryRecorder(b)
	seedQueryArchive(b, r, 1, 0, 0)
	batch := make([]item, 500)
	for i := range batch {
		batch[i] = item{1, modbus.Packet{Time: time.Unix(0, 0), RequestID: uint64(i + 1),
			Dir: modbus.DirRX, Mode: modbus.ModeTCP, Status: modbus.StatusSuccess, Raw: []byte{1, 3, 2, 0, 7}}}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := r.insert(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(500, "packets/op")
}
