package plugin

import (
	"context"
	"html/template"

	"github.com/caddyserver/caddy/v2"

	"caddysqlite/pkg/database"
)

type SQLiteTemplateFuncs struct {
	app *SQLiteDBs
}

func (SQLiteTemplateFuncs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.templates.functions.sqlite",
		New: func() caddy.Module { return new(SQLiteTemplateFuncs) },
	}
}

func (m *SQLiteTemplateFuncs) Provision(ctx caddy.Context) error {
	appIface, err := ctx.App("sqlite")
	if err != nil {
		return err
	}
	m.app = appIface.(*SQLiteDBs)
	return nil
}

func (m *SQLiteTemplateFuncs) CustomTemplateFunctions() template.FuncMap {
	return template.FuncMap{
		"sqlite": m.query,
	}
}

// query executes SQL and returns the resulting rows. Reads go through the
// shared per-DB cache; writes go through the batcher and bump the generation
// counter on commit, invalidating cached reads on the same DB. Non-RETURNING
// writes return an empty slice so templates have something iterable.
func (m *SQLiteTemplateFuncs) query(dbName, sqlText string, args ...any) (any, error) {
	db, err := m.app.Lookup(dbName)
	if err != nil {
		return nil, err
	}
	defer db.Release()

	plan, err := database.Classify(sqlText)
	if err != nil {
		return nil, err
	}

	if plan.IsWrite {
		res, err := db.WriteStmt(context.Background(), sqlText, args, plan.ReturnsRows)
		if err != nil {
			return nil, err
		}
		if res.HasRows {
			return res.Rows, nil
		}
		return []map[string]any{}, nil
	}

	return db.QueryCached(context.Background(), sqlText, args)
}

var (
	_ caddy.Module      = (*SQLiteTemplateFuncs)(nil)
	_ caddy.Provisioner = (*SQLiteTemplateFuncs)(nil)
)
