package recorder

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func migrationDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, writeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestConcurrentMigrationsPreserveLegacyRows(t *testing.T) {
	// Separate connections model independent instances upgrading the same file.
	// Start them together on fresh old schemas so neither can rely on an earlier
	// instance having already added the compression columns.
	for attempt := 0; attempt < 5; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.sqlite3")
			db := migrationDatabase(t, path)
			legacy := strings.ReplaceAll(schema, "\traw_codec  INTEGER NOT NULL DEFAULT 0,\n", "")
			legacy = strings.ReplaceAll(legacy, "\tdata_codec INTEGER NOT NULL DEFAULT 0,\n", "")
			if _, err := db.Exec(legacy); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO sessions VALUES(1,1,NULL,'MODBUS_TCP','legacy',1);
				INSERT INTO packets(session_id,time,request_id,direction,protocol,slave,tx_id,function,address,count,raw,status,rtt_us)
				VALUES(1,1,1,'TX','MODBUS_TCP',1,0,3,0,1,x'0103','SENT',0);
				INSERT INTO events(session_id,time,kind,detail,analysis,tx)
				VALUES(1,1,'READ_FAIL','旧说明','旧分析',x'0103')`); err != nil {
				t.Fatal(err)
			}
			const instances = 12
			connections := make([]*sql.DB, instances)
			for i := range connections {
				connections[i] = migrationDatabase(t, path)
			}
			start := make(chan struct{})
			errs := make(chan error, instances)
			var ready, finished sync.WaitGroup
			ready.Add(instances)
			finished.Add(instances)
			for _, connection := range connections {
				go func() {
					defer finished.Done()
					ready.Done()
					<-start
					errs <- migrate(connection)
				}()
			}
			ready.Wait()
			close(start)
			finished.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent migration failed: %v", err)
				}
			}
			var raw, tx []byte
			var detail, analysis string
			var packetCodec, eventCodec int
			if err := db.QueryRow("SELECT raw, raw_codec FROM packets WHERE session_id = 1").Scan(&raw, &packetCodec); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT detail, analysis, tx, data_codec FROM events WHERE session_id = 1").Scan(&detail, &analysis, &tx, &eventCodec); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, []byte{1, 3}) || !bytes.Equal(tx, []byte{1, 3}) || detail != "旧说明" || analysis != "旧分析" || packetCodec != 0 || eventCodec != 0 {
				t.Fatal("migration changed legacy rows or their plain codec defaults")
			}
		})
	}
}

func TestMigrationFailureRollsBackAddedColumns(t *testing.T) {
	db := migrationDatabase(t, filepath.Join(t.TempDir(), "invalid.sqlite3"))
	// A damaged predecessor has events but no packets table. Migrating events
	// succeeds first; the later failure must not leave those additions behind.
	if _, err := db.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY, session_id INTEGER NOT NULL, time INTEGER NOT NULL,
		kind TEXT NOT NULL, detail TEXT NOT NULL);
		INSERT INTO events VALUES(1,1,1,'READ_FAIL','原始记录')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err == nil {
		t.Fatal("missing packets table must fail migration")
	}
	columns, err := databaseColumns(db, "events")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"window", "analysis", "tx", "rx", "data_codec"} {
		if columns[name] {
			t.Errorf("failed migration left added column %s", name)
		}
	}
	var detail string
	if err := db.QueryRow("SELECT detail FROM events WHERE id = 1").Scan(&detail); err != nil || detail != "原始记录" {
		t.Fatalf("failed migration changed existing data: %q, %v", detail, err)
	}
}
