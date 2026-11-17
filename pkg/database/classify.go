package database

import (
	"fmt"
	"regexp"
	"strings"
)

type Plan struct {
	IsWrite     bool
	ReturnsRows bool
	Cacheable   bool
}

var (
	keywordRe      = regexp.MustCompile(`^([A-Za-z]+)`)
	returningRe    = regexp.MustCompile(`(?i)\bRETURNING\b`)
	writeKeywordRe = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|REPLACE|UPSERT|CREATE|DROP|ALTER|VACUUM|ANALYZE|REINDEX|ATTACH|DETACH)\b`)
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRe  = regexp.MustCompile(`--[^\n]*`)
)

var sideEffectPragmas = map[string]bool{
	"wal_checkpoint":     true,
	"optimize":           true,
	"incremental_vacuum": true,
	"shrink_memory":      true,
	"cache_spill":        true,
	"analysis_limit":     true,
	"soft_heap_limit":    true,
}

// hasSideEffectingUDF reports whether the stripped SQL contains a call to a
// UDF registered by pkg/database/functions.go whose result must not be cached.
// We only flag http_post: it's the one with an intentional side effect, so
// caching its result would silently skip the POST on a cache hit. http_get is
// nominally idempotent — callers who want it uncached can set `cache 0` on
// the handler.
//
// This is a word-ish text scan, not a parse. A bare string literal like
// `SELECT 'http_post'` loses caching (safe: slower, not wrong). A mention in
// a comment is already gone by stripComments.
func hasSideEffectingUDF(s string) bool {
	return strings.Contains(strings.ToUpper(s), "HTTP_POST")
}

func Classify(sqlText string) (Plan, error) {
	s := stripComments(sqlText)
	m := keywordRe.FindStringSubmatch(s)
	if m == nil {
		return Plan{}, fmt.Errorf("sqlite: cannot classify: %.60q", s)
	}
	switch strings.ToUpper(m[1]) {
	case "SELECT", "EXPLAIN", "VALUES":
		if hasSideEffectingUDF(s) {
			return Plan{ReturnsRows: true, Cacheable: false}, nil
		}
		return Plan{ReturnsRows: true, Cacheable: true}, nil
	case "PRAGMA":
		if strings.Contains(s, "=") {
			return Plan{IsWrite: true}, nil
		}
		if sideEffectPragmas[strings.ToLower(pragmaName(s))] {
			return Plan{IsWrite: true}, nil
		}
		return Plan{ReturnsRows: true, Cacheable: true}, nil
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "UPSERT":
		return Plan{IsWrite: true, ReturnsRows: returningRe.MatchString(s)}, nil
	case "CREATE", "DROP", "ALTER", "VACUUM", "ANALYZE", "REINDEX", "ATTACH", "DETACH":
		return Plan{IsWrite: true}, nil
	case "WITH":
		if writeKeywordRe.MatchString(s) {
			return Plan{
				IsWrite:     true,
				ReturnsRows: returningRe.MatchString(s),
				Cacheable:   false,
			}, nil
		}
		if hasSideEffectingUDF(s) {
			return Plan{ReturnsRows: true, Cacheable: false}, nil
		}
		return Plan{ReturnsRows: true, Cacheable: true}, nil
	default:
		return Plan{}, fmt.Errorf("sqlite: unsupported statement %q", m[1])
	}
}

// pragmaName extracts the pragma name from a PRAGMA statement, case-insensitively
// stripping the leading "PRAGMA" keyword so that both `PRAGMA wal_checkpoint`
// and `pragma wal_checkpoint` resolve to "wal_checkpoint". The returned name
// preserves its original case; callers lowercase it if needed.
func pragmaName(s string) string {
	rest := strings.TrimSpace(s)
	if len(rest) >= 6 && strings.EqualFold(rest[:6], "PRAGMA") {
		rest = strings.TrimSpace(rest[6:])
	}
	for i, c := range rest {
		if c == '(' || c == ';' || c == ' ' || c == '\t' || c == '\n' {
			return rest[:i]
		}
	}
	return rest
}

func stripComments(s string) string {
	s = blockCommentRe.ReplaceAllString(s, " ")
	s = lineCommentRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
