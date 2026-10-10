package recorder

import (
	"cmp"
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
	"time"

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

// Path is the file currently receiving records, which changes at rotation.
func (r *Recorder) Path() string {
	r.dbMu.RLock()
	defer r.dbMu.RUnlock()
	return r.path
}

// now is the clock for daily file names; tests replace it.
var now = time.Now

func today() string { return now().Format("20060102") }

// dayKey separates the date from the per-day number in daily file indexes.
const dayKey = 1_000_000

// fileIndex orders database files. Numbered files (packets-000001.db) use their
// number; daily files (packets-20261010-001.sqlite3) use date*dayKey + number.
// int64 keeps the date key intact on 32-bit ARM.
func (r *Recorder) fileIndex(path string) (int64, bool) {
	if !r.daily && path == r.root {
		return 0, true
	}
	ext := filepath.Ext(r.root)
	prefix := strings.TrimSuffix(filepath.Base(r.root), ext) + "-"
	name := filepath.Base(path)
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
		return 0, false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
	if !r.daily {
		n, err := strconv.ParseInt(middle, 10, 64)
		return n, err == nil && n > 0
	}
	day, num, ok := strings.Cut(middle, "-")
	d, err := strconv.ParseInt(day, 10, 64)
	n, err2 := strconv.ParseInt(num, 10, 64)
	if !ok || len(day) != 8 || err != nil || err2 != nil || n <= 0 || n >= dayKey {
		return 0, false
	}
	return d*dayKey + n, true
}

// fileDay is the YYYYMMDD date of a daily file, or "" for other files.
func (r *Recorder) fileDay(path string) string {
	if k, ok := r.fileIndex(path); ok && r.daily {
		return strconv.FormatInt(k/dayKey, 10)
	}
	return ""
}

// Files lists the original database and its numbered successors, oldest first.
// In daily mode it lists the daily files, oldest day first.
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
	slices.SortFunc(files, func(a, b string) int { x, _ := r.fileIndex(a); y, _ := r.fileIndex(b); return cmp.Compare(x, y) })
	return files, nil
}

// reserveFile creates the next empty database file and returns its path:
// packets-000002.db after packets-000001.db, or in daily mode the next number
// for today, packets-20261010-002.sqlite3.
func (r *Recorder) reserveFile() (string, error) {
	files, err := r.Files()
	if err != nil {
		return "", err
	}
	day := today()
	var n int64
	for _, f := range files {
		k, _ := r.fileIndex(f)
		switch {
		case !r.daily:
			n = max(n, k)
		case r.fileDay(f) == day:
			n = max(n, k%dayKey)
		}
	}
	ext := filepath.Ext(r.root)
	base := strings.TrimSuffix(r.root, ext)
	for {
		n++
		path := fmt.Sprintf("%s-%06d%s", base, n, ext)
		if r.daily {
			path = fmt.Sprintf("%s-%s-%03d%s", base, day, n, ext)
		}
		// Reserve a unique filename even when several application processes rotate.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
}

func (r *Recorder) rotateLocked() error {
	path, err := r.reserveFile()
	if err != nil {
		return err
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
	if r.daily && r.fileDay(r.path) != today() {
		// Past midnight: continue in a file for the new day. Sessions that
		// span midnight are copied into it by ensureSessionLocked.
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}
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
	return sql.Open(driverName, readDSN(path))
}

// Snapshot copies the database at path, including rows still in its WAL, into a
// new self-contained file dst (which must not exist). It is safe while writing.
func Snapshot(path, dst string) error {
	db, err := readDatabase(path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`VACUUM INTO ?`, dst)
	return err
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
