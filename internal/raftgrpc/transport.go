// Package raftgrpc implements raft.Transport over the v2 gRPC RaftService.
package raftgrpc

import (
	"context"
	"fmt"
	"sync"

	"distributed-db/internal/grpctls"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/pbconv"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Transport dials peers lazily and implements raft.Transport and
// raft.SnapshotTransport.
//
// A Transport built with New keeps the plaintext behavior used by standalone
// meta servers and development clusters. A Transport built with NewWithTLS
// uses the supplied client credentials for every peer dial; there is no
// plaintext fallback once TLS is configured.
type Transport struct {
	// creds is nil for plaintext transports and non-nil for TLS transports.
	creds credentials.TransportCredentials

	mu      sync.Mutex
	clients map[string]pbv2.RaftServiceClient
	conns   map[string]*grpc.ClientConn
}

// New returns an empty plaintext transport.
func New() *Transport {
	return newTransport(nil)
}

// NewWithTLS returns an empty transport that dials every peer with TLS (and
// mTLS when clientTLS carries a certificate). It validates the options and
// returns an error instead of silently falling back to plaintext.
func NewWithTLS(clientTLS grpctls.ClientOptions) (*Transport, error) {
	creds, err := grpctls.ClientCredentials(clientTLS)
	if err != nil {
		return nil, fmt.Errorf("raftgrpc: client TLS: %w", err)
	}
	return newTransport(creds), nil
}

func newTransport(creds credentials.TransportCredentials) *Transport {
	return &Transport{
		creds:   creds,
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
	creds := t.creds
	if creds == nil {
		// Plaintext is the explicit development default; a TLS transport
		// always carries non-nil creds.
		creds = insecure.NewCredentials()
	}
	cc, err := grpc.NewClient(peer, grpc.WithTransportCredentials(creds))
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
		Snapshot: &pbv2.Snapshot{
			Index: req.Snapshot.Index, Term: req.Snapshot.Term, Data: req.Snapshot.Data,
			Membership: pbconv.SnapshotMembershipToPB(req.Snapshot.Membership),
		},
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
