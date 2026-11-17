package metaserver

import (
	"context"
	"errors"

	"distributed-db/internal/model"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/pbconv"
	"distributed-db/internal/raft"
)

// raftService maps pbv2.RaftService to the meta Raft node.
type raftService struct {
	pbv2.UnimplementedRaftServiceServer
	s *Server
}

func (r *raftService) node() (*raft.Node, error) {
	r.s.mu.Lock()
	node := r.s.raft
	r.s.mu.Unlock()
	if node == nil {
		return nil, errors.New("metaserver: raft not started")
	}
	return node, nil
}

func (r *raftService) RequestVote(ctx context.Context, req *pbv2.RequestVoteRequest) (*pbv2.RequestVoteResponse, error) {
	node, err := r.node()
	if err != nil {
		return nil, err
	}
	resp, err := node.HandleRequestVote(ctx, &raft.RequestVoteRequest{
		Term: req.GetTerm(), CandidateID: req.GetCandidateId(),
		LastLogIndex: req.GetLastLogIndex(), LastLogTerm: req.GetLastLogTerm(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.RequestVoteResponse{Term: resp.Term, VoteGranted: resp.VoteGranted}, nil
}

func (r *raftService) AppendEntries(ctx context.Context, req *pbv2.AppendEntriesRequest) (*pbv2.AppendEntriesResponse, error) {
	node, err := r.node()
	if err != nil {
		return nil, err
	}
	entries := make([]model.Entry, 0, len(req.GetEntries()))
	for _, e := range req.GetEntries() {
		if e == nil {
			continue
		}
		entries = append(entries, model.Entry{
			Term: e.GetTerm(), Index: e.GetIndex(),
			Type: model.CommandType(e.GetType()), Key: string(e.GetKey()), Value: e.GetValue(),
		})
	}
	resp, err := node.HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term: req.GetTerm(), LeaderID: req.GetLeaderId(),
		PrevLogIndex: req.GetPrevLogIndex(), PrevLogTerm: req.GetPrevLogTerm(),
		Entries: entries, LeaderCommit: req.GetLeaderCommit(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.AppendEntriesResponse{
		Term: resp.Term, Success: resp.Success,
		MatchIndex: resp.MatchIndex, ConflictIndex: resp.ConflictIndex,
	}, nil
}

func (r *raftService) InstallSnapshot(ctx context.Context, req *pbv2.InstallSnapshotRequest) (*pbv2.InstallSnapshotResponse, error) {
	node, err := r.node()
	if err != nil {
		return nil, err
	}
	if req.GetSnapshot() == nil {
		return nil, errors.New("metaserver: nil snapshot")
	}
	resp, err := node.HandleInstallSnapshot(ctx, &raft.InstallSnapshotRequest{
		Term: req.GetTerm(), LeaderID: req.GetLeaderId(),
		Snapshot: raft.Snapshot{
			Index: req.GetSnapshot().GetIndex(), Term: req.GetSnapshot().GetTerm(),
			Data:       req.GetSnapshot().GetData(),
			Membership: pbconv.SnapshotMembershipFromPB(req.GetSnapshot().GetMembership()),
		},
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.InstallSnapshotResponse{
		Term: resp.Term, Success: resp.Success, LastIndex: resp.LastIndex,
	}, nil
}
