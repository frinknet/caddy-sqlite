package database

import (
    "context"
    "database/sql"
    "errors"
    "sync"
    "sync/atomic"
    "time"
)

type BatchConfig struct {
    Size    int
    Timeout time.Duration
}

type BatchResult struct {
    Rows     []map[string]any
    HasRows  bool
    Affected int64
}

type batchReq struct {
    sqlText string
    args    []any
    returns bool
    reply   chan batchReply
}

type batchReply struct {
    res BatchResult
    err error
}

type Batcher struct {
    db      *sql.DB
    gen     *atomic.Uint64
    size    int
    timeout time.Duration
    ch      chan *batchReq
    done    chan struct{}
    mu      sync.RWMutex
    closed  bool
}

func NewBatcher(db *sql.DB, gen *atomic.Uint64, cfg BatchConfig) *Batcher {
    if cfg.Size <= 0 {
        cfg.Size = 128
    }
    if cfg.Timeout <= 0 {
        cfg.Timeout = 5 * time.Millisecond
    }
    b := &Batcher{
        db: db, gen: gen,
        size: cfg.Size, timeout: cfg.Timeout,
        ch:   make(chan *batchReq, cfg.Size*4),
        done: make(chan struct{}),
    }
    go b.run()
    return b
}

func (b *Batcher) Submit(ctx context.Context, sqlText string, args []any, returns bool) (*BatchResult, error) {
    r := &batchReq{sqlText: sqlText, args: args, returns: returns, reply: make(chan batchReply, 1)}
    b.mu.RLock()
    if b.closed {
        b.mu.RUnlock()
        return nil, errors.New("batcher: closed")
    }
    select {
    case b.ch <- r:
        b.mu.RUnlock()
    case <-ctx.Done():
        b.mu.RUnlock()
        return nil, ctx.Err()
    }
    select {
    case resp := <-r.reply:
        return &resp.res, resp.err
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (b *Batcher) Close() error {
    b.mu.Lock()
    if b.closed {
        b.mu.Unlock()
        return nil
    }
    b.closed = true
    close(b.ch)
    b.mu.Unlock()
    <-b.done
    return nil
}

func (b *Batcher) run() {
    defer close(b.done)
    for first := range b.ch {
        batch := []*batchReq{first}
        timer := time.NewTimer(b.timeout)
    fill:
        for len(batch) < b.size {
            select {
            case r, ok := <-b.ch:
                if !ok {
                    break fill
                }
                batch = append(batch, r)
            case <-timer.C:
                break fill
            }
        }
        timer.Stop()
        b.commitBatch(batch)
    }
}
