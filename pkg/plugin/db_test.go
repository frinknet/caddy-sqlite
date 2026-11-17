package plugin

import (
	"os"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestExpandEnv(t *testing.T) {
	os.Unsetenv("CADDYSQLITE_UNSET_VAR")

	got, err := expandEnv("db/{env.CADDYSQLITE_UNSET_VAR}.db")
	if err == nil {
		t.Fatalf("missing var: got %q, want error", got)
	}
	if !strings.Contains(err.Error(), "CADDYSQLITE_UNSET_VAR") {
		t.Fatalf("error should name the variable, got %v", err)
	}

	os.Setenv("CADDYSQLITE_SET_VAR", "value")
	defer os.Unsetenv("CADDYSQLITE_SET_VAR")

	got, err = expandEnv("db/{env.CADDYSQLITE_SET_VAR}.db")
	if err != nil {
		t.Fatalf("set var: %v", err)
	}
	if got != "db/value.db" {
		t.Fatalf("got %q, want %q", got, "db/value.db")
	}

	if got, err := expandEnv("db/literal.db"); err != nil || got != "db/literal.db" {
		t.Fatalf("literal path: %q %v", got, err)
	}
}

func TestParseRejectsUnknownBackupMethod(t *testing.T) {
	d := caddyfile.NewTestDispenser(`sqlite_db main /tmp/main.db {
	backup {
		method cp
		dir    /tmp/backups
	}
}`)
	if _, err := ParseSQLiteDBOption(d, nil); err == nil {
		t.Fatal("expected unsupported backup method to be rejected")
	}
}

func TestParseAcceptsVacuumInto(t *testing.T) {
	d := caddyfile.NewTestDispenser(`sqlite_db vac /tmp/vac.db {
	backup {
		method   vacuum_into
		schedule 0 3 * * *
		dir      /tmp/backups
		retain   7
	}
}`)
	if _, err := ParseSQLiteDBOption(d, nil); err != nil {
		t.Fatalf("vacuum_into should parse: %v", err)
	}
}
