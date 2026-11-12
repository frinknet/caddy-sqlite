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
	keywordRe      = regexp.MustCompile(`^([A-Za-z]+)\b`)
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

func Classify(sqlText string) (Plan, error) {
	s := stripComments(sqlText)
	m := keywordRe.FindStringSubmatch(s)
	if m == nil {
		return Plan{}, fmt.Errorf("sqlite: cannot classify: %.60q", s)
	}
	switch strings.ToUpper(m[1]) {
	case "SELECT", "EXPLAIN", "VALUES":
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
				IsWrite:     returningRe.MatchString(s),
				ReturnsRows: true,
				Cacheable:   false,
			}, nil
		}
		return Plan{ReturnsRows: true, Cacheable: true}, nil
	default:
		return Plan{}, fmt.Errorf("sqlite: unsupported statement %q", m[1])
	}
}

func pragmaName(s string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(s, "PRAGMA"))
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
