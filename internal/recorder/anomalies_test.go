package recorder

import (
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// 异常行另存进 anomalies，带同一请求的发送帧；正常收发和取消不进这张表。
func TestAbnormalPacketsStoredWithTheirRequestFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.StartSession(modbus.ModeTCP, "synthetic", 1)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	base := modbus.Packet{Mode: modbus.ModeTCP, Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 8, Count: 2, ConnectionID: "c1"}
	at := time.Now()
	rec := func(req uint64, dir modbus.Direction, st modbus.Status, raw string, err error) {
		p := base
		at = at.Add(time.Millisecond)
		p.Time, p.RequestID, p.Dir, p.Status, p.Raw, p.Err = at, req, dir, st, frame(raw), err
		r.Record(id, p)
	}
	echo := &modbus.NonstandardError{Function: modbus.FuncReadHoldingRegisters, PDU: frame("8900080002")}
	rec(1, modbus.DirTX, modbus.StatusSent, "000e00000006010300080002", nil)
	rec(1, modbus.DirRX, modbus.StatusNonstandard, "000e00000006018900080002", echo)
	rec(2, modbus.DirTX, modbus.StatusSent, "000f00000006010300000002", nil)
	rec(2, modbus.DirRX, modbus.StatusSuccess, "000f0000000701030400000000", nil)
	rec(3, modbus.DirTX, modbus.StatusSent, "001000000006010300080002", nil)
	rec(3, modbus.DirRX, modbus.StatusLate, "000e00000006018900080002", nil)
	rec(3, modbus.DirRX, modbus.StatusTimeout, "", modbus.ErrTimeout)
	rec(4, modbus.DirTX, modbus.StatusSent, "001100000006010300080002", nil)
	rec(4, modbus.DirRX, modbus.StatusCancelled, "", nil)
	rec(5, modbus.DirTX, modbus.StatusConnectionError, "001200000006010300080002", nil)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := (&Recorder{}).AnomaliesFile(path, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		status modbus.Status
		tx, rx string
	}
	want := []row{
		{modbus.StatusNonstandard, "000e00000006010300080002", "000e00000006018900080002"},
		{modbus.StatusLate, "", "000e00000006018900080002"}, // 晚到帧不一定属于这条请求，不配对
		{modbus.StatusTimeout, "001000000006010300080002", ""},
		{modbus.StatusConnectionError, "001200000006010300080002", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("异常表应只有 %d 行（正常响应和取消不算），得到 %d：%+v", len(want), len(got), got)
	}
	for i, w := range want {
		a := got[i]
		if a.Packet.Status != w.status || hex.EncodeToString(a.TX) != w.tx || hex.EncodeToString(a.Packet.Raw) != w.rx {
			t.Errorf("第 %d 行：%s tx=%x rx=%x，期望 %+v", i, a.Packet.Status, a.TX, a.Packet.Raw, w)
		}
		if a.Packet.Address != 8 || a.Packet.Count != 2 || a.Packet.ConnectionID != "c1" {
			t.Errorf("第 %d 行请求参数丢失：%+v", i, a.Packet)
		}
		if (w.tx != "") == a.TXTime.IsZero() {
			t.Errorf("第 %d 行发送时间：%v", i, a.TXTime)
		}
	}
	if got[0].Packet.Err == nil || got[0].Packet.Err.Error() != echo.Error() {
		t.Errorf("错误说明丢失：%v", got[0].Packet.Err)
	}

	// packets 表照常保留全部记录
	ps, err := (&Recorder{}).PacketsFile(path, id, 100)
	if err != nil || len(ps) != 10 {
		t.Fatalf("packets 表应保留全部 10 行：%d %v", len(ps), err)
	}
}

// 1.4.0 之前的文件没有 anomalies 表：按路径只读查看时返回空，不报错，也不改文件。
func TestAnomaliesFromArchiveWithoutTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `DROP TABLE anomalies;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got, err := (&Recorder{}).AnomaliesFile(path, 1, 10)
	if err != nil || got != nil {
		t.Fatalf("旧文件应返回空：%v %v", got, err)
	}
	db, _ = sql.Open(driverName, path)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'anomalies'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("只读查看不能给旧文件建表：%d %v", n, err)
	}
}

// 旧文件被当作当前文件打开写入时补建 anomalies 表；Prune 一并清理。
func TestAnomaliesTableAddedToOldFileAndPruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `DROP TABLE anomalies;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, _ := r.StartSession(modbus.ModeTCP, "synthetic", 1)
	old := time.Now().Add(-time.Hour)
	r.Record(id, modbus.Packet{Time: old, RequestID: 1, Dir: modbus.DirRX, Mode: modbus.ModeTCP, Status: modbus.StatusTimeout, Err: modbus.ErrTimeout})
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := r.Anomalies(id, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("异常行没有写入补建的表")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := r.Prune(time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Anomalies(id, 10); err != nil || len(got) != 0 {
		t.Fatalf("Prune 应清掉旧的异常行：%v %v", got, err)
	}
}
