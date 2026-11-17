package cache

import (
	"testing"
	"time"
)

func TestLRUGetMiss(t *testing.T) {
	c := New(10)
	if _, ok := c.Get(1); ok {
		t.Fatal("expected miss on empty cache")
	}
}

func TestLRUPutGet(t *testing.T) {
	c := New(10)
	c.Put(1, "hello", time.Minute)
	v, ok := c.Get(1)
	if !ok {
		t.Fatal("expected hit")
	}
	if v != "hello" {
		t.Fatalf("got %v, want hello", v)
	}
}

func TestLRUExpiry(t *testing.T) {
	c := New(10)
	c.Put(1, "x", 10*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	if _, ok := c.Get(1); ok {
		t.Fatal("expected expiry")
	}
}

func TestLRUOverwrite(t *testing.T) {
	c := New(10)
	c.Put(1, "old", time.Minute)
	c.Put(1, "new", time.Minute)
	v, ok := c.Get(1)
	if !ok {
		t.Fatal("expected hit")
	}
	if v != "new" {
		t.Fatalf("got %v, want new", v)
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(2)
	c.Put(1, "a", time.Minute)
	c.Put(2, "b", time.Minute)
	c.Put(3, "c", time.Minute) // should evict 1 (least recently used)
	if _, ok := c.Get(1); ok {
		t.Fatal("expected 1 to be evicted")
	}
	if _, ok := c.Get(2); !ok {
		t.Fatal("expected 2 to survive")
	}
	if _, ok := c.Get(3); !ok {
		t.Fatal("expected 3 to survive")
	}
}

func TestLRULRUOrder(t *testing.T) {
	c := New(2)
	c.Put(1, "a", time.Minute)
	c.Put(2, "b", time.Minute)
	c.Get(1)                   // touch 1 → 2 becomes LRU
	c.Put(3, "c", time.Minute) // should evict 2
	if _, ok := c.Get(2); ok {
		t.Fatal("expected 2 to be evicted (LRU)")
	}
	if _, ok := c.Get(1); !ok {
		t.Fatal("expected 1 to survive (recently used)")
	}
}

func TestLRUZeroMaxDefaults(t *testing.T) {
	c := New(0) // defaults to 1000 internally; should not panic on Put
	c.Put(1, "x", time.Minute)
	if _, ok := c.Get(1); !ok {
		t.Fatal("expected hit on default-sized cache")
	}
}

func BenchmarkLRUGetHit(b *testing.B) {
	c := New(1024)
	c.Put(1, "value", time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Get(1); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkLRUGetMiss(b *testing.B) {
	c := New(1024)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Get(1); ok {
			b.Fatal("hit")
		}
	}
}

func BenchmarkLRUPutEvicting(b *testing.B) {
	c := New(64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Put(uint64(i), "value", time.Minute)
	}
}
