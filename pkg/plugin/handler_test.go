package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

// newTestEngine builds an engine around a real cluster: two writable schemas
// (app, media) plus the in-memory scratch, and one read-only schema (ro).
func newTestEngine(t *testing.T, profile database.Profile) *Engine {
	t.Helper()
	dir := t.TempDir()

	type schema struct {
		name string
		path string
		tbl  string
	}
	schemas := []schema{
		{"app", filepath.Join(dir, "app.db"), `CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT, asset_id INTEGER)`},
		{"media", filepath.Join(dir, "media.db"), `CREATE TABLE assets (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`},
		{"ro", filepath.Join(dir, "ro.db"), `CREATE TABLE snapshots (id INTEGER PRIMARY KEY, label TEXT)`},
	}
	var (
		attaches []database.Attach
		writable []string
		dbs      []*DBConfig
	)
	for _, s := range schemas {
		db, err := database.OpenDB(context.Background(), database.OpenConfig{Path: s.path})
		if err != nil {
			t.Fatalf("open %s: %v", s.name, err)
		}
		if _, err := db.Exec(s.tbl); err != nil {
			t.Fatalf("create %s: %v", s.name, err)
		}
		db.Close()

		readOnly := s.name == "ro"
		attaches = append(attaches, database.Attach{Schema: s.name, Path: s.path, ReadOnly: readOnly})
		if !readOnly {
			writable = append(writable, s.name)
		}
		dbs = append(dbs, &DBConfig{Name: s.name, Path: s.path, ReadOnly: readOnly})
	}

	cluster, err := database.OpenCluster(context.Background(), attaches, writable, profile)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	t.Cleanup(func() { cluster.Close() })

	return &Engine{engine: EngineConfig{
		BatchSize:    profile.BatchSize,
		BatchTimeout: profile.BatchTimeout,
		CacheTTL:     profile.CacheTTL,
		CacheMax:     profile.CacheMax,
		Timeout:      profile.Timeout,
	}, dbs: dbs, cluster: cluster, log: zap.NewNop()}
}

func testProfile() database.Profile {
	return database.Profile{
		BatchSize:    8,
		BatchTimeout: 5 * time.Millisecond,
		CacheTTL:     time.Minute,
		CacheMax:     64,
	}
}

func parseHandler(t *testing.T, body string) (*SQLite, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(body)
	m := new(SQLite)
	return m, m.UnmarshalCaddyfile(d)
}

func newHandler(t *testing.T, engine *Engine, query string, binds []Bind, output string) *SQLite {
	t.Helper()
	m := &SQLite{Query: query, Binds: binds, Output: output, engine: engine, log: zap.NewNop()}
	if err := m.parseBinds(); err != nil {
		t.Fatalf("parseBinds: %v", err)
	}
	plan, err := database.Classify(query)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	m.plan = plan
	if err := checkPlan(plan, output); err != nil {
		t.Fatalf("checkPlan: %v", err)
	}
	m.useCache = resolveUseCache(plan, output, m.Cache)
	if plan.IsWrite {
		schema, err := m.writeSchema()
		if err != nil {
			t.Fatalf("writeSchema: %v", err)
		}
		m.schema = schema
	}
	return m
}

func handlerRequest(m *SQLite, body string, repl *caddy.Replacer) (*httptest.ResponseRecorder, error) {
	if repl == nil {
		repl = caddy.NewReplacer()
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sql", strings.NewReader(body))
	req = req.WithContext(context.WithValue(context.Background(), caddy.ReplacerCtxKey, repl))
	return w, m.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("next"))
		return nil
	}))
}

func TestHandlerParseDefaults(t *testing.T) {
	m, err := parseHandler(t, `sqlite {
	query "SELECT 1 AS n"
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Output != "json" {
		t.Fatalf("output = %q, want json", m.Output)
	}
	if m.Continue || m.Status != 0 || m.Cache != nil {
		t.Fatalf("unexpected defaults: %+v", m)
	}
}

func TestHandlerParseOptions(t *testing.T) {
	m, err := parseHandler(t, `sqlite {
	query  "INSERT INTO app.notes (body) VALUES (:body)"
	bind   body {http.request.body}
	output json
	status 201
	cache  0
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Status != 201 || m.Cache == nil || *m.Cache {
		t.Fatalf("options = %+v", m)
	}
}

func TestHandlerParseContinue(t *testing.T) {
	m, err := parseHandler(t, `sqlite {
	query    "INSERT INTO app.notes (body) VALUES ('x')"
	continue
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !m.Continue {
		t.Fatal("continue not set")
	}
}

func TestHandlerParseRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing query", `sqlite { output json }`},
		{"unknown option", `sqlite {
	query "SELECT 1"
	nope 1
}`},
		{"bad output", `sqlite {
	query "SELECT 1"
	output yaml
}`},
		{"bad cache", `sqlite {
	query "SELECT 1"
	cache maybe
}`},
		{"bad status", `sqlite {
	query "SELECT 1"
	status nine
}`},
		{"status out of range", `sqlite {
	query "SELECT 1"
	status 900
}`},
		{"use is gone", `sqlite {
	use app
	query "SELECT 1"
}`},
		{"continue with output", `sqlite {
	query "SELECT 1"
	output json
	continue
}`},
		{"continue with status", `sqlite {
	query "SELECT 1"
	status 201
	continue
}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseHandler(t, c.body); err == nil {
				t.Fatalf("expected %s to be rejected", c.name)
			}
		})
	}
}

func TestHandlerParseBinds(t *testing.T) {
	m, err := parseHandler(t, `sqlite {
	query "INSERT INTO app.notes (body, asset_id) VALUES (?, ?)"
	bind  2 {http.request.uri.query.asset}
	bind  1 {http.request.body}
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := m.parseBinds(); err != nil {
		t.Fatalf("parseBinds: %v", err)
	}
	if !m.positional {
		t.Fatal("numeric names select positional mode")
	}
	if m.Binds[0].Name != "1" || m.Binds[1].Name != "2" {
		t.Fatalf("binds not sorted: %+v", m.Binds)
	}

	named, err := parseHandler(t, `sqlite {
	query "INSERT INTO app.notes (body) VALUES (:body)"
	bind  body {http.request.body}
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := named.parseBinds(); err != nil {
		t.Fatalf("parseBinds: %v", err)
	}
	if named.positional {
		t.Fatal("named bind must not select positional mode")
	}

	mixed, _ := parseHandler(t, `sqlite {
	query "INSERT INTO app.notes (body) VALUES (?)"
	bind  1 {http.request.body}
	bind  other {http.request.body}
}`)
	if err := mixed.parseBinds(); err == nil {
		t.Fatal("mixing modes must fail")
	}

	few, _ := parseHandler(t, `sqlite {
	query "INSERT INTO app.notes (body, asset_id) VALUES (?, ?)"
	bind  1 {http.request.body}
}`)
	if err := few.parseBinds(); err == nil {
		t.Fatal("placeholder count mismatch must fail")
	}
}

func TestHandlerWriteSchema(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		schema string
		err    bool
	}{
		{"qualified", `INSERT INTO app.notes (body) VALUES ('x')`, "app", false},
		{"scratch", `INSERT INTO main.hits (path) VALUES ('/x')`, "main", false},
		{"update", `UPDATE app.notes SET body = 'y' WHERE id = 1`, "app", false},
		{"delete", `DELETE FROM media.assets WHERE id = 1`, "media", false},
		{"unqualified", `INSERT INTO notes (body) VALUES ('x')`, "", true},
		{"two schemas", `INSERT INTO app.notes (id) SELECT id FROM media.assets; DELETE FROM media.assets`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &SQLite{Query: c.query}
			got, err := m.writeSchema()
			if c.err {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.schema {
				t.Fatalf("schema = %q, want %q", got, c.schema)
			}
		})
	}
}

func TestHandlerCheckPlan(t *testing.T) {
	read := database.Plan{ReturnsRows: true, Cacheable: true}
	write := database.Plan{IsWrite: true}
	writeReturning := database.Plan{IsWrite: true, ReturnsRows: true}

	cases := []struct {
		plan    database.Plan
		output  string
		wantErr bool
	}{
		{read, "json", false},
		{read, "dynamic", false},
		{read, "none", false},
		{write, "dynamic", true},
		{write, "json", false},
		{write, "xml", false},
		{write, "csv", true},
		{write, "plain", true},
		{write, "none", false},
		{writeReturning, "csv", false},
	}
	for _, c := range cases {
		err := checkPlan(c.plan, c.output)
		if c.wantErr && err == nil {
			t.Errorf("plan %+v output %s: expected error", c.plan, c.output)
		}
		if !c.wantErr && err != nil {
			t.Errorf("plan %+v output %s: %v", c.plan, c.output, err)
		}
	}
}

func TestHandlerResolveUseCache(t *testing.T) {
	yes, no := true, false
	cacheable := database.Plan{ReturnsRows: true, Cacheable: true}
	uncacheable := database.Plan{ReturnsRows: true}

	cases := []struct {
		plan   database.Plan
		output string
		cache  *bool
		want   bool
	}{
		{cacheable, "json", nil, true},
		{uncacheable, "json", nil, false},
		{cacheable, "json", &no, false},
		{uncacheable, "json", &yes, false},
		{cacheable, "dynamic", nil, false},
		{cacheable, "none", nil, false},
	}
	for _, c := range cases {
		if got := resolveUseCache(c.plan, c.output, c.cache); got != c.want {
			t.Errorf("plan %+v output %s cache %v: got %v want %v", c.plan, c.output, c.cache, got, c.want)
		}
	}
}

func TestHandlerRespondReads(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `SELECT id, body FROM app.notes ORDER BY id`, nil, "json")

	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('one'), ('two')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := handlerRequest(m, "", nil)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"body":"one"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestHandlerRespondWriteAndStatus(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `INSERT INTO app.notes (body) VALUES (?) RETURNING id, body`,
		[]Bind{{Name: "1", Value: "{http.request.body}"}}, "json")
	m.Status = 201

	repl := caddy.NewReplacer()
	repl.Set("http.request.body", "created")
	w, err := handlerRequest(m, "created", repl)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"created"`) {
		t.Fatalf("body = %s", w.Body.String())
	}

	var n int
	if err := engine.Cluster().Anchor().QueryRow(`SELECT COUNT(*) FROM app.notes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestHandlerRespondNoneSkipsBody(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `DELETE FROM app.notes WHERE id = ?`,
		[]Bind{{Name: "1", Value: "{http.request.uri.query.id}"}}, "none")
	m.Status = 204

	repl := caddy.NewReplacer()
	repl.Set("http.request.uri.query.id", "1")
	w, err := handlerRequest(m, "", repl)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != 204 {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", w.Body.String())
	}
}

func TestHandlerRespondNoneStillRunsTheStatement(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `DELETE FROM app.notes WHERE id = ?`,
		[]Bind{{Name: "1", Value: "{http.request.uri.query.id}"}}, "none")
	m.Status = 204

	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('doomed')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	repl := caddy.NewReplacer()
	repl.Set("http.request.uri.query.id", "1")
	w, err := handlerRequest(m, "", repl)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q", w.Code, w.Body.String())
	}

	var n int
	if err := engine.Cluster().Anchor().QueryRow(`SELECT COUNT(*) FROM app.notes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0: output none must still run the statement", n)
	}
}

func TestHandlerRespondDynamic(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine,
		`SELECT 'text/plain' AS content_type, 'inline' AS content, 418 AS http_code`, nil, "dynamic")

	w, err := handlerRequest(m, "", nil)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != 418 || w.Body.String() != "inline" {
		t.Fatalf("status = %d body = %q", w.Code, w.Body.String())
	}
}

func TestHandlerReadOnlySchemaIsRejected(t *testing.T) {
	engine := newTestEngine(t, testProfile())

	m := &SQLite{Query: `INSERT INTO ro.snapshots (label) VALUES ('x')`, engine: engine, log: zap.NewNop()}
	schema, err := m.writeSchema()
	if err != nil {
		t.Fatalf("writeSchema: %v", err)
	}
	if engine.Cluster().Writable(schema) {
		t.Fatal("ro schema must not be writable")
	}
}

func TestHandlerUnknownSchemaIsRejected(t *testing.T) {
	engine := newTestEngine(t, testProfile())

	m := &SQLite{Query: `INSERT INTO nope.t (x) VALUES (1)`, engine: engine, log: zap.NewNop()}
	schema, err := m.writeSchema()
	if err != nil {
		t.Fatalf("writeSchema: %v", err)
	}
	if _, ok := engine.DBConfig(schema); ok {
		t.Fatal("nope should not be declared")
	}
}

func TestHandlerUnresolvedBindReturnsError(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `INSERT INTO app.notes (body) VALUES (?)`,
		[]Bind{{Name: "1", Value: "{http.request.body.label}"}}, "json")

	repl := caddy.NewReplacer()
	repl.Set("http.request.body", `{"label":"x"}`)
	w, err := handlerRequest(m, `{"label":"x"}`, repl)
	if err == nil {
		t.Fatal("expected an unresolved bind to fail the request")
	}
	if !strings.Contains(err.Error(), "unresolved bind") {
		t.Fatalf("error = %v", err)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want nothing written", w.Body.String())
	}
}

func TestHandlerWriteFailureIsOpaque(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `INSERT INTO app.missing_table (x) VALUES (1)`, nil, "json")

	w, err := handlerRequest(m, "", nil)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "missing_table") {
		t.Fatalf("driver detail leaked: %s", w.Body.String())
	}
}

func TestHandlerTimeoutBoundsDynamicToo(t *testing.T) {
	engine := newTestEngine(t, database.Profile{
		BatchSize:    8,
		BatchTimeout: 5 * time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})
	m := newHandler(t, engine,
		`SELECT 'text/plain' AS content_type, CAST(COUNT(*) AS TEXT) AS content
		 FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 30000000) SELECT x FROM c)`,
		nil, "dynamic")

	w, err := handlerRequest(m, "", nil)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 from the deadline", w.Code)
	}
}

func TestHandlerTimeoutIsReported(t *testing.T) {
	engine := newTestEngine(t, database.Profile{
		BatchSize:    8,
		BatchTimeout: 5 * time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})
	m := newHandler(t, engine,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 30000000) SELECT COUNT(*) AS n FROM c`,
		nil, "json")

	w, err := handlerRequest(m, "", nil)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestHandlerContinuePassesThrough(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `INSERT INTO app.notes (body) VALUES (?)`,
		[]Bind{{Name: "1", Value: "{http.request.body}"}}, "json")
	m.Continue = true
	m.rootCtx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { m.Cleanup() })

	repl := caddy.NewReplacer()
	repl.Set("http.request.body", "queued")
	w, err := handlerRequest(m, "queued", repl)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the next handler's 418", w.Code)
	}
	if w.Body.String() != "next" {
		t.Fatalf("body = %q, want the next handler's body", w.Body.String())
	}

	m.wg.Wait()
	var n int
	if err := engine.Cluster().Anchor().QueryRow(`SELECT COUNT(*) FROM app.notes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want the background write to land", n)
	}
}

func TestHandlerContinueRejectsAfterCleanup(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `INSERT INTO app.notes (body) VALUES ('x')`, nil, "json")
	m.Continue = true
	m.rootCtx, m.cancel = context.WithCancel(context.Background())

	if err := m.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := handlerRequest(m, "", nil); err == nil {
		t.Fatal("expected dispatch after cleanup to fail")
	}
}

func TestHandlerCachedReadServedUntilWrite(t *testing.T) {
	engine := newTestEngine(t, testProfile())
	m := newHandler(t, engine, `SELECT COUNT(*) AS n FROM app.notes`, nil, "json")

	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('a')`, nil, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, _ := handlerRequest(m, "", nil)
	if !strings.Contains(w.Body.String(), `"n":1`) {
		t.Fatalf("first read = %s", w.Body.String())
	}

	// A write through the cluster invalidates the cached read.
	if _, err := engine.Cluster().WriteStmt(context.Background(), "app",
		`INSERT INTO app.notes (body) VALUES ('b')`, nil, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	w, _ = handlerRequest(m, "", nil)
	if !strings.Contains(w.Body.String(), `"n":2`) {
		t.Fatalf("second read = %s, want 2", w.Body.String())
	}
}
