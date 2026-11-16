package client

import (
	"strings"
	"testing"
)

func TestLeaderCacheLRUEviction(t *testing.T) {
	t.Parallel()

	cache := newLeaderCache(2)
	cache.put("a", "node-a")
	cache.put("b", "node-b")

	// Touch "a" so "b" becomes the least recently used entry.
	if addr, ok := cache.get("a"); !ok || addr != "node-a" {
		t.Fatalf("get(a) = %q, %v; want node-a, true", addr, ok)
	}

	cache.put("c", "node-c")
	if got := cache.len(); got != 2 {
		t.Fatalf("cache size = %d, want 2", got)
	}
	if _, ok := cache.get("b"); ok {
		t.Fatal("get(b) found an evicted entry")
	}
	if addr, ok := cache.get("a"); !ok || addr != "node-a" {
		t.Fatalf("get(a) = %q, %v; want node-a, true", addr, ok)
	}
	if addr, ok := cache.get("c"); !ok || addr != "node-c" {
		t.Fatalf("get(c) = %q, %v; want node-c, true", addr, ok)
	}
}

func TestLeaderCacheUpdateAndInvalidate(t *testing.T) {
	t.Parallel()

	cache := newLeaderCache(2)
	cache.put("a", "node-1")
	cache.put("a", "node-2")
	if got := cache.len(); got != 1 {
		t.Fatalf("cache size after update = %d, want 1", got)
	}
	if addr, ok := cache.get("a"); !ok || addr != "node-2" {
		t.Fatalf("get(a) = %q, %v; want node-2, true", addr, ok)
	}

	cache.invalidate("a")
	if _, ok := cache.get("a"); ok {
		t.Fatal("get(a) found an invalidated entry")
	}
	if got := cache.len(); got != 0 {
		t.Fatalf("cache size after invalidate = %d, want 0", got)
	}

	// Invalidating an absent prefix must be a no-op.
	cache.invalidate("missing")
	if got := cache.len(); got != 0 {
		t.Fatalf("cache size = %d, want 0", got)
	}
}

func TestLeaderCacheCapacityNeverGrows(t *testing.T) {
	t.Parallel()

	cache := newLeaderCache(3)
	for _, prefix := range []string{"a", "b", "c", "d", "e", "f"} {
		cache.put(prefix, "node-"+prefix)
		if got := cache.len(); got > 3 {
			t.Fatalf("cache size = %d after put(%q), want <= 3", got, prefix)
		}
	}
	if got := cache.len(); got != 3 {
		t.Fatalf("cache size = %d, want 3", got)
	}
}

func TestNewLeaderCacheDefaultCapacity(t *testing.T) {
	t.Parallel()

	cache := newLeaderCache(0)
	if cache.capacity != leaderCacheEntries {
		t.Fatalf("capacity = %d, want %d", cache.capacity, leaderCacheEntries)
	}
}

func TestRoutePrefix(t *testing.T) {
	t.Parallel()

	if got := routePrefix(nil); got != "" {
		t.Fatalf("routePrefix(nil) = %q, want empty", got)
	}
	if got := routePrefix([]byte("short")); got != "short" {
		t.Fatalf("routePrefix(short) = %q, want short", got)
	}

	long := []byte(strings.Repeat("x", leaderPrefixBytes) + "tail")
	other := []byte(strings.Repeat("x", leaderPrefixBytes) + "different-tail")
	if got, want := routePrefix(long), string(long[:leaderPrefixBytes]); got != want {
		t.Fatalf("routePrefix(long) = %q, want %q", got, want)
	}
	if routePrefix(long) != routePrefix(other) {
		t.Fatal("long keys with the same prefix must share a cache entry")
	}
}
