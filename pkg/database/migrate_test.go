package database

import (
	"context"
	"path/filepath"
	"testing"

	"testing/fstest"
)

func openMigrationDB(t *testing.T) *DB {
	t.Helper()
	pool := NewPool()
	db, err := pool.Acquire(context.Background(), filepath.Join(t.TempDir(), "m.db"), Profile{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { pool.Release(db.Path) })
	return db
}

func userVersion(t *testing.T, db *DB) int {
	t.Helper()
	var v int
	if err := db.SQL().QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func TestApplyRunsMigrationsInVersionOrder(t *testing.T) {
	db := openMigrationDB(t)
	fsys := fstest.MapFS{
		"0002_second.sql": &fstest.MapFile{Data: []byte(`INSERT INTO t (v) VALUES ('second')`)},
		"notes.txt":       &fstest.MapFile{Data: []byte(`not a migration`)},
		"notaversion.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE ignored (id INTEGER)`)},
		"0001_first.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT);
			 INSERT INTO t (v) VALUES ('first');`)},
	}

	if err := Apply(context.Background(), db.SQL(), fsys, "."); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Fatalf("user_version = %d, want 2", got)
	}

	rows, err := db.QueryDirect(context.Background(), `SELECT v FROM t ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 2 || rows[0]["v"] != "first" || rows[1]["v"] != "second" {
		t.Fatalf("migrations ran out of order: %+v", rows)
	}

	var ignored int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'ignored'`).Scan(&ignored); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if ignored != 0 {
		t.Fatal("a file without a numeric version prefix was applied")
	}
}

func TestApplyIsIdempotentAndAppliesOnlyNewFiles(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()
	first := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`)},
	}
	if err := Apply(ctx, db.SQL(), first, "."); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	if err := Apply(ctx, db.SQL(), first, "."); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := userVersion(t, db); got != 1 {
		t.Fatalf("user_version = %d after re-apply, want 1", got)
	}

	second := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`)},
		"0002_add.sql":  &fstest.MapFile{Data: []byte(`INSERT INTO t (v) VALUES ('only-once')`)},
	}
	if err := Apply(ctx, db.SQL(), second, "."); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Fatalf("user_version = %d, want 2", got)
	}

	if err := Apply(ctx, db.SQL(), second, "."); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1: an applied migration must not run twice", n)
	}
}

func TestApplyRollsBackFailedMigration(t *testing.T) {
	db := openMigrationDB(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_good.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE kept (id INTEGER)`)},
	}
	if err := Apply(ctx, db.SQL(), fsys, "."); err != nil {
		t.Fatalf("apply: %v", err)
	}

	broken := fstest.MapFS{
		"0001_good.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE kept (id INTEGER)`)},
		"0002_broken.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE half (id INTEGER);
			 THIS IS NOT SQL;`)},
	}
	if err := Apply(ctx, db.SQL(), broken, "."); err == nil {
		t.Fatal("expected the broken migration to fail")
	}
	if got := userVersion(t, db); got != 1 {
		t.Fatalf("user_version = %d after a failed migration, want 1", got)
	}

	var half int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'`).Scan(&half); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if half != 0 {
		t.Fatal("a failed migration left its partial work behind")
	}
}

func BenchmarkApplyMigration(b *testing.B) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`)},
		"0002_seed.sql": &fstest.MapFile{Data: []byte(`INSERT INTO items (label) VALUES ('a')`)},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db := benchmarkDB(b, Profile{})
		if err := Apply(ctx, db.SQL(), fsys, "."); err != nil {
			b.Fatal(err)
		}
	}
}
