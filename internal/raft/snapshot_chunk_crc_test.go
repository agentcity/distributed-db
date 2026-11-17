package raft_test

import (
	"bytes"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"distributed-db/internal/raft"
)

// setChunkCRCs makes manually built chunk requests look like they came from a
// current sender: every data chunk carries its IEEE CRC-32 and the empty Done
// marker carries zero.
func setChunkCRCs(reqs []*raft.InstallSnapshotChunkRequest) {
	for _, req := range reqs {
		if len(req.Data) == 0 {
			req.ChunkCRC32 = 0
			continue
		}
		req.ChunkCRC32 = crc32.ChecksumIEEE(req.Data)
	}
}

// snapshotRecvTempBytes returns the bytes already staged in the single
// temporary upload file of dir.
func snapshotRecvTempBytes(t *testing.T, dir string) []byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "raft-snapshot-recv-*"))
	if err != nil {
		t.Fatalf("glob staged upload: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("staged upload files = %v, want exactly one", paths)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read staged upload: %v", err)
	}
	return data
}

func TestInstallSnapshotChunkCRCRejectsCorruptedChunkBeforeWrite(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	setChunkCRCs(reqs)
	if len(reqs) < 4 {
		t.Fatalf("fixture produced %d requests, need several", len(reqs))
	}

	// Stage the first chunk, then corrupt the bytes of the next chunk while
	// keeping the checksum the sender computed over the original bytes.
	requireChunkSuccess(t, f.node, reqs[0])
	staged := snapshotRecvTempBytes(t, f.dir)
	if !bytes.Equal(staged, reqs[0].Data) {
		t.Fatalf("staged bytes = %q, want first chunk %q", staged, reqs[0].Data)
	}
	corrupt := *reqs[1]
	corrupt.Data = append([]byte(nil), reqs[1].Data...)
	corrupt.Data[0] ^= 0xff

	resp := sendChunk(t, f.node, &corrupt)
	if resp.Success {
		t.Fatal("chunk with a corrupted payload was accepted")
	}
	if !strings.Contains(resp.Error, "chunk CRC32") {
		t.Fatalf("rejection error = %q, want a chunk CRC32 mismatch", resp.Error)
	}
	if !strings.Contains(resp.Error, "offset") {
		t.Fatalf("rejection error = %q, want the rejected offset", resp.Error)
	}
	// The response points at the bytes already staged so a leader can resume
	// the upload instead of restarting it.
	if want := int64(len(staged)); resp.NextOffset != want {
		t.Fatalf("rejection NextOffset = %d, want staged offset %d", resp.NextOffset, want)
	}
	// The bad bytes were rejected before the write: the staged file is
	// unchanged and still holds only the first chunk.
	if got := snapshotRecvTempBytes(t, f.dir); !bytes.Equal(got, staged) {
		t.Fatalf("staged bytes changed after a bad chunk: %q, want %q", got, staged)
	}
	if st := f.node.Status(); st.LastApplied != 0 {
		t.Fatalf("rejected chunk advanced last applied to %d", st.LastApplied)
	}
	if stored, err := f.store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("rejected chunk persisted snapshot %+v, %v", stored, err)
	}

	// The partial upload survived, so the correct chunk at the same offset and
	// the rest of the stream finish the install.
	for _, req := range reqs[1:] {
		requireChunkSuccess(t, f.node, req)
	}
	if st := f.node.Status(); st.LastApplied != f.meta.Index {
		t.Fatalf("last applied after resume = %d, want %d", st.LastApplied, f.meta.Index)
	}
	stored, err := f.store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != f.meta.Index || !bytes.Equal(stored.Data, f.data) {
		t.Fatalf("stored snapshot = {%d, %d bytes}, want {%d, %d bytes}",
			stored.Index, len(stored.Data), f.meta.Index, len(f.data))
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkWrongChunkCRCRejected(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	setChunkCRCs(reqs)

	bad := *reqs[0]
	bad.ChunkCRC32 ^= 0xffffffff
	if bad.ChunkCRC32 == 0 {
		bad.ChunkCRC32 = 1
	}
	resp := sendChunk(t, f.node, &bad)
	if resp.Success {
		t.Fatal("chunk with a wrong chunk CRC32 was accepted")
	}
	if !strings.Contains(resp.Error, "chunk CRC32") {
		t.Fatalf("rejection error = %q, want a chunk CRC32 mismatch", resp.Error)
	}
	// Rejection happened before the first chunk created any staged state.
	if hasSnapshotRecvTemp(t, f.dir) {
		t.Fatal("rejected first chunk created a staged upload file")
	}
	if st := f.node.Status(); st.LastApplied != 0 {
		t.Fatalf("rejected chunk advanced last applied to %d", st.LastApplied)
	}

	// The same stream with correct chunk checksums installs normally.
	for _, req := range reqs {
		requireChunkSuccess(t, f.node, req)
	}
	if st := f.node.Status(); st.LastApplied != f.meta.Index {
		t.Fatalf("last applied = %d, want %d", st.LastApplied, f.meta.Index)
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkZeroChunkCRCLegacySender(t *testing.T) {
	f := newChunkRecvFixture(t)
	// chunkRequests models the legacy sender: every chunk, including the
	// final empty Done marker, leaves chunk_crc32 unset.
	reqs := chunkRequests(3, f.meta, f.data, 7)
	for i, req := range reqs {
		if req.ChunkCRC32 != 0 {
			t.Fatalf("legacy chunk %d carries ChunkCRC32 %08x", i, req.ChunkCRC32)
		}
	}
	for _, req := range reqs {
		requireChunkSuccess(t, f.node, req)
	}
	if st := f.node.Status(); st.LastApplied != f.meta.Index {
		t.Fatalf("last applied = %d, want %d", st.LastApplied, f.meta.Index)
	}
	stored, err := f.store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != f.meta.Index || !bytes.Equal(stored.Data, f.data) {
		t.Fatalf("stored snapshot = {%d, %d bytes}, want {%d, %d bytes}",
			stored.Index, len(stored.Data), f.meta.Index, len(f.data))
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkTotalCRCStillEnforcedWithChunkCRCs(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	setChunkCRCs(reqs)

	// Corrupt a payload byte and recompute the per-chunk checksum over the
	// corrupted bytes: the early check cannot detect this, so only the total
	// snapshot CRC32 at Done can reject the upload.
	bad := *reqs[0]
	bad.Data = append([]byte(nil), reqs[0].Data...)
	bad.Data[0] ^= 0xff
	bad.ChunkCRC32 = crc32.ChecksumIEEE(bad.Data)
	requireChunkSuccess(t, f.node, &bad)
	for _, req := range reqs[1 : len(reqs)-1] {
		requireChunkSuccess(t, f.node, req)
	}

	resp := sendChunk(t, f.node, reqs[len(reqs)-1])
	if resp.Success {
		t.Fatal("snapshot with a bad total CRC32 was accepted")
	}
	if !strings.Contains(resp.Error, "snapshot CRC32") {
		t.Fatalf("rejection error = %q, want a total CRC32 mismatch", resp.Error)
	}
	if st := f.node.Status(); st.LastApplied != 0 {
		t.Fatalf("corrupt upload advanced last applied to %d", st.LastApplied)
	}
	if stored, err := f.store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("corrupt upload persisted snapshot %+v, %v", stored, err)
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}
