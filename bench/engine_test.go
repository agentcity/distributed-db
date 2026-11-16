package main

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mustPut(t *testing.T, e Engine, key, value string) {
	t.Helper()
	if err := e.Put([]byte(key), []byte(value)); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func mustGet(t *testing.T, e Engine, key, want string) {
	t.Helper()
	got, err := e.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if string(got) != want {
		t.Fatalf("Get(%q) = %q, want %q", key, got, want)
	}
}

func TestMemEngineCRUD(t *testing.T) {
	e := NewMemEngine()

	if _, err := e.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if e.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", e.Len())
	}

	mustPut(t, e, "a", "1")
	mustPut(t, e, "b", "2")
	mustPut(t, e, "a", "3")
	mustGet(t, e, "a", "3")
	mustGet(t, e, "b", "2")
	if e.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", e.Len())
	}

	if err := e.Delete([]byte("a")); err != nil {
		t.Fatalf("Delete(a): %v", err)
	}
	if _, err := e.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(a) after delete error = %v, want ErrNotFound", err)
	}
	if err := e.Delete([]byte("a")); err != nil {
		t.Fatalf("deleting a missing key should be a no-op, got %v", err)
	}
	if e.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", e.Len())
	}
}

func TestMemEngineCopiesSlices(t *testing.T) {
	e := NewMemEngine()
	key := []byte("key")
	value := []byte("value")
	if err := e.Put(key, value); err != nil {
		t.Fatal(err)
	}
	key[0] = 'K'
	value[0] = 'V'

	got, err := e.Get([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("engine retained caller slices: Get = %q", got)
	}

	got[0] = 'X'
	got2, err := e.Get([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != "value" {
		t.Fatalf("Get returned an aliased slice: second Get = %q", got2)
	}
}

func TestMemEngineNilValue(t *testing.T) {
	e := NewMemEngine()
	if err := e.Put([]byte("k"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Get = %q, want empty", got)
	}
	if e.Len() != 1 {
		t.Fatalf("Len = %d, want 1", e.Len())
	}
}

func TestMemEngineScan(t *testing.T) {
	e := NewMemEngine()
	const n = 100
	// Insert in a scrambled order to make sure the tree (not insertion order)
	// defines the scan order.
	for i := 0; i < n; i++ {
		idx := (i * 37) % n
		mustPut(t, e, string(KeyForIndex(int64(idx))), fmt.Sprintf("v%03d", idx))
	}

	full, err := e.Scan(nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != n {
		t.Fatalf("full scan returned %d keys, want %d", len(full), n)
	}
	for i := 1; i < len(full); i++ {
		if bytes.Compare(full[i-1].Key, full[i].Key) >= 0 {
			t.Fatalf("scan not ascending at %d: %q >= %q", i, full[i-1].Key, full[i].Key)
		}
	}

	tests := []struct {
		name       string
		start, end int64 // -1 means nil
		limit      int
		wantFirst  int64
		wantCount  int
	}{
		{"bounded", 10, 20, 0, 10, 10},
		{"nil start", -1, 5, 0, 0, 5},
		{"nil end", 90, -1, 0, 90, 10},
		{"limit", -1, -1, 5, 0, 5},
		{"limit one", 42, -1, 1, 42, 1},
		{"empty range", 5, 5, 0, 0, 0},
		{"start past end", 80, 70, 0, 0, 0},
		{"limit larger than range", 95, -1, 100, 95, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var start, end []byte
			if tt.start >= 0 {
				start = KeyForIndex(tt.start)
			}
			if tt.end >= 0 {
				end = KeyForIndex(tt.end)
			}
			got, err := e.Scan(start, end, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantCount {
				t.Fatalf("scan returned %d keys, want %d", len(got), tt.wantCount)
			}
			if tt.wantCount > 0 {
				if want := KeyForIndex(tt.wantFirst); !bytes.Equal(got[0].Key, want) {
					t.Fatalf("first key = %q, want %q", got[0].Key, want)
				}
			}
		})
	}

	if err := e.Delete(KeyForIndex(10)); err != nil {
		t.Fatal(err)
	}
	got, err := e.Scan(KeyForIndex(10), KeyForIndex(12), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Key, KeyForIndex(11)) {
		t.Fatalf("scan after delete = %v, want only key 11", got)
	}
}

func TestMemEngineConcurrent(t *testing.T) {
	e := NewMemEngine()
	const (
		workers = 8
		perWork = 200
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWork; i++ {
				key := KeyForIndex(int64(w*perWork + i))
				if err := e.Put(key, key); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if _, err := e.Get(key); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if _, err := e.Scan(nil, nil, 3); err != nil {
					t.Errorf("Scan: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if got, want := e.Len(), workers*perWork; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	all, err := e.Scan(nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != workers*perWork {
		t.Fatalf("scan returned %d keys, want %d", len(all), workers*perWork)
	}
}

func TestMemEngineEmptyKey(t *testing.T) {
	e := NewMemEngine()
	if err := e.Put(nil, []byte("empty")); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get(nil)
	if err != nil {
		t.Fatalf("Get(nil): %v", err)
	}
	if string(got) != "empty" {
		t.Fatalf("Get(nil) = %q", got)
	}
}

func TestMemEngineLargeValue(t *testing.T) {
	e := NewMemEngine()
	value := bytes.Repeat([]byte("x"), 1<<20)
	if err := e.Put([]byte("big"), value); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get([]byte("big"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("large value round trip mismatch: got %d bytes", len(got))
	}
}
