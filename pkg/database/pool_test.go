package database

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

var (
	slowFuncOnce sync.Once
	slowFuncErr  error
	slowCalls    atomic.Int64
)

func registerSlowFunc(t *testing.T) {
	t.Helper()
	slowFuncOnce.Do(func() {
		slowFuncErr = sqlite.RegisterScalarFunction("cs_test_slow", 1,
			func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
				slowCalls.Add(1)
				time.Sleep(50 * time.Millisecond)
				return int64(1), nil
			})
	})
	if slowFuncErr != nil {
		t.Fatalf("register cs_test_slow: %v", slowFuncErr)
	}
}

func newPoolDB(t *testing.T, profile Profile) (*Pool, *DB) {
	t.Helper()
	pool := NewPool()
	db, err := pool.Acquire(context.Background(), filepath.Join(t.TempDir(), "pool.db"), profile)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { pool.Release(db.Path) })
	return pool, db
}

func seedRows(t *testing.T, db *DB, n int) {
	t.Helper()
	if _, err := db.SQL().Exec(`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.SQL().Exec(`INSERT INTO t (id) VALUES (?)`, i+1); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func TestQueryCachedServesCachedRowsUntilGenMoves(t *testing.T) {
	_, db := newPoolDB(t, Profile{CacheTTL: time.Minute, CacheMax: 16})
	seedRows(t, db, 3)

	rows, err := db.QueryCached(context.Background(), `SELECT id FROM t ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	if _, err := db.SQL().Exec(`INSERT INTO t (id) VALUES (99)`); err != nil {
		t.Fatalf("lateral write: %v", err)
	}
	rows, err = db.QueryCached(context.Background(), `SELECT id FROM t ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the cached 3", len(rows))
	}

	db.BumpGen()
	rows, err = db.QueryCached(context.Background(), `SELECT id FROM t ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("read after gen bump: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 after the generation moved", len(rows))
	}
}

func TestQueryCachedDoesNotCacheLargeResults(t *testing.T) {
	_, db := newPoolDB(t, Profile{CacheTTL: time.Minute, CacheMax: 16})
	seedRows(t, db, 101)

	rows, err := db.QueryCached(context.Background(), `SELECT id FROM t`, nil)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(rows) != 101 {
		t.Fatalf("rows = %d, want 101", len(rows))
	}

	if _, err := db.SQL().Exec(`INSERT INTO t (id) VALUES (500)`); err != nil {
		t.Fatalf("lateral write: %v", err)
	}
	rows, err = db.QueryCached(context.Background(), `SELECT id FROM t`, nil)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(rows) != 102 {
		t.Fatalf("rows = %d, want 102: results over 100 rows are never cached", len(rows))
	}
}

func TestQueryCachedCoalescesConcurrentMisses(t *testing.T) {
	registerSlowFunc(t)
	slowCalls.Store(0)

	_, db := newPoolDB(t, Profile{CacheTTL: time.Minute, CacheMax: 16})
	const callers = 8

	var wg sync.WaitGroup
	results := make([][]map[string]any, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = db.QueryCached(context.Background(), `SELECT cs_test_slow(1) AS n`, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if len(results[i]) != 1 {
			t.Fatalf("caller %d: rows = %d, want 1", i, len(results[i]))
		}
	}

	if got := slowCalls.Load(); got != 1 {
		t.Fatalf("query ran %d times, want 1: concurrent misses must share one execution", got)
	}
}

func TestQueryCachedWithoutCacheQueriesEveryTime(t *testing.T) {
	_, db := newPoolDB(t, Profile{})
	if db.cache != nil {
		t.Fatal("no CacheTTL/CacheMax must mean no cache")
	}
	seedRows(t, db, 1)

	if _, err := db.QueryCached(context.Background(), `SELECT id FROM t`, nil); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO t (id) VALUES (2)`); err != nil {
		t.Fatalf("lateral write: %v", err)
	}
	rows, err := db.QueryCached(context.Background(), `SELECT id FROM t`, nil)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

func TestScanRowsNormalizesValues(t *testing.T) {
	_, db := newPoolDB(t, Profile{})

	rows, err := db.QueryDirect(context.Background(), `SELECT x'6869' AS blob, 'text' AS txt, 42 AS num, NULL AS absent`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := rows[0]["blob"]; got != "hi" {
		t.Fatalf("blob = %#v, want the string \"hi\"", got)
	}
	if got := rows[0]["txt"]; got != "text" {
		t.Fatalf("txt = %#v", got)
	}
	if got := rows[0]["num"]; got != int64(42) {
		t.Fatalf("num = %#v (%T), want int64(42)", got, got)
	}
	if rows[0]["absent"] != nil {
		t.Fatalf("absent = %#v, want nil", rows[0]["absent"])
	}
}

func TestPoolSharesAndReopensByPath(t *testing.T) {
	pool := NewPool()
	path := filepath.Join(t.TempDir(), "shared.db")

	first, err := pool.Acquire(context.Background(), path, Profile{BatchSize: 4})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := pool.Acquire(context.Background(), path, Profile{BatchSize: 4})
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if first != second {
		t.Fatal("same path must return the same handle while referenced")
	}
	if second.batcher == nil {
		t.Fatal("a batch profile should start a batcher")
	}

	if err := pool.Release(path); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := first.SQL().Exec(`CREATE TABLE still_open (id INTEGER)`); err != nil {
		t.Fatalf("handle closed while still referenced: %v", err)
	}

	if err := pool.Release(path); err != nil {
		t.Fatalf("final release: %v", err)
	}
	if _, err := first.SQL().Exec(`SELECT 1`); err == nil {
		t.Fatal("expected the pool to close the handle once the last reference is gone")
	}

	third, err := pool.Acquire(context.Background(), path, Profile{})
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if third == first {
		t.Fatal("re-acquire should open a fresh handle")
	}
	pool.Release(path)
}
