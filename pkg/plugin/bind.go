package plugin

import (
    "database/sql"
    "fmt"
    "strings"

    "github.com/caddyserver/caddy/v2"
)

func (m *SQLiteResponse) resolveArgs(repl *caddy.Replacer) ([]any, error) {
    args := make([]any, len(m.Binds))
    for i, b := range m.Binds {
        v := repl.ReplaceAll(b.Value, "")
        if strings.ContainsAny(v, "{}") {
            return nil, fmt.Errorf("unresolved bind %q: %s", b.Name, b.Value)
        }
        if m.positional {
            args[i] = v
        } else {
            args[i] = sql.Named(b.Name, v)
        }
    }
    return args, nil
}

func isDigits(s string) bool {
    if s == "" {
        return false
    }
    for _, r := range s {
        if r < '0' || r > '9' {
            return false
        }
    }
    return true
}
