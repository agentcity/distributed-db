package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// testRangePrefix mirrors the KV server's fixed-width range prefix:
// 'r' | big-endian range ID | 0x00.
func testRangePrefix(id uint64) []byte {
	p := make([]byte, 1+8+1)
	p[0] = 'r'
	binary.BigEndian.PutUint64(p[1:9], id)
	p[9] = 0x00
	return p
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct {
		name   string
		prefix []byte
		want   []byte
	}{
		{"nil", nil, nil},
		{"empty", []byte{}, nil},
		{"one-ff", []byte{0xff}, nil},
		{"all-ff", []byte{0xff, 0xff}, nil},
		{"a", []byte("a"), []byte("b")},
		{"a-zero", []byte{'a', 0x00}, []byte{'a', 0x01}},
		{"a-ff", []byte{'a', 0xff}, []byte{'b'}},
		{"a-ff-ff", []byte{'a', 0xff, 0xff}, []byte{'b'}},
		{"zero", []byte{0x00}, []byte{0x01}},
		{"a-zero-ff", []byte{'a', 0x00, 0xff}, []byte{'a', 0x01}},
		{"range-prefix", testRangePrefix(7), append(testRangePrefix(7)[:9], 0x01)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prefixEnd(tc.prefix)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("prefixEnd(% x) = % x, want % x", tc.prefix, got, tc.want)
			}
		})
	}

	// Property: every extension of the prefix sorts strictly before its end.
	prefixes := [][]byte{
		nil,
		[]byte("a"),
		{'a', 0x00, 0xff},
		{0x00},
		{0xfe, 0xff},
		testRangePrefix(1),
		testRangePrefix(1<<40 + 3),
	}
	suffixes := [][]byte{
		nil,
		{},
		{0x00},
		{0x00, 0x00},
		{0xff},
		{0xff, 0xff},
		[]byte("user-key"),
	}
	for _, prefix := range prefixes {
		end := prefixEnd(prefix)
		for _, suffix := range suffixes {
			key := append(append([]byte(nil), prefix...), suffix...)
			if !bytes.HasPrefix(key, prefix) {
				t.Fatalf("test key % x does not have prefix % x", key, prefix)
			}
			if end != nil && bytes.Compare(key, end) >= 0 {
				t.Fatalf("key % x (prefix % x) >= prefixEnd % x", key, prefix, end)
			}
		}
	}
}

// prefixKeys collects the user keys of e in [start,end) and checks ascending
// order and the iterator error.
func prefixKeys(t *testing.T, e Engine, start, end []byte) []string {
	t.Helper()
	it, err := e.NewIter(start, end)
	if err != nil {
		t.Fatalf("NewIter(%q,%q): %v", start, end, err)
	}
	defer it.Close()
	var keys []string
	var prev []byte
	for ; it.Valid(); it.Next() {
		key := append([]byte(nil), it.Key()...)
		if prev != nil && bytes.Compare(prev, key) >= 0 {
			t.Fatalf("keys not ascending: %q after %q", key, prev)
		}
		prev = key
		keys = append(keys, string(key))
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	return keys
}

func checkPrefixKeys(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: keys = %q, want %q", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: keys = %q, want %q", name, got, want)
		}
	}
}

func TestPrefixIsolatesRanges(t *testing.T) {
	shared := openTest(t, Options{})
	r1 := Prefix(shared, testRangePrefix(1))
	r2 := Prefix(shared, testRangePrefix(2))

	// The same user key can hold different values in each range.
	mustSet(t, r1, "k", "one")
	mustSet(t, r2, "k", "two")
	if got := string(mustGet(t, r1, "k")); got != "one" {
		t.Fatalf("r1.Get(k) = %q, want one", got)
	}
	if got := string(mustGet(t, r2, "k")); got != "two" {
		t.Fatalf("r2.Get(k) = %q, want two", got)
	}

	// The physical layout is the fixed prefix followed by the user key.
	rawKey := append(testRangePrefix(1), []byte("k")...)
	if got, err := shared.Get(rawKey); err != nil || string(got) != "one" {
		t.Fatalf("shared.Get(%x) = %q, %v; want one", rawKey, got, err)
	}

	// Delete only affects its own range.
	if err := r1.Delete([]byte("k")); err != nil {
		t.Fatalf("r1.Delete(k): %v", err)
	}
	if _, err := r1.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r1.Get(k) after delete = %v, want ErrNotFound", err)
	}
	if got := string(mustGet(t, r2, "k")); got != "two" {
		t.Fatalf("r2.Get(k) after r1 delete = %q, want two", got)
	}
	if _, err := shared.Get(rawKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared.Get(%x) after r1 delete = %v, want ErrNotFound", rawKey, err)
	}

	// Empty user keys and keys that are byte prefixes of each other must stay
	// distinct within and across ranges.
	userKeys := []string{"", "a", "a\x00", "a\x00b", "aB", "ab"}
	for i, key := range userKeys {
		mustSet(t, r1, key, fmt.Sprintf("r1-%d", i))
		mustSet(t, r2, key, fmt.Sprintf("r2-%d", i))
	}
	want := []string{"", "a", "a\x00", "a\x00b", "aB", "ab"}
	checkPrefixKeys(t, "r1 all", prefixKeys(t, r1, nil, nil), want)
	// r2 still holds the earlier "k" record that r1 deleted.
	checkPrefixKeys(t, "r2 all", prefixKeys(t, r2, nil, nil), append(append([]string(nil), want...), "k"))
	for i, key := range userKeys {
		if got := string(mustGet(t, r1, key)); got != fmt.Sprintf("r1-%d", i) {
			t.Fatalf("r1.Get(%q) = %q, want r1-%d", key, got, i)
		}
		if got := string(mustGet(t, r2, key)); got != fmt.Sprintf("r2-%d", i) {
			t.Fatalf("r2.Get(%q) = %q, want r2-%d", key, got, i)
		}
	}

	// Get returns a copy: mutating it must not change the stored value.
	got := mustGet(t, r1, "ab")
	got[0] = 'X'
	if again := string(mustGet(t, r1, "ab")); again != "r1-5" {
		t.Fatalf("r1.Get(ab) after mutating result = %q, want r1-5", again)
	}
}

func TestPrefixIteratorBounds(t *testing.T) {
	shared := openTest(t, Options{})
	r1 := Prefix(shared, testRangePrefix(11))
	r2 := Prefix(shared, testRangePrefix(12))

	keys := []string{"", "a", "a\x00", "a\x00b", "ab", "b", "c", "d"}
	for _, key := range keys {
		mustSet(t, r1, key, "v-"+key)
		mustSet(t, r2, key, "other-"+key)
	}

	collect := func(start, end []byte) []string {
		t.Helper()
		it, err := r1.NewIter(start, end)
		if err != nil {
			t.Fatalf("NewIter(%q,%q): %v", start, end, err)
		}
		defer it.Close()
		var out []string
		for ; it.Valid(); it.Next() {
			key := string(it.Key())
			if got, want := string(it.Value()), "v-"+key; got != want {
				t.Fatalf("Value(%q) = %q, want %q", key, got, want)
			}
			out = append(out, key)
		}
		if err := it.Error(); err != nil {
			t.Fatalf("iterator error: %v", err)
		}
		return out
	}

	check := func(name string, start, end []byte, want ...string) {
		t.Helper()
		checkPrefixKeys(t, name, collect(start, end), want)
	}

	all := []string{"", "a", "a\x00", "a\x00b", "ab", "b", "c", "d"}
	check("all", nil, nil, all...)
	check("empty-start", []byte{}, nil, all...)
	check("empty-end", nil, []byte{})
	check("both-empty", []byte{}, []byte{})
	check("nil-to-a", nil, []byte("a"), "")
	check("a-to-nil", []byte("a"), nil, "a", "a\x00", "a\x00b", "ab", "b", "c", "d")
	check("a-to-b", []byte("a"), []byte("b"), "a", "a\x00", "a\x00b", "ab")
	check("a0-to-ab", []byte("a\x00"), []byte("ab"), "a\x00", "a\x00b")
	check("equal-bounds", []byte("ab"), []byte("ab"))
	check("reversed", []byte("b"), []byte("a"))
	check("beyond-end", []byte("z"), nil)
	check("before-start", nil, []byte(""))
	check("nil-to-z", nil, []byte("z"), all...)
}

func TestPrefixBatchAtomic(t *testing.T) {
	shared := openTest(t, Options{})
	r1 := Prefix(shared, testRangePrefix(21))
	r2 := Prefix(shared, testRangePrefix(22))

	mustSet(t, r1, "a", "old-r1")
	mustSet(t, r2, "a", "old-r2")

	b := r1.NewBatch()
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

	// Nothing is visible before Commit, in either range.
	if got := string(mustGet(t, r1, "a")); got != "old-r1" {
		t.Fatalf("r1.Get(a) before commit = %q, want old-r1", got)
	}
	if _, err := r1.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r1.Get(b) before commit = %v, want ErrNotFound", err)
	}
	if _, err := r2.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r2.Get(b) before commit = %v, want ErrNotFound", err)
	}

	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := string(mustGet(t, r2, "a")); got != "old-r2" {
		t.Fatalf("r2.Get(a) after r1 batch = %q, want old-r2", got)
	}
	if _, err := r1.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r1.Get(a) after commit = %v, want ErrNotFound", err)
	}
	if got := string(mustGet(t, r1, "b")); got != "2" {
		t.Fatalf("r1.Get(b) = %q, want 2", got)
	}
	if got := string(mustGet(t, r1, "c")); got != "3" {
		t.Fatalf("r1.Get(c) = %q, want 3", got)
	}
	if _, err := r2.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r2.Get(c) = %v, want ErrNotFound", err)
	}
	checkPrefixKeys(t, "r1 batch", prefixKeys(t, r1, nil, nil), []string{"b", "c"})

	// Commit/Close lifecycle matches the base batch.
	if err := b.Commit(); err == nil {
		t.Fatal("second Commit succeeded, want error")
	}
	if err := b.Set([]byte("d"), []byte("4")); err == nil {
		t.Fatal("Set after Commit succeeded, want error")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("batch Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second batch Close: %v", err)
	}
	// Writes: initial Set(a), then batch Set(b), Set(c) and Delete(a).
	if st := r1.Stats(); st.Writes != 4 {
		t.Fatalf("r1 Stats().Writes = %d, want 4", st.Writes)
	}
}

func TestPrefixSnapshot(t *testing.T) {
	shared := openTest(t, Options{})
	r1 := Prefix(shared, testRangePrefix(31))
	r2 := Prefix(shared, testRangePrefix(32))

	mustSet(t, r1, "a", "a1")
	mustSet(t, r1, "b", "b2")
	mustSet(t, r2, "a", "other")

	snap, err := r1.Snapshot()
	if err != nil {
		t.Fatalf("r1.Snapshot: %v", err)
	}
	defer snap.Close()

	// Writes after the snapshot must not be visible through it, and r2's data
	// must never appear in it.
	mustSet(t, r1, "b", "b22")
	mustSet(t, r1, "c", "c3")
	mustSet(t, r2, "d", "other-d")

	if got, err := snap.Get([]byte("b")); err != nil || string(got) != "b2" {
		t.Fatalf("snapshot Get(b) = %q, %v, want b2", got, err)
	}
	if got, err := snap.Get([]byte("a")); err != nil || string(got) != "a1" {
		t.Fatalf("snapshot Get(a) = %q, %v, want a1", got, err)
	}
	if _, err := snap.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot Get(c) = %v, want ErrNotFound", err)
	}
	if _, err := snap.Get([]byte("d")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot Get(d) = %v, want ErrNotFound", err)
	}

	it, err := snap.NewIter(nil, nil)
	if err != nil {
		t.Fatalf("snapshot NewIter: %v", err)
	}
	var keys []string
	for ; it.Valid(); it.Next() {
		keys = append(keys, string(it.Key()))
	}
	if err := it.Error(); err != nil {
		t.Fatalf("snapshot iterator: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("snapshot iterator Close: %v", err)
	}
	checkPrefixKeys(t, "snapshot all", keys, []string{"a", "b"})

	// Snapshot iterators honor the same bounds and prefix stripping.
	sit, err := snap.NewIter([]byte("b"), nil)
	if err != nil {
		t.Fatalf("snapshot bounded NewIter: %v", err)
	}
	if !sit.Valid() || string(sit.Key()) != "b" || string(sit.Value()) != "b2" {
		t.Fatalf("snapshot bounded first = %q/%q, want b/b2", sit.Key(), sit.Value())
	}
	sit.Next()
	if sit.Valid() {
		t.Fatalf("snapshot bounded extra key %q", sit.Key())
	}
	if err := sit.Close(); err != nil {
		t.Fatalf("snapshot bounded iterator Close: %v", err)
	}

	// The engine keeps writing while the snapshot is open.
	if got := string(mustGet(t, r1, "c")); got != "c3" {
		t.Fatalf("r1.Get(c) = %q, want c3", got)
	}

	if err := snap.Close(); err != nil {
		t.Fatalf("snapshot Close: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("second snapshot Close: %v", err)
	}
	if _, err := snap.Get([]byte("b")); !errors.Is(err, errClosed) {
		t.Fatalf("snapshot Get after Close = %v, want errClosed", err)
	}
	if got := string(mustGet(t, r1, "b")); got != "b22" {
		t.Fatalf("r1.Get(b) after snapshot close = %q, want b22", got)
	}
}

func TestPrefixStats(t *testing.T) {
	shared := openTest(t, Options{})
	r1 := Prefix(shared, testRangePrefix(41))
	r2 := Prefix(shared, testRangePrefix(42))

	mustSet(t, r1, "a", "1")
	mustSet(t, r1, "bc", "xyz")
	mustSet(t, r2, "long-key", string(bytes.Repeat([]byte("v"), 100)))

	want1 := int64(len(testRangePrefix(41))+1+1) + int64(len(testRangePrefix(41))+2+3)
	st1 := r1.Stats()
	if st1.DiskSize != want1 {
		t.Fatalf("r1 DiskSize = %d, want %d", st1.DiskSize, want1)
	}
	if st1.MemTableSize != 0 {
		t.Fatalf("r1 MemTableSize = %d, want 0", st1.MemTableSize)
	}
	if st1.Writes != 2 || st1.Reads != 0 {
		t.Fatalf("r1 counters = %+v, want writes=2 reads=0", st1)
	}

	want2 := int64(len(testRangePrefix(42)) + len("long-key") + 100)
	if st2 := r2.Stats(); st2.DiskSize != want2 {
		t.Fatalf("r2 DiskSize = %d, want %d (r1 stats must not leak)", st2.DiskSize, want2)
	}

	// Stats tracks mutations and sees the latest size only for this prefix.
	mustSet(t, r1, "a", "much-longer")
	if err := r1.Delete([]byte("bc")); err != nil {
		t.Fatalf("r1.Delete(bc): %v", err)
	}
	st1 = r1.Stats()
	want1 = int64(len(testRangePrefix(41)) + 1 + len("much-longer"))
	if st1.DiskSize != want1 {
		t.Fatalf("r1 DiskSize after update = %d, want %d", st1.DiskSize, want1)
	}
	if st1.Writes != 4 {
		t.Fatalf("r1 Writes = %d, want 4", st1.Writes)
	}
	if st2 := r2.Stats(); st2.DiskSize != want2 {
		t.Fatalf("r2 DiskSize after r1 changes = %d, want %d", st2.DiskSize, want2)
	}

	// The read counter is local as well.
	if _, err := r1.Get([]byte("a")); err != nil {
		t.Fatalf("r1.Get(a): %v", err)
	}
	if st := r1.Stats(); st.Reads != 1 {
		t.Fatalf("r1 Reads = %d, want 1", st.Reads)
	}
	if st := r2.Stats(); st.Reads != 0 {
		t.Fatalf("r2 Reads = %d, want 0", st.Reads)
	}
}

func TestPrefixCloseDoesNotCloseParent(t *testing.T) {
	shared := openTest(t, Options{})
	r := Prefix(shared, testRangePrefix(51))
	mustSet(t, r, "k", "v")

	// Wrapper Close is a no-op, idempotent, and leaves both the wrapper and
	// the shared engine usable.
	if err := r.Close(); err != nil {
		t.Fatalf("r.Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second r.Close: %v", err)
	}
	if got := string(mustGet(t, r, "k")); got != "v" {
		t.Fatalf("r.Get(k) after Close = %q, want v", got)
	}
	rawKey := append(testRangePrefix(51), []byte("k")...)
	if got := string(mustGet(t, shared, string(rawKey))); got != "v" {
		t.Fatalf("shared.Get(raw) after r.Close = %q, want v", got)
	}
	if err := r.Set([]byte("k2"), []byte("v2")); err != nil {
		t.Fatalf("r.Set after Close: %v", err)
	}

	// The real owner closing the engine makes wrapper operations fail; the
	// wrapper still never panics.
	if err := shared.Close(); err != nil {
		t.Fatalf("shared.Close: %v", err)
	}
	if _, err := r.Get([]byte("k")); !errors.Is(err, errClosed) {
		t.Fatalf("r.Get after shared Close = %v, want errClosed", err)
	}
	if st := r.Stats(); st.DiskSize != 0 {
		t.Fatalf("r.Stats after shared Close DiskSize = %d, want 0", st.DiskSize)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("r.Close after shared Close: %v", err)
	}
}

func TestPrefixArbitraryBytes(t *testing.T) {
	shared := openTest(t, Options{})
	// Prefixes containing 0x00 and 0xFF exercise bound construction without
	// relying on the KV server's fixed-width layout.
	prefix := []byte{0x00, 0xff}
	e := Prefix(shared, prefix)

	key := []byte{0xff, 0x00}
	mustSetBytes(t, e, key, []byte("value"))
	rawKey := append(append([]byte(nil), prefix...), key...)
	if got, err := shared.Get(rawKey); err != nil || string(got) != "value" {
		t.Fatalf("shared raw Get = %q, %v; want value", got, err)
	}
	if got := string(mustGet(t, e, string(key))); got != "value" {
		t.Fatalf("e.Get(% x) = %q, want value", key, got)
	}
	checkPrefixKeys(t, "arbitrary prefix", prefixKeys(t, e, nil, nil), []string{string(key)})

	// An empty prefix is an identity view over the raw keyspace.
	raw := Prefix(shared, nil)
	mustSet(t, raw, "plain", "p")
	mustSet(t, raw, "", "empty")
	if got := string(mustGet(t, shared, "plain")); got != "p" {
		t.Fatalf("shared.Get(plain) = %q, want p", got)
	}
	if got := string(mustGet(t, raw, "")); got != "empty" {
		t.Fatalf("raw.Get(empty) = %q, want empty", got)
	}
	found := make(map[string]bool)
	for _, key := range prefixKeys(t, raw, nil, nil) {
		found[key] = true
	}
	for _, want := range []string{"", "plain", string(rawKey)} {
		if !found[want] {
			t.Fatalf("raw keys missing %q; got %v", want, found)
		}
	}
}
