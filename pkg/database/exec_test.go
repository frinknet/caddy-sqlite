package database

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBatcherStatementErrorDoesNotSinkSiblings(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 300 * time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO t (id, v) VALUES (1, 'first')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	var good *BatchResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		good, errs[0] = db.WriteStmt(context.Background(), `INSERT INTO t (id, v) VALUES (2, 'good')`, nil, false)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = db.WriteStmt(context.Background(), `INSERT INTO t (id, v) VALUES (1, 'dup')`, nil, false)
	}()
	wg.Wait()

	if errs[0] != nil {
		t.Fatalf("good write failed alongside a bad sibling: %v", errs[0])
	}
	if good.Affected != 1 {
		t.Fatalf("good write affected = %d, want 1", good.Affected)
	}
	if errs[1] == nil {
		t.Fatal("duplicate key must fail")
	}
	if got := db.gen.Load(); got != 1 {
		t.Fatalf("generation = %d, want 1: the batch still commits", got)
	}

	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM t WHERE id = 2`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("good row missing from a committed batch (rows = %d)", n)
	}
}

func TestBatcherRollbackStatementFailsWholeBatch(t *testing.T) {
	db := newBatchedDB(t, Profile{BatchSize: 8, BatchTimeout: 300 * time.Millisecond})
	if _, err := db.SQL().Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO t (id, v) VALUES (1, 'first')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = db.WriteStmt(context.Background(), `INSERT INTO t (id, v) VALUES (2, 'good')`, nil, false)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = db.WriteStmt(context.Background(), `INSERT OR ROLLBACK INTO t (id, v) VALUES (1, 'dup')`, nil, false)
	}()
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("write %d reported success, want an error for every reply in the batch", i)
		}
	}
	if got := db.gen.Load(); got != 0 {
		t.Fatalf("generation = %d, want 0: a failed commit must not invalidate the cache", got)
	}
}
