package plugin

import (
	"context"
	"fmt"
	"html/template"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

type SQLiteTemplateFuncs struct {
	engine *Engine
}

func (SQLiteTemplateFuncs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.templates.functions.sqlite",
		New: func() caddy.Module { return new(SQLiteTemplateFuncs) },
	}
}

func (SQLiteTemplateFuncs) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(d.Nesting()) {
		return d.Err("sqlite template function takes no options")
	}
	return nil
}

func (m *SQLiteTemplateFuncs) Provision(ctx caddy.Context) error {
	appIface, err := ctx.App("sqlite")
	if err != nil {
		return err
	}
	engine, ok := appIface.(*Engine)
	if !ok {
		return fmt.Errorf("sqlite: unexpected app type %T", appIface)
	}
	m.engine = engine
	return nil
}

func (m *SQLiteTemplateFuncs) CustomTemplateFunctions() template.FuncMap {
	return template.FuncMap{
		"sqlite": m.query,
	}
}

// query executes SQL and returns the resulting rows. There is no database
// argument: the schema named in the statement selects the data, because every
// declared database is attached to the same pool. Reads share the cache with
// handlers; writes route to the schema's batcher and bump the generation
// counter. Non-RETURNING writes return an empty slice so templates always have
// something iterable.
func (m *SQLiteTemplateFuncs) query(sqlText string, args ...any) (any, error) {
	cluster := m.engine.Cluster()
	plan, err := database.Classify(sqlText)
	if err != nil {
		return nil, err
	}

	if plan.IsWrite {
		schema, err := database.WriteTarget(sqlText)
		if err != nil {
			return nil, err
		}
		if schema == "" {
			return nil, fmt.Errorf("sqlite: writes must name a schema (use `main.` for the in-memory scratch)")
		}
		canonical, ok := m.engine.CanonicalSchema(schema)
		if !ok {
			return nil, fmt.Errorf("sqlite: unknown database %q", schema)
		}
		res, err := cluster.WriteStmt(context.Background(), canonical, sqlText, args, plan.ReturnsRows)
		if err != nil {
			return nil, err
		}
		if res.HasRows {
			return res.Rows, nil
		}
		return []map[string]any{}, nil
	}

	return cluster.QueryCached(context.Background(), sqlText, args)
}

var (
	_ caddy.Module      = (*SQLiteTemplateFuncs)(nil)
	_ caddy.Provisioner = (*SQLiteTemplateFuncs)(nil)
)
