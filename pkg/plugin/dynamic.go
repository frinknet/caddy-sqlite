package plugin

import (
	"database/sql"
	"net/http"

	"go.uber.org/zap"
)

func (m *SQLite) writeDynamic(w http.ResponseWriter, rows *sql.Rows, log *zap.Logger) {
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
	var content []byte
	var contentType string
	var httpCode sql.NullInt64

	seen := map[string]bool{}
	dest := make([]any, len(cols))
	for i, c := range cols {
		if seen[c] {
			log.Error("dynamic: duplicate column", zap.String("col", c))
			writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
			return
		}
		seen[c] = true
		switch c {
		case "content":
			dest[i] = &content
		case "content_type":
			dest[i] = &contentType
		case "http_code":
			dest[i] = &httpCode
		default:
			log.Error("dynamic: unknown column", zap.String("col", c))
			writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
			return
		}
	}
	if !seen["content"] || !seen["content_type"] {
		log.Error("dynamic: missing required columns", zap.Strings("cols", cols))
		writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
		return
	}

	if !rows.Next() {
		log.Error("dynamic: expected exactly 1 row, got 0")
		writeFail(w, http.StatusInternalServerError, m.Output, "internal error")
		return
	}
	scanErr := rows.Scan(dest...)
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
