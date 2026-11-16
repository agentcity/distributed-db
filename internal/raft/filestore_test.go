package raft_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

func testEntry(index, term uint64, key, value string) model.Entry {
	e := model.Entry{Term: term, Index: index, Type: model.CommandPut, Key: key}
	if value != "" {
		e.Value = []byte(value)
	}
	return e
}

func findWAL(t *testing.T, dir string) string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 || des[0].IsDir() {
		t.Fatalf("expected one WAL file in %s, found %d entries", dir, len(des))
	}
	return filepath.Join(dir, des[0].Name())
}

func reopenFileStore(t *testing.T, dir string) *raft.FileStore {
	t.Helper()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen NewFileStore: %v", err)
	}
	return fs
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open WAL for append: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		t.Fatalf("append garbage: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}
}

func TestFileStorePersistsMetaEntriesAndTruncate(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.SaveMeta(5, "n2"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	first := []model.Entry{
		testEntry(1, 3, "a", "1"),
		testEntry(2, 3, "b", "2"),
		testEntry(3, 4, "c", "3"),
	}
	if err := fs.Append(first...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Term != 5 || state.VotedFor != "n2" {
		t.Fatalf("meta = term %d vote %q", state.Term, state.VotedFor)
	}
	if !reflect.DeepEqual(state.Entries, first) {
		t.Fatalf("entries = %+v, want %+v", state.Entries, first)
	}

	if err := fs.Truncate(2); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	replacement := []model.Entry{
		testEntry(2, 6, "b2", "22"),
		testEntry(3, 6, "c2", "33"),
	}
	if err := fs.Append(replacement...); err != nil {
		t.Fatalf("Append after truncate: %v", err)
	}
	if err := fs.Truncate(4); err != nil {
		t.Fatalf("Truncate beyond end should be a no-op: %v", err)
	}
	if err := fs.Truncate(0); err == nil {
		t.Fatal("Truncate(0) should fail")
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	state, err = reopened.Load()
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	want := []model.Entry{first[0], replacement[0], replacement[1]}
	if state.Term != 5 || state.VotedFor != "n2" {
		t.Fatalf("reopened meta = term %d vote %q, want 5/n2", state.Term, state.VotedFor)
	}
	if !reflect.DeepEqual(state.Entries, want) {
		t.Fatalf("reopened entries = %+v, want %+v", state.Entries, want)
	}

	next := testEntry(4, 7, "d", "4")
	if err := reopened.Append(next); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
	again, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer again.Close()
	state, err = again.Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	want = append(want, next)
	if !reflect.DeepEqual(state.Entries, want) {
		t.Fatalf("entries after second append = %+v, want %+v", state.Entries, want)
	}
}

func TestFileStoreRejectsNonContiguousAppend(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer fs.Close()

	if err := fs.Append(testEntry(2, 1, "gap", "x")); err == nil {
		t.Fatal("Append of index 2 without index 1 should fail")
	}
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Entries) != 0 {
		t.Fatalf("failed append mutated state: %+v", state.Entries)
	}
	if err := fs.Append(testEntry(1, 1, "a", "1")); err != nil {
		t.Fatalf("Append index 1: %v", err)
	}
	if err := fs.Append(testEntry(3, 1, "c", "3")); err == nil {
		t.Fatal("Append with an index gap should fail")
	}
	if err := fs.Append(testEntry(2, 1, "b", "2")); err != nil {
		t.Fatalf("Append index 2: %v", err)
	}
}

func TestFileStoreRecoversFromCorruptTail(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.SaveMeta(2, "n1"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	if err := fs.Append(testEntry(1, 1, "a", "1"), testEntry(2, 1, "b", "2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wal := findWAL(t, dir)

	// 1. A torn header (fewer than 8 bytes).
	appendBytes(t, wal, []byte{0xde, 0xad, 0xbe})
	fs = reopenFileStore(t, dir)
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load after torn header: %v", err)
	}
	if len(state.Entries) != 2 {
		t.Fatalf("entries after torn header = %d, want 2", len(state.Entries))
	}
	if err := fs.Append(testEntry(3, 2, "c", "3")); err != nil {
		t.Fatalf("Append after torn header: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2. Long trailing garbage (for example zeros from a sparse write).
	garbage := make([]byte, 100)
	appendBytes(t, wal, garbage)
	fs = reopenFileStore(t, dir)
	state, err = fs.Load()
	if err != nil {
		t.Fatalf("Load after trailing garbage: %v", err)
	}
	if len(state.Entries) != 3 {
		t.Fatalf("entries after trailing garbage = %d, want 3", len(state.Entries))
	}
	if err := fs.Append(testEntry(4, 2, "d", "4")); err != nil {
		t.Fatalf("Append after trailing garbage: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 3. A plausible but incomplete record header followed by garbage.
	appendBytes(t, wal, []byte{0, 0, 0, 64, 0x12, 0x34, 0x56, 0x78})
	fs = reopenFileStore(t, dir)
	state, err = fs.Load()
	if err != nil {
		t.Fatalf("Load after garbage header: %v", err)
	}
	if len(state.Entries) != 4 {
		t.Fatalf("entries after garbage header = %d, want 4", len(state.Entries))
	}
	if err := fs.Append(testEntry(5, 2, "e", "5")); err != nil {
		t.Fatalf("Append after garbage header: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	final, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer final.Close()
	state, err = final.Load()
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if len(state.Entries) != 5 {
		t.Fatalf("final entries = %d, want 5", len(state.Entries))
	}
	for i, e := range state.Entries {
		if e.Index != uint64(i)+1 {
			t.Fatalf("entry %d has index %d", i+1, e.Index)
		}
	}
}
