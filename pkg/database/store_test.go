package database

import (
	"context"
	"database/sql"
	"fmt"
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

func openFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := OpenDB(context.Background(), OpenConfig{Path: path})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAttachIsVisibleOnEveryConnection(t *testing.T) {
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.db")
	mediaPath := filepath.Join(dir, "media.db")

	app := openFile(t, appPath)
	if _, err := app.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, asset_id INTEGER)`); err != nil {
		t.Fatalf("create app: %v", err)
	}
	media := openFile(t, mediaPath)
	if _, err := media.Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create media: %v", err)
	}
	if _, err := media.Exec(`INSERT INTO assets (name) VALUES ('logo')`); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	if _, err := app.Exec(`INSERT INTO notes (asset_id) VALUES (1)`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "app", Path: appPath}, {Schema: "media", Path: mediaPath}},
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// A fresh connection for every query: the attach must not depend on reuse.
	pool.SetMaxIdleConns(0)

	for i := 0; i < 5; i++ {
		var name string
		if err := pool.QueryRow(
			`SELECT m.name FROM app.notes n JOIN media.assets m ON m.id = n.asset_id`).Scan(&name); err != nil {
			t.Fatalf("cross-schema join on connection %d: %v", i, err)
		}
		if name != "logo" {
			t.Fatalf("join returned %q", name)
		}
	}

	var inScratch int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&inScratch); err != nil {
		t.Fatalf("scratch lookup: %v", err)
	}
	if inScratch != 0 {
		t.Fatalf("scratch schema holds %d objects, want none: declared databases must not leak into main", inScratch)
	}
}

func TestAttachReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "media.db")

	media := openFile(t, mediaPath)
	if _, err := media.Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "media", Path: mediaPath, ReadOnly: true}},
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM media.assets`).Scan(&n); err != nil {
		t.Fatalf("read attached: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO media.assets (id) VALUES (1)`); err == nil {
		t.Fatal("write to a read-only attachment must fail")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAttachFailureSurfacesAtOpen(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "gone", Path: filepath.Join(dir, "missing", "nope.db")}},
	})
	if err == nil {
		t.Fatal("expected attaching a missing file to fail at open")
	}
	if !strings.Contains(err.Error(), "attach") {
		t.Fatalf("error should name the attach: %v", err)
	}
}

func TestAttachQuotesSchemaNames(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "odd.db")
	openFile(t, odd).Exec(`CREATE TABLE t (id INTEGER)`)

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: `weird "name"`, Path: odd}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM "weird ""name""".t`).Scan(&n); err != nil {
		t.Fatalf("query quoted schema: %v", err)
	}
}

func benchAttachFiles(b *testing.B, n int) ([]Attach, string) {
	b.Helper()
	dir := b.TempDir()
	main := filepath.Join(dir, "main.db")

	db, err := OpenDB(context.Background(), OpenConfig{Path: main})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO items (label) VALUES ('one')`); err != nil {
		b.Fatal(err)
	}
	db.Close()

	attaches := []Attach{{Schema: "db0", Path: main}}
	for i := 1; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("db%d.db", i))
		db, err := OpenDB(context.Background(), OpenConfig{Path: path})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, label TEXT)`); err != nil {
			b.Fatal(err)
		}
		db.Close()
		attaches = append(attaches, Attach{Schema: fmt.Sprintf("db%d", i), Path: path})
	}
	return attaches, main
}

func BenchmarkAttachPerConnection(b *testing.B) {
	for _, n := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("attached=%d", n), func(b *testing.B) {
			attaches, _ := benchAttachFiles(b, n)
			db, err := OpenDB(context.Background(), OpenConfig{Path: ":memory:", Attach: attaches})
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			db.SetMaxIdleConns(0)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := db.QueryRow(`SELECT COUNT(*) FROM db0.items`).Scan(new(int)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
