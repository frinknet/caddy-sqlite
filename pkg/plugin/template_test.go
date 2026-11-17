package plugin

import (
	"context"
	"testing"
)

func TestTemplateQueryReads(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}

	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('first'), ('second')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := m.query(`SELECT id, body FROM app.notes ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows, ok := got.([]map[string]any)
	if !ok {
		t.Fatalf("query returned %T, want []map[string]any", got)
	}
	if len(rows) != 2 || rows[0]["body"] != "first" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestTemplateQueryJoinsAcrossSchemas(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}
	ctx := context.Background()

	if _, err := engine.Cluster().WriteStmt(ctx, "media", `INSERT INTO media.assets (name) VALUES ('logo')`, nil, false); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	if _, err := engine.Cluster().WriteStmt(ctx, "app",
		`INSERT INTO app.notes (body, asset_id) VALUES ('hello', 1)`, nil, false); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	got, err := m.query(`SELECT n.body, m.name AS asset FROM app.notes n JOIN media.assets m ON m.id = n.asset_id`)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	rows := got.([]map[string]any)
	if len(rows) != 1 || rows[0]["asset"] != "logo" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestTemplateQueryPositionalArgs(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}

	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('one'), ('two')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := m.query(`SELECT body FROM app.notes WHERE id = ?`, int64(2))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows := got.([]map[string]any)
	if len(rows) != 1 || rows[0]["body"] != "two" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestTemplateQueryWriteInvalidatesCache(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}
	before, err := m.query(`SELECT COUNT(*) AS n FROM app.notes`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := before.([]map[string]any)[0]["n"]; n != int64(0) {
		t.Fatalf("n = %v, want 0", n)
	}

	out, err := m.query(`INSERT INTO app.notes (body) VALUES ('tpl')`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if rows, ok := out.([]map[string]any); !ok || len(rows) != 0 {
		t.Fatalf("non-RETURNING write returned %#v, want an empty slice", out)
	}
	if got := engine.Cluster().Gen(); got == 0 {
		t.Fatal("a template write must bump the generation counter")
	}

	after, err := m.query(`SELECT COUNT(*) AS n FROM app.notes`)
	if err != nil {
		t.Fatalf("read after write: %v", err)
	}
	if n := after.([]map[string]any)[0]["n"]; n != int64(1) {
		t.Fatalf("n = %v, want 1: the write must invalidate the cached read", n)
	}
}

func TestTemplateQueryWriteNeedsSchema(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}

	if _, err := m.query(`INSERT INTO notes (body) VALUES ('x')`); err == nil {
		t.Fatal("an unqualified write must fail")
	}
}

func TestTemplateQueryRejectsBadInput(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := &SQLiteTemplateFuncs{engine: engine}

	if _, err := m.query(`NOT SQL AT ALL`); err == nil {
		t.Fatal("expected an unclassifiable statement to fail")
	}
	if _, err := m.query(`SELECT 1 FROM app.no_such_table`); err == nil {
		t.Fatal("expected a missing table to fail")
	}
	if _, err := m.query(`INSERT INTO nope.t (x) VALUES (1)`); err == nil {
		t.Fatal("expected an unknown schema to fail")
	}
}
