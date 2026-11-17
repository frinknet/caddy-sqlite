package plugin

import (
	"testing"

	"github.com/caddyserver/caddy/v2"
)

func TestUnknownPlaceholder(t *testing.T) {
	repl := caddy.NewReplacer()
	repl.Set("http.request.body", `{"label":"first"}`)

	if name, ok := unknownPlaceholder(repl, "{http.request.body}"); ok {
		t.Fatalf("known placeholder %q reported unknown", name)
	}

	name, ok := unknownPlaceholder(repl, "{http.request.body.label}")
	if !ok || name != "http.request.body.label" {
		t.Fatalf("unknown placeholder not detected: %q %v", name, ok)
	}

	if name, ok := unknownPlaceholder(repl, "plain text"); ok {
		t.Fatalf("plain string reported unknown: %q", name)
	}
}

func TestResolveKeepsJSONBody(t *testing.T) {
	repl := caddy.NewReplacer()
	repl.Set("http.request.body", `{"label":"first"}`)

	m := &SQLiteResponse{
		Binds:      []Bind{{Name: "1", Value: "{http.request.body}"}},
		positional: true,
	}
	args, err := m.resolveArgs(repl)
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	if len(args) != 1 || args[0] != `{"label":"first"}` {
		t.Fatalf("unexpected args: %#v", args)
	}

	m.Binds = []Bind{{Name: "1", Value: "{http.request.body.label}"}}
	if _, err := m.resolveArgs(repl); err == nil {
		t.Fatal("expected unresolved placeholder error")
	}
}
