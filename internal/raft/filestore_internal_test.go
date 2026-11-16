package raft

import (
	"encoding/binary"
	"encoding/json"
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

func TestFileStoreAppendBatchWritesOneRecord(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer fs.Close()
	if err := fs.SaveMeta(2, "n1"); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	entries := []model.Entry{
		{Term: 2, Index: 1, Type: model.CommandPut, Key: "a", Value: []byte("1")},
		{Term: 2, Index: 2, Type: model.CommandPut, Key: "b", Value: []byte("2")},
		{Term: 2, Index: 3, Type: model.CommandPut, Key: "c", Value: []byte("3")},
	}
	if err := fs.AppendBatch(entries); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	state, err := fs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Entries) != len(entries) {
		t.Fatalf("in-memory entries = %d, want %d", len(state.Entries), len(entries))
	}

	// The whole batch must be one WAL record, which is what makes a single
	// write + fsync sufficient. Replay the framing to prove it.
	data, err := os.ReadFile(filepath.Join(dir, walFileName))
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	var types []string
	var appended []model.Entry
	for off := 0; off < len(data); {
		if len(data)-off < walFrameHdr {
			t.Fatalf("torn WAL header at offset %d", off)
		}
		if binary.BigEndian.Uint32(data[off:off+4]) != walMagic {
			t.Fatalf("bad WAL magic at offset %d", off)
		}
		plen := int(binary.BigEndian.Uint32(data[off+4 : off+8]))
		end := off + walFrameHdr + plen
		if end > len(data) {
			t.Fatalf("WAL record at offset %d overruns file", off)
		}
		var rec walRecord
		if err := json.Unmarshal(data[off+walFrameHdr:end], &rec); err != nil {
			t.Fatalf("decode WAL record at offset %d: %v", off, err)
		}
		types = append(types, rec.Type)
		if rec.Type == "append" {
			appended = rec.Entries
		}
		off = end
	}
	if len(types) != 2 || types[0] != "meta" || types[1] != "append" {
		t.Fatalf("WAL record types = %v, want [meta append] (one record per batch)", types)
	}
	if len(appended) != len(entries) {
		t.Fatalf("append record has %d entries, want %d", len(appended), len(entries))
	}

	// A non-contiguous batch must be rejected atomically.
	bad := []model.Entry{{Term: 2, Index: 5, Type: model.CommandPut, Key: "gap", Value: []byte("x")}}
	if err := fs.AppendBatch(bad); err == nil {
		t.Fatal("AppendBatch accepted a non-contiguous index")
	}
	state, err = fs.Load()
	if err != nil {
		t.Fatalf("Load after rejected batch: %v", err)
	}
	if len(state.Entries) != len(entries) {
		t.Fatalf("rejected batch changed the log: %d entries, want %d", len(state.Entries), len(entries))
	}
}
