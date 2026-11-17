package database

import (
	"fmt"
	"regexp"
	"sort"
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

var (
	insertTargetRe = regexp.MustCompile(`(?is)\b(?:INSERT|REPLACE)\s+(?:OR\s+\w+\s+)?INTO\s+([^\s(,]+)`)
	updateTargetRe = regexp.MustCompile(`(?is)\bUPDATE\s+(?:OR\s+\w+\s+)?([^\s(,]+)`)
	deleteTargetRe = regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+([^\s(,]+)`)
	createTargetRe = regexp.MustCompile(`(?is)\bCREATE\s+(?:TEMP|TEMPORARY\s+)?(?:TABLE|VIEW|INDEX|TRIGGER)\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(,]+)`)
	dropTargetRe   = regexp.MustCompile(`(?is)\bDROP\s+(?:TABLE|VIEW|INDEX|TRIGGER)\s+(?:IF\s+EXISTS\s+)?([^\s(,]+)`)
	alterTargetRe  = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+([^\s(,]+)`)
	reindexRe      = regexp.MustCompile(`(?is)\bREINDEX\s+([^\s(,]+)`)
	analyzeRe      = regexp.MustCompile(`(?is)\bANALYZE\s+([^\s(,]+)`)
	vacuumRe       = regexp.MustCompile(`(?is)\bVACUUM\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:$|INTO)`)
	pragmaRe       = regexp.MustCompile(`(?is)^\s*PRAGMA\b`)
)

// WriteTarget reports which schema a write statement writes to. The returned
// schema is empty when the statement names no schema, and error is non-nil when
// it names more than one, which SQLite cannot make atomic across attached files.
//
// This is a token scan, not a parser: it looks for the object after INTO /
// UPDATE / DELETE FROM / CREATE / DROP / ALTER / REINDEX / ANALYZE and the schema
// after VACUUM. PRAGMA writes belong to the scratch schema, which is where an
// unqualified pragma applies.
func WriteTarget(sqlText string) (string, error) {
	s := stripComments(sqlText)
	if pragmaRe.MatchString(s) {
		return ScratchSchema, nil
	}

	var schemas []string
	collect := func(re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			schemas = append(schemas, schemaOf(m[1]))
		}
	}
	for _, re := range []*regexp.Regexp{
		insertTargetRe, updateTargetRe, deleteTargetRe,
		createTargetRe, dropTargetRe, alterTargetRe,
		reindexRe, analyzeRe,
	} {
		collect(re)
	}
	// VACUUM names a schema directly, not a schema.table.
	for _, m := range vacuumRe.FindAllStringSubmatch(s, -1) {
		schemas = append(schemas, m[1])
	}

	distinct := map[string]bool{}
	for _, sc := range schemas {
		distinct[sc] = true
	}
	if len(distinct) > 1 {
		names := make([]string, 0, len(distinct))
		for sc := range distinct {
			if sc == "" {
				sc = "(unqualified)"
			}
			names = append(names, sc)
		}
		sort.Strings(names)
		return "", fmt.Errorf("sqlite: a write may name one schema, found %s", strings.Join(names, ", "))
	}
	for sc := range distinct {
		return sc, nil
	}
	return "", nil
}

// schemaOf splits "schema.table" and unquotes a quoted identifier.
func schemaOf(target string) string {
	target = strings.TrimSpace(target)
	if i := strings.Index(target, "."); i >= 0 {
		return unquoteIdentifier(target[:i])
	}
	return ""
}

func unquoteIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`' || s[0] == '[') {
		return strings.Trim(s[1:len(s)-1], "\"`]")
	}
	return s
}
