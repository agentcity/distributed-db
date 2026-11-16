package storage

import (
	"fmt"
	"strconv"
	"testing"
)

func benchOpen(b *testing.B, opts Options) Engine {
	b.Helper()
	if opts.CacheSize == 0 {
		opts.CacheSize = 64 << 20
	}
	if opts.MemTableSize == 0 {
		opts.MemTableSize = 8 << 20
	}
	e, err := Open(b.TempDir(), opts)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() {
		if err := e.Close(); err != nil {
			b.Errorf("Close: %v", err)
		}
	})
	return e
}

func BenchmarkSet(b *testing.B) {
	e := benchOpen(b, Options{})
	value := []byte("benchmark-value-0123456789abcdef")
	var kbuf [24]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := strconv.AppendInt(kbuf[:0], int64(i), 10)
		if err := e.Set(key, value); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	const n = 10000
	e := benchOpen(b, Options{})
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%08d", i))
		if err := e.Set(key, []byte("benchmark-value")); err != nil {
			b.Fatalf("Set: %v", err)
		}
		keys[i] = key
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := e.Get(keys[i%n])
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
		if len(v) != len("benchmark-value") {
			b.Fatalf("Get returned %d bytes", len(v))
		}
	}
}

func BenchmarkScan(b *testing.B) {
	const n = 10000
	e := benchOpen(b, Options{})
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%08d", i))
		if err := e.Set(key, []byte("benchmark-value")); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	var scanned int
	for i := 0; i < b.N; i++ {
		it, err := e.NewIter(nil, nil)
		if err != nil {
			b.Fatalf("NewIter: %v", err)
		}
		count := 0
		for ; it.Valid(); it.Next() {
			_ = it.Key()
			_ = it.Value()
			count++
		}
		if err := it.Error(); err != nil {
			b.Fatalf("iterator: %v", err)
		}
		if err := it.Close(); err != nil {
			b.Fatalf("iterator Close: %v", err)
		}
		if count != n {
			b.Fatalf("scanned %d keys, want %d", count, n)
		}
		scanned += count
	}
	b.ReportMetric(float64(scanned)/float64(b.N), "keys/op")
}
