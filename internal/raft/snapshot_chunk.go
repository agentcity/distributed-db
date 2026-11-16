package raft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"time"
)

// SnapshotChunkSize is the payload size the leader reads from a
// SnapshotStreamStore for each InstallSnapshotChunk RPC. It bounds the memory
// used on both sides of the transfer: the leader allocates one buffer and the
// follower rejects larger chunks.
const SnapshotChunkSize = 1 << 20

const (
	// snapshotChunkIdleTimeout is the default for how long an incomplete
	// upload may sit without a chunk before its temporary file is removed. The
	// leader restarts a failed install from offset zero, so keeping partial
	// data longer only wastes disk. Node copies it into a per-node field so a
	// test can shorten it without racing the timer goroutine.
	snapshotChunkIdleTimeout = 30 * time.Second
	// maxSnapshotChunkStreams bounds the partial-upload map. Only the current
	// leader can legitimately send a snapshot, so this is generous; it exists
	// to keep a misbehaving peer from growing the map without bound.
	maxSnapshotChunkStreams = 8
)

// snapshotChunkRecv is one in-progress chunked snapshot upload. The payload is
// accumulated in a temporary file and checksummed as it arrives; nothing but
// one chunk is ever held in memory.
type snapshotChunkRecv struct {
	index        uint64
	snapshotTerm uint64
	size         int64
	crc32        uint32
	offset       uint64
	file         *os.File
	path         string
	crc          hash.Hash32
	updated      time.Time

	// lastOffset/lastData/lastDone describe the most recently accepted chunk so
	// an immediate retry of it (the response was lost) is idempotent.
	lastOffset uint64
	lastData   []byte
	lastDone   bool

	// timer removes the upload if no chunk arrives before it fires. It is
	// rescheduled by expireSnapshotChunk while progress continues.
	timer *time.Timer
}

func dropSnapshotChunkRecv(recv *snapshotChunkRecv) {
	if recv == nil {
		return
	}
	if recv.timer != nil {
		recv.timer.Stop()
	}
	if recv.file != nil {
		_ = recv.file.Close()
	}
	if recv.path != "" {
		_ = os.Remove(recv.path)
	}
}

// dropSnapshotChunkLocked removes and deletes the partial upload for key. The
// caller holds snapshotRecvMu.
func (n *Node) dropSnapshotChunkLocked(key string) {
	recv, ok := n.snapshotRecv[key]
	if !ok {
		return
	}
	delete(n.snapshotRecv, key)
	dropSnapshotChunkRecv(recv)
}

// cleanupStaleSnapshotChunksLocked removes uploads that have not received a
// chunk for snapshotChunkIdleTimeout. The caller holds snapshotRecvMu.
func (n *Node) cleanupStaleSnapshotChunksLocked(now time.Time) {
	timeout := n.snapshotChunkTimeout
	for key, recv := range n.snapshotRecv {
		if now.Sub(recv.updated) >= timeout {
			n.dropSnapshotChunkLocked(key)
		}
	}
}

// expireSnapshotChunk is the timer callback for one partial upload. If the
// upload made progress since the timer was armed it schedules another check;
// otherwise it removes the upload and its temporary file.
func (n *Node) expireSnapshotChunk(key string, recv *snapshotChunkRecv) {
	n.snapshotRecvMu.Lock()
	defer n.snapshotRecvMu.Unlock()
	current, ok := n.snapshotRecv[key]
	if !ok || current != recv {
		return
	}
	remaining := n.snapshotChunkTimeout - time.Since(recv.updated)
	if remaining > 0 {
		recv.timer = time.AfterFunc(remaining, func() { n.expireSnapshotChunk(key, recv) })
		return
	}
	n.dropSnapshotChunkLocked(key)
}

// evictOldestSnapshotChunkLocked makes room in the bounded upload map by
// dropping the least recently updated upload. The caller holds snapshotRecvMu.
func (n *Node) evictOldestSnapshotChunkLocked() {
	var oldestKey string
	var oldest time.Time
	found := false
	for key, recv := range n.snapshotRecv {
		if !found || recv.updated.Before(oldest) {
			oldestKey = key
			oldest = recv.updated
			found = true
		}
	}
	if found {
		n.dropSnapshotChunkLocked(oldestKey)
	}
}

// discardAllSnapshotChunks removes every partial upload and its temporary
// file. It is called from Stop so a restarted node starts clean.
func (n *Node) discardAllSnapshotChunks() {
	n.snapshotRecvMu.Lock()
	defer n.snapshotRecvMu.Unlock()
	for key := range n.snapshotRecv {
		n.dropSnapshotChunkLocked(key)
	}
}

// sendInstallSnapshotChunks streams the current stored snapshot through a
// SnapshotChunkTransport. attempted is false only when the store holds no
// snapshot but the node has an in-memory payload, which is the one case where
// the legacy unary path can still work. Store, read and RPC failures report
// attempted=true so the caller retries (from offset zero) instead of
// materialising the whole payload.
func (n *Node) sendInstallSnapshotChunks(
	leaderCtx context.Context,
	peer string,
	term uint64,
	snap Snapshot,
	store SnapshotStreamStore,
	tr SnapshotChunkTransport,
) (success, attempted bool) {
	meta, rc, err := store.OpenSnapshot()
	if err != nil {
		// The store is a streaming store, so this is not a capability gap:
		// retry instead of materialising the whole payload for the legacy RPC.
		n.logger.Warn("raft: open streamed snapshot for install", "id", n.cfg.ID, "peer", peer, "error", err)
		return false, true
	}
	if rc == nil {
		// No stored snapshot. Legacy can still send an in-memory payload; with
		// no payload at all, retry once a snapshot exists.
		return false, snap.Data != nil
	}
	defer rc.Close()
	if meta.Index != snap.Index || meta.Term != snap.Term {
		n.logger.Warn("raft: stored snapshot does not match leader snapshot",
			"id", n.cfg.ID, "peer", peer,
			"stored_index", meta.Index, "stored_term", meta.Term,
			"snapshot_index", snap.Index, "snapshot_term", snap.Term)
		// A mismatch is a real error: materialising would allocate the whole
		// payload and then fail the same consistency check.
		return false, true
	}
	if meta.Size < 0 {
		n.logger.Warn("raft: stored snapshot has a negative size", "id", n.cfg.ID, "peer", peer, "size", meta.Size)
		return false, true
	}

	buf := make([]byte, SnapshotChunkSize)
	var offset uint64
	for {
		nread, rerr := io.ReadFull(rc, buf)
		if nread > 0 {
			if meta.Size > 0 && offset+uint64(nread) > uint64(meta.Size) {
				n.logger.Warn("raft: streamed snapshot grew while sending",
					"id", n.cfg.ID, "peer", peer, "sent", offset+uint64(nread), "size", meta.Size)
				return false, true
			}
			req := &InstallSnapshotChunkRequest{
				Term:         term,
				LeaderID:     n.cfg.ID,
				Index:        meta.Index,
				SnapshotTerm: meta.Term,
				Offset:       offset,
				// Copy the chunk: a transport is allowed to retain the request
				// after the call, and buf is reused for the next chunk.
				Data:  append([]byte(nil), buf[:nread]...),
				Size:  meta.Size,
				CRC32: meta.CRC32,
			}
			resp, err := n.callSnapshotChunk(leaderCtx, tr, peer, req)
			if err != nil {
				n.logger.Warn("raft: send snapshot chunk", "id", n.cfg.ID, "peer", peer,
					"offset", offset, "bytes", nread, "error", err)
				return false, true
			}
			if !n.acceptSnapshotChunkResponse(term, peer, resp) {
				return false, true
			}
			offset += uint64(nread)
		}
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		n.logger.Warn("raft: read streamed snapshot for install", "id", n.cfg.ID, "peer", peer, "error", rerr)
		return false, true
	}
	if meta.Size > 0 && int64(offset) != meta.Size {
		n.logger.Warn("raft: streamed snapshot size changed", "id", n.cfg.ID, "peer", peer,
			"read", offset, "size", meta.Size)
		return false, true
	}

	req := &InstallSnapshotChunkRequest{
		Term:         term,
		LeaderID:     n.cfg.ID,
		Index:        meta.Index,
		SnapshotTerm: meta.Term,
		Offset:       offset,
		Done:         true,
		Size:         meta.Size,
		CRC32:        meta.CRC32,
	}
	resp, err := n.callSnapshotChunk(leaderCtx, tr, peer, req)
	if err != nil {
		n.logger.Warn("raft: finish snapshot chunk install", "id", n.cfg.ID, "peer", peer, "error", err)
		return false, true
	}
	if !n.acceptSnapshotChunkResponse(term, peer, resp) {
		return false, true
	}
	return n.finishSnapshotInstall(peer, term, meta.Index, resp.LastIndex), true
}

func (n *Node) callSnapshotChunk(leaderCtx context.Context, tr SnapshotChunkTransport, peer string, req *InstallSnapshotChunkRequest) (*InstallSnapshotChunkResponse, error) {
	rpcCtx, cancel := context.WithTimeout(leaderCtx, n.appendTimeout())
	defer cancel()
	return tr.InstallSnapshotChunk(rpcCtx, peer, req)
}

// acceptSnapshotChunkResponse applies the leader-side part of one chunk
// response: a higher follower term makes the leader step down, and a rejected
// chunk (or a lost leadership) stops the stream.
func (n *Node) acceptSnapshotChunkResponse(term uint64, peer string, resp *InstallSnapshotChunkResponse) bool {
	if resp == nil {
		return false
	}
	n.mu.Lock()
	if resp.Term > n.term {
		if err := n.adoptTermLocked(resp.Term, ""); err != nil {
			n.logger.Error("raft: adopt higher term failed", "id", n.cfg.ID, "error", err)
		}
		n.mu.Unlock()
		return false
	}
	ok := !n.stopped && n.role == Leader && n.term == term && resp.Success
	n.mu.Unlock()
	if !ok && resp.Error != "" {
		n.logger.Warn("raft: follower rejected snapshot chunk", "id", n.cfg.ID, "peer", peer, "error", resp.Error)
	}
	return ok
}

// finishSnapshotInstall records a completed snapshot install on the leader.
func (n *Node) finishSnapshotInstall(peer string, term, snapIndex, lastIndex uint64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped || n.role != Leader || n.term != term {
		return false
	}
	if lastIndex > n.matchIndex[peer] {
		n.matchIndex[peer] = lastIndex
	}
	if snapIndex > n.matchIndex[peer] {
		n.matchIndex[peer] = snapIndex
	}
	n.nextIndex[peer] = n.matchIndex[peer] + 1
	n.lastAck[peer] = time.Now()
	n.maybeCommitLocked()
	return n.nextIndex[peer] <= n.lastIndexLocked()
}

// HandleInstallSnapshotChunk receives one chunk of a streamed snapshot. The
// first chunk creates a temporary file under cfg.DataDir (or the system
// temporary directory) and later chunks append to it. The snapshot is restored
// only when the Done chunk has been received and its total size and CRC32
// match; the temporary file is removed after every terminal outcome.
//
// Term and leader handling match HandleInstallSnapshot. Duplicate chunks are
// detected by offset and content: an immediate retry of the last accepted
// chunk succeeds without writing it twice, while an overlapping or gapped
// chunk is rejected cleanly. A leader retry that starts again at offset zero
// truncates the partial upload and restarts it. Partial uploads are bounded in
// number and removed after snapshotChunkIdleTimeout without progress.
//
// The response Error field reports protocol and restore failures; nil error is
// returned with Success=false for those. A nil request, a stopped node or a
// cancelled context are returned as Go/gRPC errors.
func (n *Node) HandleInstallSnapshotChunk(ctx context.Context, req *InstallSnapshotChunkRequest) (*InstallSnapshotChunkResponse, error) {
	if req == nil {
		return nil, errors.New("raft: nil InstallSnapshotChunkRequest")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	n.snapshotRecvMu.Lock()
	defer n.snapshotRecvMu.Unlock()
	n.cleanupStaleSnapshotChunksLocked(time.Now())

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return nil, ErrStopped
	}
	if req.Term < n.term {
		term := n.term
		n.mu.Unlock()
		return &InstallSnapshotChunkResponse{Term: term, Error: fmt.Sprintf("stale term %d, current term %d", req.Term, term)}, nil
	}
	if req.Term > n.term {
		if err := n.adoptTermLocked(req.Term, req.LeaderID); err != nil {
			n.mu.Unlock()
			return nil, err
		}
	} else {
		if n.role != Follower {
			n.stepDownLocked(req.LeaderID)
		} else {
			n.leaderID = req.LeaderID
		}
		n.resetElectionDeadlineLocked()
	}
	term := n.term
	last := n.lastIndexLocked()
	upToDate := req.Index <= n.snapshot.Index || req.Index <= n.commitIndex
	n.mu.Unlock()

	if req.Index == 0 {
		return n.rejectSnapshotChunk(term, "snapshot index must be positive")
	}
	if req.LeaderID == "" {
		return n.rejectSnapshotChunk(term, "leader id is required")
	}
	if req.Size < 0 {
		return n.rejectSnapshotChunk(term, fmt.Sprintf("invalid snapshot size %d", req.Size))
	}
	if len(req.Data) > SnapshotChunkSize {
		return n.rejectSnapshotChunk(term, fmt.Sprintf("chunk payload %d exceeds %d bytes", len(req.Data), SnapshotChunkSize))
	}
	if len(req.Data) == 0 && !req.Done {
		return n.rejectSnapshotChunk(term, "empty non-final chunk")
	}
	if upToDate {
		// The snapshot was already installed (or covered by commit); this also
		// makes a retried final chunk idempotent.
		n.dropSnapshotChunkLocked(req.LeaderID)
		return &InstallSnapshotChunkResponse{Term: term, Success: true, LastIndex: last}, nil
	}

	recv := n.snapshotRecv[req.LeaderID]
	if recv != nil && (recv.index != req.Index || recv.snapshotTerm != req.SnapshotTerm) {
		if req.Offset != 0 {
			return n.rejectSnapshotChunk(term, fmt.Sprintf("no active upload for snapshot {%d,%d} at offset %d", req.Index, req.SnapshotTerm, req.Offset))
		}
		n.dropSnapshotChunkLocked(req.LeaderID)
		recv = nil
	}
	if recv != nil && req.Offset == 0 && recv.offset > 0 {
		// The leader restarted the upload from the beginning after a failed or
		// lost attempt. Discard the partial data instead of wedging.
		n.dropSnapshotChunkLocked(req.LeaderID)
		recv = nil
	}
	if recv == nil {
		if req.Offset != 0 {
			return n.rejectSnapshotChunk(term, fmt.Sprintf("no active snapshot upload at offset %d", req.Offset))
		}
		if len(n.snapshotRecv) >= maxSnapshotChunkStreams {
			n.evictOldestSnapshotChunkLocked()
		}
		file, err := n.createSnapshotTemp("raft-snapshot-recv-*.tmp")
		if err != nil {
			return nil, err
		}
		recv = &snapshotChunkRecv{
			index:        req.Index,
			snapshotTerm: req.SnapshotTerm,
			size:         req.Size,
			crc32:        req.CRC32,
			file:         file,
			path:         file.Name(),
			crc:          crc32.NewIEEE(),
			updated:      time.Now(),
		}
		n.snapshotRecv[req.LeaderID] = recv
		key := req.LeaderID
		recv.timer = time.AfterFunc(n.snapshotChunkTimeout, func() { n.expireSnapshotChunk(key, recv) })
	}

	if recv.size == 0 {
		recv.size = req.Size
	}
	if recv.crc32 == 0 {
		recv.crc32 = req.CRC32
	}
	if req.Size != 0 && recv.size != req.Size {
		n.dropSnapshotChunkLocked(req.LeaderID)
		return n.rejectSnapshotChunk(term, fmt.Sprintf("snapshot size changed from %d to %d", recv.size, req.Size))
	}
	if req.CRC32 != 0 && recv.crc32 != req.CRC32 {
		n.dropSnapshotChunkLocked(req.LeaderID)
		return n.rejectSnapshotChunk(term, fmt.Sprintf("snapshot CRC32 changed from %08x to %08x", recv.crc32, req.CRC32))
	}

	if req.Offset < recv.offset {
		if req.Offset == recv.lastOffset && bytes.Equal(req.Data, recv.lastData) && req.Done == recv.lastDone {
			// A retry of the last accepted chunk: acknowledge it without
			// appending the bytes a second time.
			return &InstallSnapshotChunkResponse{Term: term, Success: true}, nil
		}
		return n.rejectSnapshotChunk(term, fmt.Sprintf("overlapping chunk at offset %d, next offset %d", req.Offset, recv.offset))
	}
	if req.Offset > recv.offset {
		return n.rejectSnapshotChunk(term, fmt.Sprintf("chunk offset gap: got %d, want %d", req.Offset, recv.offset))
	}
	if recv.size > 0 && recv.offset+uint64(len(req.Data)) > uint64(recv.size) {
		n.dropSnapshotChunkLocked(req.LeaderID)
		return n.rejectSnapshotChunk(term, fmt.Sprintf("snapshot payload exceeds declared size %d", recv.size))
	}

	if len(req.Data) > 0 {
		if _, err := recv.file.Write(req.Data); err != nil {
			n.dropSnapshotChunkLocked(req.LeaderID)
			return n.rejectSnapshotChunk(term, fmt.Sprintf("write snapshot chunk: %v", err))
		}
		_, _ = recv.crc.Write(req.Data)
		recv.lastOffset = recv.offset
		recv.offset += uint64(len(req.Data))
		recv.lastData = append(recv.lastData[:0], req.Data...)
		recv.lastDone = req.Done
		recv.updated = time.Now()
	}

	if !req.Done {
		return &InstallSnapshotChunkResponse{Term: term, Success: true}, nil
	}

	resp, err := n.finishSnapshotChunk(recv)
	n.dropSnapshotChunkLocked(req.LeaderID)
	if err != nil {
		n.mu.Lock()
		term = n.term
		n.mu.Unlock()
		return n.rejectSnapshotChunk(term, err.Error())
	}
	return resp, nil
}

// finishSnapshotChunk verifies and installs a complete upload. The caller
// holds snapshotRecvMu and removes the partial file afterwards.
func (n *Node) finishSnapshotChunk(recv *snapshotChunkRecv) (*InstallSnapshotChunkResponse, error) {
	if recv.size > 0 && int64(recv.offset) != recv.size {
		return nil, fmt.Errorf("snapshot payload size %d, want %d", recv.offset, recv.size)
	}
	if recv.crc32 != 0 && recv.crc.Sum32() != recv.crc32 {
		return nil, fmt.Errorf("snapshot CRC32 %08x, want %08x", recv.crc.Sum32(), recv.crc32)
	}
	if err := recv.file.Sync(); err != nil {
		return nil, fmt.Errorf("sync snapshot upload: %w", err)
	}
	if _, err := recv.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind snapshot upload: %w", err)
	}

	meta := SnapshotMeta{
		Index: recv.index,
		Term:  recv.snapshotTerm,
		Size:  int64(recv.offset),
		CRC32: recv.crc.Sum32(),
	}
	if err := n.RestoreSnapshotStream(meta, recv.file); err != nil {
		return nil, err
	}
	n.mu.Lock()
	term := n.term
	last := n.lastIndexLocked()
	n.mu.Unlock()
	return &InstallSnapshotChunkResponse{Term: term, Success: true, LastIndex: last}, nil
}

// rejectSnapshotChunk builds a protocol-level rejection response. With no
// format arguments the message is used verbatim, so an error string containing
// a percent sign is not reinterpreted as a format.
func (n *Node) rejectSnapshotChunk(term uint64, format string, args ...any) (*InstallSnapshotChunkResponse, error) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	return &InstallSnapshotChunkResponse{Term: term, Error: message}, nil
}
