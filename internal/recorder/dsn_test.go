package recorder

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

func TestFileURIUsesAbsoluteEscapedPaths(t *testing.T) {
	path := "/tmp/记录 #100% & +/packets?.sqlite3"
	want := "file:///tmp/%E8%AE%B0%E5%BD%95%20%23100%25%20&%20+/packets%3F.sqlite3"
	if runtime.GOOS == "windows" {
		path = `D:\记录 #100% & +\packets?.sqlite3`
		want = "file:///D:/%E8%AE%B0%E5%BD%95%20%23100%25%20&%20+/packets%3F.sqlite3"
	}
	if got := fileURI(path); got != want {
		t.Fatalf("file URI = %q, want %q", got, want)
	}
}

func TestFileURIResolvesRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	got := fileURI(filepath.Join("sub", "..", "记录 #100% & +", "packets.sqlite3"))
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.ToSlash(filepath.Join(root, "记录 #100% & +", "packets.sqlite3"))
	if runtime.GOOS == "windows" {
		wantPath = "/" + wantPath
	}
	if !strings.HasPrefix(got, "file:///") || u.Path != wantPath || u.Fragment != "" || u.RawQuery != "" || u.Host != "" {
		t.Fatalf("relative URI did not resolve to the requested path: %q, %+v", got, u)
	}
}

func TestDriverDSNsKeepPathSeparateFromOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "记录 #100% & +", "packets.sqlite3")
	for name, dsn := range map[string]string{"read": readDSN(path), "write": writeDSN(path)} {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.ToSlash(path)
			if runtime.GOOS == "windows" {
				wantPath = "/" + wantPath
			}
			if u.Scheme != "file" || u.Path != wantPath || u.Fragment != "" {
				t.Fatalf("%s DSN changed the database path: %q", name, dsn)
			}
			if name == "read" && u.Query().Get("mode") != "ro" {
				t.Fatalf("read DSN lost read-only mode: %q", dsn)
			}
			if u.RawQuery == "" {
				t.Fatalf("%s DSN lost connection settings", name)
			}
		})
	}
}

func TestDatabasePathsWithURICharacters(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "记录 #100% & +")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "packets.sqlite3")
	r, err := Open(path)
	if err != nil {
		t.Fatalf("open database at the requested path: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	id, err := r.StartSession(modbus.ModeTCP, "special-path:502", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{1, 3, 0, 1}
	r.Record(id, modbus.Packet{Time: time.Now(), RequestID: 1, Dir: modbus.DirTX, Mode: modbus.ModeTCP, Raw: raw, Status: modbus.StatusSent})
	if err := r.Log(id, Event{Kind: EventReadFail, Detail: "DSN 路径测试"}); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSession(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at the requested path: %v", err)
	}
	sessions, err := r.SessionsFile(path, 10)
	if err != nil || len(sessions) != 1 || sessions[0].ID != id || sessions[0].Packets != 1 {
		t.Fatalf("read sessions from special path: %+v, %v", sessions, err)
	}
	packets, err := r.PacketsFile(path, id, 10)
	if err != nil || len(packets) != 1 || !bytes.Equal(packets[0].Raw, raw) {
		t.Fatalf("read packets from special path: %+v, %v", packets, err)
	}
	snapshot := filepath.Join(dir, "snapshot #100% & +.sqlite3")
	if err := Snapshot(path, snapshot); err != nil {
		t.Fatalf("snapshot from special path: %v", err)
	}
	packets, err = r.PacketsFile(snapshot, id, 10)
	if err != nil || len(packets) != 1 || !bytes.Equal(packets[0].Raw, raw) {
		t.Fatalf("read snapshot from special path: %+v, %v", packets, err)
	}
	events, err := r.EventsFile(snapshot, id)
	if err != nil || len(events) != 1 || events[0].Detail != "DSN 路径测试" {
		t.Fatalf("snapshot log was not preserved: %+v, %v", events, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "记录 #100% & +" || !entries[0].IsDir() {
		t.Fatalf("SQLite created files outside the requested directory: %+v, %v", entries, err)
	}
}
