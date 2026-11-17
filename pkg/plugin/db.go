package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

const (
	// defaultOptimizeInterval is SQLite's own recommendation for PRAGMA
	// optimize: about once a day, bounded by analysis_limit.
	defaultOptimizeInterval = 24 * time.Hour

	defaultBatchSize    = 128
	defaultBatchTimeout = 5 * time.Millisecond
	defaultCacheTTL     = 5 * time.Second
	defaultCacheMax     = 1000
)

// DBConfig is one declared database. Its name is the schema every query uses to
// address it, and its path is the file attached under that schema.
type DBConfig struct {
	Name       string
	Path       string
	MigrateDir string
	ReadOnly   bool
	Optimize   time.Duration
	Backup     *database.BackupConfig
}

// EngineConfig holds the process-wide knobs: one read pool, one cache, one
// write-batching policy.
type EngineConfig struct {
	BatchSize    int
	BatchTimeout time.Duration
	CacheTTL     time.Duration
	CacheMax     int
	Timeout      time.Duration
}

type pendingConfig struct {
	engine EngineConfig
	dbs    []*DBConfig
}

var (
	pendingMu sync.Mutex
	pending   *pendingConfig
)

var identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseGlobalOption is the UnmarshalGlobalFunc for `sqlite`. It parses the
// engine knobs and every `db` declaration, stores them in `pending`, and
// returns an httpcaddyfile.App so the adapter injects the app into AppsRaw.
func ParseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	d.Next()

	p := &pendingConfig{
		engine: EngineConfig{
			BatchSize:    defaultBatchSize,
			BatchTimeout: defaultBatchTimeout,
			CacheTTL:     defaultCacheTTL,
			CacheMax:     defaultCacheMax,
		},
	}

	for n := d.Nesting(); d.NextBlock(n); {
		switch d.Val() {
		case "batch_size":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			v, err := strconv.Atoi(d.Val())
			if err != nil || v < 1 || v > 4096 {
				return nil, d.Errf("batch_size must be 1..4096")
			}
			p.engine.BatchSize = v
		case "batch_timeout":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur <= 0 || dur > time.Second {
				return nil, d.Errf("batch_timeout must be >0 and <=1s")
			}
			p.engine.BatchTimeout = dur
		case "cache_ttl":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur < 0 {
				return nil, d.Errf("cache_ttl: %v", err)
			}
			p.engine.CacheTTL = dur
		case "cache_max":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			v, err := strconv.Atoi(d.Val())
			if err != nil || v < 0 {
				return nil, d.Errf("cache_max: %v", err)
			}
			p.engine.CacheMax = v
		case "timeout":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur < 0 {
				return nil, d.Errf("timeout: %v", err)
			}
			p.engine.Timeout = dur
		case "db":
			cfg, err := parseDBBlock(d)
			if err != nil {
				return nil, err
			}
			p.dbs = append(p.dbs, cfg)
		default:
			return nil, d.Errf("sqlite: unknown option %q", d.Val())
		}
	}

	if len(p.dbs) == 0 {
		return nil, d.Err("sqlite: at least one `db <name> <file>` is required")
	}

	seenName := map[string]bool{}
	seenPath := map[string]string{}
	for _, cfg := range p.dbs {
		if seenName[cfg.Name] {
			return nil, d.Errf("sqlite: db %q declared twice", cfg.Name)
		}
		seenName[cfg.Name] = true
		if cfg.Name == database.ScratchSchema {
			continue
		}
		if other, ok := seenPath[cfg.Path]; ok {
			return nil, d.Errf("sqlite: db %q and %q both point to %q", cfg.Name, other, cfg.Path)
		}
		seenPath[cfg.Path] = cfg.Name
	}

	pendingMu.Lock()
	pending = p
	pendingMu.Unlock()

	return httpcaddyfile.App{
		Name:  "sqlite",
		Value: []byte("{}"),
	}, nil
}

func parseDBBlock(d *caddyfile.Dispenser) (*DBConfig, error) {
	var name string
	if !d.NextArg() {
		return nil, d.ArgErr()
	}
	name = d.Val()

	// `db main { ... }` declares the in-memory scratch: it has no file, so it
	// carries only the schema migrations that give it tables at boot.
	scratch := name == database.ScratchSchema
	if !identifierRe.MatchString(name) {
		return nil, d.Errf("db name %q must be an identifier (letters, digits, underscore)", name)
	}
	if name == "temp" {
		return nil, d.Errf("db name %q is reserved by SQLite", name)
	}

	var path string
	if scratch {
		if d.NextArg() {
			return nil, d.Errf("db main: the scratch schema has no file")
		}
	} else {
		if !d.NextArg() {
			return nil, d.ArgErr()
		}
		path = d.Val()
	}

	cfg := &DBConfig{Name: name, Path: path}
	for n := d.Nesting(); d.NextBlock(n); {
		switch d.Val() {
		case "migrate":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			cfg.MigrateDir = d.Val()
		case "readonly":
			cfg.ReadOnly = true
		case "optimize":
			// Bare `optimize` takes the default interval; `optimize 0` turns it
			// off. SQLite's own guidance is to run PRAGMA optimize about once a
			// day, so that is the default rather than "off".
			if !d.NextArg() {
				cfg.Optimize = defaultOptimizeInterval
				continue
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur < 0 {
				return nil, d.Errf("optimize: %v", err)
			}
			cfg.Optimize = dur
		case "backup":
			b := &database.BackupConfig{Name: name}
			for nn := d.Nesting(); d.NextBlock(nn); {
				switch d.Val() {
				case "method":
					if !d.NextArg() {
						return nil, d.ArgErr()
					}
					if d.Val() != "vacuum_into" {
						return nil, d.Errf("backup: unsupported method %q (only vacuum_into)", d.Val())
					}
					b.Method = d.Val()
				case "schedule":
					args := d.RemainingArgs()
					if len(args) == 0 {
						return nil, d.ArgErr()
					}
					b.Schedule = joinArgs(args)
				case "dir":
					if !d.NextArg() {
						return nil, d.ArgErr()
					}
					b.Dir = d.Val()
				case "retain":
					if !d.NextArg() {
						return nil, d.ArgErr()
					}
					v, err := strconv.Atoi(d.Val())
					if err != nil || v < 0 {
						return nil, d.Errf("retain: %v", err)
					}
					b.Retain = v
				default:
					return nil, d.Errf("backup: unknown option %q", d.Val())
				}
			}
			cfg.Backup = b
		default:
			return nil, d.Errf("db: unknown option %q", d.Val())
		}
	}
	if scratch && cfg.MigrateDir == "" {
		return nil, d.Errf("db main: the scratch schema needs a `migrate <dir>` to have tables")
	}
	if cfg.ReadOnly && (cfg.Optimize > 0 || cfg.Backup != nil) {
		return nil, d.Errf("db %q: backup and optimize cannot be used with read_only", name)
	}
	return cfg, nil
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// Engine is the app: it owns the cluster, applies migrations, and starts the
// per-database maintenance jobs.
type Engine struct {
	engine      EngineConfig
	dbs         []*DBConfig
	cluster     *database.Cluster
	stops       []func()
	handles     []*sql.DB
	handlesByDB map[string]*sql.DB
	log         *zap.Logger
}

func (Engine) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "sqlite",
		New: func() caddy.Module { return new(Engine) },
	}
}

func (m *Engine) Start() error { return nil }

func (m *Engine) Stop() error { return nil }

func (m *Engine) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger(m)

	pendingMu.Lock()
	p := pending
	pending = nil
	pendingMu.Unlock()

	if p == nil {
		return fmt.Errorf("sqlite: no `sqlite { db ... }` block was configured")
	}
	m.engine = p.engine
	m.dbs = p.dbs

	var (
		attaches []database.Attach
		writable []string
	)
	for _, cfg := range m.dbs {
		if cfg.Name == database.ScratchSchema {
			continue
		}
		path, err := expandEnv(cfg.Path)
		if err != nil {
			return fmt.Errorf("db %q: %w", cfg.Name, err)
		}
		if path == "" {
			return fmt.Errorf("db %q: path resolved empty", cfg.Name)
		}
		cfg.Path = path

		if cfg.MigrateDir != "" {
			if err := m.migrate(ctx, cfg); err != nil {
				m.cleanup()
				return err
			}
		}
		attaches = append(attaches, database.Attach{
			Schema:   cfg.Name,
			Path:     path,
			ReadOnly: cfg.ReadOnly,
		})
		if !cfg.ReadOnly {
			writable = append(writable, cfg.Name)
		}
	}

	profile := database.Profile{
		BatchSize:    m.engine.BatchSize,
		BatchTimeout: m.engine.BatchTimeout,
		CacheTTL:     m.engine.CacheTTL,
		CacheMax:     m.engine.CacheMax,
		Timeout:      m.engine.Timeout,
	}
	cluster, err := database.OpenCluster(ctx, attaches, writable, profile)
	if err != nil {
		m.cleanup()
		return err
	}
	m.cluster = cluster

	for _, cfg := range m.dbs {
		if cfg.Name == database.ScratchSchema {
			if err := database.Apply(ctx, cluster.Anchor(), os.DirFS(cfg.MigrateDir), "."); err != nil {
				m.cleanup()
				return fmt.Errorf("db main: migrate: %w", err)
			}
			continue
		}
		if err := m.startMaintenance(cfg); err != nil {
			m.cleanup()
			return err
		}
	}
	return nil
}

// migrate opens the file read-write, applies pending migrations, and closes it.
// A database declared readonly is migrated here and only then attached mode=ro,
// so the migration runs exactly once and serving stays read-only.
func (m *Engine) migrate(ctx caddy.Context, cfg *DBConfig) error {
	db, err := database.OpenDB(ctx, database.OpenConfig{Path: cfg.Path})
	if err != nil {
		return fmt.Errorf("db %q: migrate: %w", cfg.Name, err)
	}
	defer db.Close()

	if err := database.Apply(ctx, db, os.DirFS(cfg.MigrateDir), "."); err != nil {
		return fmt.Errorf("db %q: migrate: %w", cfg.Name, err)
	}
	return nil
}

// maintenanceDB returns the handle kept for a database's scheduled work, or nil.
func (m *Engine) maintenanceDB(cfg *DBConfig) *sql.DB {
	if db, ok := m.handlesByDB[cfg.Name]; ok {
		return db
	}
	return nil
}

func (m *Engine) startMaintenance(cfg *DBConfig) error {
	if cfg.Optimize <= 0 && cfg.Backup == nil {
		return nil
	}
	db, err := database.OpenDB(context.Background(), database.OpenConfig{Path: cfg.Path})
	if err != nil {
		return fmt.Errorf("db %q: maintenance handle: %w", cfg.Name, err)
	}
	m.handles = append(m.handles, db)
	if m.handlesByDB == nil {
		m.handlesByDB = map[string]*sql.DB{}
	}
	m.handlesByDB[cfg.Name] = db

	if cfg.Optimize > 0 && !cfg.ReadOnly {
		stop := make(chan struct{})
		go runOptimize(stop, db, cfg.Optimize, m.log)
		m.stops = append(m.stops, func() { close(stop) })
	}
	if cfg.Backup != nil && !cfg.ReadOnly {
		sched, err := database.NewBackupScheduler(*cfg.Backup, db, m.log)
		if err != nil {
			return fmt.Errorf("db %q: %w", cfg.Name, err)
		}
		m.stops = append(m.stops, sched.Close)
	}
	return nil
}

func (m *Engine) Cleanup() error {
	m.cleanup()
	return nil
}

func (m *Engine) cleanup() {
	for _, stop := range m.stops {
		stop()
	}
	// SQLite recommends running PRAGMA optimize when a connection is about to
	// close; this is that moment. Only for databases that opted in.
	for _, cfg := range m.dbs {
		if cfg.Optimize <= 0 || cfg.ReadOnly {
			continue
		}
		db := m.maintenanceDB(cfg)
		if db == nil {
			continue
		}
		if err := maintainOnce(context.Background(), db); err != nil && m.log != nil {
			m.log.Warn("final maintenance failed", zap.String("db", cfg.Name), zap.Error(err))
		}
	}
	m.stops = nil
	for _, db := range m.handles {
		db.Close()
	}
	m.handles = nil
	if m.cluster != nil {
		m.cluster.Close()
		m.cluster = nil
	}
}

// Cluster is what handlers and templates run against.
func (m *Engine) Cluster() *database.Cluster { return m.cluster }

// DBConfig returns the declaration for a schema.
func (m *Engine) DBConfig(name string) (*DBConfig, bool) {
	for _, cfg := range m.dbs {
		if cfg.Name == name {
			return cfg, true
		}
	}
	return nil, false
}

const (
	// optimizeAnalysisLimit bounds how much of each index ANALYZE may read. Our
	// request path is cached and index-driven, so planner statistics matter
	// mostly for the first miss after a cache window: a few hundred rows per
	// index is already representative for the query shapes this module serves
	// (equality and range lookups, small ORDER BYs), and it keeps the pass short
	// while requests are in flight. Higher buys nothing our plans can use.
	optimizeAnalysisLimit = 400
)

// maintainOnConn runs the maintenance pair this module actually needs, on one
// borrowed connection. Both statements share it because analysis_limit is
// connection state.
//
// The checkpoint comes first so the ANALYZE reads the main file rather than a
// WAL that has been accumulating since the last one. PASSIVE rather than
// TRUNCATE: it never blocks readers or the writer, and the WAL is already bounded
// by wal_autocheckpoint=2000, so the goal is folding committed pages back into
// the main file (cheaper cold reads, smaller VACUUM INTO backups), not shrinking
// a file that will regrow. A busy result is not an error: the next pass catches up.
func maintainOnConn(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		return fmt.Errorf("wal_checkpoint: %w", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA analysis_limit=%d", optimizeAnalysisLimit)); err != nil {
		return fmt.Errorf("analysis_limit: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA optimize"); err != nil {
		return fmt.Errorf("optimize: %w", err)
	}
	return nil
}

// maintainOnce borrows a connection for one maintenance pass.
func maintainOnce(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return maintainOnConn(ctx, conn)
}

func runOptimize(stop chan struct{}, db *sql.DB, every time.Duration, log *zap.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := maintainOnce(context.Background(), db); err != nil && log != nil {
				log.Warn("maintenance failed", zap.Error(err))
			}
		}
	}
}

var envRe = regexp.MustCompile(`\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(s string) (string, error) {
	var missing string
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[5 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if missing == "" {
			missing = name
		}
		return m
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %q is not set", missing)
	}
	return out, nil
}

var (
	_ caddy.Provisioner  = (*Engine)(nil)
	_ caddy.CleanerUpper = (*Engine)(nil)
	_ caddy.App          = (*Engine)(nil)
)
