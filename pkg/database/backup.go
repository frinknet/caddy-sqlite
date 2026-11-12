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
        cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
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
    ctx := context.Background()
    stamp := time.Now().Format("20060102-150405")
    name := s.cfg.Name
    if name == "" {
        name = "backup"
    }
    file := filepath.Join(s.cfg.Dir, fmt.Sprintf("%s-%s.db", name, stamp))
    _, err := s.db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO %q", file))
    if err != nil {
        s.log.Warn("backup failed", zap.Error(err), zap.String("file", file))
        return
    }
    s.prune(name)
}

func (s *BackupScheduler) prune(name string) {
    if s.cfg.Retain <= 0 {
        return
    }
    entries, err := os.ReadDir(s.cfg.Dir)
    if err != nil {
        return
    }
    var files []string
    for _, e := range entries {
        if !e.IsDir() && strings.HasPrefix(e.Name(), name+"-") && strings.HasSuffix(e.Name(), ".db") {
            files = append(files, filepath.Join(s.cfg.Dir, e.Name()))
        }
    }
    if len(files) <= s.cfg.Retain {
        return
    }
    sort.Strings(files)
    for _, f := range files[:len(files)-s.cfg.Retain] {
        os.Remove(f)
    }
}
