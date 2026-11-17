package cache

import (
	"testing"
	"time"
)

func BenchmarkKey(b *testing.B) {
	args := []any{"label", int64(42), "x"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Key(1, `SELECT id FROM items WHERE label = ? AND id = ? AND note = ?`, args)
	}
}

func BenchmarkKeyNoArgs(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Key(1, `SELECT COUNT(*) FROM items`, nil)
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
