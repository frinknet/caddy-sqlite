package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestBackupRunOnceProducesUsableCopy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	backupDir := filepath.Join(dir, "backups")

	db, err := OpenDB(context.Background(), OpenConfig{Path: dbPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO notes (body) VALUES ('archived')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sched, err := NewBackupScheduler(BackupConfig{
		Schedule: "@every 1h",
		Dir:      backupDir,
		Name:     "live",
	}, db, zap.NewNop())
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	defer sched.Close()

	if err := sched.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	files := backupFiles(t, backupDir, "live")
	if len(files) != 1 {
		t.Fatalf("want 1 backup file, got %v", files)
	}

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.HasPrefix(string(raw), "SQLite format 3") {
		t.Fatalf("backup is not a SQLite database: %.20q", raw)
	}

	copyDB, err := OpenDB(context.Background(), OpenConfig{Path: files[0]})
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer copyDB.Close()
	var body string
	if err := copyDB.QueryRow(`SELECT body FROM notes`).Scan(&body); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if body != "archived" {
		t.Fatalf("backup body = %q, want %q", body, "archived")
	}
}

func TestBackupRetentionPrunesOldest(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backups")

	db, err := OpenDB(context.Background(), OpenConfig{Path: filepath.Join(dir, "live.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	sched, err := NewBackupScheduler(BackupConfig{
		Schedule: "@every 1h",
		Dir:      backupDir,
		Name:     "live",
		Retain:   2,
	}, db, zap.NewNop())
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	defer sched.Close()

	var stamps []string
	for i := 0; i < 3; i++ {
		if err := sched.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
		files := backupFiles(t, backupDir, "live")
		stamps = append(stamps, filepath.Base(files[len(files)-1]))
		time.Sleep(5 * time.Millisecond)
	}

	files := backupFiles(t, backupDir, "live")
	if len(files) != 2 {
		t.Fatalf("retain 2: want 2 files, got %v", files)
	}
	if got := filepath.Base(files[0]); got != stamps[1] {
		t.Fatalf("kept %s, want the second-newest %s", got, stamps[1])
	}
	if got := filepath.Base(files[1]); got != stamps[2] {
		t.Fatalf("kept %s, want the newest %s", got, stamps[2])
	}
}

func TestBackupRejectsOtherMethods(t *testing.T) {
	db, err := OpenDB(context.Background(), OpenConfig{Path: filepath.Join(t.TempDir(), "live.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = NewBackupScheduler(BackupConfig{
		Method:   "cp",
		Schedule: "@every 1h",
		Dir:      t.TempDir(),
	}, db, zap.NewNop())
	if err == nil {
		t.Fatal("expected method cp to be rejected")
	}
}

func TestBackupRequiresScheduleAndDir(t *testing.T) {
	db, err := OpenDB(context.Background(), OpenConfig{Path: filepath.Join(t.TempDir(), "live.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := NewBackupScheduler(BackupConfig{Dir: t.TempDir()}, db, zap.NewNop()); err == nil {
		t.Fatal("expected missing schedule to error")
	}
	if _, err := NewBackupScheduler(BackupConfig{Schedule: "@every 1h"}, db, zap.NewNop()); err == nil {
		t.Fatal("expected missing dir to error")
	}
}

func backupFiles(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), name+"-") && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
