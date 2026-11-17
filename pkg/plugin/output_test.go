package plugin

import "net/http"

import "net/http/httptest"

import "strings"

import "testing"

import "github.com/frinknet/caddy-sqlite/pkg/database"

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

func TestWriteRowsUnknownFormat(t *testing.T) {
	for _, format := range []string{"nonsense", ""} {
		w := httptest.NewRecorder()
		writeRows(w, format, http.StatusOK, []map[string]any{{"id": 1}})
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("format %q: status = %d, want 500", format, w.Code)
		}
	}
}
