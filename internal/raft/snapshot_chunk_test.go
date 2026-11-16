package raft_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/raft"
)

// chunkFault lets a test intercept one chunk RPC before it reaches the network.
// handled=false forwards the request normally; handled=true returns resp/err
// instead, after the callback has optionally delivered the request itself (to
// simulate a response that was lost after the follower accepted the chunk).
type chunkFault func(peer string, req *raft.InstallSnapshotChunkRequest) (handled bool, resp *raft.InstallSnapshotChunkResponse, err error)

// chunkSnapshotTransport augments the legacy in-memory transport with the
// chunked snapshot capability and records the chunks the leader actually sent.
type chunkSnapshotTransport struct {
	*snapshotTransport

	mu     sync.Mutex
	chunks map[string][]raft.InstallSnapshotChunkRequest
	legacy map[string]int
	fault  chunkFault
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
	fault := t.fault
	t.mu.Unlock()

	if fault != nil {
		if handled, resp, err := fault(peer, req); handled {
			return resp, err
		}
	}
	h, err := t.net.handler(t.from, peer)
	if err != nil {
		return nil, err
	}
	return h.HandleInstallSnapshotChunk(ctx, req)
}

// setFault installs (or removes) the per-chunk fault injector.
func (t *chunkSnapshotTransport) setFault(fault chunkFault) {
	t.mu.Lock()
	t.fault = fault
	t.mu.Unlock()
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

// hasSnapshotRecvTemp reports whether dir holds an in-progress chunk upload.
func hasSnapshotRecvTemp(t *testing.T, dir string) bool {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), "raft-snapshot-recv-") {
			return true
		}
	}
	return false
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

// ---------------------------------------------------------------------------
// Resume: receiver reports the accepted offset
// ---------------------------------------------------------------------------

func TestInstallSnapshotChunkResponsesCarryNextOffset(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	if len(reqs) < 5 {
		t.Fatalf("fixture produced %d requests, need at least 5", len(reqs))
	}

	// An accepted chunk reports the number of bytes accepted so far.
	resp := requireChunkSuccess(t, f.node, reqs[0])
	if want := reqs[0].Offset + uint64(len(reqs[0].Data)); resp.NextOffset != int64(want) {
		t.Fatalf("accepted chunk NextOffset = %d, want %d", resp.NextOffset, want)
	}
	resp = requireChunkSuccess(t, f.node, reqs[1])
	current := reqs[1].Offset + uint64(len(reqs[1].Data))
	if resp.NextOffset != int64(current) {
		t.Fatalf("accepted chunk NextOffset = %d, want %d", resp.NextOffset, current)
	}

	// An idempotent retry reports the same current offset.
	dup := requireChunkSuccess(t, f.node, reqs[1])
	if dup.NextOffset != int64(current) {
		t.Fatalf("duplicate chunk NextOffset = %d, want current offset %d", dup.NextOffset, current)
	}

	// A gap carries the current offset too: it is the resume point, not the
	// offset the rejected request asked for.
	gap := sendChunk(t, f.node, reqs[3])
	if gap.Success {
		t.Fatal("gapped chunk was accepted")
	}
	if !strings.Contains(gap.Error, "offset gap") {
		t.Fatalf("gap error = %q, want offset gap", gap.Error)
	}
	if gap.NextOffset != int64(current) {
		t.Fatalf("gap NextOffset = %d, want current offset %d", gap.NextOffset, current)
	}
	if !hasSnapshotRecvTemp(t, f.dir) {
		t.Fatal("gap rejection removed the partial upload file")
	}

	// The chunk at the reported offset is accepted and appended.
	resp = requireChunkSuccess(t, f.node, reqs[2])
	if want := reqs[2].Offset + uint64(len(reqs[2].Data)); resp.NextOffset != int64(want) {
		t.Fatalf("resumed chunk NextOffset = %d, want %d", resp.NextOffset, want)
	}
	for _, req := range reqs[3:] {
		resp = requireChunkSuccess(t, f.node, req)
	}
	if resp.NextOffset != int64(len(f.data)) {
		t.Fatalf("done NextOffset = %d, want final size %d", resp.NextOffset, len(f.data))
	}
	if resp.LastIndex != f.meta.Index {
		t.Fatalf("done LastIndex = %d, want %d", resp.LastIndex, f.meta.Index)
	}
	assertNoSnapshotRecvTemps(t, f.dir)
}

func TestInstallSnapshotChunkResumesAfterGap(t *testing.T) {
	f := newChunkRecvFixture(t)
	reqs := chunkRequests(3, f.meta, f.data, 7)
	if len(reqs) < 6 {
		t.Fatalf("fixture produced %d requests, need at least 6", len(reqs))
	}

	// Send the first two data chunks, then skip one and ask for the next.
	for _, req := range reqs[:2] {
		requireChunkSuccess(t, f.node, req)
	}
	current := reqs[1].Offset + uint64(len(reqs[1].Data))
	resp := sendChunk(t, f.node, reqs[3])
	if resp.Success {
		t.Fatal("gapped chunk was accepted")
	}
	if resp.NextOffset != int64(current) {
		t.Fatalf("gap NextOffset = %d, want %d", resp.NextOffset, current)
	}

	// The partial upload survived the rejected chunk: resume with the chunk
	// that starts exactly at NextOffset and finish the install.
	if !hasSnapshotRecvTemp(t, f.dir) {
		t.Fatal("gap rejection removed the partial upload file")
	}
	if reqs[2].Offset != uint64(resp.NextOffset) {
		t.Fatalf("resume chunk offset = %d, want NextOffset %d", reqs[2].Offset, resp.NextOffset)
	}
	for _, req := range reqs[2:] {
		requireChunkSuccess(t, f.node, req)
	}

	st := f.node.Status()
	if st.LastApplied != f.meta.Index || st.CommitIndex != f.meta.Index {
		t.Fatalf("resumed install applied %d commit %d, want %d", st.LastApplied, st.CommitIndex, f.meta.Index)
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

// ---------------------------------------------------------------------------
// Resume: leader continues from the receiver's NextOffset
// ---------------------------------------------------------------------------

// chunkInstallSetup creates a chunked cluster with one lagging follower,
// writes a payload larger than one chunk and compacts it into a snapshot on
// the leader. The follower stays isolated so the caller can install a fault
// injector before healing the link and letting the transfer start.
func chunkInstallSetup(t *testing.T, key string, payload []byte) (*snapshotCluster, string, raft.Snapshot, *chunkSnapshotTransport) {
	t.Helper()
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

	c.net.partition(lagging, leaderID)
	c.net.partition(leaderID, lagging)
	_, leader, leaderID = c.proposeOnLeader(leader, model.Entry{Type: model.CommandPut, Key: key, Value: payload})
	snap, err := leader.Snapshot()
	if err != nil {
		t.Fatalf("leader Snapshot: %v", err)
	}
	if int64(len(snap.Data)) <= raft.SnapshotChunkSize {
		t.Fatalf("snapshot payload %d bytes does not exceed one chunk", len(snap.Data))
	}

	tr := c.chunkTrs[leaderID]
	if tr == nil {
		t.Fatal("leader has no chunk transport")
	}
	return c, lagging, snap, tr
}

// requireSucceededChunks mirrors the successful transfer shape: every data
// offset is sent once (except retriedOffset, which is sent retried times), the
// payload of every request matches the snapshot, and the final request is the
// Done marker at the snapshot size.
func requireSucceededChunks(t *testing.T, reqs []raft.InstallSnapshotChunkRequest, data []byte, retriedOffset uint64, retried int) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("leader sent no chunk requests")
	}
	expected := make(map[uint64]int)
	for off := uint64(0); off < uint64(len(data)); {
		n := uint64(raft.SnapshotChunkSize)
		if rem := uint64(len(data)) - off; rem < n {
			n = rem
		}
		expected[off] = 1
		off += n
	}
	expected[retriedOffset] = retried

	counts := make(map[uint64]int)
	for i, req := range reqs {
		if req.Done {
			if i != len(reqs)-1 {
				t.Fatalf("Done marker at request %d of %d", i, len(reqs))
			}
			if req.Offset != uint64(len(data)) {
				t.Fatalf("Done offset = %d, want %d", req.Offset, len(data))
			}
			continue
		}
		end := req.Offset + uint64(len(req.Data))
		if end > uint64(len(data)) || !bytes.Equal(req.Data, data[req.Offset:end]) {
			t.Fatalf("chunk %d at offset %d does not match the snapshot payload", i, req.Offset)
		}
		counts[req.Offset]++
	}
	if len(counts) != len(expected) {
		t.Fatalf("chunk offsets sent = %v, want %v (retried offset %d)", counts, expected, retriedOffset)
	}
	for off, want := range expected {
		if got := counts[off]; got != want {
			t.Fatalf("offset %d sent %d times, want %d (all offsets %v)", off, got, want, counts)
		}
	}
}

func TestInstallSnapshotChunkResumesAfterLostResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 2*raft.SnapshotChunkSize+(300<<10))
	c, lagging, snap, tr := chunkInstallSetup(t, "resume-lost", payload)

	// The follower accepts the second chunk (around the middle of the upload),
	// but its response never reaches the leader. The leader must retry that
	// chunk, then continue from the offset the follower confirms instead of
	// re-sending the bytes that came before it.
	var dropped atomic.Bool
	tr.setFault(func(peer string, req *raft.InstallSnapshotChunkRequest) (bool, *raft.InstallSnapshotChunkResponse, error) {
		if peer != lagging || req.Done || req.Offset != raft.SnapshotChunkSize || !dropped.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		resp, err := c.nodes[lagging].HandleInstallSnapshotChunk(context.Background(), req)
		if err != nil {
			return true, nil, err
		}
		if want := int64(req.Offset) + int64(len(req.Data)); resp.NextOffset != want {
			return true, nil, fmt.Errorf("follower NextOffset = %d, want %d", resp.NextOffset, want)
		}
		return true, nil, errors.New("snapshot network: response lost")
	})
	c.net.heal(lagging)

	eventually(t, c.timeout, "follower restores the snapshot after a lost chunk response", func() bool {
		return c.sms[lagging].appliedIndex() >= snap.Index
	})
	if got, ok := c.sms[lagging].get("resume-lost"); !ok || !bytes.Equal(got, payload) {
		t.Fatalf("follower restored value = %d bytes, ok=%v; want %d bytes", len(got), ok, len(payload))
	}
	stored, err := c.stores[lagging].LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stored.Index != snap.Index || !bytes.Equal(stored.Data, snap.Data) {
		t.Fatalf("stored snapshot = {%d, %d bytes}, want {%d, %d bytes}",
			stored.Index, len(stored.Data), snap.Index, len(snap.Data))
	}

	reqs := tr.chunkRequests(lagging)
	wantRetries := 2 // the dropped chunk and its idempotent retry
	requireSucceededChunks(t, reqs, snap.Data, raft.SnapshotChunkSize, wantRetries)
	if got := tr.legacyCalls(lagging); got != 0 {
		t.Fatalf("leader fell back to the legacy install %d times", got)
	}
	assertNoSnapshotRecvTemps(t, c.dirs[lagging])
}

func TestInstallSnapshotChunkResumesFromRejectionOffset(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 2*raft.SnapshotChunkSize+(300<<10))
	c, lagging, snap, tr := chunkInstallSetup(t, "resume-reject", payload)

	// The follower accepts the second chunk but reports it as rejected
	// together with the offset it has accepted. The sender must seek there and
	// keep going; re-sending bytes before NextOffset would make the first
	// chunk appear twice.
	var rejected atomic.Bool
	tr.setFault(func(peer string, req *raft.InstallSnapshotChunkRequest) (bool, *raft.InstallSnapshotChunkResponse, error) {
		if peer != lagging || req.Done || req.Offset != raft.SnapshotChunkSize || !rejected.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		resp, err := c.nodes[lagging].HandleInstallSnapshotChunk(context.Background(), req)
		if err != nil {
			return true, nil, err
		}
		return true, &raft.InstallSnapshotChunkResponse{
			Term:       req.Term,
			Success:    false,
			Error:      "snapshot network: response lost",
			NextOffset: resp.NextOffset,
		}, nil
	})
	c.net.heal(lagging)

	eventually(t, c.timeout, "follower restores the snapshot from a rejected-chunk offset", func() bool {
		return c.sms[lagging].appliedIndex() >= snap.Index
	})
	reqs := tr.chunkRequests(lagging)
	// The rejected chunk is not sent again: only the remaining chunk is.
	requireSucceededChunks(t, reqs, snap.Data, raft.SnapshotChunkSize, 1)
	assertNoSnapshotRecvTemps(t, c.dirs[lagging])
}

func TestInstallSnapshotChunkResumesAcrossAttempts(t *testing.T) {
	payload := bytes.Repeat([]byte("w"), raft.SnapshotChunkSize+(300<<10))
	c, lagging, snap, tr := chunkInstallSetup(t, "resume-rounds", payload)

	// The last data chunk is dropped until the leader exhausts its retry
	// budget for one replicate round. The next round must resume from the
	// offset the follower already confirmed instead of restarting at zero.
	lastOffset := uint64(0)
	for off := uint64(0); off < uint64(len(snap.Data)); {
		lastOffset = off
		n := uint64(raft.SnapshotChunkSize)
		if rem := uint64(len(snap.Data)) - off; rem < n {
			n = rem
		}
		off += n
	}
	var down atomic.Bool
	down.Store(true)
	var failed atomic.Int32
	tr.setFault(func(peer string, req *raft.InstallSnapshotChunkRequest) (bool, *raft.InstallSnapshotChunkResponse, error) {
		if peer != lagging || req.Done || req.Offset != lastOffset || !down.Load() {
			return false, nil, nil
		}
		failed.Add(1)
		return true, nil, errors.New("snapshot network: link down")
	})
	c.net.heal(lagging)

	eventually(t, c.timeout, "leader exhausts the retry budget for the last chunk", func() bool {
		if failed.Load() >= 5 {
			down.Store(false)
			return true
		}
		return false
	})
	eventually(t, c.timeout, "follower restores the snapshot after a later retry round", func() bool {
		return c.sms[lagging].appliedIndex() >= snap.Index
	})

	reqs := tr.chunkRequests(lagging)
	// One success on the last chunk plus every dropped attempt.
	requireSucceededChunks(t, reqs, snap.Data, lastOffset, int(failed.Load())+1)
	if got := tr.legacyCalls(lagging); got != 0 {
		t.Fatalf("leader fell back to the legacy install %d times", got)
	}
	assertNoSnapshotRecvTemps(t, c.dirs[lagging])
}
