package plugin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

const bgShutdownGrace = 10 * time.Second

// SQLite runs one statement per request against the cluster. With output set it
// answers; with `continue` it dispatches in the background and passes the
// request to the next handler.
type SQLite struct {
	Query    string `json:"query,omitempty"`
	Output   string `json:"output,omitempty"`
	Status   int    `json:"status,omitempty"`
	Cache    *bool  `json:"cache,omitempty"`
	Continue bool   `json:"continue,omitempty"`
	Binds    []Bind `json:"binds,omitempty"`

	parsedSQL  string
	outputSet  bool
	positional bool
	plan       database.Plan
	schema     string
	useCache   bool
	engine     *Engine
	log        *zap.Logger

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	rootCtx context.Context
	cancel  context.CancelFunc
}

func (*SQLite) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sqlite",
		New: func() caddy.Module { return new(SQLite) },
	}
}

func ParseHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(SQLite)
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *SQLite) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for n := d.Nesting(); d.NextBlock(n); {
		switch d.Val() {
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
		case "cache":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v, err := strconv.ParseBool(d.Val())
			if err != nil {
				return d.Errf("cache: %v", err)
			}
			m.Cache = &v
		case "status":
			if !d.NextArg() {
				return d.ArgErr()
			}
			code, err := strconv.Atoi(d.Val())
			if err != nil || code < 100 || code > 599 {
				return d.Errf("status must be a valid HTTP status code")
			}
			m.Status = code
		case "output":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "json", "xml", "csv", "plain", "dynamic", "none":
				m.Output = d.Val()
				m.outputSet = true
			default:
				return d.Errf("output must be json, xml, csv, plain, dynamic, or none")
			}
		case "continue":
			m.Continue = true
		default:
			return d.Errf("sqlite: unknown option %q", d.Val())
		}
	}
	if m.Query == "" {
		return d.Err("sqlite: query is required")
	}
	if m.Continue && m.outputSet {
		return d.Err("sqlite: continue cannot be combined with output")
	}
	if m.Continue && m.Status != 0 {
		return d.Err("sqlite: continue cannot be combined with status")
	}
	if m.Output == "" {
		m.Output = "json"
	}
	return nil
}

func (m *SQLite) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger(m)
	if err := m.parseBinds(); err != nil {
		return err
	}
	plan, err := database.Classify(m.Query)
	if err != nil {
		return err
	}
	m.plan = plan

	if err := checkPlan(plan, m.Output); err != nil {
		return err
	}
	m.useCache = resolveUseCache(plan, m.Output, m.Cache)

	appIface, err := ctx.App("sqlite")
	if err != nil {
		return err
	}
	engine, ok := appIface.(*Engine)
	if !ok {
		return fmt.Errorf("sqlite: unexpected app type %T", appIface)
	}
	m.engine = engine

	if m.plan.IsWrite {
		schema, err := m.writeSchema()
		if err != nil {
			return err
		}
		m.schema = schema
		if schema != database.ScratchSchema && !engine.Cluster().Writable(schema) {
			if cfg, ok := engine.DBConfig(schema); ok && cfg.ReadOnly {
				return fmt.Errorf("sqlite: cannot write to read-only database %q", schema)
			}
			return fmt.Errorf("sqlite: unknown database %q: declare it with `db %s <file>`", schema, schema)
		}
	}

	stmt, err := engine.Cluster().Anchor().PrepareContext(ctx, m.parsedSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare: %w", err)
	}
	stmt.Close()

	if m.Continue {
		m.rootCtx, m.cancel = context.WithCancel(context.Background())
	}
	return nil
}

// writeSchema resolves which schema a write touches. Writes must name exactly
// one schema: the scratch schema is spelled out as `main.`, so a forgotten
// qualifier is a config error instead of a silent scratch write.
func (m *SQLite) writeSchema() (string, error) {
	schema, err := database.WriteTarget(m.Query)
	if err != nil {
		return "", fmt.Errorf("sqlite: %w", err)
	}
	if schema == "" {
		return "", fmt.Errorf("sqlite: writes must name a schema (use `main.` for the in-memory scratch): %.60q", m.Query)
	}
	return schema, nil
}

func (m *SQLite) parseBinds() error {
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
			return fmt.Errorf("sqlite: bind %q mixes positional and named", b.Name)
		}
	}
	m.positional = mode == "pos"
	m.parsedSQL = m.Query

	if m.positional {
		nums := make([]int, len(m.Binds))
		for i, b := range m.Binds {
			n, err := strconv.Atoi(b.Name)
			if err != nil {
				return fmt.Errorf("sqlite: bind %q is not a number", b.Name)
			}
			nums[i] = n
		}
		sort.Ints(nums)
		for i, n := range nums {
			if n != i+1 {
				return fmt.Errorf("sqlite: positional binds must be 1..%d in order", len(m.Binds))
			}
		}
		sort.SliceStable(m.Binds, func(i, j int) bool {
			a, _ := strconv.Atoi(m.Binds[i].Name)
			b, _ := strconv.Atoi(m.Binds[j].Name)
			return a < b
		})
		if n := strings.Count(m.Query, "?"); n != len(m.Binds) {
			return fmt.Errorf("sqlite: %d '?' in sql but %d positional binds", n, len(m.Binds))
		}
	}
	return nil
}

func checkPlan(plan database.Plan, output string) error {
	if output == "dynamic" && plan.IsWrite {
		return fmt.Errorf("sqlite: output=dynamic requires a read query")
	}
	if output == "none" {
		return nil
	}
	if plan.IsWrite && !plan.ReturnsRows && output != "json" && output != "xml" {
		return fmt.Errorf("sqlite: writes without RETURNING require output=json or xml")
	}
	return nil
}

func resolveUseCache(plan database.Plan, output string, cache *bool) bool {
	if output == "dynamic" || output == "none" {
		return false
	}
	if cache != nil {
		return *cache && plan.Cacheable
	}
	return plan.Cacheable
}

func (m *SQLite) Cleanup() error {
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
	if m.Continue {
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(bgShutdownGrace):
			if m.log != nil {
				m.log.Warn("sqlite: timed out waiting for in-flight jobs")
			}
		}
	}
	return nil
}

func (m *SQLite) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl, _ := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if repl == nil {
		return fmt.Errorf("sqlite: no replacer in context")
	}
	args, err := m.resolveArgs(repl)
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}

	if m.Continue {
		return m.dispatch(w, r, next, args)
	}
	return m.respond(w, r, args)
}

// dispatch hands the statement to the background and lets the next handler
// answer. Dispatch failures are returned, not written: this handler does not own
// the response, so it must not claim success for a write that never queued.
func (m *SQLite) dispatch(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, args []any) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("sqlite: shutting down")
	}
	m.wg.Add(1)
	rootCtx := m.rootCtx
	m.mu.Unlock()

	if m.plan.IsWrite {
		go func() {
			defer m.wg.Done()
			if _, err := m.engine.Cluster().WriteStmt(rootCtx, m.schema, m.parsedSQL, args, m.plan.ReturnsRows); err != nil && m.log != nil {
				m.log.Warn("sqlite: background write failed", zap.Error(err), zap.String("sql", m.parsedSQL))
			}
		}()
	} else {
		go func() {
			defer m.wg.Done()
			if _, err := m.engine.Cluster().QueryDirect(rootCtx, m.parsedSQL, args); err != nil && m.log != nil {
				m.log.Warn("sqlite: background read failed", zap.Error(err), zap.String("sql", m.parsedSQL))
			}
		}()
	}
	return next.ServeHTTP(w, r)
}

func (m *SQLite) respond(w http.ResponseWriter, r *http.Request, args []any) error {
	cluster := m.engine.Cluster()

	// output none still runs the statement: it just does not serialize a body.
	if m.plan.IsWrite {
		res, err := cluster.WriteStmt(r.Context(), m.schema, m.parsedSQL, args, m.plan.ReturnsRows)
		if err != nil {
			m.logError("write failed", err)
			writeFail(w, m.statusOrDefault(http.StatusInternalServerError), m.Output, "internal error")
			return nil
		}
		if m.Output == "none" {
			w.WriteHeader(m.statusOrDefault(http.StatusNoContent))
			return nil
		}
		writeResult(w, m.Output, m.statusOrDefault(http.StatusOK), res)
		return nil
	}

	if m.Output == "none" {
		if _, err := cluster.QueryDirect(r.Context(), m.parsedSQL, args); err != nil {
			m.logError("read failed", err)
			writeFail(w, m.statusOrDefault(http.StatusInternalServerError), m.Output, "internal error")
			return nil
		}
		w.WriteHeader(m.statusOrDefault(http.StatusNoContent))
		return nil
	}

	if m.plan.IsWrite {
		res, err := cluster.WriteStmt(r.Context(), m.schema, m.parsedSQL, args, m.plan.ReturnsRows)
		if err != nil {
			m.logError("write failed", err)
			writeFail(w, m.statusOrDefault(http.StatusInternalServerError), m.Output, "internal error")
			return nil
		}
		writeResult(w, m.Output, m.statusOrDefault(http.StatusOK), res)
		return nil
	}

	if m.Output == "dynamic" {
		rows, cancel, err := cluster.QueryStream(r.Context(), m.parsedSQL, args)
		if err != nil {
			m.logError("read failed", err)
			writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
			return nil
		}
		defer cancel()
		defer rows.Close()
		m.writeDynamic(w, rows, m.log)
		return nil
	}

	var (
		data []map[string]any
		err  error
	)
	if m.useCache {
		data, err = cluster.QueryCached(r.Context(), m.parsedSQL, args)
	} else {
		data, err = cluster.QueryDirect(r.Context(), m.parsedSQL, args)
	}
	if err != nil {
		m.logError("read failed", err)
		writeFail(w, m.statusOrDefault(http.StatusInternalServerError), m.Output, "internal error")
		return nil
	}
	writeRows(w, m.Output, m.statusOrDefault(http.StatusOK), data)
	return nil
}

func (m *SQLite) statusOrDefault(fallback int) int {
	if m.Status != 0 {
		return m.Status
	}
	return fallback
}

func (m *SQLite) logError(msg string, err error) {
	if m.log != nil {
		m.log.Error("sqlite: "+msg, zap.Error(err), zap.String("sql", m.parsedSQL))
	}
}

var (
	_ caddy.Provisioner           = (*SQLite)(nil)
	_ caddy.CleanerUpper          = (*SQLite)(nil)
	_ caddyhttp.MiddlewareHandler = (*SQLite)(nil)
	_ caddyfile.Unmarshaler       = (*SQLite)(nil)
)
