package database

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A statement that resolves with ROLLBACK tears down the transaction under the
// driver, so the batch's COMMIT fails. Every reply must carry an error and the
// generation must not move, or cached reads would go stale.

func newBatchedDB(t *testing.T, profile Profile) *DB {
	t.Helper()
	pool := NewPool()
	db, err := pool.Acquire(context.Background(), filepath.Join(t.TempDir(), "batch.db"), profile)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { pool.Release(db.Path) })
	return db
}

func TestBatcherGroupsConcurrentWritesIntoOneCommit(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 200 * time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	affected := make([]int64, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := db.WriteStmt(context.Background(), `INSERT INTO t (v) VALUES (?)`, []any{"x"}, false)
			errs[i], affected[i] = err, res.Affected
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if affected[i] != 1 {
			t.Fatalf("write %d affected = %d, want 1", i, affected[i])
		}
	}

	if got := db.gen.Load(); got != 1 {
		t.Fatalf("generation = %d, want 1: three concurrent writes should share one commit", got)
	}

	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

func TestBatcherReturnsRowsForReturning(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 20 * time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := db.WriteStmt(context.Background(), `INSERT INTO t (v) VALUES (?) RETURNING id, v`, []any{"returned"}, true)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !res.HasRows || len(res.Rows) != 1 {
		t.Fatalf("want one returned row, got %+v", res)
	}
	if res.Rows[0]["v"] != "returned" {
		t.Fatalf("returned row = %+v", res.Rows[0])
	}
}

func TestBatcherFlushesOnSize(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 2, BatchTimeout: 300 * time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.WriteStmt(context.Background(), `INSERT INTO t (v) VALUES ('x')`, nil, false); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := db.gen.Load(); got != 2 {
		t.Fatalf("generation = %d, want 2: size 2 means two commits for three writes", got)
	}
}

func TestBatcherRejectsAfterClose(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 20 * time.Millisecond})
	if err := db.batcher.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := db.WriteStmt(context.Background(), `SELECT 1`, nil, false); err == nil {
		t.Fatal("expected closed batcher to reject the write")
	} else if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBatcherHonorsContextCancellation(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 300 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := db.WriteStmt(ctx, `SELECT 1`, nil, false)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit ignored context cancellation")
	}
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
