package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// applyCommand is the JSON payload stored in model.Entry.Value for the three
// transactional commands.
type applyCommand struct {
	StartTS   uint64          `json:"start_ts,omitempty"`
	CommitTS  uint64          `json:"commit_ts,omitempty"`
	Primary   []byte          `json:"primary,omitempty"`
	Mutations []mvcc.Mutation `json:"mutations,omitempty"`
	Keys      [][]byte        `json:"keys,omitempty"`
}

// stateMachine applies committed Raft commands to MVCC. Exactly one goroutine
// calls Apply (Raft's apply loop), so no extra mutex is needed.
type stateMachine struct {
	store *mvcc.Store
}

func (m *stateMachine) Apply(entry model.Entry) error {
	switch entry.Type {
	case model.CommandNoop:
		return nil
	case model.CommandPrewrite:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode prewrite: %w", err)
		}
		return m.store.Prewrite(cmd.Mutations, cmd.Primary, cmd.StartTS)
	case model.CommandCommit:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode commit: %w", err)
		}
		return m.store.Commit(cmd.Keys, cmd.StartTS, cmd.CommitTS)
	case model.CommandRollback:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode rollback: %w", err)
		}
		return m.store.Rollback(cmd.Keys, cmd.StartTS)
	default:
		return fmt.Errorf("kvserver: unknown command type %q", entry.Type)
	}
}

func encodeCommand(cmd applyCommand) ([]byte, error) { return json.Marshal(cmd) }

type kvService struct {
	pbv2.UnimplementedKVServiceServer
	s *Server
}

func (k *kvService) Get(ctx context.Context, req *pbv2.GetRequest) (*pbv2.GetResponse, error) {
	leader := k.s.raft.Leader()
	if err := k.s.raft.ReadBarrier(ctx); err != nil {
		return &pbv2.GetResponse{Leader: leader, Error: opError(err)}, nil
	}
	value, err := k.s.store.Get(req.GetKey(), req.GetTs())
	if errors.Is(err, mvcc.ErrKeyNotFound) {
		return &pbv2.GetResponse{Found: false, Leader: leader}, nil
	}
	if err != nil {
		return &pbv2.GetResponse{Leader: leader, Error: opError(err)}, nil
	}
	return &pbv2.GetResponse{Found: true, Value: value, Leader: leader}, nil
}

func (k *kvService) Scan(ctx context.Context, req *pbv2.ScanRequest) (*pbv2.ScanResponse, error) {
	leader := k.s.raft.Leader()
	if err := k.s.raft.ReadBarrier(ctx); err != nil {
		return &pbv2.ScanResponse{Leader: leader, Error: opError(err)}, nil
	}
	rows, err := k.s.store.Scan(req.GetStart(), req.GetEnd(), req.GetTs(), int(req.GetLimit()))
	if err != nil {
		return &pbv2.ScanResponse{Leader: leader, Error: opError(err)}, nil
	}
	out := make([]*pbv2.KV, 0, len(rows))
	for _, row := range rows {
		out = append(out, &pbv2.KV{Key: row.Key, Value: row.Value})
	}
	return &pbv2.ScanResponse{Kvs: out, Leader: leader}, nil
}

func (k *kvService) Prewrite(ctx context.Context, req *pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
	if len(req.GetMutations()) == 0 {
		return &pbv2.PrewriteResponse{Leader: k.s.raft.Leader(), Error: "empty mutations"}, nil
	}
	mutations := make([]mvcc.Mutation, 0, len(req.GetMutations()))
	for _, m := range req.GetMutations() {
		if m == nil {
			return &pbv2.PrewriteResponse{Leader: k.s.raft.Leader(), Error: "nil mutation"}, nil
		}
		mutations = append(mutations, mvcc.Mutation{
			Key:    m.GetKey(),
			Value:  m.GetValue(),
			Delete: m.GetDelete(),
		})
	}
	payload, err := encodeCommand(applyCommand{
		StartTS:   req.GetStartTs(),
		Primary:   req.GetPrimary(),
		Mutations: mutations,
	})
	if err != nil {
		return &pbv2.PrewriteResponse{Leader: k.s.raft.Leader(), Error: "encode"}, nil
	}
	if _, err := k.s.raft.Propose(ctx, model.Entry{Type: model.CommandPrewrite, Value: payload}); err != nil {
		return &pbv2.PrewriteResponse{Leader: k.s.raft.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.PrewriteResponse{Success: true, Leader: k.s.raft.Leader()}, nil
}

func (k *kvService) Commit(ctx context.Context, req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
	payload, err := encodeCommand(applyCommand{
		StartTS:  req.GetStartTs(),
		CommitTS: req.GetCommitTs(),
		Keys:     req.GetKeys(),
	})
	if err != nil {
		return &pbv2.CommitResponse{Leader: k.s.raft.Leader(), Error: "encode"}, nil
	}
	if _, err := k.s.raft.Propose(ctx, model.Entry{Type: model.CommandCommit, Value: payload}); err != nil {
		return &pbv2.CommitResponse{Leader: k.s.raft.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.CommitResponse{Success: true, Leader: k.s.raft.Leader()}, nil
}

func (k *kvService) Rollback(ctx context.Context, req *pbv2.RollbackRequest) (*pbv2.RollbackResponse, error) {
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(),
		Keys:    req.GetKeys(),
	})
	if err != nil {
		return &pbv2.RollbackResponse{Leader: k.s.raft.Leader(), Error: "encode"}, nil
	}
	if _, err := k.s.raft.Propose(ctx, model.Entry{Type: model.CommandRollback, Value: payload}); err != nil {
		return &pbv2.RollbackResponse{Leader: k.s.raft.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.RollbackResponse{Success: true, Leader: k.s.raft.Leader()}, nil
}

func (k *kvService) Status(_ context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	st := k.s.raft.Status()
	return &pbv2.StatusResponse{
		Id:          st.ID,
		Role:        string(st.Role),
		Term:        st.Term,
		Leader:      st.Leader,
		CommitIndex: st.CommitIndex,
		LastApplied: st.LastApplied,
	}, nil
}

type raftService struct {
	pbv2.UnimplementedRaftServiceServer
	s *Server
}

func (r *raftService) RequestVote(ctx context.Context, req *pbv2.RequestVoteRequest) (*pbv2.RequestVoteResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil RequestVote")
	}
	resp, err := r.s.raft.HandleRequestVote(ctx, &raft.RequestVoteRequest{
		Term:         req.GetTerm(),
		CandidateID:  req.GetCandidateId(),
		LastLogIndex: req.GetLastLogIndex(),
		LastLogTerm:  req.GetLastLogTerm(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.RequestVoteResponse{Term: resp.Term, VoteGranted: resp.VoteGranted}, nil
}

func (r *raftService) AppendEntries(ctx context.Context, req *pbv2.AppendEntriesRequest) (*pbv2.AppendEntriesResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil AppendEntries")
	}
	entries := make([]model.Entry, 0, len(req.GetEntries()))
	for _, e := range req.GetEntries() {
		if e == nil {
			continue
		}
		entries = append(entries, model.Entry{
			Term:  e.GetTerm(),
			Index: e.GetIndex(),
			Type:  model.CommandType(e.GetType()),
			Key:   string(e.GetKey()),
			Value: e.GetValue(),
		})
	}
	resp, err := r.s.raft.HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         req.GetTerm(),
		LeaderID:     req.GetLeaderId(),
		PrevLogIndex: req.GetPrevLogIndex(),
		PrevLogTerm:  req.GetPrevLogTerm(),
		Entries:      entries,
		LeaderCommit: req.GetLeaderCommit(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.AppendEntriesResponse{
		Term:          resp.Term,
		Success:       resp.Success,
		MatchIndex:    resp.MatchIndex,
		ConflictIndex: resp.ConflictIndex,
	}, nil
}

func opError(err error) string {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return "not leader"
	case errors.Is(err, raft.ErrTimeout):
		return "timeout"
	case errors.Is(err, raft.ErrStopped):
		return "stopped"
	case errors.Is(err, mvcc.ErrLocked):
		return "locked"
	case errors.Is(err, mvcc.ErrWriteConflict):
		return "write conflict"
	case errors.Is(err, mvcc.ErrAlreadyCommitted):
		return "already committed"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "internal error"
	}
}
