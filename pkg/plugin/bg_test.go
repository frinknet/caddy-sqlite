package plugin

import (
	"context"
	"io"
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

func newBG(t *testing.T, query string, binds []Bind, complete *CompleteConfig) *SQLiteBG {
	t.Helper()
	pool := database.NewPool()
	db, err := pool.Acquire(context.Background(), filepath.Join(t.TempDir(), "bg.db"),
		database.Profile{BatchSize: 8, BatchTimeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := db.SQL().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	m := &SQLiteBG{DB: "bg", Query: query, Binds: binds, Complete: complete, log: zap.NewNop(), db: db}
	if err := m.parseBinds(); err != nil {
		t.Fatalf("parseBinds: %v", err)
	}
	plan, err := database.Classify(query)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	m.plan = plan
	m.rootCtx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { m.Cleanup() })
	return m
}

func bgPost(t *testing.T, m *SQLiteBG, body string) *httptest.ResponseRecorder {
	t.Helper()
	repl := caddy.NewReplacer()
	repl.Set("http.request.body", body)
	ctx := context.WithValue(context.Background(), caddy.ReplacerCtxKey, repl)
	req := httptest.NewRequest(http.MethodPost, "/bg", strings.NewReader(body)).WithContext(ctx)

	w := httptest.NewRecorder()
	if err := m.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return w
}

func bgJobID(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `"job_id":"`)
	if idx < 0 {
		t.Fatalf("no job id in response: %s", body)
	}
	rest := body[idx+len(`"job_id":"`):]
	return rest[:strings.Index(rest, `"`)]
}

func rowCount(t *testing.T, m *SQLiteBG) int {
	t.Helper()
	var n int
	if err := m.db.SQL().QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestBGDispatchesWriteAndSkipsResultStore(t *testing.T) {
	m := newBG(t, `INSERT INTO items (label) VALUES (?)`, []Bind{{Name: "1", Value: "{http.request.body}"}}, nil)

	w := bgPost(t, m, "hello")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"accepted"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	jid := bgJobID(t, w.Body.String())

	m.wg.Wait()
	if n := rowCount(t, m); n != 1 {
		t.Fatalf("rows = %d, want 1: the background write never landed", n)
	}

	resultsMu.Lock()
	_, stored := results[jid]
	resultsMu.Unlock()
	if stored {
		t.Fatal("a job without on_complete must not be recorded in the results map")
	}
}

func TestBGStoresResultAndPostsWebhook(t *testing.T) {
	type delivery struct {
		method      string
		contentType string
		body        string
	}
	got := make(chan delivery, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- delivery{method: r.Method, contentType: r.Header.Get("Content-Type"), body: string(body)}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := newBG(t, `INSERT INTO items (label) VALUES ('hook')`, nil,
		&CompleteConfig{URL: srv.URL, Method: http.MethodPost})

	w := bgPost(t, m, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	jid := bgJobID(t, w.Body.String())

	select {
	case d := <-got:
		if d.method != http.MethodPost {
			t.Fatalf("webhook method = %s, want POST", d.method)
		}
		if d.contentType != "application/json" {
			t.Fatalf("webhook content type = %q", d.contentType)
		}
		if !strings.Contains(d.body, `"status":"ok"`) || !strings.Contains(d.body, jid) {
			t.Fatalf("webhook body = %s", d.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was never delivered")
	}

	m.wg.Wait()

	resultsMu.Lock()
	r, stored := results[jid]
	resultsMu.Unlock()
	if !stored {
		t.Fatal("a job with on_complete must be recorded in the results map")
	}
	if time.Until(r.expiresAt) <= 0 || time.Until(r.expiresAt) > bgResultTTL {
		t.Fatalf("result expiry = %s, want within %s", r.expiresAt, bgResultTTL)
	}
}

func TestBGWebhookGETCarriesJobIDAndStatus(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
	}))
	defer srv.Close()

	m := newBG(t, `INSERT INTO items (label) VALUES ('get-hook')`, nil,
		&CompleteConfig{URL: srv.URL, Method: http.MethodGet})

	w := bgPost(t, m, "")
	jid := bgJobID(t, w.Body.String())

	select {
	case r := <-got:
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Query().Get("job_id") != jid {
			t.Fatalf("job_id = %q, want %q", r.URL.Query().Get("job_id"), jid)
		}
		if r.URL.Query().Get("status") != "ok" {
			t.Fatalf("status = %q, want ok", r.URL.Query().Get("status"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was never delivered")
	}
	m.wg.Wait()
}

func TestBGWebhookReportsFailure(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- string(body)
	}))
	defer srv.Close()

	m := newBG(t, `INSERT INTO missing_table (x) VALUES (1)`, nil,
		&CompleteConfig{URL: srv.URL, Method: http.MethodPost})

	if w := bgPost(t, m, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for an accepted job", w.Code)
	}

	select {
	case body := <-got:
		if !strings.Contains(body, `"status":"error"`) {
			t.Fatalf("webhook body = %s, want an error status", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was never delivered")
	}
	m.wg.Wait()
}

func TestBGRejectsUnresolvedBind(t *testing.T) {
	m := newBG(t, `INSERT INTO items (label) VALUES (?)`,
		[]Bind{{Name: "1", Value: "{http.request.body.label}"}}, nil)

	w := bgPost(t, m, `{"label":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unresolved bind") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestBGCleanupRejectsNewJobs(t *testing.T) {
	m := newBG(t, `INSERT INTO items (label) VALUES ('x')`, nil, nil)

	if err := m.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	w := bgPost(t, m, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after cleanup", w.Code)
	}
	if !strings.Contains(w.Body.String(), "shutting down") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestSweepExpiredResults(t *testing.T) {
	now := time.Now()
	resultsMu.Lock()
	results["test-stale"] = &bgResult{expiresAt: now.Add(-time.Minute)}
	results["test-fresh"] = &bgResult{expiresAt: now.Add(time.Minute)}
	resultsMu.Unlock()
	t.Cleanup(func() {
		resultsMu.Lock()
		delete(results, "test-stale")
		delete(results, "test-fresh")
		resultsMu.Unlock()
	})

	sweepExpiredResults(now)

	resultsMu.Lock()
	_, stale := results["test-stale"]
	_, fresh := results["test-fresh"]
	resultsMu.Unlock()

	if stale {
		t.Fatal("expired result survived the sweep")
	}
	if !fresh {
		t.Fatal("unexpired result was evicted")
	}
}
