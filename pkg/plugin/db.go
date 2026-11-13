package plugin

import (
	"context"
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

var (
	pendingMu sync.Mutex
	pending   = map[string]*DBConfig{}
)

type DBConfig struct {
	Name         string
	Path         string
	BatchSize    int
	BatchTimeout time.Duration
	CacheTTL     time.Duration
	CacheMax     int
	ReadOnly     bool
	MigrateDir   string
	Optimize     time.Duration
	Backup       *database.BackupConfig
}

// ParseSQLiteDBOption is the UnmarshalGlobalFunc for `sqlite_db`. Caddy calls
// it once per `sqlite_db NAME PATH { ... }` block in the global options
// section. It parses the block, stores the config in `pending`, and returns an
// httpcaddyfile.App so that the Caddyfile adapter injects the `sqlite` app into
// AppsRaw. That makes ctx.App("sqlite") succeed in handlers without requiring
// the user to write `apps { sqlite {} }`.
//
// The App.Value is `{}` (not the parsed config) because multiple sqlite_db
// blocks share the same app name — real per-DB configs live in `pending` and
// are consumed by SQLiteDBs.Provision.
func ParseSQLiteDBOption(d *caddyfile.Dispenser, _ any) (any, error) {
	d.Next()
	var name, path string
	if !d.Args(&name, &path) {
		return nil, d.ArgErr()
	}

	cfg := &DBConfig{
		Name:         name,
		Path:         path,
		BatchSize:    128,
		BatchTimeout: 5 * time.Millisecond,
		CacheTTL:     5 * time.Second,
		CacheMax:     1000,
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
			cfg.BatchSize = v
		case "batch_timeout":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur <= 0 || dur > time.Second {
				return nil, d.Errf("batch_timeout must be >0 and <=1s")
			}
			cfg.BatchTimeout = dur
		case "cache_ttl":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur < 0 {
				return nil, d.Errf("cache_ttl: %v", err)
			}
			cfg.CacheTTL = dur
		case "cache_max":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			v, err := strconv.Atoi(d.Val())
			if err != nil || v < 0 {
				return nil, d.Errf("cache_max: %v", err)
			}
			cfg.CacheMax = v
		case "migrate":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			cfg.MigrateDir = d.Val()
		case "read_only":
			cfg.ReadOnly = true
		case "optimize":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil || dur <= 0 {
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
					b.Method = d.Val()
				case "schedule":
					if !d.NextArg() {
						return nil, d.ArgErr()
					}
					b.Schedule = d.Val()
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
			return nil, d.Errf("sqlite_db: unknown option %q", d.Val())
		}
	}

	pendingMu.Lock()
	pending[name] = cfg
	pendingMu.Unlock()

	return httpcaddyfile.App{
		Name:  "sqlite",
		Value: []byte("{}"),
	}, nil
}

type SQLiteDBs struct {
	configs map[string]*DBConfig
	pool    *database.Pool
	stops   map[string][]func()
	log     *zap.Logger
}

func (SQLiteDBs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "sqlite",
		New: func() caddy.Module { return new(SQLiteDBs) },
	}
}

func (m *SQLiteDBs) Provision(ctx caddy.Context) (err error) {
	m.log = ctx.Logger(m)
	m.pool = database.NewPool()
	m.configs = map[string]*DBConfig{}
	m.stops = map[string][]func(){}

	pendingMu.Lock()
	cfgs := pending
	pending = map[string]*DBConfig{}
	pendingMu.Unlock()

	seen := map[string]string{}
	for name, cfg := range cfgs {
		p := expandEnv(cfg.Path)
		if other, ok := seen[p]; ok {
			return fmt.Errorf("sqlite_db %q and %q both point to %q", name, other, p)
		}
		seen[p] = name
	}

	defer func() {
		if err != nil {
			m.cleanupAll()
		}
	}()

	for name, cfg := range cfgs {
		if err := m.provisionOne(ctx, name, cfg); err != nil {
			return err
		}
	}
	return nil
}

func (m *SQLiteDBs) provisionOne(ctx caddy.Context, name string, cfg *DBConfig) error {
	path := expandEnv(cfg.Path)
	if path == "" {
		return fmt.Errorf("sqlite_db %q: path resolved empty", name)
	}
	if cfg.ReadOnly && cfg.MigrateDir != "" {
		return fmt.Errorf("sqlite_db %q: migrate cannot be used with read_only", name)
	}

	profile := database.Profile{
		BatchSize:    cfg.BatchSize,
		BatchTimeout: cfg.BatchTimeout,
		CacheTTL:     cfg.CacheTTL,
		CacheMax:     cfg.CacheMax,
		ReadOnly:     cfg.ReadOnly,
	}
	db, err := m.pool.Acquire(ctx, path, profile)
	if err != nil {
		return fmt.Errorf("sqlite_db %q: %w", name, err)
	}
	m.configs[name] = cfg

	if cfg.MigrateDir != "" {
		if err := database.Apply(ctx, db.SQL(), os.DirFS(cfg.MigrateDir), "."); err != nil {
			return fmt.Errorf("sqlite_db %q: migrate: %w", name, err)
		}
	}

	if cfg.Optimize > 0 && !cfg.ReadOnly {
		stop := make(chan struct{})
		go runOptimize(stop, db, cfg.Optimize, m.log)
		m.stops[path] = append(m.stops[path], func() { close(stop) })
	}

	if cfg.Backup != nil && !cfg.ReadOnly {
		sched, err := database.NewBackupScheduler(*cfg.Backup, db.SQL(), m.log)
		if err != nil {
			return fmt.Errorf("sqlite_db %q: backup: %w", name, err)
		}
		m.stops[path] = append(m.stops[path], sched.Close)
	}
	return nil
}

func (m *SQLiteDBs) cleanupAll() {
	for _, cfg := range m.configs {
		path := expandEnv(cfg.Path)
		for _, s := range m.stops[path] {
			s()
		}
		m.pool.Release(path)
	}
	m.stops = nil
	m.configs = nil
}

func (m *SQLiteDBs) Cleanup() error {
	m.cleanupAll()
	return nil
}

func (m *SQLiteDBs) Lookup(nameOrPath string) (*database.DB, error) {
	if cfg, ok := m.configs[nameOrPath]; ok {
		return m.pool.Acquire(context.Background(), expandEnv(cfg.Path), database.Profile{
			BatchSize:    cfg.BatchSize,
			BatchTimeout: cfg.BatchTimeout,
			CacheTTL:     cfg.CacheTTL,
			CacheMax:     cfg.CacheMax,
			ReadOnly:     cfg.ReadOnly,
		})
	}
	return m.pool.Acquire(context.Background(), expandEnv(nameOrPath), database.Profile{})
}

func runOptimize(stop chan struct{}, db *database.DB, every time.Duration, log *zap.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if _, err := db.SQL().Exec("PRAGMA optimize"); err != nil {
				log.Warn("optimize failed", zap.Error(err))
			}
		}
	}
}

var envRe = regexp.MustCompile(`\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[5 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

var (
	_ caddy.Module       = (*SQLiteDBs)(nil)
	_ caddy.Provisioner  = (*SQLiteDBs)(nil)
	_ caddy.CleanerUpper = (*SQLiteDBs)(nil)
)
