package kvserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// peerTransport implements raft.Transport over gRPC. M1 has no TLS/mTLS; that
// is added together with the production transport in M3.
//
// One transport is created per range group. rangeID is attached to every Raft
// RPC so the receiving RangeHost can dispatch it to the right group.
type peerTransport struct {
	log     *slog.Logger
	rangeID uint64

	mu      sync.Mutex
	clients map[string]pbv2.RaftServiceClient
	conns   map[string]*grpc.ClientConn
	addrs   map[string]string
	closed  bool
}

func newPeerTransport(log *slog.Logger, rangeID uint64, peerAddrs map[string]string) *peerTransport {
	if log == nil {
		log = slog.Default()
	}
	return &peerTransport{
		log:     log,
		rangeID: rangeID,
		clients: make(map[string]pbv2.RaftServiceClient),
		conns:   make(map[string]*grpc.ClientConn),
		addrs:   peerAddrs,
	}
}

func (t *peerTransport) client(peer string) (pbv2.RaftServiceClient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("kvserver: transport closed")
	}
	if c, ok := t.clients[peer]; ok {
		return c, nil
	}
	addr := peer
	if mapped := t.addrs[peer]; mapped != "" {
		addr = mapped
	}
	cc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16<<20),
			grpc.MaxCallSendMsgSize(16<<20),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("kvserver: dial %s: %w", peer, err)
	}
	c := pbv2.NewRaftServiceClient(cc)
	t.conns[peer] = cc
	t.clients[peer] = c
	return c, nil
}

func (t *peerTransport) RequestVote(ctx context.Context, peer string, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.RequestVote(ctx, &pbv2.RequestVoteRequest{
		Term:         req.Term,
		CandidateId:  req.CandidateID,
		LastLogIndex: req.LastLogIndex,
		LastLogTerm:  req.LastLogTerm,
		RangeId:      t.rangeID,
	})
	if err != nil {
		return nil, err
	}
	return &raft.RequestVoteResponse{Term: resp.GetTerm(), VoteGranted: resp.GetVoteGranted()}, nil
}

func (t *peerTransport) AppendEntries(ctx context.Context, peer string, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	entries := make([]*pbv2.LogEntry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, &pbv2.LogEntry{
			Term:  e.Term,
			Index: e.Index,
			Type:  string(e.Type),
			Key:   []byte(e.Key),
			Value: e.Value,
		})
	}
	resp, err := c.AppendEntries(ctx, &pbv2.AppendEntriesRequest{
		Term:         req.Term,
		LeaderId:     req.LeaderID,
		PrevLogIndex: req.PrevLogIndex,
		PrevLogTerm:  req.PrevLogTerm,
		Entries:      entries,
		LeaderCommit: req.LeaderCommit,
		RangeId:      t.rangeID,
	})
	if err != nil {
		return nil, err
	}
	return &raft.AppendEntriesResponse{
		Term:          resp.GetTerm(),
		Success:       resp.GetSuccess(),
		MatchIndex:    resp.GetMatchIndex(),
		ConflictIndex: resp.GetConflictIndex(),
	}, nil
}

func (t *peerTransport) InstallSnapshot(ctx context.Context, peer string, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.InstallSnapshot(ctx, &pbv2.InstallSnapshotRequest{
		Term:     req.Term,
		LeaderId: req.LeaderID,
		Snapshot: &pbv2.Snapshot{
			Index: req.Snapshot.Index,
			Term:  req.Snapshot.Term,
			Data:  req.Snapshot.Data,
		},
		RangeId: t.rangeID,
	})
	if err != nil {
		return nil, err
	}
	return &raft.InstallSnapshotResponse{
		Term:      resp.GetTerm(),
		Success:   resp.GetSuccess(),
		LastIndex: resp.GetLastIndex(),
	}, nil
}

func (t *peerTransport) InstallSnapshotChunk(ctx context.Context, peer string, req *raft.InstallSnapshotChunkRequest) (*raft.InstallSnapshotChunkResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.InstallSnapshotChunk(ctx, &pbv2.InstallSnapshotChunkRequest{
		RangeId:      t.rangeID,
		Term:         req.Term,
		LeaderId:     req.LeaderID,
		Index:        req.Index,
		SnapshotTerm: req.SnapshotTerm,
		Offset:       req.Offset,
		Data:         req.Data,
		Done:         req.Done,
		Size:         req.Size,
		Crc32:        req.CRC32,
	})
	if err != nil {
		return nil, err
	}
	return &raft.InstallSnapshotChunkResponse{
		Term:       resp.GetTerm(),
		Success:    resp.GetSuccess(),
		LastIndex:  resp.GetLastIndex(),
		Error:      resp.GetError(),
		NextOffset: resp.GetNextOffset(),
	}, nil
}

// TimeoutNow implements raft.TimeoutNowTransport. It is invoked by
// Node.TransferLeadership when this node transfers a range's leadership.
func (t *peerTransport) TimeoutNow(ctx context.Context, peer string, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.TimeoutNow(ctx, &pbv2.TimeoutNowRequest{
		RangeId:  t.rangeID,
		Term:     req.Term,
		LeaderId: req.LeaderID,
	})
	if err != nil {
		return nil, err
	}
	return &raft.TimeoutNowResponse{
		Term:    resp.GetTerm(),
		Success: resp.GetSuccess(),
	}, nil
}

func (t *peerTransport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	conns := t.conns
	t.conns = nil
	t.clients = nil
	t.mu.Unlock()
	for addr, cc := range conns {
		if err := cc.Close(); err != nil {
			t.log.Debug("close peer connection", "peer", addr, "error", err)
		}
	}
}

var _ raft.Transport = (*peerTransport)(nil)
var _ raft.SnapshotTransport = (*peerTransport)(nil)
var _ raft.SnapshotChunkTransport = (*peerTransport)(nil)
var _ raft.TimeoutNowTransport = (*peerTransport)(nil)
