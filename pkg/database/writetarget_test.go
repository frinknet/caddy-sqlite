package database

import "testing"

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
