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
		if name, ok := unknownPlaceholder(repl, b.Value); ok {
			return nil, fmt.Errorf("unresolved bind %q: {%s}", b.Name, name)
		}
		v := repl.ReplaceAll(b.Value, "")
		if m.positional {
			args[i] = v
		} else {
			args[i] = sql.Named(b.Name, v)
		}
	}
	return args, nil
}

func unknownPlaceholder(repl *caddy.Replacer, tmpl string) (string, bool) {
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			continue
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			break
		}
		name := tmpl[i+1 : i+end]
		if _, ok := repl.Get(name); !ok {
			return name, true
		}
		i += end
	}
	return "", false
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
