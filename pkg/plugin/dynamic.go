package plugin

import (
    "database/sql"
    "net/http"

    "go.uber.org/zap"
)

func (m *SQLiteResponse) writeDynamic(w http.ResponseWriter, rows *sql.Rows, log *zap.Logger) {
    cols, err := rows.Columns()
    if err != nil {
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }
    if len(cols) < 2 || len(cols) > 3 {
        log.Error("dynamic: expected 2 or 3 columns", zap.Strings("cols", cols))
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }
    var hasContent, hasContentType, hasHTTPCode bool
    for _, c := range cols {
        switch c {
        case "content":
            hasContent = true
        case "content_type":
            hasContentType = true
        case "http_code":
            hasHTTPCode = true
        }
    }
    if !hasContent || !hasContentType {
        log.Error("dynamic: missing required columns", zap.Strings("cols", cols))
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }
    expected := 2
    if hasHTTPCode {
        expected = 3
    }
    if len(cols) != expected {
        log.Error("dynamic: column count mismatch",
            zap.Int("expected", expected), zap.Int("got", len(cols)), zap.Strings("cols", cols))
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }

    if !rows.Next() {
        log.Error("dynamic: expected exactly 1 row, got 0")
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }
    var content []byte
    var contentType string
    var httpCode sql.NullInt64
    var scanErr error
    if hasHTTPCode {
        scanErr = rows.Scan(&content, &contentType, &httpCode)
    } else {
        scanErr = rows.Scan(&content, &contentType)
    }
    if scanErr != nil {
        log.Error("dynamic: scan", zap.Error(scanErr))
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }
    if rows.Next() {
        log.Error("dynamic: expected exactly 1 row, got >1")
        writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
        return
    }

    status := http.StatusOK
    if httpCode.Valid {
        status = int(httpCode.Int64)
    }
    if contentType != "" {
        w.Header().Set("Content-Type", contentType)
    }
    w.WriteHeader(status)
    if len(content) > 0 {
        w.Write(content)
    }
}
