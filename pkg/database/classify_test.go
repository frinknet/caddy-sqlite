package database

import (
	"regexp"
	"strings"
	"testing"
)

// Regression guard: lowercase `pragma wal_checkpoint` is a side-effecting write.

// Sanity: the comment stripper must not leave stray newlines or unbalanced
// whitespace that breaks the keyword regex.

// schemaIdentifier mirrors the identifier rule the config layer enforces on
// database names: whatever WriteTarget returns must be usable as a schema.
var schemaIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

func TestClassifyPragmaLowercaseSideEffect(t *testing.T) {
	got, err := Classify("pragma wal_checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsWrite {
		t.Errorf("Classify(%q) = %+v, want write (side-effecting pragma)", "pragma wal_checkpoint", got)
	}
}

func TestStripComments(t *testing.T) {
	if got := stripComments("  -- x\n  SELECT 1  "); !strings.HasPrefix(got, "SELECT") {
		t.Errorf("stripComments left leading junk: %q", got)
	}
}

func TestScratchWriteTarget(t *testing.T) {
	cases := []struct {
		sql    string
		schema string
		err    bool
	}{
		{`INSERT INTO app.notes (body) VALUES ('x')`, "app", false},
		{`INSERT OR REPLACE INTO app.notes (id) VALUES (1)`, "app", false},
		{`UPDATE app.notes SET body = 'y' WHERE id = 1`, "app", false},
		{`DELETE FROM app.notes WHERE id = 1`, "app", false},
		{`INSERT INTO main.hits (path) VALUES ('/x')`, "main", false},
		{`WITH recent AS (SELECT 1) INSERT INTO media.assets (id) VALUES (1)`, "media", false},
		{`INSERT INTO notes (body) VALUES ('x')`, "", false},
		{`INSERT INTO app.notes (id) SELECT id FROM media.assets`, "app", false},
		{`INSERT INTO app.notes (id) SELECT id FROM media.assets; DELETE FROM media.assets`, "", true},
		{`INSERT INTO "app"."notes" (id) VALUES (1)`, "app", false},
		{`CREATE TABLE IF NOT EXISTS e2e.items (id INTEGER PRIMARY KEY)`, "e2e", false},
		{`DROP TABLE e2e.items`, "e2e", false},
		{`ALTER TABLE app.notes ADD COLUMN note TEXT`, "app", false},
		{`REINDEX media.assets`, "media", false},
		{`ANALYZE app.notes`, "app", false},
		{`VACUUM app`, "app", false},
		{`VACUUM`, "", false},
		{`PRAGMA journal_mode = WAL`, "main", false},
		{`PRAGMA app.wal_checkpoint`, "main", false},
		{`CREATE TABLE items (id INTEGER)`, "", false},
		{`CREATE TABLE app.a (id INTEGER); CREATE TABLE media.b (id INTEGER)`, "", true},
	}
	for _, c := range cases {
		got, err := WriteTarget(c.sql)
		if c.err {
			if err == nil {
				t.Errorf("%s: expected error, got %q", c.sql, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.sql, err)
			continue
		}
		if got != c.schema {
			t.Errorf("%s: schema = %q, want %q", c.sql, got, c.schema)
		}
	}
}

func TestNamedPlaceholders(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{`SELECT v FROM t WHERE id = :id AND kind = @kind`, []string{"id", "kind"}},
		{`UPDATE t SET v = $v WHERE id = :id`, []string{"v", "id"}},
		{`INSERT INTO t (v) VALUES (:v), (:v)`, []string{"v"}},
		{`SELECT v FROM t WHERE at = '12:30'`, nil},
		{`SELECT v FROM t WHERE note = "weird:name"`, nil},
		{`SELECT v FROM t -- trailing :comment
		 WHERE id = :id`, []string{"id"}},
		{`SELECT v FROM t WHERE id = ?`, nil},
		{`SELECT 1`, nil},
	}
	for _, c := range cases {
		got := NamedPlaceholders(c.sql)
		if len(got) != len(c.want) {
			t.Errorf("NamedPlaceholders(%q) = %v, want %v", c.sql, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("NamedPlaceholders(%q) = %v, want %v", c.sql, got, c.want)
				break
			}
		}
	}
}

func FuzzClassify(f *testing.F) {
	seeds := []string{
		`SELECT id, label FROM app.notes WHERE id = ?`,
		`INSERT INTO app.notes (body) VALUES ('x')`,
		`INSERT INTO app.notes (id) SELECT id FROM media.assets`,
		`UPDATE app.notes SET body = 'y' WHERE id = 1`,
		`DELETE FROM app.notes WHERE id = 1`,
		`CREATE TABLE IF NOT EXISTS app.t (id INTEGER PRIMARY KEY)`,
		`DROP TABLE app.t`,
		`ALTER TABLE app.t ADD COLUMN x TEXT`,
		`VACUUM app`,
		`PRAGMA wal_checkpoint`,
		`PRAGMA app.optimize`,
		`WITH recent AS (SELECT id FROM app.notes) INSERT INTO app.t (id) SELECT id FROM recent`,
		`/* comment */ SELECT 1 -- trailing`,
		`INSERT OR REPLACE INTO app.t (id) VALUES (1)`,
		`REPLACE INTO app.t (id) VALUES (1)`,
		`INSERT INTO "app"."notes" (id) VALUES (1)`,
		`INSERT INTO app.notes (id) VALUES (1); DELETE FROM media.assets`,
		`SELECT http_post('http://x', 'y')`,
		``,
		`'`,
		`{`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sqlText string) {
		plan, err := Classify(sqlText)
		if err != nil {
			return // unclassifiable input is a legitimate answer
		}

		schema, err := WriteTarget(sqlText)
		if err != nil {
			// The only error WriteTarget may report is a multi-schema write.
			return
		}
		if !plan.IsWrite {
			return
		}
		if schema == "" {
			return // unqualified: the handler rejects it, by design
		}
		if !schemaIdentifier.MatchString(schema) {
			t.Fatalf("WriteTarget(%q) = %q, which is not an identifier", sqlText, schema)
		}
	})
}

func BenchmarkClassify(b *testing.B) {
	cases := map[string]string{
		"select":  `SELECT id, label FROM items WHERE id = ? ORDER BY id`,
		"insert":  `INSERT INTO items (label) VALUES (?)`,
		"cte":     `WITH recent AS (SELECT id FROM items ORDER BY id DESC LIMIT 10) SELECT * FROM recent`,
		"pragma":  `PRAGMA wal_checkpoint`,
		"comment": `/* leading comment */ SELECT 1 -- trailing`,
	}
	for name, sqlText := range cases {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Classify(sqlText); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
