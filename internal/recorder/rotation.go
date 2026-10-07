package recorder

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"
	"modbus-ai-studio/internal/modbus"
)

// MaxFileBytes limits each SQLite database to 50 MB (decimal bytes).
// SQLite's temporary WAL/SHM files are separate from the database file.
const MaxFileBytes int64 = 50_000_000

type sessionRecord struct {
	start            int64
	end              sql.NullInt64
	protocol, target string
	window           int
}

func (s sessionRecord) insert(db *sql.DB, id int64) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO sessions (id, started_at, ended_at, protocol, target, window) VALUES (?, ?, ?, ?, ?, ?)`, id, s.start, s.end, s.protocol, s.target, s.window)
	return err
}

// Random process prefix plus increasing IDs avoids reusing IDs after rotation,
// while preserving creation order for sessions started in the same millisecond.
var sessionIDs struct {
	sync.Mutex
	next int64
}

func nextSessionID() (int64, error) {
	sessionIDs.Lock()
	defer sessionIDs.Unlock()
	if sessionIDs.next == 0 {
		n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
		if err != nil {
			return 0, err
		}
		sessionIDs.next = n.Int64() + 1
	}
	id := sessionIDs.next
	sessionIDs.next++
	return id, nil
}

func isFull(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrFull
}

// Path is the file currently receiving records, which changes at rotation.
func (r *Recorder) Path() string {
	r.dbMu.RLock()
	defer r.dbMu.RUnlock()
	return r.path
}

func (r *Recorder) fileIndex(path string) (int, bool) {
	if path == r.root {
		return 0, true
	}
	ext := filepath.Ext(r.root)
	prefix := strings.TrimSuffix(filepath.Base(r.root), ext) + "-"
	name := filepath.Base(path)
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext))
	return n, err == nil && n > 0
}

// Files lists the original database and its numbered successors, oldest first.
func (r *Recorder) Files() ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(r.root))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		path := filepath.Join(filepath.Dir(r.root), entry.Name())
		if _, ok := r.fileIndex(path); ok && !entry.IsDir() {
			files = append(files, path)
		}
	}
	slices.SortFunc(files, func(a, b string) int { x, _ := r.fileIndex(a); y, _ := r.fileIndex(b); return x - y })
	return files, nil
}

func (r *Recorder) rotateLocked() error {
	files, err := r.Files()
	if err != nil {
		return err
	}
	n := 0
	if len(files) > 0 {
		n, _ = r.fileIndex(files[len(files)-1])
	}
	ext := filepath.Ext(r.root)
	var path string
	for {
		n++
		path = fmt.Sprintf("%s-%06d%s", strings.TrimSuffix(r.root, ext), n, ext)
		// Reserve a unique filename even when several application processes rotate.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		break
	}
	next, err := openDatabase(path)
	if err != nil {
		return err
	}
	old := r.db
	r.db, r.path = next, path
	// Closing SQLite checkpoints the WAL when possible. Never rename/delete the
	// archive: a management tool or another process may still have it open.
	return old.Close()
}

func (r *Recorder) writeLocked(write func() error) error {
	err := write()
	if !isFull(err) {
		return err
	}
	if err := r.rotateLocked(); err != nil {
		return err
	}
	// One retry only: a single row larger than an empty file cannot be stored.
	return write()
}

func (r *Recorder) ensureSessionLocked(id int64) error {
	var exists int
	err := r.db.QueryRow("SELECT 1 FROM sessions WHERE id = ?", id).Scan(&exists)
	if err == nil {
		if _, ok := r.sessions[id]; !ok {
			var s sessionRecord
			if err := r.db.QueryRow("SELECT started_at, ended_at, protocol, target, window FROM sessions WHERE id = ?", id).Scan(&s.start, &s.end, &s.protocol, &s.target, &s.window); err != nil {
				return err
			}
			r.sessions[id] = s
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if s, ok := r.sessions[id]; ok {
		return s.insert(r.db, id)
	}
	files, err := r.Files()
	if err != nil {
		return err
	}
	for i := len(files) - 1; i >= 0; i-- {
		if files[i] == r.path {
			continue
		}
		db, err := readDatabase(files[i])
		if err != nil {
			return err
		}
		var s sessionRecord
		err = db.QueryRow("SELECT started_at, ended_at, protocol, target, window FROM sessions WHERE id = ?", id).Scan(&s.start, &s.end, &s.protocol, &s.target, &s.window)
		db.Close()
		if err == nil {
			r.sessions[id] = s
			return s.insert(r.db, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return fmt.Errorf("找不到报文会话 %d", id)
}

func readDatabase(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=ro&_busy_timeout=5000")
}

// File queries use independent read-only connections, including for the active
// file, so a UI refresh can safely overlap writes and a file rotation.
func (r *Recorder) SessionsFile(path string, limit int) ([]Session, error) {
	db, err := readDatabase(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return (&Recorder{db: db}).Sessions(limit)
}

func (r *Recorder) PacketsFile(path string, session int64, limit int) ([]modbus.Packet, error) {
	db, err := readDatabase(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return (&Recorder{db: db}).Packets(session, limit)
}

func (r *Recorder) EventsFile(path string, session int64) ([]Event, error) {
	db, err := readDatabase(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return (&Recorder{db: db}).Events(session)
}
