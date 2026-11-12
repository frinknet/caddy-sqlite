package plugin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"caddysqlite/pkg/database"
)

type Bind struct {
	Name  string
	Value string
}

type SQLiteResponse struct {
	DB     string `json:"db,omitempty"`
	Query  string `json:"query,omitempty"`
	Cache  *bool  `json:"cache,omitempty"`
	Output string `json:"output,omitempty"`
	Binds  []Bind `json:"binds,omitempty"`

	parsedSQL  string
	positional bool
	plan       database.Plan
	db         *database.DB
	useCache   bool
	log        *zap.Logger
}

func (SQLiteResponse) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sqlite_response",
		New: func() caddy.Module { return new(SQLiteResponse) },
	}
}

func ParseResponse(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(SQLiteResponse)
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *SQLiteResponse) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
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
		case "cache":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v, err := strconv.ParseBool(d.Val())
			if err != nil {
				return d.Errf("cache: %v", err)
			}
			m.Cache = &v
		case "output":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "json", "xml", "csv", "plain", "dynamic":
				m.Output = d.Val()
			default:
				return d.Errf("output must be json, xml, csv, plain, or dynamic")
			}
		default:
			return d.Errf("sqlite_response: unknown option %q", d.Val())
		}
	}
	if m.Query == "" {
		return d.Err("sqlite_response: query is required")
	}
	if m.DB == "" {
		return d.Err("sqlite_response: use is required")
	}
	return nil
}

func (m *SQLiteResponse) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger(m)
	if m.Output == "" {
		m.Output = "json"
	}
	if err := m.parseBinds(); err != nil {
		return err
	}
	plan, err := database.Classify(m.Query)
	if err != nil {
		return err
	}
	m.plan = plan

	if m.Output == "dynamic" && m.plan.IsWrite {
		return fmt.Errorf("sqlite_response: output=dynamic requires a read query")
	}
	if m.plan.IsWrite && !m.plan.ReturnsRows && m.Output != "json" && m.Output != "xml" {
		return fmt.Errorf("sqlite_response: writes without RETURNING require output=json or xml")
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
		return fmt.Errorf("sqlite_response: cannot write to read-only DB %q", m.DB)
	}
	m.db = db

	stmt, err := db.SQL().PrepareContext(ctx, m.parsedSQL)
	if err != nil {
		db.Release()
		m.db = nil
		return fmt.Errorf("sqlite_response: prepare: %w", err)
	}
	stmt.Close()

	m.useCache = m.plan.Cacheable
	if m.Cache != nil {
		m.useCache = *m.Cache && m.plan.Cacheable
	}
	if m.Output == "dynamic" {
		m.useCache = false
	}
	return nil
}

func (m *SQLiteResponse) parseBinds() error {
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
			return fmt.Errorf("sqlite_response: bind %q mixes positional and named", b.Name)
		}
	}
	m.positional = mode == "pos"
	m.parsedSQL = m.Query

	if m.positional {
		nums := make([]int, len(m.Binds))
		for i, b := range m.Binds {
			n, err := strconv.Atoi(b.Name)
			if err != nil {
				return fmt.Errorf("sqlite_response: bind %q is not a number", b.Name)
			}
			nums[i] = n
		}
		sort.Ints(nums)
		for i, n := range nums {
			if n != i+1 {
				return fmt.Errorf("sqlite_response: positional binds must be 1..%d in order, got %v", len(m.Binds), nums)
			}
		}

		sort.SliceStable(m.Binds, func(i, j int) bool {
			a, _ := strconv.Atoi(m.Binds[i].Name)
			b, _ := strconv.Atoi(m.Binds[j].Name)
			return a < b
		})

		if n := strings.Count(m.Query, "?"); n != len(m.Binds) {
			return fmt.Errorf("sqlite_response: %d '?' in sql but %d positional binds", n, len(m.Binds))
		}
	}
	return nil
}

func (m *SQLiteResponse) Cleanup() error {
	if m.db != nil {
		err := m.db.Release()
		m.db = nil
		return err
	}
	return nil
}

func (m *SQLiteResponse) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	repl, _ := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if repl == nil {
		writeFail(w, http.StatusInternalServerError, m.Output, "no replacer in context")
		return nil
	}

	args, err := m.resolveArgs(repl)
	if err != nil {
		writeFail(w, http.StatusBadRequest, m.Output, err.Error())
		return nil
	}

	if m.plan.IsWrite {
		res, err := m.db.WriteStmt(r.Context(), m.parsedSQL, args, m.plan.ReturnsRows)
		if err != nil {
			m.log.Error("sqlite write failed", zap.Error(err), zap.String("sql", m.parsedSQL))
			writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
			return nil
		}
		writeResult(w, m.Output, res)
		return nil
	}

	if m.Output == "dynamic" {
		rows, err := m.db.SQL().QueryContext(r.Context(), m.parsedSQL, args...)
		if err != nil {
			m.log.Error("sqlite read failed", zap.Error(err), zap.String("sql", m.parsedSQL))
			writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
			return nil
		}
		defer rows.Close()
		m.writeDynamic(w, rows, m.log)
		return nil
	}

	var data []map[string]any
	if m.useCache {
		data, err = m.db.QueryCached(r.Context(), m.parsedSQL, args)
	} else {
		data, err = m.db.QueryDirect(r.Context(), m.parsedSQL, args)
	}
	if err != nil {
		m.log.Error("sqlite read failed", zap.Error(err), zap.String("sql", m.parsedSQL))
		writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
		return nil
	}
	writeRows(w, m.Output, data)
	return nil
}

var (
	_ caddy.Provisioner           = (*SQLiteResponse)(nil)
	_ caddy.CleanerUpper          = (*SQLiteResponse)(nil)
	_ caddyhttp.MiddlewareHandler = (*SQLiteResponse)(nil)
	_ caddyfile.Unmarshaler       = (*SQLiteResponse)(nil)
)
