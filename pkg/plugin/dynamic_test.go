package plugin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/frinknet/caddy-sqlite/pkg/database"
)

func dynamicTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenDB(context.Background(), database.OpenConfig{Path: filepath.Join(t.TempDir(), "dyn.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func runDynamic(t *testing.T, db *sql.DB, query string) *httptest.ResponseRecorder {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	m := &SQLite{Output: "dynamic"}
	w := httptest.NewRecorder()
	m.writeDynamic(w, rows, zap.NewNop())
	return w
}

func TestDynamicMapsColumnsByName(t *testing.T) {
	db := dynamicTestDB(t)

	// Deliberately not in the canonical order: the serializer must follow the
	// column names, not their positions.
	w := runDynamic(t, db, `SELECT 'text/plain' AS content_type, 'hello' AS content, 201 AS http_code`)

	if w.Code != 201 {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("content type = %q, want text/plain", got)
	}
	if got := w.Body.String(); got != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
}

func TestDynamicWithoutHTTPCodeDefaultsTo200(t *testing.T) {
	db := dynamicTestDB(t)

	w := runDynamic(t, db, `SELECT 'application/json' AS content_type, '{}' AS content`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := w.Body.String(); got != "{}" {
		t.Fatalf("body = %q", got)
	}
}

func TestDynamicRejectsBadShapes(t *testing.T) {
	db := dynamicTestDB(t)
	if _, err := db.Exec(`CREATE TABLE pages (id INTEGER PRIMARY KEY, content TEXT, content_type TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pages (content, content_type) VALUES ('a', 'text/plain'), ('b', 'text/plain')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cases := []struct {
		name  string
		query string
	}{
		{"no rows", `SELECT 'text/plain' AS content_type, 'x' AS content WHERE 0`},
		{"two rows", `SELECT content_type, content FROM pages`},
		{"missing content", `SELECT 'text/plain' AS content_type`},
		{"missing content_type", `SELECT 'x' AS content`},
		{"unknown column", `SELECT 'x' AS content, 'text/plain' AS content_type, 1 AS extra`},
		{"duplicate column", `SELECT 'x' AS content, 'y' AS content, 'text/plain' AS content_type`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := runDynamic(t, db, c.query)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestDynamicEmptyContentTypeLeavesHeaderAlone(t *testing.T) {
	db := dynamicTestDB(t)

	w := runDynamic(t, db, `SELECT '' AS content_type, 'body' AS content`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "" {
		t.Fatalf("content type = %q, want it left unset", got)
	}
	if got := w.Body.String(); got != "body" {
		t.Fatalf("body = %q", got)
	}
}

func TestDynamicErrorBodyIsEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	writeFail(w, http.StatusBadGateway, "dynamic", "boom")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", w.Body.String())
	}
}

func TestWriteFailFormats(t *testing.T) {
	cases := []struct {
		format      string
		status      int
		contentType string
		body        string
	}{
		{"json", http.StatusBadRequest, "application/json", `{"status":"error","message":"bad <input>"}`},
		{"xml", http.StatusBadRequest, "application/xml", `<error><message>bad &lt;input&gt;</message></error>`},
		{"csv", http.StatusBadRequest, "text/csv", ``},
		{"plain", http.StatusBadRequest, "text/plain; charset=utf-8", `bad <input>`},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeFail(w, c.status, c.format, "bad <input>")

			if w.Code != c.status {
				t.Fatalf("status = %d, want %d", w.Code, c.status)
			}
			if got := w.Header().Get("Content-Type"); got != c.contentType {
				t.Fatalf("content type = %q, want %q", got, c.contentType)
			}
			got := strings.TrimSpace(w.Body.String())
			if c.format == "json" {
				// encoding/json escapes < and > so the error body stays
				// safe to embed in HTML.
				if !strings.Contains(got, `"status":"error"`) || !strings.Contains(got, `"message":"bad \u003cinput\u003e"`) {
					t.Fatalf("body = %s", got)
				}
				return
			}
			if got != c.body {
				t.Fatalf("body = %q, want %q", got, c.body)
			}
		})
	}
}

func TestWriteResultAffectedAndRows(t *testing.T) {
	w := httptest.NewRecorder()
	writeResult(w, "json", http.StatusOK, &database.BatchResult{Affected: 3})
	if !strings.Contains(w.Body.String(), `"affected":3`) {
		t.Fatalf("json body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	writeResult(w, "xml", http.StatusOK, &database.BatchResult{Affected: 3})
	if !strings.Contains(w.Body.String(), "<affected>3</affected>") {
		t.Fatalf("xml body = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	writeResult(w, "plain", http.StatusOK, &database.BatchResult{Affected: 3})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("plain write result status = %d, want 500", w.Code)
	}

	w = httptest.NewRecorder()
	writeResult(w, "json", http.StatusOK, &database.BatchResult{HasRows: true, Rows: []map[string]any{{"id": int64(1)}}})
	if !strings.Contains(w.Body.String(), `"rows":[{"id":1}]`) {
		t.Fatalf("returning body = %s", w.Body.String())
	}
}

func TestCSVRowsHeaderDataAndNils(t *testing.T) {
	w := httptest.NewRecorder()
	writeRows(w, "csv", http.StatusOK, []map[string]any{
		{"id": int64(1), "label": "first", "note": nil},
		{"id": int64(2), "label": "second", "note": "x"},
	})

	want := "id,label,note\n1,first,\n2,second,x\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("csv body = %q, want %q", got, want)
	}
}

func TestCSVRowsEmptyResultIsHeaderless(t *testing.T) {
	w := httptest.NewRecorder()
	writeRows(w, "csv", http.StatusOK, nil)
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestPlainRowsLayout(t *testing.T) {
	w := httptest.NewRecorder()
	writeRows(w, "plain", http.StatusOK, []map[string]any{
		{"id": int64(1), "label": "first"},
		{"id": int64(2), "label": "second"},
	})

	want := "1\nfirst\n\n2\nsecond\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("plain body = %q, want %q", got, want)
	}
}

func TestXMLRowsEscapesAndSanitizesTags(t *testing.T) {
	w := httptest.NewRecorder()
	writeRows(w, "xml", http.StatusOK, []map[string]any{{"a b": "<x>"}})

	got := w.Body.String()
	if !strings.Contains(got, "<a_b>&lt;x&gt;</a_b>") {
		t.Fatalf("xml body = %s", got)
	}
}

func TestWriteRowsUnknownFormat(t *testing.T) {
	for _, format := range []string{"nonsense", ""} {
		w := httptest.NewRecorder()
		writeRows(w, format, http.StatusOK, []map[string]any{{"id": 1}})
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("format %q: status = %d, want 500", format, w.Code)
		}
	}
}
