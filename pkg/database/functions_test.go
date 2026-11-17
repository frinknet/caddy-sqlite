package database

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

var (
	httpFuncsOnce sync.Once
	httpFuncsErr  error
)

// openUDFTestDB registers the UDFs once per process (the driver rejects a
// duplicate name) and hands back a fresh file-backed DB.
func openUDFTestDB(t *testing.T) *sql.DB {
	t.Helper()
	httpFuncsOnce.Do(func() { httpFuncsErr = RegisterHTTPFuncs() })
	if httpFuncsErr != nil {
		t.Fatalf("RegisterHTTPFuncs: %v", httpFuncsErr)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "udf.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHTTPGetUDFReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pong")
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	var got string
	if err := db.QueryRow("SELECT http_get(?)", srv.URL).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != "pong" {
		t.Errorf("http_get body = %q, want %q", got, "pong")
	}
}

func TestHTTPGetUDFIgnoresStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	var got string
	if err := db.QueryRow("SELECT http_get(?)", srv.URL).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != "boom" {
		t.Errorf("http_get body = %q, want %q", got, "boom")
	}
}

func TestHTTPGetUDFTransportFailureIsNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()

	db := openUDFTestDB(t)
	var isNull bool
	if err := db.QueryRow("SELECT http_get(?) IS NULL", dead).Scan(&isNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !isNull {
		t.Error("http_get on an unreachable host should return NULL")
	}
}

func TestHTTPGetUDFRejectsNonTextURL(t *testing.T) {
	db := openUDFTestDB(t)
	if _, err := db.Exec("SELECT http_get(1)"); err == nil {
		t.Error("http_get(1) should return a SQL error, got nil")
	}
}

func TestHTTPGetUDFArityIsFixed(t *testing.T) {
	db := openUDFTestDB(t)
	if _, err := db.Exec("SELECT http_get('http://x', 'http://y')"); err == nil {
		t.Error("http_get is registered with nArg=1; a 2-arg call should fail")
	}
}

func TestHTTPPostUDFSendsBodyAndContentType(t *testing.T) {
	type received struct {
		method      string
		contentType string
		body        string
	}
	var got received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = received{r.Method, r.Header.Get("Content-Type"), string(b)}
		io.WriteString(w, `{"accepted":true}`)
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	var out string
	if err := db.QueryRow("SELECT http_post(?, ?)", srv.URL, `{"a":1}`).Scan(&out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != `{"accepted":true}` {
		t.Errorf("http_post body = %q, want response body", out)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json by default", got.contentType)
	}
	if got.body != `{"a":1}` {
		t.Errorf("server saw body %q, want the bound body", got.body)
	}
}

func TestHTTPPostUDFExplicitContentType(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	if _, err := db.Exec("SELECT http_post(?, ?, ?)", srv.URL, "a=1", "application/x-www-form-urlencoded"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if seen != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want the third argument", seen)
	}
}

func TestHTTPPostUDFTransportFailureIsNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()

	db := openUDFTestDB(t)
	var isNull bool
	if err := db.QueryRow("SELECT http_post(?, ?) IS NULL", dead, "{}").Scan(&isNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !isNull {
		t.Error("http_post on an unreachable host should return NULL")
	}
}

func TestHTTPPostUDFArityValidated(t *testing.T) {
	db := openUDFTestDB(t)
	for _, q := range []string{
		"SELECT http_post('http://x')",
		"SELECT http_post('http://x', 'y', 'z', 'w')",
	} {
		_, err := db.Exec(q)
		if err == nil {
			t.Errorf("%s should fail", q)
			continue
		}
		if !strings.Contains(err.Error(), "http_post") {
			t.Errorf("%s: error should name the function, got %v", q, err)
		}
	}
}

func TestHTTPGetUDFBodyIsCappedAtOneMiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 2<<20))
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	var n int
	if err := db.QueryRow("SELECT length(http_get(?))", srv.URL).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1<<20 {
		t.Errorf("body length = %d, want %d (1 MiB cap)", n, 1<<20)
	}
}

// The driver resolves UDFs at connection time, so a query issued through a
// pooled connection still sees them regardless of which conn it lands on.
func TestUDFsAvailableAcrossPooledConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	defer srv.Close()

	db := openUDFTestDB(t)
	db.SetMaxOpenConns(4)
	for i := 0; i < 8; i++ {
		var got string
		if err := db.QueryRow("SELECT http_get(?)", srv.URL).Scan(&got); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !strings.Contains(got, "yes") {
			t.Fatalf("iteration %d: body = %q", i, got)
		}
	}
}
