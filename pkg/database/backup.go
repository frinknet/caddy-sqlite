package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type BackupConfig struct {
	Method   string
	Schedule string
	Dir      string
	Retain   int
	Name     string
}

type BackupScheduler struct {
	cfg BackupConfig
	db  *sql.DB
	log *zap.Logger
	c   *cron.Cron
}

func NewBackupScheduler(cfg BackupConfig, db *sql.DB, log *zap.Logger) (*BackupScheduler, error) {
	if cfg.Method == "" {
		cfg.Method = "vacuum_into"
	}
	if cfg.Method != "vacuum_into" {
		return nil, fmt.Errorf("backup: unsupported method %q (only vacuum_into)", cfg.Method)
	}
	if cfg.Schedule == "" {
		return nil, fmt.Errorf("backup: schedule is required")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("backup: dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	c := cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)))
	s := &BackupScheduler{cfg: cfg, db: db, log: log, c: c}
	if _, err := c.AddFunc(cfg.Schedule, s.run); err != nil {
		return nil, fmt.Errorf("backup schedule: %w", err)
	}
	c.Start()
	return s, nil
}

func (s *BackupScheduler) Close() {
	if s.c != nil {
		s.c.Stop()
	}
}

func (s *BackupScheduler) run() {
	if err := s.RunOnce(context.Background()); err != nil {
		s.log.Warn("backup failed", zap.Error(err))
	}
}

// RunOnce performs one backup and then prunes older files according to
// retain. It runs on its own connection so a busy pool cannot starve it.
func (s *BackupScheduler) RunOnce(ctx context.Context) error {
	name := s.backupName()
	base := filepath.Join(s.cfg.Dir, fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405.000")))
	file := base + ".db"
	for n := 1; ; n++ {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			break
		}
		if n > 1000 {
			return fmt.Errorf("backup: no unused filename near %q", base)
		}
		file = fmt.Sprintf("%s-%d.db", base, n)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", file); err != nil {
		return fmt.Errorf("backup: vacuum into %q: %w", file, err)
	}
	s.prune(name)
	return nil
}

func (s *BackupScheduler) backupName() string {
	if s.cfg.Name == "" {
		return "backup"
	}
	return s.cfg.Name
}

func (s *BackupScheduler) prune(name string) {
	if s.cfg.Retain <= 0 {
		return
	}
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return
	}
	type backupFile struct {
		path string
		mod  time.Time
	}
	var files []backupFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), name+"-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, backupFile{filepath.Join(s.cfg.Dir, e.Name()), info.ModTime()})
	}
	if len(files) <= s.cfg.Retain {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[s.cfg.Retain:] {
		os.Remove(f.path)
	}
}
