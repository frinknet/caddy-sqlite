package plugin

import (
    "encoding/json"
    "fmt"
    "net/http"

    "caddysqlite/pkg/database"
)

type response struct {
    Status  string `json:"status"`
    Data    any    `json:"data,omitempty"`
    Message string `json:"message,omitempty"`
}

func writeRows(w http.ResponseWriter, format string, rows []map[string]any) {
    switch format {
    case "json":
        writeJSONRows(w, rows)
    case "xml":
        writeXMLRows(w, rows)
    case "csv":
        writeCSVRows(w, rows)
    case "plain":
        writePlainRows(w, rows)
    default:
        http.Error(w, "internal error", http.StatusInternalServerError)
    }
}

func writeResult(w http.ResponseWriter, format string, res *database.BatchResult) {
    if res.HasRows {
        writeRows(w, format, res.Rows)
        return
    }
    switch format {
    case "json":
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(response{
            Status: "ok",
            Data:   map[string]any{"affected": res.Affected},
        })
    case "xml":
        w.Header().Set("Content-Type", "application/xml")
        w.WriteHeader(http.StatusOK)
        fmt.Fprintf(w, "<response status=\"ok\"><affected>%d</affected></response>", res.Affected)
    default:
        http.Error(w, "internal error", http.StatusInternalServerError)
    }
}

func writeFail(w http.ResponseWriter, code int, format, msg string) {
    switch format {
    case "json":
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(code)
        json.NewEncoder(w).Encode(response{Status: "error", Message: msg})
    case "xml":
        w.Header().Set("Content-Type", "application/xml")
        w.WriteHeader(code)
        fmt.Fprintf(w, "<error><message>%s</message></error>", xmlEscape(msg))
    case "csv":
        w.Header().Set("Content-Type", "text/csv")
        w.WriteHeader(code)
    case "plain":
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        w.WriteHeader(code)
        w.Write([]byte(msg))
    case "dynamic":
        w.WriteHeader(code)
    default:
        http.Error(w, msg, code)
    }
}
