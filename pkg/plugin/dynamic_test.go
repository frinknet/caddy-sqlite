package plugin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
