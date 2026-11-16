package raft_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// ---------------------------------------------------------------------------
// A streaming state machine: it extends snapshotSM with the optional
// SnapshotStreamer / SnapshotStreamRestorer capabilities so the streaming
// paths can be exercised without materialising the payload inside Raft.
// ---------------------------------------------------------------------------

type streamSnapshotSM struct {
	*snapshotSM
}

func newStreamSnapshotSM() *streamSnapshotSM {
	return &streamSnapshotSM{snapshotSM: newSnapshotSM()}
}

func (s *streamSnapshotSM) StreamSnapshot(w io.Writer) (uint64, error) {
	s.mu.Lock()
	payload := snapshotPayload{
		Index:  s.lastIndex,
		Values: make(map[string]string, len(s.values)),
	}
	for k, v := range s.values {
		payload.Values[k] = string(v)
	}
	index := s.lastIndex
	s.mu.Unlock()
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return 0, err
	}
	return index, nil
}

func (s *streamSnapshotSM) RestoreSnapshotStream(r io.Reader) error {
	var payload snapshotPayload
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return fmt.Errorf("decode snapshot stream: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = make(map[string][]byte, len(payload.Values))
	for k, v := range payload.Values {
		s.values[k] = []byte(v)
	}
	s.byIndex = make(map[uint64]model.Entry)
	s.lastIndex = payload.Index
	return nil
}

// failingReader yields its data and then returns err forever. It is used to
// simulate a state-machine or transport stream that fails halfway through.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// trackingReader records whether it was consumed at all.
type trackingReader struct {
	reads int
}

func (r *trackingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func assertNoSnapshotTemps(t *testing.T, dir string) {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dir, "snapshot"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read snapshot directory: %v", err)
	}
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Fatalf("leftover temporary snapshot file %q", de.Name())
		}
	}
}

// snapshotStreamFixture persists a payload at index/term through a source
// FileStore and returns the resulting metadata plus the exact payload bytes.
func snapshotStreamFixture(t *testing.T, index, term uint64) (raft.SnapshotMeta, []byte) {
	t.Helper()
	data, err := json.Marshal(snapshotPayload{
		Index:  index,
		Values: map[string]string{"a": "1", "b": "2"},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore fixture: %v", err)
	}
	defer store.Close()
	if err := store.SaveSnapshotFromReader(index, term, bytes.NewReader(data)); err != nil {
		t.Fatalf("SaveSnapshotFromReader fixture: %v", err)
	}
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot fixture: %v", err)
	}
	if rc == nil {
		t.Fatal("OpenSnapshot fixture returned no reader")
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read fixture snapshot: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fixture snapshot payload changed")
	}
	return meta, data
}

// ---------------------------------------------------------------------------
// FileStore streaming primitives
// ---------------------------------------------------------------------------

func TestFileStoreSnapshotStreamRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot empty: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatal("OpenSnapshot on an empty store returned a reader")
	}
	if meta != (raft.SnapshotMeta{}) {
		t.Fatalf("empty snapshot meta = %+v, want zero", meta)
	}

	payload := bytes.Repeat([]byte("streamed-snapshot-"), 4096)
	if err := store.SaveSnapshotFromReader(7, 3, bytes.NewReader(payload)); err != nil {
		t.Fatalf("SaveSnapshotFromReader: %v", err)
	}

	meta, rc, err = store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	if rc == nil {
		t.Fatal("OpenSnapshot returned no reader")
	}
	if meta.Index != 7 || meta.Term != 3 {
		t.Fatalf("meta position = {%d %d}, want {7 3}", meta.Index, meta.Term)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("meta size = %d, want %d", meta.Size, len(payload))
	}
	if meta.CRC32 != crc32.ChecksumIEEE(payload) {
		t.Fatalf("meta CRC32 = %08x, want %08x", meta.CRC32, crc32.ChecksumIEEE(payload))
	}
	if meta.File == "" {
		t.Fatal("meta file locator is empty")
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read streamed snapshot: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close streamed snapshot: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed payload differs (%d bytes vs %d)", len(got), len(payload))
	}

	// LoadSnapshot must read through the same file and agree.
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != 7 || stored.Term != 3 || !bytes.Equal(stored.Data, payload) {
		t.Fatalf("LoadSnapshot = {%d %d %d bytes}", stored.Index, stored.Term, len(stored.Data))
	}
	assertNoSnapshotTemps(t, dir)

	// A newer snapshot replaces the data file and removes the superseded one.
	newer := []byte("newer")
	if err := store.SaveSnapshotFromReader(9, 4, bytes.NewReader(newer)); err != nil {
		t.Fatalf("SaveSnapshotFromReader newer: %v", err)
	}
	meta, rc, err = store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot newer: %v", err)
	}
	if meta.Index != 9 || meta.Term != 4 || meta.Size != int64(len(newer)) {
		t.Fatalf("newer meta = %+v", meta)
	}
	got, err = io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, newer) {
		t.Fatalf("newer payload = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot", "data-7.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded data file still exists: %v", err)
	}
	assertNoSnapshotTemps(t, dir)

	// An older-or-equal snapshot is a no-op and must not consume the reader.
	untouched := &trackingReader{}
	if err := store.SaveSnapshotFromReader(9, 5, untouched); err != nil {
		t.Fatalf("SaveSnapshotFromReader stale: %v", err)
	}
	if untouched.reads != 0 {
		t.Fatalf("stale snapshot consumed the reader %d times", untouched.reads)
	}
	stored, err = store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after stale save: %v", err)
	}
	if stored.Index != 9 || string(stored.Data) != "newer" {
		t.Fatalf("stale save changed the snapshot: %+v", stored)
	}
}

func TestFileStoreSaveSnapshotFromReaderFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	oldData := []byte("old-payload")
	if err := store.SaveSnapshot(raft.Snapshot{Index: 3, Term: 2, Data: oldData}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	bad := &failingReader{data: bytes.Repeat([]byte("x"), 1024), err: errors.New("boom")}
	if err := store.SaveSnapshotFromReader(4, 2, bad); err == nil {
		t.Fatal("SaveSnapshotFromReader accepted a failing reader")
	}

	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after failure: %v", err)
	}
	if stored.Index != 3 || stored.Term != 2 || !bytes.Equal(stored.Data, oldData) {
		t.Fatalf("failed save changed the snapshot: {%d %d %q}", stored.Index, stored.Term, stored.Data)
	}
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot after failure: %v", err)
	}
	if meta.Index != 3 {
		t.Fatalf("meta after failure = %+v", meta)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, oldData) {
		t.Fatalf("payload after failure = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot", "data-4.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed save left data-4.bin: %v", err)
	}
	assertNoSnapshotTemps(t, dir)
}

// ---------------------------------------------------------------------------
// Node streaming snapshot creation
// ---------------------------------------------------------------------------

func TestSnapshotToStoreStreamsAndPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	sm := newStreamSnapshotSM()
	cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(node.Stop)
	eventually(t, testWait, "single node becomes leader", node.IsLeader)

	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}} {
		if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: kv[0], Value: []byte(kv[1])}, testWait); err != nil {
			t.Fatalf("Propose %s: %v", kv[0], err)
		}
	}
	before := node.Status()
	meta, err := node.SnapshotToStore()
	if err != nil {
		t.Fatalf("SnapshotToStore: %v", err)
	}
	if meta.Index < 3 || meta.Index != before.LastApplied {
		t.Fatalf("streamed snapshot index %d, want %d (>= 3)", meta.Index, before.LastApplied)
	}
	if meta.Term == 0 {
		t.Fatal("streamed snapshot term is 0")
	}
	if meta.Size == 0 || meta.CRC32 == 0 || meta.File == "" {
		t.Fatalf("streamed meta incomplete: %+v", meta)
	}
	st := node.Status()
	if st.LastIndex != meta.Index {
		t.Fatalf("last index after streamed snapshot = %d, want %d", st.LastIndex, meta.Index)
	}
	if st.LastApplied != meta.Index {
		t.Fatalf("last applied after streamed snapshot = %d, want %d", st.LastApplied, meta.Index)
	}

	// The store can reopen the payload and it matches what the state machine
	// held at snapshot time.
	gotMeta, rc, err := store.OpenSnapshot()
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	if gotMeta != meta {
		t.Fatalf("stored meta = %+v, want %+v", gotMeta, meta)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read stored snapshot: %v", err)
	}
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode stored snapshot: %v", err)
	}
	if payload.Index != meta.Index || payload.Values["a"] != "1" || payload.Values["b"] != "2" {
		t.Fatalf("stored payload = %+v", payload)
	}
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != meta.Index || stored.Term != meta.Term || !bytes.Equal(stored.Data, data) {
		t.Fatalf("LoadSnapshot = {%d %d %d bytes}", stored.Index, stored.Term, len(stored.Data))
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertEntriesFrom(t, state.Entries, meta.Index+1)

	// The legacy []byte API still works for a state machine that also
	// implements Snapshotter: it must return real data for the current index.
	snap, err := node.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after streaming: %v", err)
	}
	if snap.Index != meta.Index || len(snap.Data) == 0 {
		t.Fatalf("Snapshot after streaming = {%d %d bytes}, want index %d with data", snap.Index, len(snap.Data), meta.Index)
	}
	if err := json.Unmarshal(snap.Data, &payload); err != nil || payload.Index != meta.Index {
		t.Fatalf("Snapshot payload = %+v, %v", payload, err)
	}

	// A second call with no new applied state is a no-op.
	again, err := node.SnapshotToStore()
	if err != nil {
		t.Fatalf("second SnapshotToStore: %v", err)
	}
	if again.Index != meta.Index || again.Size != meta.Size || again.CRC32 != meta.CRC32 {
		t.Fatalf("second SnapshotToStore meta = %+v, want %+v", again, meta)
	}
}

func TestSnapshotToStoreFailureCleansScratch(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	net := newSnapshotNetwork()
	sm := &failingStreamSM{snapshotSM: newSnapshotSM()}
	cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(node.Stop)
	eventually(t, testWait, "single node becomes leader", node.IsLeader)
	if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "k", Value: []byte("v")}, testWait); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if _, err := node.SnapshotToStore(); err == nil {
		t.Fatal("SnapshotToStore accepted a failing stream")
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), "raft-snapshot-") && strings.HasSuffix(de.Name(), ".tmp") {
			t.Fatalf("leftover scratch file %q", de.Name())
		}
	}
	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != 0 {
		t.Fatalf("failed SnapshotToStore persisted snapshot %d", stored.Index)
	}
}

type failingStreamSM struct {
	*snapshotSM
}

func (s *failingStreamSM) StreamSnapshot(w io.Writer) (uint64, error) {
	_, _ = w.Write([]byte("partial snapshot data"))
	return 0, errors.New("stream failed")
}

// ---------------------------------------------------------------------------
// Node streaming snapshot installation
// ---------------------------------------------------------------------------

func TestRestoreSnapshotStreamIntoNewNode(t *testing.T) {
	meta, data := snapshotStreamFixture(t, 5, 3)

	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	sm := newStreamSnapshotSM()
	cfg := snapshotTestConfig("n2", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, newSnapshotNetwork().transport("n2"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}

	if err := node.RestoreSnapshotStream(meta, bytes.NewReader(data)); err != nil {
		t.Fatalf("RestoreSnapshotStream: %v", err)
	}
	st := node.Status()
	if st.CommitIndex != meta.Index || st.LastApplied != meta.Index || st.LastIndex != meta.Index {
		t.Fatalf("status after restore = commit %d applied %d last %d, want %d", st.CommitIndex, st.LastApplied, st.LastIndex, meta.Index)
	}
	if got, ok := sm.get("a"); !ok || string(got) != "1" {
		t.Fatalf("restored a = %q, %v", got, ok)
	}
	if got, ok := sm.get("b"); !ok || string(got) != "2" {
		t.Fatalf("restored b = %q, %v", got, ok)
	}
	if sm.appliedIndex() != meta.Index {
		t.Fatalf("state machine index = %d, want %d", sm.appliedIndex(), meta.Index)
	}

	stored, err := store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != meta.Index || stored.Term != meta.Term || !bytes.Equal(stored.Data, data) {
		t.Fatalf("persisted snapshot = {%d %d %d bytes}, want {%d %d}", stored.Index, stored.Term, len(stored.Data), meta.Index, meta.Term)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertEntriesFrom(t, state.Entries, meta.Index+1)
	if len(state.Entries) != 0 {
		t.Fatalf("restored durable entries = %+v, want none", state.Entries)
	}

	// Idempotent: the same snapshot is ignored without touching the stream.
	untouched := &trackingReader{}
	if err := node.RestoreSnapshotStream(meta, untouched); err != nil {
		t.Fatalf("idempotent RestoreSnapshotStream: %v", err)
	}
	if untouched.reads != 0 {
		t.Fatalf("idempotent restore consumed the reader %d times", untouched.reads)
	}
}

func TestRestoreSnapshotStreamSuffixRules(t *testing.T) {
	newNode := func(t *testing.T) (*raft.Node, *raft.FileStore, *streamSnapshotSM) {
		t.Helper()
		dir := t.TempDir()
		store, err := raft.NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		entries := []model.Entry{
			testEntry(1, 1, "a", "1"),
			testEntry(2, 1, "b", "2"),
			testEntry(3, 1, "c", "3"),
			testEntry(4, 1, "d", "4"),
		}
		if err := store.Append(entries...); err != nil {
			t.Fatalf("Append: %v", err)
		}
		sm := newStreamSnapshotSM()
		cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
		node, err := raft.New(cfg, store, sm, newSnapshotNetwork().transport("n1"))
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		return node, store, sm
	}

	t.Run("matching term keeps suffix", func(t *testing.T) {
		node, store, _ := newNode(t)
		meta, data := snapshotStreamFixture(t, 2, 1)
		if err := node.RestoreSnapshotStream(meta, bytes.NewReader(data)); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		st := node.Status()
		if st.LastApplied != 2 || st.CommitIndex != 2 || st.LastIndex != 4 {
			t.Fatalf("status = commit %d applied %d last %d, want 2/2/4", st.CommitIndex, st.LastApplied, st.LastIndex)
		}
		state, err := store.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(state.Entries) != 2 || state.Entries[0].Index != 3 || state.Entries[1].Index != 4 {
			t.Fatalf("durable suffix = %+v, want entries 3,4", state.Entries)
		}
	})

	t.Run("different term drops suffix", func(t *testing.T) {
		node, store, _ := newNode(t)
		meta, data := snapshotStreamFixture(t, 2, 9)
		if err := node.RestoreSnapshotStream(meta, bytes.NewReader(data)); err != nil {
			t.Fatalf("RestoreSnapshotStream: %v", err)
		}
		st := node.Status()
		if st.LastApplied != 2 || st.CommitIndex != 2 || st.LastIndex != 2 {
			t.Fatalf("status = commit %d applied %d last %d, want 2/2/2", st.CommitIndex, st.LastApplied, st.LastIndex)
		}
		state, err := store.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(state.Entries) != 0 {
			t.Fatalf("divergent suffix survived restore: %+v", state.Entries)
		}
	})
}

func TestRestoreSnapshotStreamRejectsCorruptPayload(t *testing.T) {
	meta, data := snapshotStreamFixture(t, 5, 3)
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	sm := newStreamSnapshotSM()
	cfg := snapshotTestConfig("n2", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, newSnapshotNetwork().transport("n2"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}

	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 0xff
	if err := node.RestoreSnapshotStream(meta, bytes.NewReader(corrupt)); err == nil {
		t.Fatal("RestoreSnapshotStream accepted a corrupt payload")
	}
	if stored, err := store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("corrupt restore persisted a snapshot: %+v, %v", stored, err)
	}
	if sm.appliedIndex() != 0 {
		t.Fatalf("corrupt restore changed the state machine index to %d", sm.appliedIndex())
	}
	if st := node.Status(); st.CommitIndex != 0 || st.LastApplied != 0 {
		t.Fatalf("corrupt restore changed status: commit %d applied %d", st.CommitIndex, st.LastApplied)
	}

	// A declared size that does not match the stream is rejected too.
	badSize := meta
	badSize.Size++
	if err := node.RestoreSnapshotStream(badSize, bytes.NewReader(data)); err == nil {
		t.Fatal("RestoreSnapshotStream accepted a size mismatch")
	}
}

func TestSnapshotStreamUnsupported(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()
	// snapshotSM implements Snapshotter but not SnapshotStreamer.
	node, err := raft.New(snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), store, newSnapshotSM(), newSnapshotNetwork().transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	if _, err := node.SnapshotToStore(); !errors.Is(err, raft.ErrSnapshotStreamUnsupported) {
		t.Fatalf("SnapshotToStore = %v, want ErrSnapshotStreamUnsupported", err)
	}

	// Streaming SM but a store that hides OpenSnapshot/SaveSnapshotFromReader.
	legacyDir := t.TempDir()
	legacyFS, err := raft.NewFileStore(legacyDir)
	if err != nil {
		t.Fatalf("NewFileStore legacy: %v", err)
	}
	defer legacyFS.Close()
	legacyNode, err := raft.New(snapshotTestConfig("n2", nil, legacyDir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond), legacyStore{LogStore: legacyFS}, newStreamSnapshotSM(), newSnapshotNetwork().transport("n2"))
	if err != nil {
		t.Fatalf("raft.New legacy: %v", err)
	}
	if _, err := legacyNode.SnapshotToStore(); !errors.Is(err, raft.ErrSnapshotStreamUnsupported) {
		t.Fatalf("SnapshotToStore legacy = %v, want ErrSnapshotStreamUnsupported", err)
	}
	if err := legacyNode.RestoreSnapshotStream(raft.SnapshotMeta{Index: 1, Term: 1}, bytes.NewReader(nil)); !errors.Is(err, raft.ErrSnapshotStreamUnsupported) {
		t.Fatalf("RestoreSnapshotStream legacy = %v, want ErrSnapshotStreamUnsupported", err)
	}
}

// countingStreamSM counts which restore capability New chose.
type countingStreamSM struct {
	*streamSnapshotSM
	streamRestores int
	byteRestores   int
}

func (s *countingStreamSM) RestoreSnapshotStream(r io.Reader) error {
	s.streamRestores++
	return s.streamSnapshotSM.RestoreSnapshotStream(r)
}

func (s *countingStreamSM) RestoreSnapshot(data []byte) error {
	s.byteRestores++
	return s.streamSnapshotSM.RestoreSnapshot(data)
}

func TestRestartRestoresSnapshotFromStream(t *testing.T) {
	dir := t.TempDir()
	store, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	net := newSnapshotNetwork()
	sm := newStreamSnapshotSM()
	cfg := snapshotTestConfig("n1", nil, dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, sm, net.transport("n1"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.register("n1", node)
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, testWait, "single node becomes leader", node.IsLeader)
	if _, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "a", Value: []byte("1")}, testWait); err != nil {
		t.Fatalf("Propose a: %v", err)
	}
	meta, err := node.SnapshotToStore()
	if err != nil {
		t.Fatalf("SnapshotToStore: %v", err)
	}
	afterIndex, err := propose(t, node, model.Entry{Type: model.CommandPut, Key: "b", Value: []byte("2")}, testWait)
	if err != nil {
		t.Fatalf("Propose b: %v", err)
	}
	node.Stop()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := raft.NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	restoredSM := &countingStreamSM{streamSnapshotSM: newStreamSnapshotSM()}
	restarted, err := raft.New(cfg, reopened, restoredSM, net.transport("n1"))
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	net.register("n1", restarted)
	t.Cleanup(restarted.Stop)

	if restoredSM.streamRestores != 1 || restoredSM.byteRestores != 0 {
		t.Fatalf("restore calls = stream %d, bytes %d; want 1, 0", restoredSM.streamRestores, restoredSM.byteRestores)
	}
	if got, ok := restoredSM.get("a"); !ok || string(got) != "1" {
		t.Fatalf("snapshot value a = %q, %v", got, ok)
	}
	if st := restarted.Status(); st.LastApplied < meta.Index {
		t.Fatalf("restarted last applied %d < snapshot index %d", st.LastApplied, meta.Index)
	}
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	eventually(t, testWait, "restarted node replays suffix", func() bool {
		got, ok := restoredSM.get("b")
		return ok && string(got) == "2"
	})
	if st := restarted.Status(); st.LastApplied < afterIndex {
		t.Fatalf("restarted last applied %d < %d", st.LastApplied, afterIndex)
	}
}
