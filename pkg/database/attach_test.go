package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func openFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := OpenDB(context.Background(), OpenConfig{Path: path})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAttachIsVisibleOnEveryConnection(t *testing.T) {
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.db")
	mediaPath := filepath.Join(dir, "media.db")

	app := openFile(t, appPath)
	if _, err := app.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, asset_id INTEGER)`); err != nil {
		t.Fatalf("create app: %v", err)
	}
	media := openFile(t, mediaPath)
	if _, err := media.Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create media: %v", err)
	}
	if _, err := media.Exec(`INSERT INTO assets (name) VALUES ('logo')`); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	if _, err := app.Exec(`INSERT INTO notes (asset_id) VALUES (1)`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "app", Path: appPath}, {Schema: "media", Path: mediaPath}},
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// A fresh connection for every query: the attach must not depend on reuse.
	pool.SetMaxIdleConns(0)

	for i := 0; i < 5; i++ {
		var name string
		if err := pool.QueryRow(
			`SELECT m.name FROM app.notes n JOIN media.assets m ON m.id = n.asset_id`).Scan(&name); err != nil {
			t.Fatalf("cross-schema join on connection %d: %v", i, err)
		}
		if name != "logo" {
			t.Fatalf("join returned %q", name)
		}
	}

	var inScratch int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&inScratch); err != nil {
		t.Fatalf("scratch lookup: %v", err)
	}
	if inScratch != 0 {
		t.Fatalf("scratch schema holds %d objects, want none: declared databases must not leak into main", inScratch)
	}
}

func TestAttachReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "media.db")

	media := openFile(t, mediaPath)
	if _, err := media.Exec(`CREATE TABLE assets (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "media", Path: mediaPath, ReadOnly: true}},
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM media.assets`).Scan(&n); err != nil {
		t.Fatalf("read attached: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO media.assets (id) VALUES (1)`); err == nil {
		t.Fatal("write to a read-only attachment must fail")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAttachFailureSurfacesAtOpen(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: "gone", Path: filepath.Join(dir, "missing", "nope.db")}},
	})
	if err == nil {
		t.Fatal("expected attaching a missing file to fail at open")
	}
	if !strings.Contains(err.Error(), "attach") {
		t.Fatalf("error should name the attach: %v", err)
	}
}

func TestAttachQuotesSchemaNames(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "odd.db")
	openFile(t, odd).Exec(`CREATE TABLE t (id INTEGER)`)

	pool, err := OpenDB(context.Background(), OpenConfig{
		Path:   ":memory:",
		Attach: []Attach{{Schema: `weird "name"`, Path: odd}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM "weird ""name""".t`).Scan(&n); err != nil {
		t.Fatalf("query quoted schema: %v", err)
	}
}
