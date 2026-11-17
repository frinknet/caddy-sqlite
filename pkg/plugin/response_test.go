package plugin

import "net/http"

import "net/http/httptest"

import "strings"

import "testing"

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
