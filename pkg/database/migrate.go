package database

import (
    "context"
    "database/sql"
    "fmt"
    "io/fs"
    "path"
    "sort"
    "strconv"
    "strings"
)

func Apply(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) error {
    var current int
    if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
        return err
    }
    entries, err := fs.ReadDir(fsys, dir)
    if err != nil {
        return err
    }
    type mig struct {
        version int
        name    string
    }
    var migs []mig
    for _, e := range entries {
        if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
            continue
        }
        parts := strings.SplitN(e.Name(), "_", 2)
        if len(parts) < 2 {
            continue
        }
        v, err := strconv.Atoi(parts[0])
        if err != nil {
            continue
        }
        migs = append(migs, mig{version: v, name: e.Name()})
    }
    sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
    for _, m := range migs {
        if m.version <= current {
            continue
        }
        body, err := fs.ReadFile(fsys, path.Join(dir, m.name))
        if err != nil {
            return err
        }
        tx, err := db.BeginTx(ctx, nil)
        if err != nil {
            return err
        }
        if _, err := tx.ExecContext(ctx, string(body)); err != nil {
            tx.Rollback()
            return fmt.Errorf("migrate %s: %w", m.name, err)
        }
        if _, err := tx.ExecContext(ctx,
            fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
            tx.Rollback()
            return fmt.Errorf("migrate %s: set version: %w", m.name, err)
        }
        if err := tx.Commit(); err != nil {
            return fmt.Errorf("migrate %s: commit: %w", m.name, err)
        }
        current = m.version
    }
    return nil
}
