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

	m := &SQLite{
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

func BenchmarkResolveArgs(b *testing.B) {
	m := &SQLite{
		Binds: []Bind{
			{Name: "1", Value: "{http.request.body}"},
			{Name: "2", Value: "{http.request.uri.query.page}"},
		},
		positional: true,
	}
	repl := caddy.NewReplacer()
	repl.Set("http.request.body", `{"label":"x"}`)
	repl.Set("http.request.uri.query.page", "3")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.resolveArgs(repl); err != nil {
			b.Fatal(err)
		}
	}
}
