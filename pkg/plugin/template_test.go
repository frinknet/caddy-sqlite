package plugin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

func newTemplateFuncs(t *testing.T) (*SQLiteTemplateFuncs, *database.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tpl.db")
	profile := database.Profile{BatchSize: 4, BatchTimeout: 5 * time.Millisecond, CacheTTL: time.Minute, CacheMax: 8}

	pool := database.NewPool()
	db, err := pool.Acquire(context.Background(), path, profile)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { pool.Release(path) })

	app := &SQLiteDBs{
		configs: map[string]*DBConfig{
			"t": {
				Name:         "t",
				Path:         path,
				BatchSize:    profile.BatchSize,
				BatchTimeout: profile.BatchTimeout,
				CacheTTL:     profile.CacheTTL,
				CacheMax:     profile.CacheMax,
			},
		},
		pool: pool,
	}
	return &SQLiteTemplateFuncs{app: app}, db
}

func TestTemplateQueryReads(t *testing.T) {
	m, db := newTemplateFuncs(t)
	if _, err := db.SQL().Exec(`INSERT INTO items (label) VALUES ('first'), ('second')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := m.query("t", `SELECT id, label FROM items ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows, ok := got.([]map[string]any)
	if !ok {
		t.Fatalf("query returned %T, want []map[string]any", got)
	}
	if len(rows) != 2 || rows[0]["label"] != "first" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestTemplateQueryWriteBumpsGenerationAndUncachesReads(t *testing.T) {
	m, _ := newTemplateFuncs(t)

	before, err := m.query("t", `SELECT COUNT(*) AS n FROM items`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := before.([]map[string]any)[0]["n"]; n != int64(0) {
		t.Fatalf("n = %v, want 0", n)
	}

	out, err := m.query("t", `INSERT INTO items (label) VALUES ('tpl')`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if rows, ok := out.([]map[string]any); !ok || len(rows) != 0 {
		t.Fatalf("non-RETURNING write returned %#v, want an empty slice", out)
	}

	// The read above was cached (one row learned from a :=100 row result), so
	// seeing the new row here proves the write moved the generation counter.
	after, err := m.query("t", `SELECT COUNT(*) AS n FROM items`)
	if err != nil {
		t.Fatalf("read after write: %v", err)
	}
	if n := after.([]map[string]any)[0]["n"]; n != int64(1) {
		t.Fatalf("n = %v, want 1: the write must invalidate the cached read", n)
	}
}

func TestTemplateQueryWriteWithReturning(t *testing.T) {
	m, _ := newTemplateFuncs(t)

	out, err := m.query("t", `INSERT INTO items (label) VALUES ('ret') RETURNING id, label`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	rows, ok := out.([]map[string]any)
	if !ok || len(rows) != 1 || rows[0]["label"] != "ret" {
		t.Fatalf("returned %#v", out)
	}
}

func TestTemplateQueryRejectsBadInput(t *testing.T) {
	m, _ := newTemplateFuncs(t)

	if _, err := m.query("nope", `SELECT 1`); err == nil {
		t.Fatal("expected an unknown database to fail")
	}
	if _, err := m.query("t", `NOT SQL AT ALL`); err == nil {
		t.Fatal("expected an unclassifiable statement to fail")
	}
}

func TestRunOptimizeStopsOnSignal(t *testing.T) {
	pool := database.NewPool()
	path := filepath.Join(t.TempDir(), "opt.db")
	db, err := pool.Acquire(context.Background(), path, database.Profile{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer pool.Release(path)

	stop := make(chan struct{})
	done := make(chan struct{})
	stopAt := time.Now().Add(30 * time.Millisecond)
	go func() {
		runOptimize(stop, db, 5*time.Millisecond, nil)
		close(done)
	}()

	time.Sleep(time.Until(stopAt))
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runOptimize did not return after its stop channel closed")
	}
}
