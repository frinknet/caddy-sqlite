package cache

import "testing"

func TestKeyStable(t *testing.T) {
	k1 := Key(1, "SELECT", []any{"x", int64(42)})
	k2 := Key(1, "SELECT", []any{"x", int64(42)})
	if k1 != k2 {
		t.Fatal("same inputs must produce same key")
	}
}

func TestKeyGenDifferent(t *testing.T) {
	k1 := Key(1, "SELECT", []any{"x"})
	k2 := Key(2, "SELECT", []any{"x"})
	if k1 == k2 {
		t.Fatal("different gen must produce different key")
	}
}

func TestKeySQLDifferent(t *testing.T) {
	k1 := Key(1, "SELECT 1", []any{})
	k2 := Key(1, "SELECT 2", []any{})
	if k1 == k2 {
		t.Fatal("different sql must produce different key")
	}
}

func TestKeyArgsDiffer(t *testing.T) {
	k1 := Key(1, "SELECT", []any{"a"})
	k2 := Key(1, "SELECT", []any{"b"})
	if k1 == k2 {
		t.Fatal("different args must produce different key")
	}
}

func TestKeyTypeSensitive(t *testing.T) {
	k1 := Key(1, "SELECT", []any{"42"})
	k2 := Key(1, "SELECT", []any{int64(42)})
	if k1 == k2 {
		t.Fatal("string vs int64 must produce different key")
	}
}

// Regression guard: args are length-delimited, so [a, b] cannot collide with
// a single arg that happens to contain the same bytes.
func TestKeyArgsBoundary(t *testing.T) {
	k1 := Key(1, "SELECT", []any{"a", "b"})
	k2 := Key(1, "SELECT", []any{"astring:b"})
	if k1 == k2 {
		t.Fatal("cache key collision between [a,b] and [astring:b]")
	}
}
