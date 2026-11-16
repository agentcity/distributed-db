// Package raftgrpc implements raft.Transport over the v2 gRPC RaftService.
package raftgrpc

import (
	"context"
	"fmt"
	"sync"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Transport dials peers lazily and implements raft.Transport and
// raft.SnapshotTransport.
type Transport struct {
	mu      sync.Mutex
	clients map[string]pbv2.RaftServiceClient
	conns   map[string]*grpc.ClientConn
}

// New returns an empty transport.
func New() *Transport {
	return &Transport{
		clients: make(map[string]pbv2.RaftServiceClient),
		conns:   make(map[string]*grpc.ClientConn),
	}
}

func (t *Transport) client(peer string) (pbv2.RaftServiceClient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.clients[peer]; ok {
		return c, nil
	}
	cc, err := grpc.NewClient(peer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("raftgrpc: dial %s: %w", peer, err)
	}
	c := pbv2.NewRaftServiceClient(cc)
	t.conns[peer] = cc
	t.clients[peer] = c
	return c, nil
}

// RequestVote implements raft.Transport.
func (t *Transport) RequestVote(ctx context.Context, peer string, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.RequestVote(ctx, &pbv2.RequestVoteRequest{
		Term: req.Term, CandidateId: req.CandidateID,
		LastLogIndex: req.LastLogIndex, LastLogTerm: req.LastLogTerm,
	})
	if err != nil {
		return nil, err
	}
	return &raft.RequestVoteResponse{
		Term: resp.GetTerm(), VoteGranted: resp.GetVoteGranted(),
	}, nil
}

// AppendEntries implements raft.Transport.
func (t *Transport) AppendEntries(ctx context.Context, peer string, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	entries := make([]*pbv2.LogEntry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, &pbv2.LogEntry{
			Term: e.Term, Index: e.Index, Type: string(e.Type), Key: []byte(e.Key), Value: e.Value,
		})
	}
	resp, err := c.AppendEntries(ctx, &pbv2.AppendEntriesRequest{
		Term: req.Term, LeaderId: req.LeaderID, PrevLogIndex: req.PrevLogIndex,
		PrevLogTerm: req.PrevLogTerm, Entries: entries, LeaderCommit: req.LeaderCommit,
	})
	if err != nil {
		return nil, err
	}
	return &raft.AppendEntriesResponse{
		Term: resp.GetTerm(), Success: resp.GetSuccess(),
		MatchIndex: resp.GetMatchIndex(), ConflictIndex: resp.GetConflictIndex(),
	}, nil
}

// InstallSnapshot implements raft.SnapshotTransport.
func (t *Transport) InstallSnapshot(ctx context.Context, peer string, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	c, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.InstallSnapshot(ctx, &pbv2.InstallSnapshotRequest{
		Term: req.Term, LeaderId: req.LeaderID,
		Snapshot: &pbv2.Snapshot{Index: req.Snapshot.Index, Term: req.Snapshot.Term, Data: req.Snapshot.Data},
	})
	if err != nil {
		return nil, err
	}
	return &raft.InstallSnapshotResponse{
		Term: resp.GetTerm(), Success: resp.GetSuccess(), LastIndex: resp.GetLastIndex(),
	}, nil
}

// Close closes all peer connections.
func (t *Transport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cc := range t.conns {
		_ = cc.Close()
	}
	t.conns = nil
	t.clients = nil
}

var _ raft.Transport = (*Transport)(nil)
var _ raft.SnapshotTransport = (*Transport)(nil)
