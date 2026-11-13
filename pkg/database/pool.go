package database

import (
    "context"
    "database/sql"
    "strconv"
    "sync"
    "sync/atomic"
    "time"

    "golang.org/x/sync/singleflight"

    "github.com/frinknet/caddy-sqlite/pkg/cache"
)

type Profile struct {
    BatchSize    int
    BatchTimeout time.Duration
    CacheTTL     time.Duration
    CacheMax     int
    ReadOnly     bool
}

type DB struct {
    Path    string
    sqlDB   *sql.DB
    batcher *Batcher
    cache   *cache.LRU
    flight  singleflight.Group
    gen     atomic.Uint64
    profile Profile
    pool    *Pool
    refs    int
}

type Pool struct {
    mu  sync.Mutex
    dbs map[string]*DB
}

func NewPool() *Pool {
    return &Pool{dbs: map[string]*DB{}}
}

func (p *Pool) Acquire(ctx context.Context, path string, profile Profile) (*DB, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if db, ok := p.dbs[path]; ok {
        db.refs++
        return db, nil
    }
    sqlDB, err := OpenDB(ctx, OpenConfig{Path: path, ReadOnly: profile.ReadOnly})
    if err != nil {
        return nil, err
    }
    db := &DB{
        Path:    path,
        sqlDB:   sqlDB,
        profile: profile,
        pool:    p,
        refs:    1,
    }
    if profile.CacheTTL > 0 && profile.CacheMax > 0 {
        db.cache = cache.New(profile.CacheMax)
    }
    if !profile.ReadOnly && profile.BatchSize > 0 {
        db.batcher = NewBatcher(sqlDB, &db.gen, BatchConfig{
            Size:    profile.BatchSize,
            Timeout: profile.BatchTimeout,
        })
    }
    p.dbs[path] = db
    return db, nil
}

func (p *Pool) Release(path string) error {
    p.mu.Lock()
    defer p.mu.Unlock()
    db, ok := p.dbs[path]
    if !ok {
        return nil
    }
    db.refs--
    if db.refs > 0 {
        return nil
    }
    delete(p.dbs, path)
    if db.batcher != nil {
        db.batcher.Close()
    }
    return db.sqlDB.Close()
}

func (d *DB) SQL() *sql.DB          { return d.sqlDB }
func (d *DB) ReadOnly() bool        { return d.profile.ReadOnly }
func (d *DB) BumpGen()              { d.gen.Add(1) }
func (d *DB) Release() error        { return d.pool.Release(d.Path) }

func (d *DB) QueryCached(ctx context.Context, sqlText string, args []any) ([]map[string]any, error) {
    if d.cache == nil {
        return d.queryDirect(ctx, sqlText, args)
    }
    gen := d.gen.Load()
    key := cache.Key(gen, sqlText, args)
    if v, ok := d.cache.Get(key); ok {
        return v.([]map[string]any), nil
    }
    ch := d.flight.DoChan(strconv.FormatUint(key, 16), func() (any, error) {
        if v, ok := d.cache.Get(key); ok {
            return v, nil
        }
        rows, err := d.queryDirect(ctx, sqlText, args)
        if err != nil {
            return nil, err
        }
        if len(rows) <= 100 {
            d.cache.Put(key, rows, d.profile.CacheTTL)
        }
        return rows, nil
    })
    select {
    case r := <-ch:
        if r.Err != nil {
            return nil, r.Err
        }
        return r.Val.([]map[string]any), nil
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (d *DB) QueryDirect(ctx context.Context, sqlText string, args []any) ([]map[string]any, error) {
    return d.queryDirect(ctx, sqlText, args)
}

func (d *DB) queryDirect(ctx context.Context, sqlText string, args []any) ([]map[string]any, error) {
    rows, err := d.sqlDB.QueryContext(ctx, sqlText, args...)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    return ScanRows(rows)
}

func (d *DB) WriteStmt(ctx context.Context, sqlText string, args []any, returnsRows bool) (*BatchResult, error) {
    if d.batcher == nil {
        if returnsRows {
            rows, err := d.sqlDB.QueryContext(ctx, sqlText, args...)
            if err != nil {
                return nil, err
            }
            defer rows.Close()
            data, err := ScanRows(rows)
            if err != nil {
                return nil, err
            }
            d.gen.Add(1)
            return &BatchResult{Rows: data, HasRows: true}, nil
        }
        res, err := d.sqlDB.ExecContext(ctx, sqlText, args...)
        if err != nil {
            return nil, err
        }
        n, _ := res.RowsAffected()
        d.gen.Add(1)
        return &BatchResult{Affected: n}, nil
    }
    return d.batcher.Submit(ctx, sqlText, args, returnsRows)
}

func ScanRows(rows *sql.Rows) ([]map[string]any, error) {
    cols, err := rows.Columns()
    if err != nil {
        return nil, err
    }
    n := len(cols)
    out := make([]map[string]any, 0, 8)
    vals := make([]any, n)
    ptrs := make([]any, n)
    for i := range vals {
        ptrs[i] = &vals[i]
    }
    for rows.Next() {
        if err := rows.Scan(ptrs...); err != nil {
            return nil, err
        }
        row := make(map[string]any, n)
        for i, c := range cols {
            if b, ok := vals[i].([]byte); ok {
                row[c] = string(b)
            } else {
                row[c] = vals[i]
            }
        }
        out = append(out, row)
    }
    return out, rows.Err()
}
