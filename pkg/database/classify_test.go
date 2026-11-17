package database

import (
	"strings"
	"testing"
)

func TestClassifySelect(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Plan
	}{
		{"plain select", "SELECT id FROM users", Plan{ReturnsRows: true, Cacheable: true}},
		{"select star", "SELECT * FROM users WHERE x = 1", Plan{ReturnsRows: true, Cacheable: true}},
		{"lowercase", "select id from users", Plan{ReturnsRows: true, Cacheable: true}},
		{"leading whitespace", "  \n\t SELECT 1", Plan{ReturnsRows: true, Cacheable: true}},
		{"explain", "EXPLAIN SELECT 1", Plan{ReturnsRows: true, Cacheable: true}},
		{"values", "VALUES (1), (2), (3)", Plan{ReturnsRows: true, Cacheable: true}},
		{"select with http_post", "SELECT http_post('http://x', 'y')", Plan{ReturnsRows: true, Cacheable: false}},
		{"select with http_get", "SELECT http_get('http://x')", Plan{ReturnsRows: true, Cacheable: true}},
		{"select with comment stripped", "SELECT 1 -- http_post\n", Plan{ReturnsRows: true, Cacheable: true}},
		{"select with block comment", "SELECT /* http_post */ 1", Plan{ReturnsRows: true, Cacheable: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyInsert(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Plan
	}{
		{"plain insert", "INSERT INTO users (id) VALUES (1)", Plan{IsWrite: true}},
		{"insert returning", "INSERT INTO users (id) VALUES (1) RETURNING id", Plan{IsWrite: true, ReturnsRows: true}},
		{"insert returning lowercase", "insert into users (id) values (1) returning id", Plan{IsWrite: true, ReturnsRows: true}},
		{"insert lowercase", "insert into users (id) values (1)", Plan{IsWrite: true}},
		{"replace", "REPLACE INTO users (id) VALUES (1)", Plan{IsWrite: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyUpdateDelete(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Plan
	}{
		{"update", "UPDATE users SET x = 1", Plan{IsWrite: true}},
		{"update returning", "UPDATE users SET x = 1 RETURNING id", Plan{IsWrite: true, ReturnsRows: true}},
		{"delete", "DELETE FROM users WHERE id = 1", Plan{IsWrite: true}},
		{"delete returning", "DELETE FROM users WHERE id = 1 RETURNING id", Plan{IsWrite: true, ReturnsRows: true}},
		{"upsert", "UPSERT INTO users (id) VALUES (1)", Plan{IsWrite: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyDDL(t *testing.T) {
	stmts := []string{
		"CREATE TABLE t (x INTEGER)",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN y TEXT",
		"VACUUM",
		"ANALYZE",
		"REINDEX",
		"ATTACH DATABASE '/tmp/x.db' AS x",
		"DETACH DATABASE x",
	}
	for _, sql := range stmts {
		t.Run(sql, func(t *testing.T) {
			got, err := Classify(sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", sql, err)
			}
			if got != (Plan{IsWrite: true}) {
				t.Errorf("Classify(%q) = %+v, want write", sql, got)
			}
		})
	}
}

func TestClassifyPragma(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Plan
	}{
		{"user_version read", "PRAGMA user_version", Plan{ReturnsRows: true, Cacheable: true}},
		{"user_version write", "PRAGMA user_version = 5", Plan{IsWrite: true}},
		{"wal_checkpoint", "PRAGMA wal_checkpoint", Plan{IsWrite: true}},
		{"optimize", "PRAGMA optimize", Plan{IsWrite: true}},
		{"incremental_vacuum", "PRAGMA incremental_vacuum", Plan{IsWrite: true}},
		{"shrink_memory", "PRAGMA shrink_memory", Plan{IsWrite: true}},
		{"case insensitive", "pragma optimize", Plan{IsWrite: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyWith(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Plan
	}{
		{"with select", "WITH x AS (SELECT 1) SELECT * FROM x", Plan{ReturnsRows: true, Cacheable: true}},
		{"with insert", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", Plan{IsWrite: true}},
		{"with insert returning", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x RETURNING id", Plan{IsWrite: true, ReturnsRows: true}},
		{"with update", "WITH x AS (SELECT 1) UPDATE t SET y = 1", Plan{IsWrite: true}},
		{"with delete", "WITH x AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM x)", Plan{IsWrite: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("Classify(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestClassifyComments(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"leading line comment", "-- leading\nSELECT 1"},
		{"trailing line comment", "SELECT 1 -- trailing"},
		{"block comment", "/* block */ SELECT 1"},
		{"inline block", "SELECT /* inline */ 1"},
		{"multiple comments", "/*a*/ -- b\nSELECT /*c*/ 1 -- d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify(tt.sql)
			if err != nil {
				t.Fatalf("Classify(%q) error: %v", tt.sql, err)
			}
			if !got.ReturnsRows || !got.Cacheable || got.IsWrite {
				t.Errorf("Classify(%q) = %+v, want cached read", tt.sql, got)
			}
		})
	}
}

func TestClassifyUnsupported(t *testing.T) {
	for _, sql := range []string{"", "   ", "-- only a comment", "FOOBAR baz"} {
		t.Run(sql, func(t *testing.T) {
			if _, err := Classify(sql); err == nil {
				t.Errorf("Classify(%q) expected error, got nil", sql)
			}
		})
	}
}

// Regression guard: lowercase `pragma wal_checkpoint` is a side-effecting write.
func TestClassifyPragmaLowercaseSideEffect(t *testing.T) {
	got, err := Classify("pragma wal_checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsWrite {
		t.Errorf("Classify(%q) = %+v, want write (side-effecting pragma)", "pragma wal_checkpoint", got)
	}
}

// Sanity: the comment stripper must not leave stray newlines or unbalanced
// whitespace that breaks the keyword regex.
func TestStripComments(t *testing.T) {
	if got := stripComments("  -- x\n  SELECT 1  "); !strings.HasPrefix(got, "SELECT") {
		t.Errorf("stripComments left leading junk: %q", got)
	}
}
