package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDSNDefaults(t *testing.T) {
	dsn := buildDSN(OpenConfig{Path: "/tmp/x.db"})

	if !strings.HasPrefix(dsn, "file:") || !strings.Contains(dsn, "x.db") {
		t.Fatalf("dsn = %s, want a file: URI for the configured path", dsn)
	}
	for _, want := range []string{
		"busy_timeout%285000%29",
		"cache_size%28-32000%29",
		"foreign_keys%28ON%29",
		"temp_store%28MEMORY%29",
		"journal_mode%28WAL%29",
		"synchronous%28NORMAL%29",
		"wal_autocheckpoint%282000%29",
		"mmap_size%28268435456%29",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn missing %q: %s", want, dsn)
		}
	}
}

func TestBuildDSNReadOnlySkipsWriterPragmas(t *testing.T) {
	dsn := buildDSN(OpenConfig{Path: "/tmp/x.db", ReadOnly: true})

	if !strings.Contains(dsn, "mode=ro") {
		t.Errorf("read-only dsn missing mode=ro: %s", dsn)
	}
	for _, unwanted := range []string{"journal_mode", "synchronous", "mmap_size"} {
		if strings.Contains(dsn, unwanted) {
			t.Errorf("read-only dsn should not set %s: %s", unwanted, dsn)
		}
	}
}

func TestBuildDSNMemoryIsSharedAndSkipsMmap(t *testing.T) {
	dsn := buildDSN(OpenConfig{Path: ":memory:"})

	if !strings.HasPrefix(dsn, "file::memory:?") {
		t.Fatalf("memory dsn = %s, want file::memory:?...", dsn)
	}
	if !strings.Contains(dsn, "cache=shared") {
		t.Fatalf("memory dsn must be shared so every pooled connection sees one DB: %s", dsn)
	}
	if strings.Contains(dsn, "mmap_size") {
		t.Fatalf("memory dsn should skip mmap: %s", dsn)
	}
}

func TestBuildDSNKeepsFileURLOptions(t *testing.T) {
	dsn := buildDSN(OpenConfig{Path: "file:/tmp/x.db?_txlock=immediate"})
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Fatalf("existing query options were dropped: %s", dsn)
	}
	if !strings.Contains(dsn, "_txlock=immediate&_pragma=") {
		t.Fatalf("pragma options must be appended with &: %s", dsn)
	}
}

func TestOpenDBAppliesPragmas(t *testing.T) {
	db, err := OpenDB(context.Background(), OpenConfig{Path: filepath.Join(t.TempDir(), "pragmas.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	cases := []struct {
		pragma string
		want   string
	}{
		{`PRAGMA journal_mode`, "wal"},
		{`PRAGMA foreign_keys`, "1"},
		{`PRAGMA busy_timeout`, "5000"},
		{`PRAGMA temp_store`, "2"},
		{`PRAGMA cache_size`, "-32000"},
		{`PRAGMA wal_autocheckpoint`, "2000"},
		{`PRAGMA mmap_size`, "268435456"},
	}
	for _, c := range cases {
		var got string
		if err := db.QueryRow(c.pragma).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.pragma, err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.pragma, got, c.want)
		}
	}
}

func TestOpenDBReadOnlyRejectsWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")

	rw, err := OpenDB(ctx, OpenConfig{Path: path})
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if _, err := rw.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rw.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rw.Close()

	ro, err := OpenDB(ctx, OpenConfig{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer ro.Close()

	var n int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	if _, err := ro.Exec(`INSERT INTO t (id) VALUES (2)`); err == nil {
		t.Fatal("expected a write to a read-only DB to fail")
	}
}

func TestOpenDBMemoryIsSharedAcrossConnections(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, OpenConfig{Path: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := writer.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer reader.Close()

	var n int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("second connection: %v", err)
	}
	if n != 1 {
		t.Fatalf("second connection saw %d rows, want 1: :memory: is not shared", n)
	}
}
