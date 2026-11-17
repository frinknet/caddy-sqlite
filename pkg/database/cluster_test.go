package database

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestCluster(t *testing.T, profile Profile) (*Cluster, string, string) {
	t.Helper()
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.db")
	mediaPath := filepath.Join(dir, "media.db")

	app := openFile(t, appPath)
	if _, err := app.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT, asset_id INTEGER)`); err != nil {
		t.Fatalf("create app: %v", err)
	}
	media := openFile(t, mediaPath)
	if _, err := media.Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`); err != nil {
		t.Fatalf("create media: %v", err)
	}

	c, err := OpenCluster(context.Background(),
		[]Attach{{Schema: "app", Path: appPath}, {Schema: "media", Path: mediaPath}},
		[]string{"app", "media"},
		profile)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, appPath, mediaPath
}

func TestClusterReadsJoinAcrossSchemas(t *testing.T) {
	c, _, _ := openTestCluster(t, Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond})

	if _, err := c.WriteStmt(context.Background(), "media", `INSERT INTO media.assets (name) VALUES ('logo')`, nil, false); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	if _, err := c.WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body, asset_id) VALUES ('hello', 1)`, nil, false); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	rows, err := c.QueryCached(context.Background(),
		`SELECT n.body, m.name FROM app.notes n JOIN media.assets m ON m.id = n.asset_id`, nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "logo" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestClusterWritesRouteToTheirOwnSchema(t *testing.T) {
	c, appPath, mediaPath := openTestCluster(t, Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond})

	if _, err := c.WriteStmt(context.Background(), "app", `INSERT INTO app.notes (body) VALUES ('a')`, nil, false); err != nil {
		t.Fatalf("write app: %v", err)
	}
	if _, err := c.WriteStmt(context.Background(), "media", `INSERT INTO media.assets (name) VALUES ('m')`, nil, false); err != nil {
		t.Fatalf("write media: %v", err)
	}

	var appRows, mediaRows int
	if err := openFile(t, appPath).QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&appRows); err != nil {
		t.Fatalf("read app file: %v", err)
	}
	if err := openFile(t, mediaPath).QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&mediaRows); err != nil {
		t.Fatalf("read media file: %v", err)
	}
	if appRows != 1 || mediaRows != 1 {
		t.Fatalf("files hold app=%d media=%d, want 1 each", appRows, mediaRows)
	}
}

func TestClusterUnknownSchemaIsAnError(t *testing.T) {
	c, _, _ := openTestCluster(t, Profile{})

	_, err := c.WriteStmt(context.Background(), "nope", `INSERT INTO nope.t (x) VALUES (1)`, nil, false)
	if err == nil {
		t.Fatal("expected an unknown schema to fail")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClusterScratchWritesStayInMemory(t *testing.T) {
	c, appPath, _ := openTestCluster(t, Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond})
	ctx := context.Background()

	if _, err := c.WriteStmt(ctx, ScratchSchema, `CREATE TABLE hits (id INTEGER PRIMARY KEY, path TEXT)`, nil, false); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	if _, err := c.WriteStmt(ctx, ScratchSchema, `INSERT INTO main.hits (path) VALUES ('/x')`, nil, false); err != nil {
		t.Fatalf("scratch write: %v", err)
	}

	rows, err := c.QueryDirect(ctx, `SELECT path FROM main.hits`, nil)
	if err != nil {
		t.Fatalf("scratch read: %v", err)
	}
	if len(rows) != 1 || rows[0]["path"] != "/x" {
		t.Fatalf("rows = %+v", rows)
	}

	var appTables int
	if err := openFile(t, appPath).QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'hits'`).Scan(&appTables); err != nil {
		t.Fatalf("read app file: %v", err)
	}
	if appTables != 0 {
		t.Fatal("scratch data leaked into a declared database file")
	}
}

func TestClusterWriteInvalidatesEveryCachedRead(t *testing.T) {
	c, _, _ := openTestCluster(t, Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond, CacheTTL: time.Minute, CacheMax: 16})
	ctx := context.Background()

	if _, err := c.WriteStmt(ctx, "app", `INSERT INTO app.notes (body) VALUES ('a')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, err := c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if first[0]["n"] != int64(1) {
		t.Fatalf("n = %v, want 1", first[0]["n"])
	}

	// A write to a *different* schema must still invalidate this cached read,
	// because the cache and the counter are cluster-wide.
	if _, err := c.WriteStmt(ctx, "media", `INSERT INTO media.assets (name) VALUES ('m')`, nil, false); err != nil {
		t.Fatalf("write media: %v", err)
	}
	if got := c.Gen(); got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}

	second, err := c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil)
	if err != nil {
		t.Fatalf("read after write: %v", err)
	}
	if second[0]["n"] != int64(1) {
		t.Fatalf("n = %v, want 1", second[0]["n"])
	}
}

func TestClusterCachesReadsUntilWrite(t *testing.T) {
	c, appPath, _ := openTestCluster(t, Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond, CacheTTL: time.Minute, CacheMax: 16})
	ctx := context.Background()

	if _, err := c.WriteStmt(ctx, "app", `INSERT INTO app.notes (body) VALUES ('a')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// A write that bypasses the cluster (another process, the sqlite3 CLI)
	// leaves the cache alone until something bumps the generation.
	if _, err := openFile(t, appPath).Exec(`INSERT INTO notes (body) VALUES ('lateral')`); err != nil {
		t.Fatalf("lateral write: %v", err)
	}

	rows, err := c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil)
	if err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if rows[0]["n"] != int64(1) {
		t.Fatalf("n = %v, want the cached 1", rows[0]["n"])
	}

	if _, err := c.WriteStmt(ctx, "app", `INSERT INTO app.notes (body) VALUES ('b')`, nil, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	rows, err = c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil)
	if err != nil {
		t.Fatalf("read after write: %v", err)
	}
	if rows[0]["n"] != int64(3) {
		t.Fatalf("n = %v, want 3", rows[0]["n"])
	}
}

func TestClusterReadOnlyAttachmentRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "media.db")
	openFile(t, mediaPath).Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY)`)

	c, err := OpenCluster(context.Background(),
		[]Attach{{Schema: "media", Path: mediaPath, ReadOnly: true}},
		nil,
		Profile{})
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	defer c.Close()

	// The cluster refuses it before SQLite sees it: a read-only schema has no writer.
	if _, err := c.WriteStmt(context.Background(), "media", `INSERT INTO media.assets (id) VALUES (1)`, nil, false); err == nil {
		t.Fatal("expected a write to a schema with no writer to fail")
	}

	if _, err := c.Anchor().Exec(`INSERT INTO media.assets (id) VALUES (1)`); err == nil {
		t.Fatal("expected SQLite to reject the write on a mode=ro attachment")
	}
}

func TestClusterTimeoutCapsStatements(t *testing.T) {
	c, _, _ := openTestCluster(t, Profile{Timeout: 20 * time.Millisecond})

	_, err := c.QueryDirect(context.Background(), `SELECT 1 AS n`, nil)
	if err != nil {
		t.Fatalf("fast query should succeed: %v", err)
	}

	// A deliberately slow statement: recursive CTE counting to 30 million.
	_, err = c.QueryDirect(context.Background(),
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 30000000) SELECT COUNT(*) FROM c`, nil)
	if err == nil {
		t.Fatal("expected the statement to hit the configured timeout")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClusterConcurrentReadsAndWrites(t *testing.T) {
	c, _, _ := openTestCluster(t, Profile{BatchSize: 32, BatchTimeout: time.Millisecond, CacheTTL: time.Second, CacheMax: 64})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := c.WriteStmt(ctx, "app", `INSERT INTO app.notes (body) VALUES ('x')`, nil, false); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := c.QueryCached(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	wg.Wait()

	rows, err := c.QueryDirect(ctx, `SELECT COUNT(*) AS n FROM app.notes`, nil)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if rows[0]["n"] != int64(20) {
		t.Fatalf("n = %v, want 20", rows[0]["n"])
	}
}

func BenchmarkAnchorQueryReusedConnection(b *testing.B) {
	attaches, _ := benchAttachFiles(b, 4)
	db, err := OpenDB(context.Background(), OpenConfig{Path: ":memory:", Attach: attaches})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.QueryRow(`SELECT COUNT(*) FROM db0.items`).Scan(new(int)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnchorJoinParallel(b *testing.B) {
	attaches, _ := benchAttachFiles(b, 4)
	db, err := OpenDB(context.Background(), OpenConfig{Path: ":memory:", Attach: attaches})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM db0.items i JOIN db1.t x ON 1=1`).Scan(&n); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkClusterWriteParallel(b *testing.B) {
	attaches, main := benchAttachFiles(b, 3)
	cluster, err := OpenCluster(context.Background(), attaches,
		[]string{"db0", "db1", "db2"},
		Profile{BatchSize: 128, BatchTimeout: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	defer cluster.Close()
	_ = main

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := cluster.WriteStmt(context.Background(), "db1",
				`INSERT INTO db1.t (label) VALUES ('x')`, nil, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkClusterQueryCachedHit(b *testing.B) {
	attaches, _ := benchAttachFiles(b, 3)
	cluster, err := OpenCluster(context.Background(), attaches,
		[]string{"db0", "db1", "db2"},
		Profile{CacheTTL: time.Minute, CacheMax: 64})
	if err != nil {
		b.Fatal(err)
	}
	defer cluster.Close()

	ctx := context.Background()
	if _, err := cluster.QueryCached(ctx, `SELECT id, label FROM db0.items`, nil); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cluster.QueryCached(ctx, `SELECT id, label FROM db0.items`, nil); err != nil {
			b.Fatal(err)
		}
	}
}
