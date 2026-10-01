// Package recorder 是 Packet Recorder 的 SQLite 一侧：把每次连接（会话）和全部收发记录存进本机数据库，
// 供事后查看（设计文档 14.3）。表里的 status、protocol 取值与 modbus.Status、modbus.Mode 一致。
package recorder

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"modbus-ai-studio/internal/modbus"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         INTEGER PRIMARY KEY,
	started_at INTEGER NOT NULL, -- Unix 毫秒
	ended_at   INTEGER,          -- 断开时间；NULL 表示没有正常断开
	protocol   TEXT    NOT NULL, -- MODBUS_TCP、RTU_OVER_TCP、ASCII_OVER_TCP、MODBUS_RTU、MODBUS_ASCII
	target     TEXT    NOT NULL, -- IP:端口，或串口和参数
	window     INTEGER NOT NULL  -- 主窗口编号
);
CREATE TABLE IF NOT EXISTS packets (
	id         INTEGER PRIMARY KEY,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	time       INTEGER NOT NULL, -- Unix 微秒
	request_id INTEGER NOT NULL, -- 同一请求的 TX 行和结果行相同
	direction  TEXT    NOT NULL, -- TX / RX
	protocol   TEXT    NOT NULL,
	slave      INTEGER NOT NULL,
	tx_id      INTEGER NOT NULL, -- 仅 Modbus TCP
	function   INTEGER NOT NULL,
	address    INTEGER NOT NULL,
	count      INTEGER NOT NULL,
	raw        BLOB,             -- 超时等没有收到字节的结果行为空
	status     TEXT    NOT NULL, -- SENT、SUCCESS、TIMEOUT、EXCEPTION、CRC_ERROR、LATE_RESPONSE ……
	rtt_us     INTEGER NOT NULL, -- 仅结果行
	error      TEXT
);
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	time       INTEGER NOT NULL, -- Unix 毫秒
	kind       TEXT    NOT NULL, -- DISCONNECT、RECONNECT
	detail     TEXT    NOT NULL  -- 原因和断开前最后一条请求
);
CREATE INDEX IF NOT EXISTS packets_session ON packets(session_id, id);
CREATE INDEX IF NOT EXISTS packets_time ON packets(time);
`

type item struct {
	session int64
	p       modbus.Packet
}

// Recorder 写入是异步的：Record 只把记录放进缓冲，后台每 200 ms 或满 500 条批量写一次事务，
// 磁盘再慢也不拖慢通信。缓冲满时丢弃并计数（Dropped）。
type Recorder struct {
	db      *sql.DB
	mu      sync.RWMutex // 保护 closed，Close 之后的 Record 直接丢弃
	closed  bool
	ch      chan item
	done    chan struct{}
	Dropped atomic.Int64 // 缓冲满或写入失败而丢掉的记录数，状态栏会提示
}

// DefaultPath 返回本机数据库位置：macOS 为 ~/Library/Application Support/ModbusAIStudio/packets.db，
// Windows 为 %AppData%\ModbusAIStudio\packets.db。所有主窗口、所有进程共用一个库。
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "ModbusAIStudio")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "packets.db"), nil
}

// Open 打开（不存在时创建）数据库。WAL 模式，多个进程可以同时写。
func Open(path string) (*Recorder, error) { return open(path, 10000) }

func open(path string, buffer int) (*Recorder, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err == nil {
		_, err = db.Exec(schema)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, fmt.Errorf("打开报文数据库 %s 失败：%w", path, err)
	}
	r := &Recorder{db: db, ch: make(chan item, buffer), done: make(chan struct{})}
	go r.loop()
	return r, nil
}

// Record 记录一条收发。不阻塞，可以在收发回调里直接调用。
func (r *Recorder) Record(session int64, p modbus.Packet) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.Dropped.Add(1)
		return
	}
	select {
	case r.ch <- item{session, p}:
	default:
		r.Dropped.Add(1)
	}
}

func (r *Recorder) loop() {
	defer close(r.done)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var batch []item
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.insert(batch); err != nil {
			r.Dropped.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case it, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			if batch = append(batch, it); len(batch) >= 500 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (r *Recorder) insert(batch []item) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO packets (session_id, time, request_id, direction, protocol, slave, tx_id, function,
		address, count, raw, status, rtt_us, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, it := range batch {
		p := it.p
		var e any
		if p.Err != nil {
			e = p.Err.Error()
		}
		if _, err := st.Exec(it.session, p.Time.UnixMicro(), p.RequestID, string(p.Dir), string(p.Mode), p.Slave, p.TxID,
			byte(p.Function), p.Address, p.Count, p.Raw, string(p.Status), p.RTT.Microseconds(), e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close 写完缓冲里剩下的记录后关闭数据库。
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.ch)
	r.mu.Unlock()
	<-r.done
	return r.db.Close()
}

// StartSession 在连接建立时调用，返回会话 ID。
func (r *Recorder) StartSession(mode modbus.Mode, target string, window int) (int64, error) {
	res, err := r.db.Exec(`INSERT INTO sessions (started_at, protocol, target, window) VALUES (?, ?, ?, ?)`,
		time.Now().UnixMilli(), string(mode), target, window)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndSession 在断开时调用。
func (r *Recorder) EndSession(id int64) error {
	_, err := r.db.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// 连接事件的种类。
const (
	EventDisconnect = "DISCONNECT"
	EventReconnect  = "RECONNECT"
)

// Event 记录一次断开或重连，写得很少，直接同步写入。
func (r *Recorder) Event(session int64, kind, detail string) error {
	_, err := r.db.Exec(`INSERT INTO events (session_id, time, kind, detail) VALUES (?, ?, ?, ?)`,
		session, time.Now().UnixMilli(), kind, detail)
	return err
}

// ConnEvent 是一条断开或重连记录。
type ConnEvent struct {
	Time   time.Time
	Kind   string
	Detail string
}

// Events 按时间正序返回会话的断开、重连记录。
func (r *Recorder) Events(session int64) ([]ConnEvent, error) {
	rows, err := r.db.Query(`SELECT time, kind, detail FROM events WHERE session_id = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnEvent
	for rows.Next() {
		var e ConnEvent
		var t int64
		if err := rows.Scan(&t, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Session 是一次连接的概况。
type Session struct {
	ID          int64
	Start, End  time.Time // End 为零值表示进行中或没有正常断开
	Mode        modbus.Mode
	Target      string
	Window      int
	Packets     int
	Errors      int // 状态不是 SENT / SUCCESS 的记录数
	Disconnects int // 连接中途断开的次数
}

// Sessions 按时间倒序返回最近的会话。
func (r *Recorder) Sessions(limit int) ([]Session, error) {
	rows, err := r.db.Query(`SELECT s.id, s.started_at, s.ended_at, s.protocol, s.target, s.window,
		COUNT(p.id), COALESCE(SUM(p.status NOT IN ('SENT', 'SUCCESS')), 0),
		(SELECT COUNT(*) FROM events e WHERE e.session_id = s.id AND e.kind = 'DISCONNECT')
		FROM sessions s LEFT JOIN packets p ON p.session_id = s.id
		GROUP BY s.id ORDER BY s.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&s.ID, &start, &end, &s.Mode, &s.Target, &s.Window, &s.Packets, &s.Errors, &s.Disconnects); err != nil {
			return nil, err
		}
		s.Start = time.UnixMilli(start)
		if end.Valid {
			s.End = time.UnixMilli(end.Int64)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Packets 返回会话最后 limit 条收发记录，按时间正序。
func (r *Recorder) Packets(session int64, limit int) ([]modbus.Packet, error) {
	rows, err := r.db.Query(`SELECT time, request_id, direction, protocol, slave, tx_id, function, address, count, raw,
		status, rtt_us, error FROM packets WHERE session_id = ? ORDER BY id DESC LIMIT ?`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modbus.Packet
	for rows.Next() {
		var p modbus.Packet
		var t, rtt int64
		var fn byte
		var e sql.NullString
		if err := rows.Scan(&t, &p.RequestID, &p.Dir, &p.Mode, &p.Slave, &p.TxID, &fn, &p.Address, &p.Count, &p.Raw,
			&p.Status, &rtt, &e); err != nil {
			return nil, err
		}
		p.Time, p.Function, p.RTT = time.UnixMicro(t), modbus.FunctionCode(fn), time.Duration(rtt)*time.Microsecond
		if e.Valid {
			p.Err = errors.New(e.String)
		}
		out = append(out, p)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// Prune 删除 before 之前的收发记录，以及已经没有记录、也早于 before 的会话。
// 目前只按时间清理，库太大时再加按大小清理
func (r *Recorder) Prune(before time.Time) error {
	if _, err := r.db.Exec(`DELETE FROM packets WHERE time < ?`, before.UnixMicro()); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DELETE FROM events WHERE time < ?`, before.UnixMilli()); err != nil {
		return err
	}
	_, err := r.db.Exec(`DELETE FROM sessions WHERE COALESCE(ended_at, started_at) < ?
		AND NOT EXISTS (SELECT 1 FROM packets WHERE session_id = sessions.id)
		AND NOT EXISTS (SELECT 1 FROM events WHERE session_id = sessions.id)`, before.UnixMilli())
	return err
}
