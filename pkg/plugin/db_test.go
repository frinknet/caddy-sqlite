package plugin

import (
	"context"
	"database/sql"
	"github.com/frinknet/caddy-sqlite/pkg/database"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func parseGlobal(t *testing.T, body string) (*pendingConfig, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(body)
	if _, err := ParseGlobalOption(d, nil); err != nil {
		return nil, err
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	return pending, nil
}

func TestParseGlobalDefaults(t *testing.T) {
	p, err := parseGlobal(t, `sqlite {
	db app /var/lib/caddy/app.db
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.engine.BatchSize != defaultBatchSize {
		t.Fatalf("batch_size = %d, want %d", p.engine.BatchSize, defaultBatchSize)
	}
	if p.engine.BatchTimeout != defaultBatchTimeout {
		t.Fatalf("batch_timeout = %s", p.engine.BatchTimeout)
	}
	if p.engine.CacheTTL != defaultCacheTTL || p.engine.CacheMax != defaultCacheMax {
		t.Fatalf("cache defaults = %s/%d", p.engine.CacheTTL, p.engine.CacheMax)
	}
	if p.engine.Timeout != 0 {
		t.Fatalf("timeout = %s, want disabled by default", p.engine.Timeout)
	}
	if len(p.dbs) != 1 || p.dbs[0].Name != "app" || p.dbs[0].Path != "/var/lib/caddy/app.db" {
		t.Fatalf("dbs = %+v", p.dbs)
	}
	if p.dbs[0].ReadOnly {
		t.Fatal("db should be writable by default")
	}
}

func TestParseGlobalEngineKnobs(t *testing.T) {
	p, err := parseGlobal(t, `sqlite {
	batch_size    32
	batch_timeout 10ms
	cache_ttl     30s
	cache_max     64
	timeout       2s

	db app /tmp/app.db
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.engine.BatchSize != 32 || p.engine.BatchTimeout != 10*time.Millisecond {
		t.Fatalf("batching = %d/%s", p.engine.BatchSize, p.engine.BatchTimeout)
	}
	if p.engine.CacheTTL != 30*time.Second || p.engine.CacheMax != 64 {
		t.Fatalf("cache = %s/%d", p.engine.CacheTTL, p.engine.CacheMax)
	}
	if p.engine.Timeout != 2*time.Second {
		t.Fatalf("timeout = %s", p.engine.Timeout)
	}
}

func TestParseGlobalDBOptions(t *testing.T) {
	p, err := parseGlobal(t, `sqlite {
	db app /var/lib/caddy/app.db {
		migrate  /srv/www/migrations
		optimize 24h
		backup {
			schedule 0 3 * * *
			dir      /srv/backups
			retain   7
		}
	}

	db media /srv/media.db {
		readonly
	}
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.dbs) != 2 {
		t.Fatalf("dbs = %d", len(p.dbs))
	}
	app := p.dbs[0]
	if app.MigrateDir != "/srv/www/migrations" {
		t.Fatalf("migrate = %q", app.MigrateDir)
	}
	if app.Optimize != 24*time.Hour {
		t.Fatalf("optimize = %s", app.Optimize)
	}
	if app.Backup == nil {
		t.Fatal("backup missing")
	}
	if app.Backup.Schedule != "0 3 * * *" {
		t.Fatalf("schedule = %q, want the whole five-field expression", app.Backup.Schedule)
	}
	if app.Backup.Dir != "/srv/backups" || app.Backup.Retain != 7 {
		t.Fatalf("backup = %+v", app.Backup)
	}
	if app.Backup.Name != "app" {
		t.Fatalf("backup name = %q, want the schema name", app.Backup.Name)
	}
	if !p.dbs[1].ReadOnly {
		t.Fatal("media should be read-only")
	}
}

func TestParseGlobalRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no db", `sqlite { batch_size 8 }`},
		{"unknown engine option", `sqlite {
	nope 1
	db app /tmp/a.db
}`},
		{"unknown db option", `sqlite {
	db app /tmp/a.db {
		nope 1
	}
}`},
		{"duplicate name", `sqlite {
	db app /tmp/a.db
	db app /tmp/b.db
}`},
		{"duplicate path", `sqlite {
	db app  /tmp/a.db
	db app2 /tmp/a.db
}`},
		{"bad name", `sqlite { db 9bad /tmp/a.db }`},
		{"reserved name", `sqlite { db main /tmp/a.db }`},
		{"reserved temp", `sqlite { db temp /tmp/a.db }`},
		{"bad batch size", `sqlite {
	batch_size 0
	db app /tmp/a.db
}`},
		{"bad batch timeout", `sqlite {
	batch_timeout 5s
	db app /tmp/a.db
}`},
		{"bad backup method", `sqlite {
	db app /tmp/a.db {
		backup {
			method cp
			schedule @daily
			dir /tmp/backups
		}
	}
}`},
		{"bad retain", `sqlite {
	db app /tmp/a.db {
		backup {
			schedule @daily
			dir /tmp/backups
			retain -1
		}
	}
}`},
		{"bad timeout", `sqlite {
	timeout nonsense
	db app /tmp/a.db
}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseGlobal(t, c.body); err == nil {
				t.Fatalf("expected %s to be rejected", c.name)
			}
		})
	}
}

func TestParseGlobalExpandsEnvInPaths(t *testing.T) {
	t.Setenv("CADDYSQLITE_TEST_DB", "/tmp/from-env.db")
	p, err := parseGlobal(t, `sqlite {
	db app {env.CADDYSQLITE_TEST_DB}
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.dbs[0].Path != "{env.CADDYSQLITE_TEST_DB}" {
		t.Fatalf("path = %q, want the placeholder kept until provision", p.dbs[0].Path)
	}
}

func TestExpandEnvRejectsMissingVariable(t *testing.T) {
	if _, err := expandEnv("db/{env.CADDYSQLITE_DEFINITELY_UNSET}.db"); err == nil {
		t.Fatal("expected a missing variable to be an error")
	} else if !strings.Contains(err.Error(), "CADDYSQLITE_DEFINITELY_UNSET") {
		t.Fatalf("error should name the variable: %v", err)
	}

	if got, err := expandEnv("literal.db"); err != nil || got != "literal.db" {
		t.Fatalf("literal = %q, %v", got, err)
	}
}

func TestOptimizeDefaultsAndDisable(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"bare takes the daily default", `sqlite {
	db app /tmp/a.db {
		optimize
	}
}`, defaultOptimizeInterval},
		{"explicit interval", `sqlite {
	db app /tmp/a.db {
		optimize 6h
	}
}`, 6 * time.Hour},
		{"zero disables", `sqlite {
	db app /tmp/a.db {
		optimize 0
	}
}`, 0},
		{"absent stays off", `sqlite {
	db app /tmp/a.db
}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parseGlobal(t, c.body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := p.dbs[0].Optimize; got != c.want {
				t.Fatalf("optimize = %s, want %s", got, c.want)
			}
		})
	}

	if _, err := parseGlobal(t, `sqlite {
	db app /tmp/a.db {
		optimize nonsense
	}
}`); err == nil {
		t.Fatal("a non-duration must be rejected")
	}
}

func TestOptimizeOnConnBoundsAndRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opt.db")
	db, err := database.OpenDB(context.Background(), database.OpenConfig{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(`INSERT INTO items (label) VALUES (?)`, "x"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX items_label ON items(label)`); err != nil {
		t.Fatalf("index: %v", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	if err := maintainOnConn(ctx, conn); err != nil {
		t.Fatalf("optimizeOnConn: %v", err)
	}

	var limit int
	if err := conn.QueryRowContext(ctx, `PRAGMA analysis_limit`).Scan(&limit); err != nil {
		t.Fatalf("read analysis_limit: %v", err)
	}
	if limit != optimizeAnalysisLimit {
		t.Fatalf("analysis_limit = %d, want %d", limit, optimizeAnalysisLimit)
	}
}

func TestEngineCleanupRunsFinalOptimize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opt.db")
	handle, err := database.OpenDB(context.Background(), database.OpenConfig{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := handle.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	m := &Engine{
		dbs:         []*DBConfig{{Name: "app", Path: path, Optimize: time.Hour}},
		handles:     []*sql.DB{handle},
		handlesByDB: map[string]*sql.DB{"app": handle},
	}
	m.cleanup()

	if err := handle.Ping(); err == nil {
		t.Fatal("cleanup should have closed the maintenance handle")
	}
}

func TestRunOptimizeStopsOnSignal(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		runOptimize(stop, engine.Cluster().Anchor(), 5*time.Millisecond, nil)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runOptimize did not return after its stop channel closed")
	}
}

func FuzzParseGlobalOption(f *testing.F) {
	seeds := []string{
		`sqlite {
	db app /tmp/app.db
}`,
		`sqlite {
	batch_size    32
	batch_timeout 10ms
	cache_ttl     30s
	cache_max     64
	timeout       2s

	db app /tmp/app.db {
		migrate  /srv/migrations
		optimize 24h
		backup {
			schedule 0 3 * * *
			dir      /srv/backups
			retain   7
		}
	}
	db media /tmp/media.db {
		readonly
	}
	db main {
		migrate /srv/scratch
	}
}`,
		`sqlite {`,
		`sqlite { db }`,
		`sqlite { db main }`,
		`sqlite { db temp /x }`,
		`sqlite { db app /x db app /y }`,
		`sqlite { batch_size }`,
		`sqlite { batch_size 99999 db app /x }`,
		`sqlite { timeout }`,
		`sqlite { db app /x { backup { } } }`,
		`sqlite { db app /x { backup { method } } }`,
		``,
		`{`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, cfg string) {
		d := caddyfile.NewTestDispenser(cfg)
		if _, err := ParseGlobalOption(d, nil); err != nil {
			return
		}
		pendingMu.Lock()
		p := pending
		pendingMu.Unlock()
		if p == nil {
			t.Fatal("a successful parse left nothing pending")
		}
		for _, db := range p.dbs {
			if db.Name == "" {
				t.Fatal("a parsed database has no name")
			}
			if db.Name != "main" && db.Path == "" {
				t.Fatalf("database %q has no path", db.Name)
			}
		}
	})
}
