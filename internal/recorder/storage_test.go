package recorder

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

func TestCompressedPayloadsPersistAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.sqlite3")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	id, err := r.StartSession(modbus.ModeTCP, "compression:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte{0, 1, 0, 42}, 4096)
	e := Event{Kind: EventReadFail, Detail: strings.Repeat("读取失败。", 100), Analysis: strings.Repeat("寄存器响应超时，请检查网络。\n", 1000), TX: raw, RX: raw}
	if err := r.Log(id, e); err != nil {
		t.Fatal(err)
	}
	r.Record(id, modbus.Packet{Time: time.Now(), Dir: modbus.DirTX, Mode: modbus.ModeTCP, Status: modbus.StatusSent, Raw: raw})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := readDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored int
	if err := db.QueryRow("SELECT length(raw) FROM packets WHERE session_id = ?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored >= len(raw)/2 {
		t.Fatalf("报文未压缩：%d / %d", stored, len(raw))
	}
	if err := db.QueryRow("SELECT length(CAST(analysis AS BLOB)) FROM events WHERE session_id = ?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored >= len(e.Analysis)/2 {
		t.Fatalf("诊断内容未压缩：%d / %d", stored, len(e.Analysis))
	}
	ps, err := r.PacketsFile(path, id, 10)
	if err != nil || len(ps) != 1 || !bytes.Equal(ps[0].Raw, raw) {
		t.Fatalf("压缩报文读取失败：%v", err)
	}
	es, err := r.EventsFile(path, id)
	if err != nil || len(es) != 1 {
		t.Fatalf("压缩日志读取失败：%v", err)
	}
	if got := es[0]; got.Detail != e.Detail || got.Analysis != e.Analysis || !bytes.Equal(got.TX, raw) || !bytes.Equal(got.RX, raw) {
		t.Fatal("压缩还原改变了日志内容")
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("数据库完整性：%s %v", integrity, err)
	}
}

func TestRecordOwnsQueuedBytes(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "packets.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	id, err := r.StartSession(modbus.ModeTCP, "buffer:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{1, 3, 0, 42}
	r.dbMu.Lock()
	r.Record(id, modbus.Packet{Time: time.Now(), Dir: modbus.DirTX, Mode: modbus.ModeTCP, Status: modbus.StatusSent, Raw: raw})
	raw[0] = 255
	r.dbMu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	ps, err := r.PacketsFile(r.Path(), id, 1)
	if err != nil || len(ps) != 1 || !bytes.Equal(ps[0].Raw, []byte{1, 3, 0, 42}) {
		t.Fatalf("异步写入引用了调用者修改后的字节：%v %v", ps, err)
	}
}

func TestCloseReportsAsyncWriteFailureAndWaitsForAllCallers(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "packets.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	// 不存在的会话使后台事务失败；Close 必须将错误传给调用方。
	r.Record(123, modbus.Packet{Time: time.Now(), Dir: modbus.DirTX, Mode: modbus.ModeTCP, Status: modbus.StatusSent})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = r.Close() }()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Errorf("第 %d 个 Close 忽略了落盘失败", i)
		}
	}
	if got := r.Dropped.Load(); got != 1 {
		t.Fatalf("丢弃统计 = %d", got)
	}
}

func TestReadUnmigratedCompressedSchemaPredecessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.ReplaceAll(schema, "\traw_codec  INTEGER NOT NULL DEFAULT 0,\n", "")
	legacy = strings.ReplaceAll(legacy, "\tdata_codec INTEGER NOT NULL DEFAULT 0,\n", "")
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES(1,1,NULL,'MODBUS_TCP','legacy',1);
		INSERT INTO packets(session_id,time,request_id,direction,protocol,slave,tx_id,function,address,count,raw,status,rtt_us) VALUES(1,1,1,'TX','MODBUS_TCP',1,0,3,0,1,x'0103','SENT',0);
		INSERT INTO events(session_id,time,kind,detail,analysis,tx) VALUES(1,1,'READ_FAIL','旧说明','旧分析',x'0103')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(filepath.Join(t.TempDir(), "current.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ps, err := r.PacketsFile(path, 1, 10)
	if err != nil || len(ps) != 1 || !bytes.Equal(ps[0].Raw, []byte{1, 3}) {
		t.Fatalf("旧报文读取：%v", err)
	}
	es, err := r.EventsFile(path, 1)
	if err != nil || len(es) != 1 || es[0].Detail != "旧说明" || es[0].Analysis != "旧分析" {
		t.Fatalf("旧日志读取：%v", err)
	}
	db, err = readDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"packets", "events"} {
		cols, err := databaseColumns(db, table)
		if err != nil {
			t.Fatal(err)
		}
		if cols["raw_codec"] || cols["data_codec"] {
			t.Fatalf("只读旧库被修改：%s", table)
		}
	}
}

func TestLegacyDatabaseMigratesWithoutChangingPlainPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.ReplaceAll(schema, "\traw_codec  INTEGER NOT NULL DEFAULT 0,\n", "")
	legacy = strings.ReplaceAll(legacy, "\tdata_codec INTEGER NOT NULL DEFAULT 0,\n", "")
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES(1,1,NULL,'MODBUS_TCP','legacy',1);
		INSERT INTO events(session_id,time,kind,detail,analysis) VALUES(1,1,'READ_FAIL','旧说明','旧分析')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Log(1, Event{Kind: EventReadOK, Detail: "恢复", Analysis: strings.Repeat("新分析", 1000)}); err != nil {
		t.Fatal(err)
	}
	es, err := r.Events(1)
	if err != nil || len(es) != 2 || es[0].Detail != "旧说明" || es[0].Analysis != "旧分析" || es[1].Analysis != strings.Repeat("新分析", 1000) {
		t.Fatalf("迁移改变了记录：%v %v", es, err)
	}
}

func TestCorruptCompressedRowsReturnErrors(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "corrupt.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.StartSession(modbus.ModeTCP, "corrupt:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO packets(session_id,time,request_id,direction,protocol,slave,tx_id,function,address,count,raw,status,rtt_us,raw_codec) VALUES(?,1,1,'TX','MODBUS_TCP',1,0,3,0,1,x'0103','SENT',0,1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Packets(id, 10); err == nil {
		t.Fatal("损坏的压缩报文被接受")
	}
	if _, err := r.db.Exec(`INSERT INTO events(session_id,time,kind,detail,analysis,data_codec) VALUES(?,1,'READ_FAIL','说明',x'0103',4)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Events(id); err == nil {
		t.Fatal("损坏的压缩日志被接受")
	}
	if _, err := r.db.Exec("UPDATE events SET data_codec = 16"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Events(id); err == nil {
		t.Fatal("未知日志压缩标记被接受")
	}
}

func TestSnapshotKeepsCompressedRowsAndWALData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.sqlite3")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.StartSession(modbus.ModeTCP, "snapshot:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	analysis := strings.Repeat("快照包含已提交的WAL内容。", 1000)
	if err := r.Log(id, Event{Kind: EventReadFail, Detail: "快照", Analysis: analysis}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite3")
	if err := Snapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	es, err := r.EventsFile(snapshot, id)
	if err != nil || len(es) != 1 || es[0].Analysis != analysis {
		t.Fatalf("快照记录不完整：%v", err)
	}
	if err := Snapshot(path, snapshot); err == nil {
		t.Fatal("快照不应覆盖已有文件")
	}
}

func TestDailyRecorderRejectsWritesAfterClose(t *testing.T) {
	advance := fakeDay(t, time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local))
	r, err := OpenDaily(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.StartSession(modbus.ModeTCP, "closed:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	path := r.Path()
	advance(24 * time.Hour)
	if err := r.Log(id, Event{Kind: EventReadFail, Detail: "关闭后"}); err == nil {
		t.Error("关闭后的日志写入重新打开了数据库")
	}
	if _, err := r.StartSession(modbus.ModeTCP, "late:502", 2); err == nil {
		t.Error("关闭后创建会话成功")
	}
	if err := r.EndSession(id); err == nil {
		t.Error("关闭后更新会话成功")
	}
	if r.Path() != path {
		t.Error("关闭后发生了日切")
	}
	files, err := r.Files()
	if err != nil || len(files) != 1 {
		t.Fatalf("关闭后新建了文件：%v %v", files, err)
	}
}
