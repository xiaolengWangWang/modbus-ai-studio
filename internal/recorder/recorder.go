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
	connection_id TEXT NOT NULL DEFAULT '',
	error      TEXT
);
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	time       INTEGER NOT NULL, -- Unix 毫秒
	kind       TEXT    NOT NULL, -- CONNECT_FAIL、READ_FAIL、READ_OK、DISCONNECT、RECONNECT
	detail     TEXT    NOT NULL, -- 一行结论
	window     INTEGER NOT NULL DEFAULT 0,  -- 读取窗口编号，与连接有关的为 0
	analysis   TEXT    NOT NULL DEFAULT '', -- 原因分析和原始报文的逐字段解析
	tx         BLOB,                        -- 出错请求的原始报文
	rx         BLOB                         -- 收到的原始响应，超时为空
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
	db       *sql.DB
	dbMu     sync.RWMutex // protects database rotation and synchronous queries/writes
	root     string
	path     string
	sessions map[int64]sessionRecord // metadata for queued packets across file boundaries
	mu       sync.RWMutex            // 保护 closed，Close 之后的 Record 直接丢弃
	closed   bool
	ch       chan item
	done     chan struct{}
	Dropped  atomic.Int64 // 缓冲满或写入失败而丢掉的记录数，状态栏会提示
	running  sync.Map     // 本进程里开始了、还没结束的会话 ID
}

// DefaultPath 返回本机数据库位置：macOS 为 ~/Library/Application Support/ModbusAIStudio/packets.db，
// Windows 为 %AppData%\ModbusAIStudio\packets.db。写满后使用同目录下的编号文件。
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
	r := &Recorder{root: path, path: path, sessions: make(map[int64]sessionRecord), ch: make(chan item, buffer), done: make(chan struct{})}
	files, err := r.Files()
	if err != nil {
		return nil, err
	}
	if len(files) > 0 {
		r.path = files[len(files)-1]
	}
	r.db, err = openDatabase(r.path)
	if err == nil {
		var size, pages int64
		err = r.db.QueryRow("PRAGMA page_size").Scan(&size)
		if err == nil {
			err = r.db.QueryRow("PRAGMA page_count").Scan(&pages)
		}
		if err == nil && pages*size >= MaxFileBytes {
			err = r.rotateLocked()
		}
	}
	if err != nil {
		if r.db != nil {
			r.db.Close()
		}
		return nil, fmt.Errorf("打开报文数据库 %s 失败：%w", path, err)
	}
	go r.loop()
	return r, nil
}

func openDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	// max_page_count is connection-local; keep the configured connection alive.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err == nil {
		_, err = db.Exec(schema)
	}
	if err == nil {
		err = migrate(db)
	}
	if err == nil {
		var size int64
		err = db.QueryRow("PRAGMA page_size").Scan(&size)
		if err == nil {
			_, err = db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", MaxFileBytes/size))
		}
		if err == nil {
			_, err = db.Exec("PRAGMA journal_size_limit = 0")
		}
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, fmt.Errorf("打开报文数据库 %s 失败：%w", path, err)
	}
	return db, nil
}

// migrate 给旧库的 events 表补上后来加的列（0.10.0 起记录故障分析和原始报文）。
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(events)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, c := range []struct{ name, def string }{
		{"window", "INTEGER NOT NULL DEFAULT 0"}, {"analysis", "TEXT NOT NULL DEFAULT ''"}, {"tx", "BLOB"}, {"rx", "BLOB"},
	} {
		if !have[c.name] {
			if _, err := db.Exec(`ALTER TABLE events ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	columns, err := databaseColumns(db, "packets")
	if err != nil {
		return err
	}
	if !columns["connection_id"] {
		_, err = db.Exec(`ALTER TABLE packets ADD COLUMN connection_id TEXT NOT NULL DEFAULT ''`)
	}
	return err
}

func databaseColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
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
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	return r.insertLocked(batch)
}

func (r *Recorder) insertLocked(batch []item) error {
	err := r.writeLocked(func() error {
		for _, it := range batch {
			if err := r.ensureSessionLocked(it.session); err != nil {
				return err
			}
		}
		return r.insertBatch(batch)
	})
	// Split oversized transactions only when SQLite reports the size limit.
	if isFull(err) && len(batch) > 1 {
		middle := len(batch) / 2
		if err := r.insertLocked(batch[:middle]); err != nil {
			return err
		}
		return r.insertLocked(batch[middle:])
	}
	return err
}

func (r *Recorder) insertBatch(batch []item) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO packets (session_id, time, request_id, direction, protocol, slave, tx_id, function,
		address, count, raw, status, rtt_us, error, connection_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			byte(p.Function), p.Address, p.Count, p.Raw, string(p.Status), p.RTT.Microseconds(), e, p.ConnectionID); err != nil {
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
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	return r.db.Close()
}

// StartSession 在连接建立时调用，返回会话 ID。
func (r *Recorder) StartSession(mode modbus.Mode, target string, window int) (int64, error) {
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	id, err := nextSessionID()
	if err != nil {
		return 0, err
	}
	s := sessionRecord{start: time.Now().UnixMilli(), protocol: string(mode), target: target, window: window}
	err = r.writeLocked(func() error { return s.insert(r.db, id) })
	if err != nil {
		return 0, err
	}
	if err == nil {
		r.sessions[id] = s
		r.running.Store(id, true)
	}
	return id, err
}

// EndSession 在断开时调用。
func (r *Recorder) EndSession(id int64) error {
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	r.running.Delete(id)
	end := time.Now().UnixMilli()
	err := r.writeLocked(func() error {
		if err := r.ensureSessionLocked(id); err != nil {
			return err
		}
		_, err := r.db.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, end, id)
		return err
	})
	if err == nil {
		s := r.sessions[id]
		s.end = sql.NullInt64{Int64: end, Valid: true}
		r.sessions[id] = s
	}
	return err
}

// Running 表示会话是本进程里正在进行的连接。没有结束时间、又不在进行中的会话，是程序异常退出留下的。
func (r *Recorder) Running(id int64) bool {
	_, ok := r.running.Load(id)
	return ok
}

// 日志的种类。
const (
	EventConnectFail = "CONNECT_FAIL" // 连接或重连失败
	EventReadFail    = "READ_FAIL"    // 读取窗口开始出错（同一种错误连续出现只记第一次）
	EventReadOK      = "READ_OK"      // 读取窗口恢复正常
	EventDisconnect  = "DISCONNECT"   // 连接中途断开
	EventReconnect   = "RECONNECT"    // 重连成功
	EventConnect     = "CONNECT"      // 连接建立，TCP 记两端地址
)

// Event 是一条日志：连接失败、读取失败与恢复、断开、重连，带原因分析和出错时抓到的原始报文。
type Event struct {
	Time     time.Time
	Kind     string
	Window   int    // 读取窗口编号，与连接有关的为 0
	Detail   string // 一行结论
	Analysis string // 原因分析和原始报文的逐字段解析
	TX, RX   []byte // 出错请求和收到的响应，没有时为空
}

// Log 写一条日志。写得很少（同一种错误连续出现只记一次），直接同步写入。Time 为零值时取当前时间。
func (r *Recorder) Log(session int64, e Event) error {
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	return r.writeLocked(func() error {
		if err := r.ensureSessionLocked(session); err != nil {
			return err
		}
		_, err := r.db.Exec(`INSERT INTO events (session_id, time, kind, detail, window, analysis, tx, rx) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			session, e.Time.UnixMilli(), e.Kind, e.Detail, e.Window, e.Analysis, e.TX, e.RX)
		return err
	})
}

// Events 按时间正序返回会话的日志。
func (r *Recorder) Events(session int64) ([]Event, error) {
	r.dbMu.RLock()
	defer r.dbMu.RUnlock()
	rows, err := r.db.Query(`SELECT time, kind, detail, window, analysis, tx, rx FROM events WHERE session_id = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&t, &e.Kind, &e.Detail, &e.Window, &e.Analysis, &e.TX, &e.RX); err != nil {
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
	Faults      int // 连接失败、读取出错的次数（同一种错误连续出现算一次）
}

// Sessions 按时间倒序返回最近的会话。
func (r *Recorder) Sessions(limit int) ([]Session, error) {
	r.dbMu.RLock()
	defer r.dbMu.RUnlock()
	rows, err := r.db.Query(`SELECT s.id, s.started_at, s.ended_at, s.protocol, s.target, s.window,
		COUNT(p.id), COALESCE(SUM(p.status NOT IN ('SENT', 'SUCCESS')), 0),
		(SELECT COUNT(*) FROM events e WHERE e.session_id = s.id AND e.kind = 'DISCONNECT'),
		(SELECT COUNT(*) FROM events e WHERE e.session_id = s.id AND e.kind IN ('CONNECT_FAIL', 'READ_FAIL'))
		FROM sessions s LEFT JOIN packets p ON p.session_id = s.id
		GROUP BY s.id ORDER BY s.started_at DESC, s.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&s.ID, &start, &end, &s.Mode, &s.Target, &s.Window, &s.Packets, &s.Errors, &s.Disconnects, &s.Faults); err != nil {
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
	r.dbMu.RLock()
	defer r.dbMu.RUnlock()
	columns, err := databaseColumns(r.db, "packets")
	if err != nil {
		return nil, err
	}
	connectionColumn := "''"
	if columns["connection_id"] {
		connectionColumn = "connection_id"
	}
	rows, err := r.db.Query(`SELECT time, request_id, direction, protocol, slave, tx_id, function, address, count, raw,
		status, rtt_us, error, `+connectionColumn+` FROM packets WHERE session_id = ? ORDER BY id DESC LIMIT ?`, session, limit)
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
			&p.Status, &rtt, &e, &p.ConnectionID); err != nil {
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
func (r *Recorder) Prune(before time.Time) error {
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
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
