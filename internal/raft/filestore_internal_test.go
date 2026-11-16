package raft

import (
	"os"
	"path/filepath"
	"testing"

	"distributed-db/internal/model"
)

// These white-box tests exercise the exact WAL framing (magic, length, CRC) to
// prove that a crash in the middle of the final record is discarded rather
// than making the store unloadable.

func newWALWithOneEntry(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.SaveMeta(1, "n1"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	if err := fs.Append(model.Entry{
		Term: 1, Index: 1, Type: model.CommandPut, Key: "a", Value: []byte("1"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir, filepath.Join(dir, walFileName)
}

func appendRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		t.Fatalf("write WAL: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}
}

func TestFileStoreDropsTornFinalPayload(t *testing.T) {
	dir, path := newWALWithOneEntry(t)
	frame, err := encodeWalRecord(walRecord{
		Type:    "append",
		Entries: []model.Entry{{Term: 2, Index: 2, Type: model.CommandPut, Key: "b", Value: []byte("2")}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	appendRaw(t, path, frame[:len(frame)-5])

	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen with torn payload: %v", err)
	}
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Entries) != 1 || state.Term != 1 || state.VotedFor != "n1" {
		t.Fatalf("state after torn payload = %+v", state)
	}
	if err := fs.Append(model.Entry{Term: 2, Index: 2, Type: model.CommandPut, Key: "b", Value: []byte("2")}); err != nil {
		t.Fatalf("append after tail truncation: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	final, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer final.Close()
	state, err = final.Load()
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if len(state.Entries) != 2 || state.Entries[1].Key != "b" {
		t.Fatalf("final state = %+v", state)
	}
}

func TestFileStoreDropsBadChecksumFinalRecord(t *testing.T) {
	dir, path := newWALWithOneEntry(t)
	frame, err := encodeWalRecord(walRecord{
		Type:    "append",
		Entries: []model.Entry{{Term: 2, Index: 2, Type: model.CommandPut, Key: "b", Value: []byte("2")}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frame[8] ^= 0xff // corrupt the CRC, keep framing intact
	appendRaw(t, path, frame)

	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen with bad checksum: %v", err)
	}
	defer fs.Close()
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Entries) != 1 {
		t.Fatalf("entries after bad checksum = %d, want 1", len(state.Entries))
	}
	if err := fs.Append(model.Entry{Term: 2, Index: 2, Type: model.CommandPut, Key: "b", Value: []byte("2")}); err != nil {
		t.Fatalf("append after dropping corrupt record: %v", err)
	}
}
