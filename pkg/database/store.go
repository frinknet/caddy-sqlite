package database

import (
    "context"
    "database/sql"
    "fmt"
    "net/url"
    "runtime"
    "strings"

    _ "modernc.org/sqlite"
)

type OpenConfig struct {
    Path     string
    ReadOnly bool
}

func OpenDB(ctx context.Context, cfg OpenConfig) (*sql.DB, error) {
    dsn := buildDSN(cfg)
    db, err := sql.Open("sqlite", dsn)
    if err != nil {
        return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
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
