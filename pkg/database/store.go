package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

type OpenConfig struct {
	Path     string
	ReadOnly bool
	Attach   []Attach
}

// Attach describes a database file mounted into every connection of a pool
// under Schema. ReadOnly mounts it with mode=ro, so writes to that schema fail
// with SQLite's own error.
type Attach struct {
	Schema   string
	Path     string
	ReadOnly bool
}

func OpenDB(ctx context.Context, cfg OpenConfig) (*sql.DB, error) {
	dsn := buildDSN(cfg)
	var db *sql.DB
	if len(cfg.Attach) == 0 {
		var err error
		db, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
		}
	} else {
		// Reuse the registered driver instance: user-defined functions are
		// registered against it, so building a fresh driver would drop them.
		probe, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
		}
		drv := probe.Driver()
		probe.Close()
		db = sql.OpenDB(&attachConnector{drv: drv, dsn: dsn, attach: cfg.Attach})
	}
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	n = n*2 + 1
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}
	if !cfg.ReadOnly {
		if err := probeWritable(ctx, db); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite: not writable: %w", err)
		}
	}

	return db, nil
}

func buildDSN(cfg OpenConfig) string {
	path := cfg.Path
	isMemory := path == ":memory:" || strings.Contains(path, ":memory:")

	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "cache_size(-32000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "temp_store(MEMORY)")

	if cfg.ReadOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "wal_autocheckpoint(2000)")
		if !isMemory {
			q.Add("_pragma", "mmap_size(268435456)")
		}
	}

	if isMemory {
		q.Set("cache", "shared")
		return "file::memory:?" + q.Encode()
	}
	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + q.Encode()
	}
	return "file:" + url.PathEscape(path) + "?" + q.Encode()
}

func probeWritable(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS __caddy_sqlite_probe__ (x INTEGER)"); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// attachConnector mounts the configured files on every new connection before
// database/sql hands it out. SQLite's ATTACH is per-connection state, so doing
// it here is what makes a schema visible to the whole pool.
type attachConnector struct {
	drv    driver.Driver
	dsn    string
	attach []Attach
}

func (c *attachConnector) Driver() driver.Driver { return c.drv }

func (c *attachConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	preparer, ok := conn.(driver.ConnPrepareContext)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("sqlite: driver cannot prepare statements with context")
	}
	for _, a := range c.attach {
		stmt, err := preparer.PrepareContext(ctx, `ATTACH DATABASE ? AS `+quoteIdentifier(a.Schema))
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("sqlite: attach %q: %w", a.Schema, err)
		}
		execer, ok := stmt.(driver.StmtExecContext)
		if !ok {
			stmt.Close()
			conn.Close()
			return nil, fmt.Errorf("sqlite: driver cannot execute statements with context")
		}
		_, err = execer.ExecContext(ctx, []driver.NamedValue{{Ordinal: 1, Value: attachURI(a)}})
		stmt.Close()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("sqlite: attach %q at %s: %w", a.Schema, a.Path, err)
		}
	}
	return conn, nil
}

func attachURI(a Attach) string {
	if a.ReadOnly {
		return "file:" + url.PathEscape(a.Path) + "?mode=ro"
	}
	return "file:" + url.PathEscape(a.Path)
}

func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
