package storage

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble"
)

// openTest opens a temporary engine and registers its Close as cleanup.
func openTest(t *testing.T, opts Options) Engine {
	t.Helper()
	if opts.CacheSize == 0 {
		opts.CacheSize = 8 << 20
	}
	if opts.MemTableSize == 0 {
		opts.MemTableSize = 1 << 20
	}
	e, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

func mustGet(t *testing.T, e Engine, key string) []byte {
	t.Helper()
	v, err := e.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return v
}

func mustSet(t *testing.T, e Engine, key, value string) {
	t.Helper()
	if err := e.Set([]byte(key), []byte(value)); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func TestOpenRejectsUnknownCompression(t *testing.T) {
	if _, err := Open(t.TempDir(), Options{Compression: "brotli"}); err == nil {
		t.Fatal("Open with unknown compression succeeded, want error")
	}
}

func TestOpenEmptyDir(t *testing.T) {
	if _, err := Open("", Options{}); err == nil {
		t.Fatal("Open with empty dir succeeded, want error")
	}
}

func TestParseCompression(t *testing.T) {
	cases := []struct {
		in      string
		want    pebble.Compression
		wantErr bool
	}{
		{"", pebble.SnappyCompression, false},
		{"snappy", pebble.SnappyCompression, false},
		{"SNAPPY", pebble.SnappyCompression, false},
		{"zstd", pebble.ZstdCompression, false},
		{"Zstd", pebble.ZstdCompression, false},
		{"none", pebble.NoCompression, false},
		{" none ", pebble.NoCompression, false},
		{"gzip", pebble.NoCompression, true},
		{"lz4", pebble.NoCompression, true},
	}
	for _, tc := range cases {
		got, err := parseCompression(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCompression(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCompression(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseCompression(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestOpenOptionsAndCacheLifecycle(t *testing.T) {
	e, err := Open(t.TempDir(), Options{
		CacheSize:       1 << 20,
		MemTableSize:    512 << 10,
		MaxOpenFiles:    64,
		Compression:     "zstd",
		WALBytesPerSync: 32 << 10,
		BytesPerSync:    32 << 10,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	impl, ok := e.(*db)
	if !ok {
		t.Fatalf("engine type = %T, want *db", e)
	}
	if impl.cache == nil {
		t.Fatal("engine did not create a block cache")
	}
	if err := e.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if impl.cache != nil {
		t.Fatal("cache was not released on Close")
	}
}

func TestCRUD(t *testing.T) {
	e := openTest(t, Options{})

	if _, err := e.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}

	mustSet(t, e, "a", "1")
	if got := string(mustGet(t, e, "a")); got != "1" {
		t.Fatalf("Get(a) = %q, want 1", got)
	}

	// Overwrite.
	mustSet(t, e, "a", "2")
	if got := string(mustGet(t, e, "a")); got != "2" {
		t.Fatalf("Get(a) after overwrite = %q, want 2", got)
	}

	// Empty key is still a key.
	mustSet(t, e, "", "empty-key")
	if got := string(mustGet(t, e, "")); got != "empty-key" {
		t.Fatalf("Get(empty key) = %q, want empty-key", got)
	}

	if err := e.Delete([]byte("a")); err != nil {
		t.Fatalf("Delete(a): %v", err)
	}
	if _, err := e.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(a) after delete error = %v, want ErrNotFound", err)
	}

	// Deleting a missing key is not an error.
	if err := e.Delete([]byte("a")); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}

	// Delete then Set resurrects the key.
	mustSet(t, e, "a", "3")
	if got := string(mustGet(t, e, "a")); got != "3" {
		t.Fatalf("Get(a) after resurrect = %q, want 3", got)
	}
}

func TestGetReturnsCopyAndZeroValue(t *testing.T) {
	e := openTest(t, Options{})

	// A zero-length value exists and is not ErrNotFound.
	if err := e.Set([]byte("zero"), []byte{}); err != nil {
		t.Fatalf("Set(zero): %v", err)
	}
	zero, err := e.Get([]byte("zero"))
	if err != nil {
		t.Fatalf("Get(zero): %v", err)
	}
	if len(zero) != 0 {
		t.Fatalf("Get(zero) len = %d, want 0", len(zero))
	}
	if zero == nil {
		t.Fatal("Get(zero) = nil, want non-nil empty slice")
	}

	original := []byte("value")
	if err := e.Set([]byte("k"), original); err != nil {
		t.Fatalf("Set(k): %v", err)
	}
	got, err := e.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get(k): %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("Get(k) = %q, want %q", got, original)
	}
	// Mutating the result must not affect the engine.
	got[0] = 'X'
	if again := mustGet(t, e, "k"); !bytes.Equal(again, original) {
		t.Fatalf("Get(k) after mutating previous result = %q, want %q", again, original)
	}

	// Mutating the input slice after Set must not affect the engine either.
	in := []byte("abc")
	if err := e.Set([]byte("m"), in); err != nil {
		t.Fatalf("Set(m): %v", err)
	}
	in[0] = 'z'
	if got := mustGet(t, e, "m"); string(got) != "abc" {
		t.Fatalf("Get(m) = %q, want abc", got)
	}
}

func TestBatchCommit(t *testing.T) {
	e := openTest(t, Options{})

	mustSet(t, e, "a", "old")
	mustSet(t, e, "keep", "yes")

	b := e.NewBatch()
	defer b.Close()

	if err := b.Set([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("batch Set(b): %v", err)
	}
	if err := b.Set([]byte("c"), []byte("3")); err != nil {
		t.Fatalf("batch Set(c): %v", err)
	}
	if err := b.Delete([]byte("a")); err != nil {
		t.Fatalf("batch Delete(a): %v", err)
	}
	if got := b.Count(); got != 3 {
		t.Fatalf("batch Count() = %d, want 3", got)
	}

	// Nothing is visible before Commit.
	if _, err := e.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(b) before commit error = %v, want ErrNotFound", err)
	}
	if _, err := e.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(c) before commit error = %v, want ErrNotFound", err)
	}
	if got := mustGet(t, e, "a"); string(got) != "old" {
		t.Fatalf("Get(a) before commit = %q, want old", got)
	}

	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := mustGet(t, e, "b"); string(got) != "2" {
		t.Fatalf("Get(b) = %q, want 2", got)
	}
	if got := mustGet(t, e, "c"); string(got) != "3" {
		t.Fatalf("Get(c) = %q, want 3", got)
	}
	if _, err := e.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(a) after commit error = %v, want ErrNotFound", err)
	}
	if got := mustGet(t, e, "keep"); string(got) != "yes" {
		t.Fatalf("Get(keep) = %q, want yes", got)
	}

	// Double commit and post-commit mutations are rejected, not applied.
	if err := b.Commit(); err == nil {
		t.Fatal("second Commit succeeded, want error")
	}
	if err := b.Set([]byte("d"), []byte("4")); err == nil {
		t.Fatal("Set after Commit succeeded, want error")
	}
	if _, err := e.Get([]byte("d")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(d) after rejected Set error = %v, want ErrNotFound", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("batch Close after commit: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second batch Close: %v", err)
	}
	if err := b.Set([]byte("e"), []byte("5")); err == nil {
		t.Fatal("Set after Close succeeded, want error")
	}
}

func TestBatchCloseWithoutCommit(t *testing.T) {
	e := openTest(t, Options{})
	mustSet(t, e, "a", "1")

	b := e.NewBatch()
	if err := b.Set([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("batch Set: %v", err)
	}
	if err := b.Delete([]byte("a")); err != nil {
		t.Fatalf("batch Delete: %v", err)
	}
	if got := b.Count(); got != 2 {
		t.Fatalf("Count() = %d, want 2", got)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Commit(); err == nil {
		t.Fatal("Commit after Close succeeded, want error")
	}
	if got := mustGet(t, e, "a"); string(got) != "1" {
		t.Fatalf("Get(a) after uncommitted batch = %q, want 1", got)
	}
	if _, err := e.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(b) after uncommitted batch error = %v, want ErrNotFound", err)
	}
}

func TestBatchLarge(t *testing.T) {
	e := openTest(t, Options{})
	b := e.NewBatch()
	defer b.Close()
	const n = 1000
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%04d", i)
		if err := b.Set([]byte(key), []byte(key)); err != nil {
			t.Fatalf("batch Set %d: %v", i, err)
		}
	}
	if got := b.Count(); got != n {
		t.Fatalf("Count() = %d, want %d", got, n)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%04d", i)
		if got := string(mustGet(t, e, key)); got != key {
			t.Fatalf("Get(%s) = %q, want %q", key, got, key)
		}
	}
}

func TestIteratorBounds(t *testing.T) {
	e := openTest(t, Options{})
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		mustSet(t, e, k, "v-"+k)
	}

	collect := func(start, end []byte) []string {
		t.Helper()
		it, err := e.NewIter(start, end)
		if err != nil {
			t.Fatalf("NewIter: %v", err)
		}
		defer it.Close()
		var keys []string
		var prev string
		for ; it.Valid(); it.Next() {
			k := string(it.Key())
			if prev != "" && k <= prev {
				t.Fatalf("keys not ascending: %q after %q", k, prev)
			}
			prev = k
			if got := string(it.Value()); got != "v-"+k {
				t.Fatalf("Value(%q) = %q, want %q", k, got, "v-"+k)
			}
			keys = append(keys, k)
		}
		if err := it.Error(); err != nil {
			t.Fatalf("iterator error: %v", err)
		}
		return keys
	}
	check := func(name string, start, end []byte, want ...string) {
		t.Helper()
		got := collect(start, end)
		if len(got) != len(want) {
			t.Fatalf("%s: keys = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: keys = %v, want %v", name, got, want)
			}
		}
	}

	check("all", nil, nil, "a", "b", "c", "d", "e")
	check("range", []byte("b"), []byte("d"), "b", "c")
	check("no-end", []byte("c"), nil, "c", "d", "e")
	check("no-start", nil, []byte("c"), "a", "b")
	check("empty-start", []byte{}, nil, "a", "b", "c", "d", "e")
	check("single", []byte("b"), []byte("c"), "b")
	check("equal-bounds", []byte("c"), []byte("c"))
	check("reversed", []byte("e"), []byte("a"))
	check("empty-end", nil, []byte{})
	check("beyond-end", []byte("f"), nil)
	check("before-start", nil, []byte{0x00})
}

func TestIteratorValueValidUntilNext(t *testing.T) {
	e := openTest(t, Options{})
	mustSet(t, e, "a", "alpha")
	mustSet(t, e, "b", "beta")

	it, err := e.NewIter(nil, nil)
	if err != nil {
		t.Fatalf("NewIter: %v", err)
	}
	defer it.Close()
	if !it.Valid() {
		t.Fatal("iterator not valid at first key")
	}
	// Use Key/Value immediately, as required by the contract.
	if k, v := string(it.Key()), string(it.Value()); k != "a" || v != "alpha" {
		t.Fatalf("first = %q/%q, want a/alpha", k, v)
	}
	it.Next()
	if k, v := string(it.Key()), string(it.Value()); k != "b" || v != "beta" {
		t.Fatalf("second = %q/%q, want b/beta", k, v)
	}
	it.Next()
	if it.Valid() {
		t.Fatalf("iterator valid at %q after end", it.Key())
	}
	if it.Error() != nil {
		t.Fatalf("Error at end: %v", it.Error())
	}
	// Methods on an exhausted iterator are harmless.
	if it.Key() != nil || it.Value() != nil {
		t.Fatal("Key/Value non-nil on invalid iterator")
	}
	it.Next()
	if it.Valid() {
		t.Fatal("iterator valid after Next past end")
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if it.Valid() {
		t.Fatal("iterator valid after Close")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	e := openTest(t, Options{})
	mustSet(t, e, "k", "v1")
	mustSet(t, e, "gone", "here")
	mustSet(t, e, "shared", "old")

	snap, err := e.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Close()

	// Writes after the snapshot must not be visible through it.
	mustSet(t, e, "k", "v2")
	mustSet(t, e, "new", "later")
	mustSet(t, e, "shared", "new")
	if err := e.Delete([]byte("gone")); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := string(mustGet(t, e, "k")); got != "v2" {
		t.Fatalf("engine Get(k) = %q, want v2", got)
	}

	if got, err := snap.Get([]byte("k")); err != nil || string(got) != "v1" {
		t.Fatalf("snapshot Get(k) = %q, %v, want v1", got, err)
	}
	if got, err := snap.Get([]byte("gone")); err != nil || string(got) != "here" {
		t.Fatalf("snapshot Get(gone) = %q, %v, want here", got, err)
	}
	if got, err := snap.Get([]byte("shared")); err != nil || string(got) != "old" {
		t.Fatalf("snapshot Get(shared) = %q, %v, want old", got, err)
	}
	if _, err := snap.Get([]byte("new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot Get(new) error = %v, want ErrNotFound", err)
	}

	// Snapshot iterators observe the same fixed view.
	it, err := snap.NewIter(nil, nil)
	if err != nil {
		t.Fatalf("snapshot NewIter: %v", err)
	}
	var keys []string
	for ; it.Valid(); it.Next() {
		keys = append(keys, string(it.Key()))
	}
	if err := it.Error(); err != nil {
		t.Fatalf("snapshot iterator error: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("snapshot iterator Close: %v", err)
	}
	want := []string{"gone", "k", "shared"}
	if len(keys) != len(want) {
		t.Fatalf("snapshot keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("snapshot keys = %v, want %v", keys, want)
		}
	}

	// The engine keeps writing while the snapshot remains open.
	mustSet(t, e, "after", "yes")
	if got := string(mustGet(t, e, "after")); got != "yes" {
		t.Fatalf("engine Get(after) = %q, want yes", got)
	}

	if err := snap.Close(); err != nil {
		t.Fatalf("snapshot Close: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("second snapshot Close: %v", err)
	}
	if _, err := snap.Get([]byte("k")); err == nil {
		t.Fatal("snapshot Get after Close succeeded, want error")
	}
}

func TestSnapshotIndependentOfSecondSnapshot(t *testing.T) {
	e := openTest(t, Options{})
	mustSet(t, e, "k", "one")
	s1, err := e.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}
	defer s1.Close()
	mustSet(t, e, "k", "two")
	s2, err := e.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	defer s2.Close()
	mustSet(t, e, "k", "three")

	if got, err := s1.Get([]byte("k")); err != nil || string(got) != "one" {
		t.Fatalf("s1 Get(k) = %q, %v, want one", got, err)
	}
	if got, err := s2.Get([]byte("k")); err != nil || string(got) != "two" {
		t.Fatalf("s2 Get(k) = %q, %v, want two", got, err)
	}
	if got := mustGet(t, e, "k"); string(got) != "three" {
		t.Fatalf("engine Get(k) = %q, want three", got)
	}
}

func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()

	e, err := Open(dir, Options{SyncWrites: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Set([]byte("persist"), []byte("value")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := e.Set([]byte("zero"), []byte{}); err != nil {
		t.Fatalf("Set zero: %v", err)
	}
	if err := e.Delete([]byte("never-existed")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	e2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := string(mustGet(t, e2, "persist")); got != "value" {
		t.Fatalf("Get(persist) after reopen = %q, want value", got)
	}
	if got, err := e2.Get([]byte("zero")); err != nil || len(got) != 0 {
		t.Fatalf("Get(zero) after reopen = %q, %v, want empty value", got, err)
	}
	if _, err := e2.Get([]byte("never-existed")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(never-existed) = %v, want ErrNotFound", err)
	}
	if err := e2.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
}

func TestFlushMakesDataDurable(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	mustSet(t, e, "k", "v")
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	st := e.Stats()
	if st.DiskSize <= 0 {
		t.Fatalf("Stats().DiskSize = %d after flush, want > 0", st.DiskSize)
	}
}

func TestStats(t *testing.T) {
	e := openTest(t, Options{})

	st := e.Stats()
	if st.Reads != 0 || st.Writes != 0 {
		t.Fatalf("fresh Stats = %+v, want zero counters", st)
	}

	mustSet(t, e, "a", "1")
	mustGet(t, e, "a")
	if _, err := e.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v", err)
	}

	b := e.NewBatch()
	if err := b.Set([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("batch Set: %v", err)
	}
	if err := b.Delete([]byte("a")); err != nil {
		t.Fatalf("batch Delete: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("batch Close: %v", err)
	}

	snap, err := e.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := snap.Get([]byte("b")); err != nil {
		t.Fatalf("snapshot Get: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("snapshot Close: %v", err)
	}

	// 1 Set + 2 committed batch mutations; reads: 2 Gets + 1 snapshot Get.
	st = e.Stats()
	if st.Writes != 3 {
		t.Fatalf("Stats().Writes = %d, want 3", st.Writes)
	}
	if st.Reads != 3 {
		t.Fatalf("Stats().Reads = %d, want 3", st.Reads)
	}
	if st.MemTableSize < 0 || st.DiskSize < 0 {
		t.Fatalf("Stats() sizes negative: %+v", st)
	}

	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	st = e.Stats()
	if st.DiskSize <= 0 {
		t.Fatalf("Stats().DiskSize = %d after flush, want > 0", st.DiskSize)
	}
}

func TestCompressionRoundTrip(t *testing.T) {
	for _, compression := range []string{"", "snappy", "zstd", "none", "SNAPPY", " Zstd "} {
		compression := compression
		t.Run(fmt.Sprintf("%q", compression), func(t *testing.T) {
			dir := t.TempDir()
			e, err := Open(dir, Options{Compression: compression, CacheSize: 1 << 20})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			for i := 0; i < 100; i++ {
				k := fmt.Sprintf("key-%03d", i)
				if err := e.Set([]byte(k), []byte(k)); err != nil {
					t.Fatalf("Set: %v", err)
				}
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			for i := 0; i < 100; i++ {
				k := fmt.Sprintf("key-%03d", i)
				if got := string(mustGet(t, e, k)); got != k {
					t.Fatalf("Get(%s) = %q, want %q", k, got, k)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			e2, err := Open(dir, Options{})
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer e2.Close()
			for i := 0; i < 100; i++ {
				k := fmt.Sprintf("key-%03d", i)
				if got := string(mustGet(t, e2, k)); got != k {
					t.Fatalf("Get(%s) after reopen = %q, want %q", k, got, k)
				}
			}
		})
	}
}

func TestSyncWrites(t *testing.T) {
	for _, sync := range []bool{true, false} {
		sync := sync
		t.Run(fmt.Sprintf("sync=%v", sync), func(t *testing.T) {
			dir := t.TempDir()
			e, err := Open(dir, Options{SyncWrites: sync, WALBytesPerSync: 1 << 20, BytesPerSync: 1 << 20})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			impl, ok := e.(*db)
			if !ok {
				t.Fatalf("engine type = %T, want *db", e)
			}
			if impl.syncWrites != sync {
				t.Fatalf("syncWrites = %v, want %v", impl.syncWrites, sync)
			}
			if got := impl.writeOpts.Sync; got != sync {
				t.Fatalf("write options Sync = %v, want %v", got, sync)
			}

			b := e.NewBatch()
			if err := b.Set([]byte("batch-key"), []byte("batch-value")); err != nil {
				t.Fatalf("batch Set: %v", err)
			}
			if err := b.Delete([]byte("missing")); err != nil {
				t.Fatalf("batch Delete: %v", err)
			}
			if err := b.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if err := b.Close(); err != nil {
				t.Fatalf("batch Close: %v", err)
			}
			if err := e.Set([]byte("set-key"), []byte("set-value")); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if err := e.Delete([]byte("set-key")); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Even without sync, a clean Close flushes and preserves data.
			e2, err := Open(dir, Options{SyncWrites: sync})
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer e2.Close()
			if got := string(mustGet(t, e2, "batch-key")); got != "batch-value" {
				t.Fatalf("Get(batch-key) = %q, want batch-value", got)
			}
			if _, err := e2.Get([]byte("set-key")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(set-key) = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestLargeValues(t *testing.T) {
	e := openTest(t, Options{MemTableSize: 1 << 20, CacheSize: 8 << 20})
	const size = 1 << 20 // 1 MiB
	value := make([]byte, size)
	for i := range value {
		value[i] = byte(i*31 + 7)
	}
	mustSetBytes(t, e, []byte("big"), value)
	got, err := e.Get([]byte("big"))
	if err != nil {
		t.Fatalf("Get(big): %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("Get(big) mismatch: len %d, want %d", len(got), len(value))
	}

	// Two large values in one atomic batch.
	b := e.NewBatch()
	v2 := bytes.Repeat([]byte("x"), size/2)
	v3 := bytes.Repeat([]byte("y"), size/2)
	if err := b.Set([]byte("big2"), v2); err != nil {
		t.Fatalf("batch Set(big2): %v", err)
	}
	if err := b.Set([]byte("big3"), v3); err != nil {
		t.Fatalf("batch Set(big3): %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("batch Commit: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("batch Close: %v", err)
	}
	if got, err := e.Get([]byte("big2")); err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("Get(big2): err=%v equal=%v", err, bytes.Equal(got, v2))
	}
	if got, err := e.Get([]byte("big3")); err != nil || !bytes.Equal(got, v3) {
		t.Fatalf("Get(big3): err=%v equal=%v", err, bytes.Equal(got, v3))
	}

	// Large values survive a flush and reopen.
	dir := t.TempDir()
	e2, err := Open(dir, Options{SyncWrites: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustSetBytes(t, e2, []byte("big"), value)
	if err := e2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	e3, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e3.Close()
	got, err = e3.Get([]byte("big"))
	if err != nil {
		t.Fatalf("Get(big) after reopen: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("Get(big) after reopen mismatch: len %d, want %d", len(got), len(value))
	}
}

func mustSetBytes(t *testing.T, e Engine, key, value []byte) {
	t.Helper()
	if err := e.Set(key, value); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	e := openTest(t, Options{MemTableSize: 1 << 20})

	const (
		writers   = 8
		readers   = 8
		perWriter = 100
	)
	start := make(chan struct{})
	errCh := make(chan error, writers+readers+1)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				key := fmt.Sprintf("w%d-%03d", w, i)
				if err := e.Set([]byte(key), []byte(key)); err != nil {
					errCh <- fmt.Errorf("Set(%s): %w", key, err)
					return
				}
			}
		}()
	}

	for r := 0; r < readers; r++ {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter*2; i++ {
				key := fmt.Sprintf("w%d-%03d", (i+r)%writers, i%perWriter)
				v, err := e.Get([]byte(key))
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if err != nil {
					errCh <- fmt.Errorf("Get(%s): %w", key, err)
					return
				}
				if string(v) != key {
					errCh <- fmt.Errorf("Get(%s) = %q", key, v)
					return
				}
			}
		}()
	}

	// Concurrent snapshots and iterators exercise the lifecycle locking.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 20; i++ {
			snap, err := e.Snapshot()
			if err != nil {
				errCh <- fmt.Errorf("Snapshot: %w", err)
				return
			}
			it, err := snap.NewIter(nil, nil)
			if err != nil {
				snap.Close()
				errCh <- fmt.Errorf("snapshot NewIter: %w", err)
				return
			}
			n := 0
			for ; it.Valid(); it.Next() {
				n++
			}
			if err := it.Error(); err != nil {
				errCh <- fmt.Errorf("snapshot iterator: %w", err)
			}
			if err := it.Close(); err != nil {
				errCh <- fmt.Errorf("snapshot iterator Close: %w", err)
			}
			if err := snap.Close(); err != nil {
				errCh <- fmt.Errorf("snapshot Close: %w", err)
			}
			_ = n
		}
	}()

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			key := fmt.Sprintf("w%d-%03d", w, i)
			if got := string(mustGet(t, e, key)); got != key {
				t.Fatalf("Get(%s) = %q, want %q", key, got, key)
			}
		}
	}
	if st := e.Stats(); st.Writes != writers*perWriter {
		t.Fatalf("Stats().Writes = %d, want %d", st.Writes, writers*perWriter)
	}
}

func TestEngineClosedOperations(t *testing.T) {
	e := openTest(t, Options{})
	mustSet(t, e, "k", "v")
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := e.Get([]byte("k")); !errors.Is(err, errClosed) {
		t.Fatalf("Get after Close = %v, want errClosed", err)
	}
	if err := e.Set([]byte("k"), []byte("v")); !errors.Is(err, errClosed) {
		t.Fatalf("Set after Close = %v, want errClosed", err)
	}
	if err := e.Delete([]byte("k")); !errors.Is(err, errClosed) {
		t.Fatalf("Delete after Close = %v, want errClosed", err)
	}
	if _, err := e.NewIter(nil, nil); !errors.Is(err, errClosed) {
		t.Fatalf("NewIter after Close = %v, want errClosed", err)
	}
	if _, err := e.Snapshot(); !errors.Is(err, errClosed) {
		t.Fatalf("Snapshot after Close = %v, want errClosed", err)
	}
	if err := e.Flush(); !errors.Is(err, errClosed) {
		t.Fatalf("Flush after Close = %v, want errClosed", err)
	}

	// Stats must remain safe to call (counters only after close).
	st := e.Stats()
	if st.Writes != 1 {
		t.Fatalf("Stats after Close Writes = %d, want 1", st.Writes)
	}

	// A batch obtained after Close fails cleanly instead of panicking.
	b := e.NewBatch()
	if err := b.Set([]byte("x"), []byte("y")); err == nil {
		t.Fatal("batch Set after engine Close succeeded, want error")
	}
	if err := b.Commit(); err == nil {
		t.Fatal("batch Commit after engine Close succeeded, want error")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("batch Close: %v", err)
	}
}
