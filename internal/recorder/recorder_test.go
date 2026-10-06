package recorder

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

func TestRecordAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.StartSession(modbus.ModeRTUOverTCP, "192.168.1.20:502", 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Microsecond)
	want := []modbus.Packet{
		{Time: now, RequestID: 7, Dir: modbus.DirTX, Mode: modbus.ModeRTUOverTCP, Slave: 1, Function: 3, Address: 346, Count: 2,
			Raw: []byte{1, 3, 1, 0x5A, 0, 2, 0xE5, 0xE3}, Status: modbus.StatusSent},
		{Time: now.Add(12 * time.Millisecond), RequestID: 7, Dir: modbus.DirRX, Mode: modbus.ModeRTUOverTCP, Slave: 1, Function: 3,
			Address: 346, Count: 2, Raw: []byte{1, 3, 4, 0, 0, 0x41, 0x70, 0x9A, 0x33}, Status: modbus.StatusSuccess, RTT: 12 * time.Millisecond},
		{Time: now.Add(time.Second), RequestID: 8, Dir: modbus.DirRX, Mode: modbus.ModeRTUOverTCP, Slave: 1, Function: 3,
			Address: 600, Count: 4, Status: modbus.StatusTimeout, Err: modbus.ErrTimeout},
	}
	for _, p := range want {
		r.Record(id, p)
	}
	readFail := Event{Kind: EventReadFail, Window: 3, Detail: "窗口 3 · 40601–40604 · 超时：1000 ms 内无响应",
		Analysis: "设备有应答，但约 1100 ms 才到", TX: []byte{1, 3, 2, 0x58, 0, 4, 0xC4, 0x62}}
	for _, e := range []Event{
		readFail,
		{Kind: EventDisconnect, Detail: "发送 Slave 1 03 40347×2 后被设备断开：EOF"},
		{Kind: EventReconnect, Detail: "第 1 次重连成功"},
	} {
		if err := r.Log(id, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.EndSession(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil { // Close 会先写完缓冲
		t.Fatal(err)
	}
	r.Record(id, want[0]) // 关闭后记录不能 panic，只计入丢弃
	if r.Dropped.Load() != 1 {
		t.Errorf("关闭后的记录应计入丢弃，Dropped = %d", r.Dropped.Load())
	}

	r, err = Open(path) // 重新打开，数据还在
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ss, err := r.Sessions(10)
	if err != nil || len(ss) != 1 || ss[0].Packets != 3 || ss[0].Errors != 1 || ss[0].Target != "192.168.1.20:502" ||
		ss[0].Mode != modbus.ModeRTUOverTCP || ss[0].Window != 2 || ss[0].End.IsZero() || ss[0].Disconnects != 1 || ss[0].Faults != 1 {
		t.Fatalf("会话 %+v %v", ss, err)
	}
	ev, err := r.Events(id)
	if err != nil || len(ev) != 3 || ev[1].Kind != EventDisconnect || ev[2].Detail != "第 1 次重连成功" {
		t.Fatalf("日志 %+v %v", ev, err)
	}
	if e := ev[0]; e.Kind != EventReadFail || e.Window != 3 || e.Analysis != readFail.Analysis || !reflect.DeepEqual(e.TX, readFail.TX) || e.RX != nil {
		t.Errorf("读取失败日志 %+v", e)
	}
	got, err := r.Packets(id, 100)
	if err != nil || len(got) != 3 {
		t.Fatalf("报文 %d 条 %v", len(got), err)
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.Time.Equal(w.Time) || (g.Err == nil) != (w.Err == nil) {
			t.Errorf("第 %d 条时间或错误不符：%v %v / %v %v", i, g.Time, g.Err, w.Time, w.Err)
		}
		g.Time, w.Time, g.Err, w.Err = time.Time{}, time.Time{}, nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Errorf("第 %d 条：\n%+v\n%+v", i, g, w)
		}
	}
	if got[2].Err.Error() != modbus.ErrTimeout.Error() {
		t.Errorf("错误文字 %q", got[2].Err)
	}
	if last, _ := r.Packets(id, 1); len(last) != 1 || last[0].RequestID != 8 {
		t.Errorf("limit 应取最后的记录：%+v", last)
	}

	// 清理：记录早于截止时间的删掉，空的旧会话一起删
	if err := r.Prune(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ss, _ := r.Sessions(10); len(ss) != 0 {
		t.Errorf("清理后应没有会话：%+v", ss)
	}
}

// 缓冲满时 Record 立即返回并计数，绝不阻塞通信。
func TestRecordNeverBlocks(t *testing.T) {
	r, err := open(filepath.Join(t.TempDir(), "p.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, _ := r.StartSession(modbus.ModeTCP, "x", 1)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		r.Record(id, modbus.Packet{Time: time.Now(), Dir: modbus.DirTX, Mode: modbus.ModeTCP, Status: modbus.StatusSent})
	}
	if time.Since(start) > time.Second {
		t.Errorf("10000 次 Record 用了 %v，不应阻塞", time.Since(start))
	}
	if r.Dropped.Load() == 0 {
		t.Error("缓冲只有 1 条，应有丢弃")
	}
}

func TestOpenError(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir", "p.db")); err == nil {
		t.Error("目录不存在应报错")
	} else if errors.Unwrap(err) == nil {
		t.Errorf("错误应包含原因：%v", err)
	}
}

// 旧版本的库没有日志的分析和原始报文列，打开时自动补上，原有记录保留。
func TestMigrateOldEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packets.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id INTEGER PRIMARY KEY, started_at INTEGER NOT NULL, ended_at INTEGER,
		protocol TEXT NOT NULL, target TEXT NOT NULL, window INTEGER NOT NULL);
		CREATE TABLE events (id INTEGER PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		time INTEGER NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL);
		INSERT INTO sessions VALUES (1, 1, NULL, 'MODBUS_TCP', '10.0.0.1:502', 1);
		INSERT INTO events VALUES (1, 1, 2, 'DISCONNECT', '旧记录');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Log(1, Event{Kind: EventConnectFail, Detail: "重连失败", Analysis: "连接被拒绝"}); err != nil {
		t.Fatal(err)
	}
	ev, err := r.Events(1)
	if err != nil || len(ev) != 2 || ev[0].Detail != "旧记录" || ev[0].Analysis != "" || ev[1].Analysis != "连接被拒绝" {
		t.Fatalf("%+v %v", ev, err)
	}
}

// 本进程里开始、还没结束的会话是“进行中”；结束后不是。
func TestRunningSessions(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "packets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.StartSession(modbus.ModeTCP, "10.0.0.1:502", 1)
	if err != nil || !r.Running(id) {
		t.Fatalf("开始后应为进行中：%v %v", r.Running(id), err)
	}
	if err := r.EndSession(id); err != nil || r.Running(id) {
		t.Errorf("结束后不应再是进行中：%v %v", r.Running(id), err)
	}
}
