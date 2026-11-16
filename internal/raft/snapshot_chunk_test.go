package raft_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// chunkSnapshotTransport augments the legacy in-memory transport with the
// chunked snapshot capability and records the chunks the leader actually sent.
type chunkSnapshotTransport struct {
	*snapshotTransport

	mu     sync.Mutex
	chunks map[string][]raft.InstallSnapshotChunkRequest
	legacy map[string]int
}

func newChunkSnapshotTransport(net *snapshotNetwork, from string) *chunkSnapshotTransport {
	return &chunkSnapshotTransport{
		snapshotTransport: &snapshotTransport{net: net, from: from},
		chunks:            make(map[string][]raft.InstallSnapshotChunkRequest),
		legacy:            make(map[string]int),
	}
}

func (t *chunkSnapshotTransport) InstallSnapshot(ctx context.Context, peer string, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	t.mu.Lock()
	t.legacy[peer]++
	t.mu.Unlock()
	return t.snapshotTransport.InstallSnapshot(ctx, peer, req)
}

func (t *chunkSnapshotTransport) InstallSnapshotChunk(ctx context.Context, peer string, req *raft.InstallSnapshotChunkRequest) (*raft.InstallSnapshotChunkResponse, error) {
	cp := *req
	cp.Data = append([]byte(nil), req.Data...)
	t.mu.Lock()
	t.chunks[peer] = append(t.chunks[peer], cp)
	t.mu.Unlock()

	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleInstallSnapshotChunk(ctx, req)
}

func (t *chunkSnapshotTransport) chunkRequests(peer string) []raft.InstallSnapshotChunkRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]raft.InstallSnapshotChunkRequest(nil), t.chunks[peer]...)
}

func (t *chunkSnapshotTransport) legacyCalls(peer string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.legacy[peer]
}

func assertNoSnapshotRecvTemps(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		des, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read data dir: %v", err)
		}
		leftover := ""
		for _, de := range des {
			if strings.HasPrefix(de.Name(), "raft-snapshot-recv-") {
				leftover = de.Name()
				break
			}
		}
		if leftover == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leftover chunk upload file %q", leftover)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Leader-side chunk streaming
// ---------------------------------------------------------------------------

func TestInstallSnapshotChunkedTransfersChunks(t *testing.T) {
	c := newSnapshotChunkCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids)
	lagging := ""
	for _, id := range c.ids {
		if id != leaderID {
			lagging = id
			break
		}
	}
	if lagging == "" {
		t.Fatal("no lagging follower")
	}

	// Isolate one follower so it misses the writes and the compaction.
	c.net.partition(lagging, leaderID)
	c.net.partition(leaderID, lagging)

	// A payload larger than SnapshotChunkSize forces more than one data chunk.
	large := bytes.Repeat([]byte("x"), raft.SnapshotChunkSize+(128<<10))
	_, _, _ = c.proposeOnLeader(leader, model.Entry{Type: model.CommandPut, Key: "large", Value: large})
	_, _, _ = c.proposeOnLeader(leader, model.Entry{Type: model.CommandPut, Key: "small", Value: []byte("value")})
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	if int64(len(snap.Data)) <= raft.SnapshotChunkSize {
		t.Fatalf("snapshot payload %d bytes does not exceed one chunk", len(snap.Data))
	}

	c.net.heal(lagging)
	c.net.heal(leaderID)

	eventually(t, c.timeout, "lagging follower restores the chunked snapshot", func() bool {
		stored, err := c.stores[lagging].LoadSnapshot()
		if err != nil {
			return false
		}
		return stored.Index >= snap.Index
	})
	c.requireValue(c.ids, "small", "value")

	tr := c.chunkTrs[leaderID]
	if tr == nil {
		t.Fatal("leader has no chunk transport")
	}
	reqs := tr.chunkRequests(lagging)
	if len(reqs) < 3 {
		t.Fatalf("chunk requests = %d, want at least 3 (two data chunks + done)", len(reqs))
	}
	var offset uint64
	for i, req := range reqs {
		if req.Term == 0 || req.LeaderID != leaderID {
			t.Fatalf("chunk %d has bad term/leader: %+v", i, req)
		}
		if req.Index != snap.Index || req.SnapshotTerm != snap.Term {
			t.Fatalf("chunk %d position = {%d,%d}, want {%d,%d}", i, req.Index, req.SnapshotTerm, snap.Index, snap.Term)
		}
		if req.Size != int64(len(snap.Data)) || req.CRC32 == 0 {
			t.Fatalf("chunk %d metadata = size %d crc %08x, want size %d", i, req.Size, req.CRC32, len(snap.Data))
		}
		if req.Offset != offset {
			t.Fatalf("chunk %d offset = %d, want %d", i, req.Offset, offset)
		}
		if len(req.Data) > raft.SnapshotChunkSize {
			t.Fatalf("chunk %d payload = %d bytes, want <= %d", i, len(req.Data), raft.SnapshotChunkSize)
		}
		if req.Done {
			if i != len(reqs)-1 {
				t.Fatalf("done chunk at position %d of %d", i, len(reqs))
			}
			if len(req.Data) != 0 {
				t.Fatalf("final done chunk carries %d bytes", len(req.Data))
			}
		}
		offset += uint64(len(req.Data))
	}
	if !reqs[len(reqs)-1].Done {
		t.Fatal("last chunk is not marked done")
	}
	if offset != uint64(len(snap.Data)) {
		t.Fatalf("chunks carry %d bytes, want %d", offset, len(snap.Data))
	}
	if got := tr.legacyCalls(lagging); got != 0 {
		t.Fatalf("leader used the legacy install %d times instead of chunks", got)
	}
	if got, ok := c.sms[lagging].get("large"); !ok || !bytes.Equal(got, large) {
		t.Fatalf("lagging follower did not restore the large value (%d bytes, ok=%v)", len(got), ok)
	}
	assertNoSnapshotRecvTemps(t, c.dirs[lagging])
}

func TestInstallSnapshotLegacyFallbackWithoutChunkTransport(t *testing.T) {
	// newSnapshotCluster transports implement only SnapshotTransport; the
	// leader must materialise the payload and use the legacy unary RPC.
	c := newSnapshotCluster(t, 3)
	leader, leaderID := c.waitLeader(c.ids)
	lagging := ""
	for _, id := range c.ids {
		if id != leaderID {
			lagging = id
			break
		}
	}
	c.net.partition(lagging, leaderID)
	c.net.partition(leaderID, lagging)

	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("legacy%d", i)
		c.proposeOnLeader(leader, model.Entry{Type: model.CommandPut, Key: key, Value: []byte("v")})
	}
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	c.net.heal(lagging)
	c.net.heal(leaderID)

	eventually(t, c.timeout, "follower catches up via legacy InstallSnapshot", func() bool {
		stored, err := c.stores[lagging].LoadSnapshot()
		return err == nil && stored.Index >= snap.Index
	})
	if got := c.net.countInstall(leaderID, lagging); got == 0 {
		t.Fatal("no legacy InstallSnapshot was sent")
	}
	c.requireValue(c.ids, "legacy2", "v")
	if len(c.chunkTrs) != 0 {
		t.Fatal("legacy cluster unexpectedly has chunk transports")
	}
}

// ---------------------------------------------------------------------------
// Receiver validation
// ---------------------------------------------------------------------------

type chunkRecvFixture struct {
	node  *raft.Node
	store *raft.FileStore
	sm    *streamSnapshotSM
	dir   string
	meta  raft.SnapshotMeta
	data  []byte
}

func newChunkRecvFixture(t *testing.T) *chunkRecvFixture {
	t.Helper()
	f := &chunkRecvFixture{}
	f.meta, f.data = snapshotStreamFixture(t, 5, 3)
	f.dir = t.TempDir()
	store, err := raft.NewFileStore(f.dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	f.store = store
	f.sm = newStreamSnapshotSM()
	cfg := snapshotTestConfig("n2", nil, f.dir, 40*time.Millisecond, 80*time.Millisecond, 10*time.Millisecond)
	node, err := raft.New(cfg, store, f.sm, newSnapshotNetwork().transport("n2"))
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	f.node = node
	t.Cleanup(func() {
		node.Stop()
		_ = store.Close()
	})
	return f
}

// chunkRequests splits data into offset-addressed requests and appends the
// final done marker.
func chunkRequests(term uint64, meta raft.SnapshotMeta, data []byte, chunkSize int) []*raft.InstallSnapshotChunkRequest {
	var reqs []*raft.InstallSnapshotChunkRequest
	var offset uint64
	for offset < uint64(len(data)) {
		end := offset + uint64(chunkSize)
		if end > uint64(len(data)) {
			end = uint64(len(data))
		}
		reqs = append(reqs, &raft.InstallSnapshotChunkRequest{
			Term:         term,
			LeaderID:     "leader",
			Index:        meta.Index,
			SnapshotTerm: meta.Term,
			Offset:       offset,
			Data:         append([]byte(nil), data[offset:end]...),
			Size:         int64(len(data)),
			CRC32:        meta.CRC32,
		})
		offset = end
	}
	reqs = append(reqs, &raft.InstallSnapshotChunkRequest{
		Term:         term,
		LeaderID:     "leader",
		Index:        meta.Index,
		SnapshotTerm: meta.Term,
		Offset:       uint64(len(data)),
		Done:         true,
		Size:         int64(len(data)),
		CRC32:        meta.CRC32,
	})
	return reqs
}

func sendChunk(t *testing.T, node *raft.Node, req *raft.InstallSnapshotChunkRequest) *raft.InstallSnapshotChunkResponse {
	t.Helper()
	resp, err := node.HandleInstallSnapshotChunk(context.Background(), req)
	if err != nil {
		t.Fatalf("HandleInstallSnapshotChunk(offset %d): %v", req.Offset, err)
	}
	return resp
}

func requireChunkSuccess(t *testing.T, node *raft.Node, req *raft.InstallSnapshotChunkRequest) *raft.InstallSnapshotChunkResponse {
	t.Helper()
	resp := sendChunk(t, node, req)
	if !resp.Success {
		t.Fatalf("chunk offset %d rejected: term %d error %q", req.Offset, resp.Term, resp.Error)
	}
	return resp
}

func TestInstallSnapshotChunkSuccess(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	if len(reqs) < 3 {
		t.Fatalf("fixture produced %d requests, want several", len(reqs))
	}
	for _, req := range reqs {
		resp := requireChunkSuccess(t, f.node, req)
		// next_offset reports the bytes accepted so far, i.e. the offset a
		// resumed upload should continue from.
		want := int64(req.Offset) + int64(len(req.Data))
		if resp.NextOffset != want {
			t.Fatalf("chunk offset %d next_offset = %d, want %d", req.Offset, resp.NextOffset, want)
		}
	}
	st := f.node.Status()
	if st.CommitIndex != f.meta.Index || st.LastApplied != f.meta.Index || st.LastIndex != f.meta.Index {
		t.Fatalf("status after chunk install = commit %d applied %d last %d, want %d",
			st.CommitIndex, st.LastApplied, st.LastIndex, f.meta.Index)
	}
	if got, ok := f.sm.get("a"); !ok || string(got) != "1" {
		t.Fatalf("restored a = %q, %v", got, ok)
	}
	stored, err := f.store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != f.meta.Index || stored.Term != f.meta.Term || !bytes.Equal(stored.Data, f.data) {
		t.Fatalf("stored snapshot = {%d %d %d bytes}, want {%d %d %d bytes}",
			stored.Index, stored.Term, len(stored.Data), f.meta.Index, f.meta.Term, len(f.data))
	}
	assertNoSnapshotRecvTemps(t, f.dir)

	// A retried final chunk after a lost response is idempotent.
	last := reqs[len(reqs)-1]
	resp := requireChunkSuccess(t, f.node, last)
	if resp.LastIndex != f.meta.Index {
		t.Fatalf("retried done response last index %d, want %d", resp.LastIndex, f.meta.Index)
	}
}

func TestInstallSnapshotChunkOffsetGap(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	if len(reqs) < 4 {
		t.Fatalf("fixture produced %d requests, need at least 4", len(reqs))
	}
	requireChunkSuccess(t, f.node, reqs[0])

	// Skip reqs[1]; its offset lies beyond the bytes received so far.
	resp := sendChunk(t, f.node, reqs[2])
	if resp.Success {
		t.Fatal("gapped chunk was accepted")
	}
	if !strings.Contains(resp.Error, "offset gap") {
		t.Fatalf("gap error = %q, want offset gap", resp.Error)
	}
	if st := f.node.Status(); st.LastApplied != 0 {
		t.Fatalf("gapped chunk advanced last applied to %d", st.LastApplied)
	}
	if stored, err := f.store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("gapped chunk persisted snapshot %+v, %v", stored, err)
	}

	// Restarting at offset zero after the rejection recovers cleanly.
	for _, req := range reqs {
		requireChunkSuccess(t, f.node, req)
	}
	if got := f.node.Status().LastApplied; got != f.meta.Index {
		t.Fatalf("last applied after restart = %d, want %d", got, f.meta.Index)
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkCRCMismatch(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	// Corrupt one payload byte but keep the declared checksum so the receiver
	// can only detect it at the end.
	reqs[0].Data[0] ^= 0xff
	for i, req := range reqs {
		resp := sendChunk(t, f.node, req)
		last := i == len(reqs)-1
		if last {
			if resp.Success {
				t.Fatal("snapshot with a bad CRC32 was accepted")
			}
			if !strings.Contains(resp.Error, "CRC32") {
				t.Fatalf("CRC error = %q, want CRC32 mismatch", resp.Error)
			}
			continue
		}
		if !resp.Success {
			t.Fatalf("chunk %d rejected before finalization: %q", i, resp.Error)
		}
	}
	if st := f.node.Status(); st.LastApplied != 0 {
		t.Fatalf("corrupt chunk install advanced last applied to %d", st.LastApplied)
	}
	if stored, err := f.store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("corrupt chunk install persisted snapshot %+v, %v", stored, err)
	}
	assertNoSnapshotRecvTemps(t, f.dir)

	// The failed upload was dropped, so a clean retry from offset zero works.
	for _, req := range chunkRequests(3, f.meta, f.data, 7) {
		requireChunkSuccess(t, f.node, req)
	}
}

func TestInstallSnapshotChunkDuplicate(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)

	requireChunkSuccess(t, f.node, reqs[0])
	// An immediate retry of the same chunk (lost response) is acknowledged
	// without appending its bytes twice.
	requireChunkSuccess(t, f.node, reqs[0])
	for _, req := range reqs[1:] {
		requireChunkSuccess(t, f.node, req)
	}
	stored, err := f.store.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != f.meta.Index || !bytes.Equal(stored.Data, f.data) {
		t.Fatalf("duplicate chunk corrupted the snapshot: index %d, %d bytes", stored.Index, len(stored.Data))
	}
	// The receiver accepted the duplicate without leaving a second upload.
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkMissingDone(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	for _, req := range reqs[:len(reqs)-1] {
		requireChunkSuccess(t, f.node, req)
	}
	// All payload bytes arrived but done was never sent: nothing is installed.
	if st := f.node.Status(); st.LastApplied != 0 || st.CommitIndex != 0 {
		t.Fatalf("incomplete upload installed at commit %d applied %d", st.CommitIndex, st.LastApplied)
	}
	if stored, err := f.store.LoadSnapshot(); err != nil || stored.Index != 0 {
		t.Fatalf("incomplete upload persisted snapshot %+v, %v", stored, err)
	}
	des, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	foundPartial := false
	for _, de := range des {
		if strings.HasPrefix(de.Name(), "raft-snapshot-recv-") {
			foundPartial = true
		}
	}
	if !foundPartial {
		t.Fatal("partial upload file is missing before done")
	}

	requireChunkSuccess(t, f.node, reqs[len(reqs)-1])
	if got := f.node.Status().LastApplied; got != f.meta.Index {
		t.Fatalf("last applied after late done = %d, want %d", got, f.meta.Index)
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkRejectsStaleTermAndBadChunks(t *testing.T) {
	f := newChunkRecvFixture(t)
	if _, err := f.node.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{Term: 5, CandidateID: "n2"}); err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	reqs := chunkRequests(4, f.meta, f.data, 7)
	resp := sendChunk(t, f.node, reqs[0])
	if resp.Success || resp.Term != 5 {
		t.Fatalf("stale-term response = %+v, want success false term 5", resp)
	}
	if _, err := f.node.HandleInstallSnapshotChunk(context.Background(), nil); err == nil {
		t.Fatal("nil chunk request must fail")
	}

	bad := *reqs[0]
	bad.Term = 5
	bad.Offset = 1
	resp = sendChunk(t, f.node, &bad)
	if resp.Success || !strings.Contains(resp.Error, "offset") {
		t.Fatalf("out-of-order first chunk response = %+v", resp)
	}

	empty := *reqs[0]
	empty.Term = 5
	empty.Data = nil
	resp = sendChunk(t, f.node, &empty)
	if resp.Success || !strings.Contains(resp.Error, "empty") {
		t.Fatalf("empty non-final chunk response = %+v", resp)
	}

	tooBig := *reqs[0]
	tooBig.Term = 5
	tooBig.Data = make([]byte, raft.SnapshotChunkSize+1)
	resp = sendChunk(t, f.node, &tooBig)
	if resp.Success || !strings.Contains(resp.Error, "exceeds") {
		t.Fatalf("oversized chunk response = %+v", resp)
	}
}
