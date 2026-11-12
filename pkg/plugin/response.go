package plugin

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

func writeJSONRows(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response{
		Status: "ok",
		Data:   map[string]any{"rows": rows},
	})
}

func writeXMLRows(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("<response status=\"ok\"><rows>"))
	for _, row := range rows {
		w.Write([]byte("<row>"))
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tag := xmlSafeTag(k)
			w.Write([]byte("<" + tag + ">"))
			w.Write([]byte(xmlEscape(fmt.Sprintf("%v", row[k]))))
			w.Write([]byte("</" + tag + ">"))
		}
		w.Write([]byte("</row>"))
	}
	w.Write([]byte("</rows></response>"))
}

func xmlSafeTag(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func writeCSVRows(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "text/csv")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if len(rows) == 0 {
		return
	}
	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	cw.Write(cols)
	for _, row := range rows {
		rec := make([]string, len(cols))
		for i, c := range cols {
			if v := row[c]; v != nil {
				rec[i] = fmt.Sprintf("%v", v)
			}
		}
		cw.Write(rec)
	}
}

func writePlainRows(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for i, row := range rows {
		if i > 0 {
			w.Write([]byte("\n"))
		}
		cols := make([]string, 0, len(row))
		for k := range row {
			cols = append(cols, k)
		}
		sort.Strings(cols)
		for _, c := range cols {
			if v := row[c]; v != nil {
				fmt.Fprintf(w, "%v\n", v)
			} else {
				w.Write([]byte("\n"))
			}
		}
	}
}

func xmlEscape(s string) string {
	var b []byte
	for _, r := range s {
		switch r {
		case '<':
			b = append(b, "&lt;"...)
		case '>':
			b = append(b, "&gt;"...)
		case '&':
			b = append(b, "&amp;"...)
		case '"':
			b = append(b, "&quot;"...)
		case '\'':
			b = append(b, "&apos;"...)
		default:
			b = append(b, string(r)...)
		}
	}
	return string(b)
}
