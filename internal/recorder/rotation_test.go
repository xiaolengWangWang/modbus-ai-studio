package recorder

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

func TestOversizedLegacyFileIsPreservedAndNewWritesUseSuccessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES (1, 1, NULL, 'MODBUS_TCP', 'legacy:502', 1);
		INSERT INTO events (session_id, time, kind, detail, tx) VALUES (1, 1, 'READ_FAIL', '旧记录', zeroblob(51000000))`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.Stat(path)
	if err != nil || before.Size() < 50000000 {
		t.Fatalf("旧库应超出上限：%v", err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Path() == path {
		t.Fatal("超限旧库应归档，新记录写入新文件")
	}
	if err := r.Log(1, Event{Kind: EventReadOK, Detail: "新记录"}); err != nil {
		t.Fatal(err)
	}
	events, err := r.EventsFile(path, 1)
	if err != nil || len(events) != 1 || events[0].Detail != "旧记录" || len(events[0].TX) != 51000000 {
		t.Fatalf("旧数据应完整保留：%d %v", len(events), err)
	}
	current, err := r.Events(1)
	if err != nil || len(current) != 1 || current[0].Detail != "新记录" {
		t.Fatalf("应在新文件继续：%v", err)
	}
}

func TestQueuedPacketsSurviveRotationAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.StartSession(modbus.ModeTCP, "packets:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	const count = 125
	for i := 0; i < count; i++ {
		r.Record(id, modbus.Packet{Time: time.Now(), RequestID: uint64(i + 1), Dir: modbus.DirTX, Mode: modbus.ModeTCP, Raw: make([]byte, 600000), Status: modbus.StatusSent})
	}
	if err := r.EndSession(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if dropped := r.Dropped.Load(); dropped != 0 {
		t.Fatalf("文件切换不能丢失待写报文：%d", dropped)
	}
	files, err := r.Files()
	if err != nil || len(files) < 2 {
		t.Fatalf("未切换文件：%v %v", files, err)
	}
	ids := make(map[uint64]bool)
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.Size() > 50000000 {
			t.Fatalf("大小超限：%s %v", file, err)
		}
		packets, err := r.PacketsFile(file, id, count)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range packets {
			if ids[p.RequestID] || len(p.Raw) != 600000 {
				t.Fatalf("重复或损坏报文：%d", p.RequestID)
			}
			ids[p.RequestID] = true
		}
	}
	if len(ids) != count {
		t.Fatalf("报文不完整：%d / %d", len(ids), count)
	}
}

func TestConcurrentRecordersRotateWithoutOverwritingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var wg sync.WaitGroup
	for _, r := range []*Recorder{first, second} {
		id, err := r.StartSession(modbus.ModeTCP, "multi:502", 1)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 35; i++ {
				if err := r.Log(id, Event{Kind: EventReadFail, Detail: "多开", TX: make([]byte, 900000)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	first.Close()
	second.Close()
	files, err := first.Files()
	if err != nil || len(files) < 2 {
		t.Fatalf("未切换文件：%v %v", files, err)
	}
	total := 0
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.Size() > 50000000 {
			t.Fatalf("大小超限：%s %v", file, err)
		}
		ss, err := first.SessionsFile(file, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			total += s.Faults
		}
	}
	if total != 70 {
		t.Fatalf("多开切换丢失数据：%d / 70", total)
	}
}

// Real payloads exceed 50 MB: rotation must preserve every row and session FK.
func TestDatabaseRotatesAt50MBWithoutLosingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	id, err := r.StartSession(modbus.ModeTCP, "rotation:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	const count = 65
	for i := 0; i < count; i++ {
		if err := r.Log(id, Event{Kind: EventReadFail, Detail: fmt.Sprint(i), TX: make([]byte, 900000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.EndSession(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.db"))
	if err != nil || len(files) < 2 {
		t.Fatalf("超过 50 MB 后应创建新文件，旧文件保留：%v %v", files, err)
	}
	total := 0
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 50000000 {
			t.Errorf("单个文件超出 50 MB：%s %d", file, info.Size())
		}
		db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(file)+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM events WHERE session_id = ?", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
		var integrity string
		if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("%s: %s %v", file, integrity, err)
		}
		rows, err := db.Query("PRAGMA foreign_key_check")
		if err != nil || rows.Next() {
			t.Fatalf("会话引用断开：%s %v", file, err)
		}
		rows.Close()
		db.Close()
	}
	if total != count {
		t.Fatalf("切换文件丢失记录：%d / %d", total, count)
	}
	r, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := r.Sessions(10)
	if err != nil || len(ss) != 1 || ss[0].End.IsZero() {
		t.Fatalf("重启应继续最新文件且保留连接结束状态：%+v %v", ss, err)
	}
	if err := r.Log(id, Event{Time: time.Now(), Kind: EventReadOK, Detail: "重启后"}); err != nil {
		t.Fatal(err)
	}
}
