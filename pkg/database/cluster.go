package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"

	"github.com/frinknet/caddy-sqlite/pkg/cache"
)

// ScratchSchema is the schema the read pool carries as its main database: a
// shared in-memory database for cross-request scratch that must not touch disk.
// It lives as long as at least one pooled connection stays open.
const ScratchSchema = "main"

// Cluster is the engine: one pool whose connections carry every declared
// database as an attached schema, one batcher per writable schema, and one
// shared cache and generation counter.
//
// Reads run on the pool, so any query may join across schemas. Writes are
// routed to the batcher of the schema named in the statement, which keeps every
// batch inside one file: SQLite cannot make a transaction spanning attached
// databases atomic in WAL mode, so that routing is what preserves atomicity.
type Cluster struct {
	anchor  *sql.DB
	profile Profile

	mu       sync.Mutex
	writable map[string]bool
	batchers map[string]*Batcher

	cache  *cache.LRU
	flight singleflight.Group
	gen    atomic.Uint64

	closed atomic.Bool
}

// OpenCluster opens the pool and attaches every entry. Entries with ReadOnly
// set are mounted mode=ro, so a write reaching that schema fails with SQLite's
// own error. Only schemas listed in writable get a batcher; writes naming
// anything else are refused before reaching SQLite.
func OpenCluster(ctx context.Context, attaches []Attach, writable []string, profile Profile) (*Cluster, error) {
	anchor, err := OpenDB(ctx, OpenConfig{Path: ":memory:", Attach: attaches})
	if err != nil {
		return nil, err
	}
	c := &Cluster{
		anchor:   anchor,
		profile:  profile,
		writable: map[string]bool{},
		batchers: map[string]*Batcher{},
	}
	for _, s := range writable {
		c.writable[s] = true
		if profile.BatchSize > 0 {
			c.batchers[s] = NewBatcher(anchor, &c.gen, BatchConfig{
				Size:    profile.BatchSize,
				Timeout: profile.BatchTimeout,
			})
		}
	}
	if profile.CacheTTL > 0 && profile.CacheMax > 0 {
		c.cache = cache.New(profile.CacheMax)
	}
	return c, nil
}

func (c *Cluster) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	var errs []error
	c.mu.Lock()
	for _, b := range c.batchers {
		errs = append(errs, b.Close())
	}
	c.batchers = map[string]*Batcher{}
	c.mu.Unlock()
	if c.anchor != nil {
		errs = append(errs, c.anchor.Close())
	}
	return errors.Join(errs...)
}

// Gen reports the generation counter shared by every schema: any commit bumps
// it, so a write to one schema invalidates cached reads of all of them.
func (c *Cluster) Gen() uint64 { return c.gen.Load() }

// Anchor exposes the pool. Maintenance work must not use it: migrations,
// backups and optimization run against each file on its own handle.
func (c *Cluster) Anchor() *sql.DB { return c.anchor }

// Writable reports whether a schema accepts writes.
func (c *Cluster) Writable(schema string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writable[schema]
}

func (p Profile) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.Timeout)
}

func (c *Cluster) QueryCached(ctx context.Context, sqlText string, args []any) ([]map[string]any, error) {
	ctx, cancel := c.profile.deadline(ctx)
	defer cancel()

	if c.cache == nil {
		return queryRows(ctx, c.anchor, sqlText, args)
	}
	gen := c.gen.Load()
	key := cache.Key(gen, sqlText, args)
	if v, ok := c.cache.Get(key); ok {
		return v.([]map[string]any), nil
	}
	ch := c.flight.DoChan(strconv.FormatUint(key, 16), func() (any, error) {
		if v, ok := c.cache.Get(key); ok {
			return v, nil
		}
		rows, err := queryRows(ctx, c.anchor, sqlText, args)
		if err != nil {
			return nil, err
		}
		if len(rows) <= 100 {
			c.cache.Put(key, rows, c.profile.CacheTTL)
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

func (c *Cluster) QueryDirect(ctx context.Context, sqlText string, args []any) ([]map[string]any, error) {
	ctx, cancel := c.profile.deadline(ctx)
	defer cancel()
	return queryRows(ctx, c.anchor, sqlText, args)
}

// QueryStream runs a read and hands back the open rows, bounded by the
// configured timeout. The caller must close the rows and call cancel when done
// reading, so the deadline outlives the stream.
func (c *Cluster) QueryStream(ctx context.Context, sqlText string, args []any) (*sql.Rows, context.CancelFunc, error) {
	ctx, cancel := c.profile.deadline(ctx)
	rows, err := c.anchor.QueryContext(ctx, sqlText, args...)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return rows, cancel, nil
}

// WriteStmt routes a write to the batcher of the schema it names. Scratch
// writes (empty schema, or ScratchSchema) run unbatched: there is no file to
// group commits on.
func (c *Cluster) WriteStmt(ctx context.Context, schema, sqlText string, args []any, returnsRows bool) (*BatchResult, error) {
	ctx, cancel := c.profile.deadline(ctx)
	defer cancel()

	if schema == "" || schema == ScratchSchema {
		res, err := execDirect(ctx, c.anchor, sqlText, args, returnsRows)
		if err != nil {
			return nil, err
		}
		c.gen.Add(1)
		return res, nil
	}

	c.mu.Lock()
	batcher := c.batchers[schema]
	allowed := c.writable[schema]
	c.mu.Unlock()

	if !allowed {
		return nil, fmt.Errorf("sqlite: schema %q is not writable", schema)
	}
	if batcher != nil {
		return batcher.Submit(ctx, sqlText, args, returnsRows)
	}
	res, err := execDirect(ctx, c.anchor, sqlText, args, returnsRows)
	if err != nil {
		return nil, err
	}
	c.gen.Add(1)
	return res, nil
}

func queryRows(ctx context.Context, db *sql.DB, sqlText string, args []any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanRows(rows)
}

func execDirect(ctx context.Context, db *sql.DB, sqlText string, args []any, returnsRows bool) (*BatchResult, error) {
	if returnsRows {
		rows, err := db.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		data, err := ScanRows(rows)
		if err != nil {
			return nil, err
		}
		return &BatchResult{Rows: data, HasRows: true}, nil
	}
	res, err := db.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	return &BatchResult{Affected: n}, nil
}
