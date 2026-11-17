package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

const (
	bgResultTTL      = 5 * time.Minute
	bgWebhookTimeout = 30 * time.Second
	bgShutdownGrace  = 10 * time.Second
)

type CompleteConfig struct {
	URL    string
	Method string
}

type SQLiteBG struct {
	DB       string          `json:"db,omitempty"`
	Query    string          `json:"query,omitempty"`
	Binds    []Bind          `json:"binds,omitempty"`
	Complete *CompleteConfig `json:"complete,omitempty"`

	parsedSQL  string
	positional bool
	plan       database.Plan
	db         *database.DB
	log        *zap.Logger

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	rootCtx context.Context
	cancel  context.CancelFunc
}

var nextJobID atomic.Uint64

func jobID() string {
	n := nextJobID.Add(1)
	t := time.Now().UnixNano()
	return fmt.Sprintf("%x-%x", t, n)
}

type bgResult struct {
	rows      []map[string]any
	err       error
	expiresAt time.Time
}

var (
	resultsMu   sync.Mutex
	results     = map[string]*bgResult{}
	sweeperOnce sync.Once
)

// startResultsSweeper launches a single background goroutine that evicts
// expired results from the shared results map. Called once (sweeperOnce) from
// the first SQLiteBG.Provision. Runs for the lifetime of the process.
func startResultsSweeper() {
	sweeperOnce.Do(func() {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for range t.C {
				sweepExpiredResults(time.Now())
			}
		}()
	})
}

func sweepExpiredResults(now time.Time) {
	resultsMu.Lock()
	for id, r := range results {
		if now.After(r.expiresAt) {
			delete(results, id)
		}
	}
	resultsMu.Unlock()
}

// CaddyModule uses a pointer receiver: SQLiteBG carries a sync.Mutex, and a
// value receiver would copy the lock.
func (*SQLiteBG) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sqlite_bg",
		New: func() caddy.Module { return new(SQLiteBG) },
	}
}

func ParseBG(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(SQLiteBG)
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *SQLiteBG) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for n := d.Nesting(); d.NextBlock(n); {
		switch d.Val() {
		case "use":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.DB = d.Val()
		case "query":
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Query = d.Val()
		case "bind":
			if !d.NextArg() {
				return d.ArgErr()
			}
			name := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			m.Binds = append(m.Binds, Bind{Name: name, Value: d.Val()})
		case "on_complete":
			m.Complete = &CompleteConfig{Method: http.MethodPost}
			for nn := d.Nesting(); d.NextBlock(nn); {
				switch d.Val() {
				case "url":
					if !d.NextArg() {
						return d.ArgErr()
					}
					m.Complete.URL = d.Val()
				case "method":
					if !d.NextArg() {
						return d.ArgErr()
					}
					v := d.Val()
					if v != http.MethodPost && v != http.MethodGet {
						return d.Errf("on_complete method must be POST or GET")
					}
					m.Complete.Method = v
				default:
					return d.Errf("on_complete: unknown option %q", d.Val())
				}
			}
		default:
			return d.Errf("sqlite_bg: unknown option %q", d.Val())
		}
	}
	if m.Query == "" {
		return d.Err("sqlite_bg: query is required")
	}
	if m.DB == "" {
		return d.Err("sqlite_bg: use is required")
	}
	return nil
}

func (m *SQLiteBG) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger(m)
	if err := m.parseBinds(); err != nil {
		return err
	}
	plan, err := database.Classify(m.Query)
	if err != nil {
		return err
	}
	m.plan = plan

	if m.Complete != nil {
		if m.Complete.URL == "" {
			return fmt.Errorf("sqlite_bg: on_complete requires a url")
		}
		if m.Complete.Method == "" {
			m.Complete.Method = http.MethodPost
		}
		if m.Complete.Method != http.MethodPost && m.Complete.Method != http.MethodGet {
			return fmt.Errorf("sqlite_bg: on_complete method must be POST or GET")
		}
	}

	appIface, err := ctx.App("sqlite")
	if err != nil {
		return err
	}
	db, err := appIface.(*SQLiteDBs).Lookup(m.DB)
	if err != nil {
		return err
	}
	if m.plan.IsWrite && db.ReadOnly() {
		db.Release()
		return fmt.Errorf("sqlite_bg: cannot write to read-only DB %q", m.DB)
	}
	m.db = db

	stmt, err := db.SQL().PrepareContext(ctx, m.parsedSQL)
	if err != nil {
		db.Release()
		m.db = nil
		return fmt.Errorf("sqlite_bg: prepare: %w", err)
	}
	stmt.Close()

	m.rootCtx, m.cancel = context.WithCancel(context.Background())
	startResultsSweeper()
	return nil
}

func (m *SQLiteBG) parseBinds() error {
	mode := ""
	for i, b := range m.Binds {
		isPos := isDigits(b.Name)
		if i == 0 {
			if isPos {
				mode = "pos"
			} else {
				mode = "named"
			}
		} else if isPos != (mode == "pos") {
			return fmt.Errorf("sqlite_bg: bind %q mixes positional and named", b.Name)
		}
	}
	m.positional = mode == "pos"
	m.parsedSQL = m.Query

	if m.positional {
		nums := make([]int, len(m.Binds))
		for i, b := range m.Binds {
			n, err := strconv.Atoi(b.Name)
			if err != nil {
				return fmt.Errorf("sqlite_bg: bind %q is not a number", b.Name)
			}
			nums[i] = n
		}
		sort.Ints(nums)
		for i, n := range nums {
			if n != i+1 {
				return fmt.Errorf("sqlite_bg: positional binds must be 1..%d in order", len(m.Binds))
			}
		}
		sort.SliceStable(m.Binds, func(i, j int) bool {
			a, _ := strconv.Atoi(m.Binds[i].Name)
			b, _ := strconv.Atoi(m.Binds[j].Name)
			return a < b
		})
		if n := strings.Count(m.Query, "?"); n != len(m.Binds) {
			return fmt.Errorf("sqlite_bg: %d '?' in sql but %d positional binds", n, len(m.Binds))
		}
	}
	return nil
}

func (m *SQLiteBG) resolveArgs(repl *caddy.Replacer) ([]any, error) {
	args := make([]any, len(m.Binds))
	for i, b := range m.Binds {
		if name, ok := unknownPlaceholder(repl, b.Value); ok {
			return nil, fmt.Errorf("unresolved bind %q: {%s}", b.Name, name)
		}
		v := repl.ReplaceAll(b.Value, "")
		if m.positional {
			args[i] = v
		} else {
			args[i] = sql.Named(b.Name, v)
		}
	}
	return args, nil
}

func (m *SQLiteBG) Cleanup() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bgShutdownGrace):
		if m.log != nil {
			m.log.Warn("sqlite_bg: timed out waiting for in-flight jobs")
		}
	}

	if m.db != nil {
		err := m.db.Release()
		m.db = nil
		return err
	}
	return nil
}

func (m *SQLiteBG) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	repl, _ := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if repl == nil {
		writeFail(w, http.StatusInternalServerError, "json", "no replacer in context")
		return nil
	}
	args, err := m.resolveArgs(repl)
	if err != nil {
		writeFail(w, http.StatusBadRequest, "json", err.Error())
		return nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		writeFail(w, http.StatusServiceUnavailable, "json", "shutting down")
		return nil
	}
	m.wg.Add(1)
	rootCtx := m.rootCtx
	m.mu.Unlock()

	jid := jobID()
	if m.plan.IsWrite {
		go m.runWrite(jid, rootCtx, args)
	} else {
		go m.runRead(jid, rootCtx, args)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"status": "accepted",
		"job_id": jid,
	})
	return nil
}

func (m *SQLiteBG) runWrite(jid string, ctx context.Context, args []any) {
	defer m.wg.Done()
	res, err := m.db.WriteStmt(ctx, m.parsedSQL, args, m.plan.ReturnsRows)
	m.storeResult(jid, err, nil, res)
}

func (m *SQLiteBG) runRead(jid string, ctx context.Context, args []any) {
	defer m.wg.Done()
	data, err := m.db.QueryDirect(ctx, m.parsedSQL, args)
	m.storeResult(jid, err, data, nil)
}

func (m *SQLiteBG) storeResult(jid string, err error, rows []map[string]any, res *database.BatchResult) {
	r := &bgResult{}
	if err != nil {
		r.err = err
	} else if res != nil {
		if res.HasRows {
			r.rows = res.Rows
		} else {
			r.rows = []map[string]any{}
		}
	} else {
		r.rows = rows
	}

	if m.Complete != nil && m.Complete.URL != "" {
		r.expiresAt = time.Now().Add(bgResultTTL)
		resultsMu.Lock()
		results[jid] = r
		resultsMu.Unlock()
		m.wg.Add(1)
		go m.deliverWebhook(jid, r)
	}
}

func (m *SQLiteBG) deliverWebhook(jid string, r *bgResult) {
	defer m.wg.Done()

	status := "ok"
	if r.err != nil {
		status = "error"
	}

	ctx, cancel := context.WithTimeout(context.Background(), bgWebhookTimeout)
	defer cancel()

	var req *http.Request
	var err error
	if m.Complete.Method == http.MethodPost {
		body := fmt.Sprintf(`{"status":%q,"job_id":%q}`, status, jid)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, m.Complete.URL, strings.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		target := m.Complete.URL + "?job_id=" + url.PathEscape(jid) + "&status=" + status
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	}
	if err != nil {
		m.log.Warn("webhook request build failed",
			zap.Error(err), zap.String("url", m.Complete.URL))
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.log.Warn("webhook failed",
			zap.Error(err), zap.String("url", m.Complete.URL),
			zap.String("method", m.Complete.Method))
		return
	}
	resp.Body.Close()
}

var (
	_ caddy.Provisioner           = (*SQLiteBG)(nil)
	_ caddy.CleanerUpper          = (*SQLiteBG)(nil)
	_ caddyhttp.MiddlewareHandler = (*SQLiteBG)(nil)
	_ caddyfile.Unmarshaler       = (*SQLiteBG)(nil)
)
