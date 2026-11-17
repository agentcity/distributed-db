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
	"sync"
	"time"
)

// SnapshotChunkSize is the payload size the leader reads from a
// SnapshotStreamStore for each InstallSnapshotChunk RPC. It bounds the memory
// used on both sides of the transfer: the leader allocates one buffer and the
// follower rejects larger chunks.
const SnapshotChunkSize = 1 << 20

const (
	// snapshotChunkIdleTimeout is the default for how long an incomplete
	// upload may sit without a chunk before its temporary file is removed. A
	// leader resumes from the last confirmed offset, so the partial data is
	// kept while the retry can still reach this node; only an upload that
	// makes no progress for this long is discarded. Node copies it into a
	// per-node field so a test can shorten it without racing the timer
	// goroutine.
	snapshotChunkIdleTimeout = 30 * time.Second
	// maxSnapshotChunkStreams bounds the partial-upload map. Only the current
	// leader can legitimately send a snapshot, so this is generous; it exists
	// to keep a misbehaving peer from growing the map without bound.
	maxSnapshotChunkStreams = 8
	// maxSnapshotChunkFailures bounds how many consecutive chunk RPCs may fail
	// without receiver-confirmed forward progress. A peer that stops answering
	// cannot keep one replicate loop busy indefinitely; the next round resumes
	// from the last confirmed offset instead of restarting the upload.
	maxSnapshotChunkFailures = 5
	// snapshotChunkRetryDelay is the first pause before retrying a failed
	// chunk RPC. The delay grows linearly with the failure count and is capped
	// by snapshotChunkMaxRetryDelay, so a short transport outage is ridden out
	// without blocking the replicate loop for long.
	snapshotChunkRetryDelay    = 20 * time.Millisecond
	snapshotChunkMaxRetryDelay = 200 * time.Millisecond
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
	// membership is the optional snapshot membership envelope repeated by the
	// leader. It is validated on every chunk and installed with the snapshot.
	membership *SnapshotMembership
	file       *os.File
	path       string
	crc        hash.Hash32
	updated    time.Time

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
// file, and forgets this node's sender resume points. It is called from Stop so
// a restarted node starts clean.
func (n *Node) discardAllSnapshotChunks() {
	n.snapshotRecvMu.Lock()
	for key := range n.snapshotRecv {
		n.dropSnapshotChunkLocked(key)
	}
	n.snapshotRecvMu.Unlock()
	clearSnapshotChunkSendOffsetsForNode(n)
}

// sendInstallSnapshotChunks streams the current stored snapshot through a
// SnapshotChunkTransport. attempted is false only when the store holds no
// snapshot but the node has an in-memory payload, which is the one case where
// the legacy unary path can still work.
//
// A failed chunk RPC does not restart the transfer from zero. The receiver
// reports NextOffset (the number of payload bytes it accepted) with every
// response, and the sender seeks the snapshot stream there, so bytes the
// follower already has are neither re-read nor re-sent. A transport error
// leaves the receiver state unknown, so the same chunk is retried; that is
// idempotent because the receiver recognises an exact retry of its last
// accepted chunk. Progress is remembered per peer, so a failure that outlives
// the retry budget resumes on the next replicate round. Only a rejection
// without a usable NextOffset (the follower lost the upload or refused the
// snapshot) falls back to a fresh upload at offset zero.
//
// Store and snapshot read failures report attempted=true so the caller retries
// the install instead of materialising the whole payload. Leadership loss and
// a higher term from the follower stop the stream immediately.
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

	// Resume where this leader last had bytes confirmed for this peer. The
	// receiver validates the offset against its own partial upload, so a stale
	// entry costs one rejected chunk, not a corrupt install.
	offset := loadSnapshotChunkSendOffset(n, peer, meta.Index, meta.Term)
	if meta.Size > 0 && offset > uint64(meta.Size) {
		offset = 0
	}
	stream := &snapshotChunkStream{r: rc}
	if offset > 0 {
		if err := stream.position(offset); err != nil {
			n.logger.Warn("raft: seek streamed snapshot for resume", "id", n.cfg.ID, "peer", peer,
				"offset", offset, "error", err)
			clearSnapshotChunkSendOffset(n, peer)
			return false, true
		}
	}

	// One reusable 1 MiB read buffer bounds the memory used by the sender.
	buf := make([]byte, SnapshotChunkSize)
	var pending []byte // bytes of the chunk currently being retried
	eof := false
	failures := 0
	// Repeat the membership envelope on every chunk so the receiver validates
	// that the whole upload belongs to one snapshot and can install the
	// configuration when the final chunk arrives. Fall back to the leader's
	// in-memory snapshot when the store cannot persist membership metadata.
	chunkMembership := cloneMembership(meta.Membership)
	if chunkMembership == nil {
		chunkMembership = cloneMembership(snap.Membership)
	}

	for {
		if !n.isLeaderFor(term) {
			return false, true
		}

		if pending == nil && !eof {
			nread, rerr := stream.read(buf)
			if nread > 0 {
				if meta.Size > 0 && offset+uint64(nread) > uint64(meta.Size) {
					n.logger.Warn("raft: streamed snapshot grew while sending",
						"id", n.cfg.ID, "peer", peer, "sent", offset+uint64(nread), "size", meta.Size)
					clearSnapshotChunkSendOffset(n, peer)
					return false, true
				}
				pending = buf[:nread]
			}
			if rerr != nil {
				if !isSnapshotChunkEOF(rerr) {
					n.logger.Warn("raft: read streamed snapshot for install", "id", n.cfg.ID, "peer", peer, "error", rerr)
					clearSnapshotChunkSendOffset(n, peer)
					return false, true
				}
				eof = true
			}
			if pending == nil && !eof {
				// io.ReadFull does not normally return (0, nil); treat it as a
				// transient read and retry within the same failure budget.
				failures++
				if failures >= maxSnapshotChunkFailures || !n.waitSnapshotChunkRetry(leaderCtx, failures) {
					return false, true
				}
				continue
			}
		}

		isDone := pending == nil
		if isDone && meta.Size > 0 && int64(offset) != meta.Size {
			n.logger.Warn("raft: streamed snapshot size changed", "id", n.cfg.ID, "peer", peer,
				"read", offset, "size", meta.Size)
			clearSnapshotChunkSendOffset(n, peer)
			return false, true
		}
		req := &InstallSnapshotChunkRequest{
			Term:         term,
			LeaderID:     n.cfg.ID,
			Index:        meta.Index,
			SnapshotTerm: meta.Term,
			Offset:       offset,
			Size:         meta.Size,
			CRC32:        meta.CRC32,
			Membership:   chunkMembership,
		}
		if isDone {
			req.Done = true
			// The final Done marker is always empty, so its chunk checksum is
			// zero by definition.
		} else {
			// Copy the chunk: a transport is allowed to retain the request
			// after the call, and buf is reused for the next chunk. Compute the
			// checksum over exactly the bytes this request carries, including
			// when pending was trimmed after a partial acceptance.
			req.Data = append([]byte(nil), pending...)
			req.ChunkCRC32 = crc32.ChecksumIEEE(pending)
		}

		resp, rpcErr := n.callSnapshotChunk(leaderCtx, tr, peer, req)
		if n.acceptSnapshotChunkResponse(term, peer, resp) {
			if isDone {
				// Clear the resume point so a later install of a newer
				// snapshot starts from the beginning.
				clearSnapshotChunkSendOffset(n, peer)
				return n.finishSnapshotInstall(peer, term, meta.Index, resp.LastIndex), true
			}
			offset += uint64(len(pending))
			pending = nil
			failures = 0
			storeSnapshotChunkSendOffset(n, peer, meta.Index, meta.Term, offset)
			continue
		}

		// The chunk was rejected or the RPC failed. Leadership may have
		// changed while the request was in flight; never retry for an old term.
		if !n.isLeaderFor(term) {
			return false, true
		}

		chunkLen := uint64(len(pending)) // zero for the Done marker
		if rpcErr == nil && resp != nil && !resp.Success {
			next, ok := snapshotChunkResumeOffset(resp, meta.Size, offset, chunkLen)
			if !ok {
				// The follower has no upload to continue (it timed out,
				// restarted or refused this snapshot). Forget the resume point
				// and let the next attempt start at offset zero.
				n.logger.Warn("raft: follower cannot resume snapshot upload",
					"id", n.cfg.ID, "peer", peer, "offset", offset, "error", resp.Error)
				clearSnapshotChunkSendOffset(n, peer)
				return false, true
			}
			switch {
			case next > offset:
				// The receiver confirmed bytes before next. Drop the prefix of
				// the chunk it already has instead of re-reading or re-sending
				// it, then continue at next.
				if next < offset+chunkLen {
					pending = pending[next-offset:]
				} else {
					pending = nil
				}
				offset = next
				failures = 0
			case next < offset:
				// The receiver is behind the stream; rewind and resend from
				// its offset. Non-seekable snapshot streams cannot rewind, so
				// fall back to a fresh upload in that case.
				if err := stream.position(next); err != nil {
					n.logger.Warn("raft: cannot rewind streamed snapshot", "id", n.cfg.ID, "peer", peer,
						"offset", next, "error", err)
					clearSnapshotChunkSendOffset(n, peer)
					return false, true
				}
				offset = next
				pending = nil
				eof = false
				failures++
			default:
				// Same offset: retry the chunk or the Done marker.
				failures++
			}
			storeSnapshotChunkSendOffset(n, peer, meta.Index, meta.Term, offset)
			if failures >= maxSnapshotChunkFailures {
				n.logger.Warn("raft: snapshot chunk rejected repeatedly",
					"id", n.cfg.ID, "peer", peer, "offset", offset, "error", resp.Error)
				return false, true
			}
			if failures > 0 && !n.waitSnapshotChunkRetry(leaderCtx, failures) {
				return false, true
			}
			continue
		}

		// Transport error or nil response. The receiver may or may not have
		// accepted the chunk, so resend exactly these bytes: an exact retry is
		// idempotent at the receiver, which answers with its NextOffset.
		failures++
		if failures >= maxSnapshotChunkFailures {
			n.logger.Warn("raft: send snapshot chunk failed repeatedly",
				"id", n.cfg.ID, "peer", peer, "offset", offset, "error", rpcErr)
			// Keep the confirmed offset so the next replicate round resumes
			// here instead of starting over.
			return false, true
		}
		if !n.waitSnapshotChunkRetry(leaderCtx, failures) {
			return false, true
		}
	}
}

// snapshotChunkResumeOffset extracts a usable resume offset from a rejection.
// next is the number of payload bytes the receiver accepted. It must be
// positive (zero means the follower has no upload to resume), cannot exceed
// the declared snapshot size, and cannot be ahead of the bytes this request
// carried.
func snapshotChunkResumeOffset(resp *InstallSnapshotChunkResponse, size int64, offset, chunkLen uint64) (uint64, bool) {
	if resp == nil || resp.Success || resp.NextOffset <= 0 {
		return 0, false
	}
	next := uint64(resp.NextOffset)
	if next > offset+chunkLen {
		return 0, false
	}
	if size > 0 && next > uint64(size) {
		return 0, false
	}
	return next, true
}

// isSnapshotChunkEOF reports whether a short read simply means the snapshot
// stream ended.
func isSnapshotChunkEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// waitSnapshotChunkRetry pauses before retrying a chunk RPC and reports whether
// the retry may proceed. The delay grows linearly with the failure count and is
// capped, so a brief transport outage is ridden out without stalling the
// replicate loop for long. It returns false when the term context ends (the
// node stopped or lost leadership).
func (n *Node) waitSnapshotChunkRetry(ctx context.Context, failures int) bool {
	delay := snapshotChunkRetryDelay * time.Duration(failures)
	if delay > snapshotChunkMaxRetryDelay {
		delay = snapshotChunkMaxRetryDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// snapshotChunkSendKey identifies one leader-to-peer chunked snapshot
// transfer. The node pointer keeps state from different nodes (and from test
// clusters that reuse ids) apart.
type snapshotChunkSendKey struct {
	node *Node
	peer string
}

// snapshotChunkSendProgress is how far one peer has confirmed a snapshot
// upload: enough to resume a retry in a later replicate round without
// re-reading the whole snapshot.
type snapshotChunkSendProgress struct {
	index   uint64
	term    uint64
	offset  uint64
	updated time.Time
}

// snapshotChunkSends is a bounded resume cache: at most one entry per
// (node, peer). Entries are replaced on snapshot changes, dropped on completion
// and on Node.Stop, and ignored after snapshotChunkIdleTimeout without an
// update. It lives in this file so the chunked sender is self-contained.
var snapshotChunkSends = struct {
	mu sync.Mutex
	m  map[snapshotChunkSendKey]snapshotChunkSendProgress
}{m: make(map[snapshotChunkSendKey]snapshotChunkSendProgress)}

// loadSnapshotChunkSendOffset returns how many bytes of snapshot {index, term}
// this peer already confirmed, or zero when the transfer must start over.
func loadSnapshotChunkSendOffset(n *Node, peer string, index, term uint64) uint64 {
	snapshotChunkSends.mu.Lock()
	defer snapshotChunkSends.mu.Unlock()
	key := snapshotChunkSendKey{node: n, peer: peer}
	progress, ok := snapshotChunkSends.m[key]
	if !ok {
		return 0
	}
	if progress.index != index || progress.term != term || time.Since(progress.updated) >= n.snapshotChunkTimeout {
		delete(snapshotChunkSends.m, key)
		return 0
	}
	return progress.offset
}

// storeSnapshotChunkSendOffset records receiver-confirmed progress. offset is
// the number of payload bytes the follower has accepted.
func storeSnapshotChunkSendOffset(n *Node, peer string, index, term, offset uint64) {
	snapshotChunkSends.mu.Lock()
	defer snapshotChunkSends.mu.Unlock()
	snapshotChunkSends.m[snapshotChunkSendKey{node: n, peer: peer}] = snapshotChunkSendProgress{
		index:   index,
		term:    term,
		offset:  offset,
		updated: time.Now(),
	}
}

// clearSnapshotChunkSendOffset forgets the resume point for one peer.
func clearSnapshotChunkSendOffset(n *Node, peer string) {
	snapshotChunkSends.mu.Lock()
	defer snapshotChunkSends.mu.Unlock()
	delete(snapshotChunkSends.m, snapshotChunkSendKey{node: n, peer: peer})
}

// clearSnapshotChunkSendOffsetsForNode removes every resume point of one node.
// It is called from Stop.
func clearSnapshotChunkSendOffsetsForNode(n *Node) {
	snapshotChunkSends.mu.Lock()
	defer snapshotChunkSends.mu.Unlock()
	for key := range snapshotChunkSends.m {
		if key.node == n {
			delete(snapshotChunkSends.m, key)
		}
	}
}

// snapshotChunkStream reads the stored snapshot sequentially and can be
// repositioned to an offset the receiver confirmed. FileStore returns an
// *os.File, so resuming is a Seek; a non-seekable snapshot stream can still
// skip forward without allocating, but cannot rewind.
type snapshotChunkStream struct {
	r   io.Reader
	pos uint64
}

// errSnapshotChunkRewind reports that a non-seekable snapshot stream cannot
// return to an earlier offset.
var errSnapshotChunkRewind = errors.New("raft: snapshot stream cannot rewind")

func (s *snapshotChunkStream) position(off uint64) error {
	if off == s.pos {
		return nil
	}
	if seeker, ok := s.r.(io.Seeker); ok {
		if _, err := seeker.Seek(int64(off), io.SeekStart); err != nil {
			return err
		}
		s.pos = off
		return nil
	}
	if off < s.pos {
		return errSnapshotChunkRewind
	}
	if _, err := io.CopyN(io.Discard, s.r, int64(off-s.pos)); err != nil {
		return err
	}
	s.pos = off
	return nil
}

func (s *snapshotChunkStream) read(buf []byte) (int, error) {
	n, err := io.ReadFull(s.r, buf)
	s.pos += uint64(n)
	return n, err
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
	if !n.isReplicationTargetLocked(peer) {
		// The member was removed while the snapshot was in flight; do not
		// resurrect bookkeeping for it.
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
// When the request carries ChunkCRC32, it is verified before the bytes are
// written: a mismatch rejects the chunk and leaves the partial upload (and
// its file) untouched, so the leader can resume from NextOffset. A zero
// ChunkCRC32 means the sender did not supply one; such chunks, including every
// chunk from a legacy sender, are staged and checked by the total CRC32 at the
// end.
//
// Term and leader handling match HandleInstallSnapshot. Duplicate chunks are
// detected by offset and content: an immediate retry of the last accepted
// chunk succeeds without writing it twice. A chunk beyond the current offset
// (a gap) or one that overlaps already accepted data is rejected, but the
// partial upload is kept; the response's NextOffset tells the leader which
// offset to resume from. A leader retry that starts again at offset zero is the
// only supported reset: it discards (truncates) the partial upload and starts
// over, because the protocol has no upload identifier. Partial uploads are
// bounded in number and removed after snapshotChunkIdleTimeout without
// progress.
//
// NextOffset is set in every response: the number of accepted payload bytes
// after an accepted chunk, the current offset after an idempotent retry, the
// current offset after a gap rejection, and the final snapshot size after a
// successful Done.
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
	// Verify the per-chunk checksum before touching any staged state or
	// writing to the temporary file. A zero checksum means the sender did not
	// supply one (legacy sender, or the empty Done marker); the total CRC32
	// remains the end-to-end check for those uploads. Rejecting here leaves an
	// existing partial upload intact, so the leader can resume from its last
	// acknowledged offset.
	if req.ChunkCRC32 != 0 {
		if got := crc32.ChecksumIEEE(req.Data); got != req.ChunkCRC32 {
			message := fmt.Sprintf("chunk CRC32 mismatch at offset %d: got %08x, want %08x", req.Offset, got, req.ChunkCRC32)
			// Report the offset already staged for this snapshot so the leader
			// can resume instead of restarting the upload. The staged file is
			// not touched.
			if recv := n.snapshotRecv[req.LeaderID]; recv != nil &&
				recv.index == req.Index && recv.snapshotTerm == req.SnapshotTerm {
				return n.rejectSnapshotChunkAt(term, recv.offset, message)
			}
			return n.rejectSnapshotChunk(term, message)
		}
	}
	reqMembership, err := normalizeSnapshotMembership(req.Membership)
	if err != nil {
		return n.rejectSnapshotChunk(term, err.Error())
	}
	if upToDate {
		// The snapshot was already installed (or covered by commit); this also
		// makes a retried final chunk idempotent. Report the final size so a
		// leader that lost the response knows the upload is complete.
		n.dropSnapshotChunkLocked(req.LeaderID)
		next := req.Offset + uint64(len(req.Data))
		if req.Size > 0 && next > uint64(req.Size) {
			next = uint64(req.Size)
		}
		return &InstallSnapshotChunkResponse{
			Term:       term,
			Success:    true,
			LastIndex:  last,
			NextOffset: int64(next),
		}, nil
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
		// lost attempt. This is the only supported way to reset an upload
		// (the protocol has no upload identifier), so discard the partial
		// data, truncate it and start over instead of wedging. Every other
		// rejection keeps the partial upload so the leader can resume it.
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
			membership:   cloneMembership(reqMembership),
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
	if reqMembership != nil {
		if recv.membership == nil {
			recv.membership = cloneMembership(reqMembership)
		} else if !membershipEqual(recv.membership, reqMembership) {
			n.dropSnapshotChunkLocked(req.LeaderID)
			return n.rejectSnapshotChunk(term, "snapshot membership changed during upload")
		}
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
			return &InstallSnapshotChunkResponse{Term: term, Success: true, NextOffset: int64(recv.offset)}, nil
		}
		return n.rejectSnapshotChunk(term, fmt.Sprintf("overlapping chunk at offset %d, next offset %d", req.Offset, recv.offset))
	}
	if req.Offset > recv.offset {
		// Tell the leader which offset the upload has actually reached so it
		// can seek there and continue instead of restarting at zero.
		return n.rejectSnapshotChunkAt(term, recv.offset, fmt.Sprintf("chunk offset gap: got %d, want %d", req.Offset, recv.offset))
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
		return &InstallSnapshotChunkResponse{Term: term, Success: true, NextOffset: int64(recv.offset)}, nil
	}

	nextOffset := int64(recv.offset)
	resp, err := n.finishSnapshotChunk(recv)
	n.dropSnapshotChunkLocked(req.LeaderID)
	if err != nil {
		n.mu.Lock()
		term = n.term
		n.mu.Unlock()
		return n.rejectSnapshotChunk(term, err.Error())
	}
	resp.NextOffset = nextOffset
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
		Index:      recv.index,
		Term:       recv.snapshotTerm,
		Size:       int64(recv.offset),
		CRC32:      recv.crc.Sum32(),
		Membership: cloneMembership(recv.membership),
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

// rejectSnapshotChunkAt builds a rejection that also reports the offset the
// receiver has already accepted. The leader uses it to resume the upload from
// that point; a rejection without a usable NextOffset makes the leader restart
// from zero.
func (n *Node) rejectSnapshotChunkAt(term, nextOffset uint64, format string, args ...any) (*InstallSnapshotChunkResponse, error) {
	resp, err := n.rejectSnapshotChunk(term, format, args...)
	if resp != nil {
		resp.NextOffset = int64(nextOffset)
	}
	return resp, err
}
