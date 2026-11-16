package mvcc

import (
	"bytes"
	"errors"
	"sort"
	"testing"
	"time"
)

// requireRecordKeySet compares the raw keys of records with want without
// depending on the order in which the range scan visited them.
func requireRecordKeySet(t *testing.T, records []Record, want ...[]byte) {
	t.Helper()
	got := make([]string, len(records))
	for i := range records {
		got[i] = string(records[i].Key)
	}
	wantKeys := make([]string, len(want))
	for i := range want {
		wantKeys[i] = string(want[i])
	}
	sort.Strings(got)
	sort.Strings(wantKeys)
	if len(got) != len(wantKeys) {
		t.Fatalf("record keys = %q, want %q", got, wantKeys)
	}
	for i := range wantKeys {
		if got[i] != wantKeys[i] {
			t.Fatalf("record keys = %q, want %q", got, wantKeys)
		}
	}
}

func TestExportImportAndDeleteRange(t *testing.T) {
	source, sourceEngine := newTestStoreWithEngine(t)
	base := uint64(time.Now().UnixMilli()) << 18

	putValue(t, source, "a", "va", base+1, base+2)
	putValue(t, source, "b", "vb", base+3, base+4)
	putValue(t, source, "c", "vc1", base+5, base+6)
	putValue(t, source, "c", "vc2", base+9, base+10)
	putValue(t, source, "d", "vd", base+11, base+12)

	records, err := source.ExportRange([]byte("c"), nil)
	if err != nil {
		t.Fatalf("ExportRange(c, nil): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("c"), base+6),
		writeKey([]byte("c"), base+6),
		dataKey([]byte("c"), base+10),
		writeKey([]byte("c"), base+10),
		dataKey([]byte("d"), base+12),
		writeKey([]byte("d"), base+12),
	)
	// The export is in ascending raw key order.
	for i := 1; i < len(records); i++ {
		if bytes.Compare(records[i-1].Key, records[i].Key) >= 0 {
			t.Fatalf("records not in raw key order at %d: %x >= %x", i, records[i-1].Key, records[i].Key)
		}
	}
	// Every exported value is an exact copy of the stored record.
	for _, rec := range records {
		stored, err := sourceEngine.Get(rec.Key)
		if err != nil {
			t.Fatalf("source.Get(%x): %v", rec.Key, err)
		}
		if !bytes.Equal(stored, rec.Value) {
			t.Fatalf("exported value for %x = %x, want %x", rec.Key, rec.Value, stored)
		}
	}

	target, targetEngine := newTestStoreWithEngine(t)
	if err := target.ImportRecords(records); err != nil {
		t.Fatalf("ImportRecords: %v", err)
	}
	// The imported versions are visible at the same snapshots as on the source.
	requireValue(t, target, "c", base+6, "vc1")
	requireValue(t, target, "c", base+9, "vc1")
	requireValue(t, target, "c", base+10, "vc2")
	requireValue(t, target, "c", base+100, "vc2")
	requireValue(t, target, "d", base+100, "vd")
	requireKeyNotFound(t, target, "a", base+100)
	requireKeyNotFound(t, target, "b", base+100)
	requireKeys(t, scanKeys(t, target, nil, nil, base+100, 0), []string{"c", "d"})
	if !engineHas(t, targetEngine, dataKey([]byte("c"), base+10)) ||
		!engineHas(t, targetEngine, writeKey([]byte("d"), base+12)) {
		t.Fatal("imported data/write records are missing")
	}

	// Import copies the input: mutating the records afterwards must not change
	// the target store.
	for i := range records {
		for j := range records[i].Value {
			records[i].Value[j] = 'X'
		}
	}
	requireValue(t, target, "c", base+100, "vc2")
	requireValue(t, target, "d", base+100, "vd")

	// DeleteRange on the source removes exactly the exported user-key range.
	if err := source.DeleteRange([]byte("c"), nil); err != nil {
		t.Fatalf("DeleteRange(c, nil): %v", err)
	}
	requireKeyNotFound(t, source, "c", base+100)
	requireKeyNotFound(t, source, "d", base+100)
	requireValue(t, source, "a", base+100, "va")
	requireValue(t, source, "b", base+100, "vb")
	for _, rawKey := range [][]byte{
		dataKey([]byte("c"), base+6),
		writeKey([]byte("c"), base+6),
		dataKey([]byte("c"), base+10),
		writeKey([]byte("c"), base+10),
		dataKey([]byte("d"), base+12),
		writeKey([]byte("d"), base+12),
	} {
		if engineHas(t, sourceEngine, rawKey) {
			t.Fatalf("raw record %x survived DeleteRange", rawKey)
		}
	}
	remaining, err := source.ExportRange([]byte("c"), nil)
	if err != nil {
		t.Fatalf("ExportRange after delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("ExportRange after delete = %d records, want 0", len(remaining))
	}
	all, err := source.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange(nil, nil) after delete: %v", err)
	}
	requireRecordKeySet(t, all,
		dataKey([]byte("a"), base+2), writeKey([]byte("a"), base+2),
		dataKey([]byte("b"), base+4), writeKey([]byte("b"), base+4),
	)
}

// TestExportRangeBoundariesAndPrefixKeys exercises user keys that are prefixes
// of each other, which do not form a contiguous raw key range because the
// version is appended to the user key.
func TestExportRangeBoundariesAndPrefixKeys(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18

	keys := []string{"a", "a\x00", "a0", "b"}
	commitTS := func(i int) uint64 { return base + uint64(2*i+2) }
	for i, key := range keys {
		putValue(t, store, key, "v"+key, base+uint64(2*i+1), commitTS(i))
	}

	// [a, b) contains the three keys below b.
	records, err := store.ExportRange([]byte("a"), []byte("b"))
	if err != nil {
		t.Fatalf("ExportRange(a, b): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("a"), commitTS(0)), writeKey([]byte("a"), commitTS(0)),
		dataKey([]byte("a\x00"), commitTS(1)), writeKey([]byte("a\x00"), commitTS(1)),
		dataKey([]byte("a0"), commitTS(2)), writeKey([]byte("a0"), commitTS(2)),
	)

	// The end bound has a byte after a prefix key: the shorter key must still
	// be included, while a0 (which is greater than a\x01) is excluded.
	records, err = store.ExportRange([]byte("a"), []byte("a\x01"))
	if err != nil {
		t.Fatalf("ExportRange(a, a\\x01): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("a"), commitTS(0)), writeKey([]byte("a"), commitTS(0)),
		dataKey([]byte("a\x00"), commitTS(1)), writeKey([]byte("a\x00"), commitTS(1)),
	)

	// A lower bound is exclusive of everything below it.
	records, err = store.ExportRange([]byte("a\x00"), []byte("a0"))
	if err != nil {
		t.Fatalf("ExportRange(a\\x00, a0): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("a\x00"), commitTS(1)), writeKey([]byte("a\x00"), commitTS(1)),
	)

	records, err = store.ExportRange([]byte("a0"), nil)
	if err != nil {
		t.Fatalf("ExportRange(a0, nil): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("a0"), commitTS(2)), writeKey([]byte("a0"), commitTS(2)),
		dataKey([]byte("b"), commitTS(3)), writeKey([]byte("b"), commitTS(3)),
	)

	for _, tc := range []struct{ start, end []byte }{
		{[]byte("a"), []byte("a")},
		{[]byte("b"), []byte("a")},
		{[]byte("z"), nil},
	} {
		records, err := store.ExportRange(tc.start, tc.end)
		if err != nil {
			t.Fatalf("ExportRange(%q, %q): %v", tc.start, tc.end, err)
		}
		if len(records) != 0 {
			t.Fatalf("ExportRange(%q, %q) = %d records, want 0", tc.start, tc.end, len(records))
		}
	}
}

func TestExportRangeCopiesLocks(t *testing.T) {
	source, sourceEngine := newTestStoreWithEngine(t)
	base := uint64(time.Now().UnixMilli()) << 18
	startTS := base + 10

	if err := source.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("pending")}}, []byte("c"), startTS); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}
	// This lock is outside [c, d) and must not be exported.
	if err := source.Prewrite([]Mutation{{Key: []byte("d"), Value: []byte("outside")}}, []byte("d"), startTS+2); err != nil {
		t.Fatalf("Prewrite d: %v", err)
	}

	records, err := source.ExportRange([]byte("c"), []byte("d"))
	if err != nil {
		t.Fatalf("ExportRange(c, d): %v", err)
	}
	requireRecordKeySet(t, records,
		dataKey([]byte("c"), startTS),
		lockKey([]byte("c")),
	)
	for _, rec := range records {
		stored, err := sourceEngine.Get(rec.Key)
		if err != nil {
			t.Fatalf("source.Get(%x): %v", rec.Key, err)
		}
		if !bytes.Equal(stored, rec.Value) {
			t.Fatalf("exported value for %x = %x, want %x", rec.Key, rec.Value, stored)
		}
	}
	// Exporting is non-destructive.
	requireErrIs(t, mustGetErr(source, "c", startTS), ErrLocked)

	target, targetEngine := newTestStoreWithEngine(t)
	if err := target.ImportRecords(records); err != nil {
		t.Fatalf("ImportRecords: %v", err)
	}
	if !engineHas(t, targetEngine, lockKey([]byte("c"))) {
		t.Fatal("lock record was not imported")
	}
	if !engineHas(t, targetEngine, dataKey([]byte("c"), startTS)) {
		t.Fatal("provisional data record was not imported")
	}
	if engineHas(t, targetEngine, lockKey([]byte("d"))) {
		t.Fatal("lock outside the range was imported")
	}
	requireErrIs(t, mustGetErr(target, "c", startTS), ErrLocked)

	importedLock, err := targetEngine.Get(lockKey([]byte("c")))
	if err != nil {
		t.Fatalf("target.Get(lock c): %v", err)
	}
	sourceLock, err := sourceEngine.Get(lockKey([]byte("c")))
	if err != nil {
		t.Fatalf("source.Get(lock c): %v", err)
	}
	if !bytes.Equal(importedLock, sourceLock) {
		t.Fatalf("imported lock = %x, want %x", importedLock, sourceLock)
	}

	// The copied lock is usable: committing it publishes the pending value.
	if err := target.Commit([][]byte{[]byte("c")}, startTS, startTS+1); err != nil {
		t.Fatalf("Commit imported lock: %v", err)
	}
	requireValue(t, target, "c", startTS+1, "pending")
	if engineHas(t, targetEngine, lockKey([]byte("c"))) {
		t.Fatal("lock survived commit on the target store")
	}
}

func TestExportRangeReturnsDeepCopies(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	base := uint64(time.Now().UnixMilli()) << 18
	putValue(t, store, "a", "value", base+1, base+2)

	records, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("ExportRange = %d records, want 2", len(records))
	}
	original := make(map[string][]byte, len(records))
	for _, rec := range records {
		original[string(rec.Key)] = append([]byte(nil), rec.Value...)
	}

	// Mutate every byte of the returned keys and values. If they aliased the
	// iterator or the engine, the store and a later export would change.
	for i := range records {
		for j := range records[i].Key {
			records[i].Key[j] = 0xff
		}
		for j := range records[i].Value {
			records[i].Value[j] = 0xff
		}
	}

	requireValue(t, store, "a", base+2, "value")
	raw, err := engine.Get(dataKey([]byte("a"), base+2))
	if err != nil {
		t.Fatalf("engine.Get(data a): %v", err)
	}
	if string(raw) != "value" {
		t.Fatalf("stored data value = %q after mutating the export, want %q", raw, "value")
	}

	again, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("second ExportRange: %v", err)
	}
	if len(again) != len(original) {
		t.Fatalf("second ExportRange = %d records, want %d", len(again), len(original))
	}
	for _, rec := range again {
		want, ok := original[string(rec.Key)]
		if !ok {
			t.Fatalf("second export has unexpected key %x", rec.Key)
		}
		if !bytes.Equal(rec.Value, want) {
			t.Fatalf("second export value for %x = %x, want %x", rec.Key, rec.Value, want)
		}
	}
}

func TestExportImportDeleteRangeEmpty(t *testing.T) {
	store := newTestStore(t)
	base := uint64(time.Now().UnixMilli()) << 18
	putValue(t, store, "a", "va", base+1, base+2)

	emptyRanges := []struct {
		name       string
		start, end []byte
	}{
		{"equal bounds", []byte("a"), []byte("a")},
		{"inverted bounds", []byte("c"), []byte("b")},
		{"empty end", nil, []byte{}},
	}
	for _, tc := range emptyRanges {
		records, err := store.ExportRange(tc.start, tc.end)
		if err != nil {
			t.Fatalf("%s: ExportRange: %v", tc.name, err)
		}
		if len(records) != 0 {
			t.Fatalf("%s: ExportRange = %d records, want 0", tc.name, len(records))
		}
		if err := store.DeleteRange(tc.start, tc.end); err != nil {
			t.Fatalf("%s: DeleteRange: %v", tc.name, err)
		}
		requireValue(t, store, "a", base+2, "va")
	}

	// Empty imports are no-ops and must not disturb existing data.
	if err := store.ImportRecords(nil); err != nil {
		t.Fatalf("ImportRecords(nil): %v", err)
	}
	if err := store.ImportRecords([]Record{}); err != nil {
		t.Fatalf("ImportRecords(empty): %v", err)
	}
	requireValue(t, store, "a", base+2, "va")

	empty := newTestStore(t)
	records, err := empty.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("empty store ExportRange: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("empty store ExportRange = %d records, want 0", len(records))
	}
	if err := empty.DeleteRange(nil, nil); err != nil {
		t.Fatalf("empty store DeleteRange: %v", err)
	}
}

func TestDeleteRangeRemovesOnlyUserKeyRange(t *testing.T) {
	store, engine := newTestStoreWithEngine(t)
	base := uint64(time.Now().UnixMilli()) << 18

	putValue(t, store, "a", "va", base+1, base+2)
	putValue(t, store, "b", "vb", base+3, base+4)
	putValue(t, store, "c", "vc", base+5, base+6)
	putValue(t, store, "d", "vd", base+7, base+8)

	// A lock on c must be removed by DeleteRange too.
	lockStart := base + 100
	if err := store.Prewrite([]Mutation{{Key: []byte("c"), Value: []byte("pending")}}, []byte("c"), lockStart); err != nil {
		t.Fatalf("Prewrite c: %v", err)
	}

	// An unknown column family is neither exported nor deleted.
	unknownKey := []byte{'e', 'b'}
	if err := engine.Set(unknownKey, []byte("keep")); err != nil {
		t.Fatalf("engine.Set unknown CF: %v", err)
	}

	if err := store.DeleteRange([]byte("b"), []byte("d")); err != nil {
		t.Fatalf("DeleteRange(b, d): %v", err)
	}
	requireValue(t, store, "a", base+200, "va")
	requireValue(t, store, "d", base+200, "vd")
	requireKeyNotFound(t, store, "b", base+200)
	// The lock was deleted rather than left behind, so this is not ErrLocked.
	requireKeyNotFound(t, store, "c", base+200)
	for _, rawKey := range [][]byte{
		dataKey([]byte("b"), base+4), writeKey([]byte("b"), base+4),
		dataKey([]byte("c"), base+6), writeKey([]byte("c"), base+6),
		dataKey([]byte("c"), lockStart), lockKey([]byte("c")),
	} {
		if engineHas(t, engine, rawKey) {
			t.Fatalf("raw record %x survived DeleteRange", rawKey)
		}
	}
	if !engineHas(t, engine, dataKey([]byte("a"), base+2)) ||
		!engineHas(t, engine, writeKey([]byte("d"), base+8)) {
		t.Fatal("DeleteRange removed records outside its user-key bounds")
	}
	if !engineHas(t, engine, unknownKey) {
		t.Fatal("DeleteRange removed an unknown column family record")
	}

	records, err := store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange: %v", err)
	}
	for _, rec := range records {
		switch rec.Key[0] {
		case dataCF, lockCF, writeCF:
		default:
			t.Fatalf("ExportRange returned unknown column family key %x", rec.Key)
		}
	}

	// A nil/nil delete clears all known column families but leaves unknown
	// column families alone.
	if err := store.DeleteRange(nil, nil); err != nil {
		t.Fatalf("DeleteRange(nil, nil): %v", err)
	}
	requireKeyNotFound(t, store, "a", base+200)
	requireKeyNotFound(t, store, "d", base+200)
	requireKeys(t, scanKeys(t, store, nil, nil, base+200, 0), nil)
	records, err = store.ExportRange(nil, nil)
	if err != nil {
		t.Fatalf("ExportRange after full delete: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("ExportRange after full delete = %d records, want 0", len(records))
	}
	if !engineHas(t, engine, unknownKey) {
		t.Fatal("full DeleteRange removed an unknown column family record")
	}
}

func TestImportRecordsAtomicOnFailure(t *testing.T) {
	engine := newFakeEngine()
	// The fake is required here to inject an atomic batch failure.
	store := New(engine)

	records := []Record{
		{Key: lockKey([]byte("a")), Value: []byte("lock")},
		{Key: dataKey([]byte("a"), 10), Value: []byte("data")},
		{Key: writeKey([]byte("a"), 10), Value: encodeWrite(writeRecord{kind: kindRollback, startTS: 10})},
	}
	engine.failNextBatchCommit = errors.New("injected import failure")
	if err := store.ImportRecords(records); err == nil {
		t.Fatal("ImportRecords succeeded despite injected batch failure")
	}
	for _, rec := range records {
		if engineHas(t, engine, rec.Key) {
			t.Fatalf("record %x was written despite the failed import", rec.Key)
		}
	}

	// The failed batch is closed and does not prevent a later retry.
	if err := store.ImportRecords(records); err != nil {
		t.Fatalf("retry ImportRecords: %v", err)
	}
	for _, rec := range records {
		got, err := engine.Get(rec.Key)
		if err != nil {
			t.Fatalf("engine.Get(%x): %v", rec.Key, err)
		}
		if !bytes.Equal(got, rec.Value) {
			t.Fatalf("imported %x = %x, want %x", rec.Key, got, rec.Value)
		}
	}
}

func TestDeleteRangeAtomicOnFailure(t *testing.T) {
	engine := newFakeEngine()
	// The fake is required here to inject an atomic batch failure.
	store := New(engine)
	base := uint64(time.Now().UnixMilli()) << 18
	putValue(t, store, "a", "va", base+1, base+2)
	putValue(t, store, "b", "vb", base+3, base+4)

	engine.failNextBatchCommit = errors.New("injected delete failure")
	if err := store.DeleteRange(nil, nil); err == nil {
		t.Fatal("DeleteRange succeeded despite injected batch failure")
	}
	requireValue(t, store, "a", base+2, "va")
	requireValue(t, store, "b", base+4, "vb")
	if !engineHas(t, engine, dataKey([]byte("a"), base+2)) ||
		!engineHas(t, engine, writeKey([]byte("b"), base+4)) {
		t.Fatal("DeleteRange partially applied records despite the failed batch")
	}

	if err := store.DeleteRange(nil, nil); err != nil {
		t.Fatalf("retry DeleteRange: %v", err)
	}
	requireKeyNotFound(t, store, "a", base+2)
	requireKeyNotFound(t, store, "b", base+4)
}
