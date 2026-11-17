package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

func benchRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"id":    int64(i + 1),
			"label": "row-" + string(rune('a'+i%26)),
			"note":  nil,
		}
	}
	return rows
}

func BenchmarkWriteRowsJSON(b *testing.B) {
	rows := benchRows(50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		writeRows(w, "json", http.StatusOK, rows)
	}
}

func BenchmarkWriteRowsCSV(b *testing.B) {
	rows := benchRows(50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		writeRows(w, "csv", http.StatusOK, rows)
	}
}

func BenchmarkWriteRowsPlain(b *testing.B) {
	rows := benchRows(50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		writeRows(w, "plain", http.StatusOK, rows)
	}
}

func BenchmarkResolveArgs(b *testing.B) {
	m := &SQLite{
		Binds: []Bind{
			{Name: "1", Value: "{http.request.body}"},
			{Name: "2", Value: "{http.request.uri.query.page}"},
		},
		positional: true,
	}
	repl := caddy.NewReplacer()
	repl.Set("http.request.body", `{"label":"x"}`)
	repl.Set("http.request.uri.query.page", "3")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.resolveArgs(repl); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkServeHTTPCachedRead(b *testing.B) {
	engine := newBenchEngine(b)
	m := &SQLite{Query: `SELECT id, body FROM app.notes`, Output: "json", engine: engine, log: zap.NewNop()}
	if err := m.parseBinds(); err != nil {
		b.Fatal(err)
	}
	m.plan, _ = database.Classify(m.Query)
	m.useCache = resolveUseCache(m.plan, m.Output, m.Cache)

	repl := caddy.NewReplacer()
	ctx := context.WithValue(context.Background(), caddy.ReplacerCtxKey, repl)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/sql", nil).WithContext(ctx)
		if err := m.ServeHTTP(w, req, nil); err != nil {
			b.Fatal(err)
		}
		if w.Code != http.StatusOK {
			b.Fatalf("status = %d", w.Code)
		}
	}
}

func newBenchEngine(b *testing.B) *Engine {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := database.OpenDB(context.Background(), database.OpenConfig{Path: path})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := db.Exec(`INSERT INTO notes (body) VALUES ('x')`); err != nil {
			b.Fatal(err)
		}
	}
	db.Close()

	profile := database.Profile{BatchSize: 128, BatchTimeout: time.Millisecond, CacheTTL: time.Minute, CacheMax: 64}
	cluster, err := database.OpenCluster(context.Background(),
		[]database.Attach{{Schema: "app", Path: path}}, []string{"app"}, profile)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { cluster.Close() })
	return &Engine{engine: EngineConfig{BatchSize: 128, BatchTimeout: time.Millisecond, CacheTTL: time.Minute, CacheMax: 64},
		dbs: []*DBConfig{{Name: "app", Path: path}}, cluster: cluster, log: zap.NewNop()}
}

func BenchmarkJSONEncodeResponse(b *testing.B) {
	rows := benchRows(50)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(responseOf(rows)); err != nil {
			b.Fatal(err)
		}
	}
}

func responseOf(rows []map[string]any) any {
	return struct {
		Status string           `json:"status"`
		Data   map[string]any   `json:"data"`
		Rows   []map[string]any `json:"-"`
	}{Status: "ok", Data: map[string]any{"rows": rows}}
}

var _ = strings.TrimSpace
