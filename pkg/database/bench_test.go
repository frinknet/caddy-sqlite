package database

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func BenchmarkClassify(b *testing.B) {
	cases := map[string]string{
		"select":  `SELECT id, label FROM items WHERE id = ? ORDER BY id`,
		"insert":  `INSERT INTO items (label) VALUES (?)`,
		"cte":     `WITH recent AS (SELECT id FROM items ORDER BY id DESC LIMIT 10) SELECT * FROM recent`,
		"pragma":  `PRAGMA wal_checkpoint`,
		"comment": `/* leading comment */ SELECT 1 -- trailing`,
	}
	for name, sqlText := range cases {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Classify(sqlText); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkDB(b *testing.B, profile Profile) *DB {
	b.Helper()
	pool := NewPool()
	path := filepath.Join(b.TempDir(), "bench.db")
	db, err := pool.Acquire(context.Background(), path, profile)
	if err != nil {
		b.Fatalf("acquire: %v", err)
	}
	b.Cleanup(func() { pool.Release(path) })
	return db
}

func BenchmarkBatcherSubmit(b *testing.B) {
	db := benchmarkDB(b, Profile{BatchSize: 128, BatchTimeout: time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.WriteStmt(ctx, `INSERT INTO items (label) VALUES (?)`, []any{"x"}, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBatcherSubmitParallel(b *testing.B) {
	db := benchmarkDB(b, Profile{BatchSize: 128, BatchTimeout: time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := db.WriteStmt(ctx, `INSERT INTO items (label) VALUES (?)`, []any{"x"}, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkQueryCachedHit(b *testing.B) {
	db := benchmarkDB(b, Profile{CacheTTL: time.Minute, CacheMax: 64})
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.SQL().Exec(`INSERT INTO items (label) VALUES (?)`, "row"); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := db.QueryCached(ctx, `SELECT id, label FROM items`, nil); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.QueryCached(ctx, `SELECT id, label FROM items`, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryDirect(b *testing.B) {
	db := benchmarkDB(b, Profile{})
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.SQL().Exec(`INSERT INTO items (label) VALUES (?)`, "row"); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryDirect(ctx, `SELECT id, label FROM items`, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 50 {
			b.Fatalf("rows = %d", len(rows))
		}
	}
}

func BenchmarkQueryCachedLargeResultSkipsCache(b *testing.B) {
	db := benchmarkDB(b, Profile{CacheTTL: time.Minute, CacheMax: 64})
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		if _, err := db.SQL().Exec(`INSERT INTO items (label) VALUES (?)`, "row"); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryCached(ctx, `SELECT id, label FROM items`, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 101 {
			b.Fatalf("rows = %d", len(rows))
		}
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
