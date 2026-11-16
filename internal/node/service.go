package node

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"distributed-db/internal/model"
	"distributed-db/internal/pb"
	"distributed-db/internal/raft"
)

// Service implements pb.NodeServiceServer. Raft RPCs are delegated to the
// local Raft node; client operations are served by this node directly and
// require it to be the shard leader. Not-leader is a normal response with the
// known leader address and an error string, never a gRPC status error.
type Service struct {
	pb.UnimplementedNodeServiceServer
	node *Node
}

// NewService wraps a node in the gRPC NodeService implementation.
func NewService(n *Node) *Service {
	return &Service{node: n}
}

// RequestVote handles a Raft vote request.
func (s *Service) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil RequestVoteRequest")
	}
	resp, err := s.node.raft.HandleRequestVote(ctx, &raft.RequestVoteRequest{
		Term:         req.GetTerm(),
		CandidateID:  req.GetCandidateId(),
		LastLogIndex: req.GetLastLogIndex(),
		LastLogTerm:  req.GetLastLogTerm(),
	})
	if err != nil {
		s.node.metrics.errorsTotal.Inc()
		return nil, status.Errorf(codes.Internal, "request vote: %v", err)
	}
	return &pb.RequestVoteResponse{
		Term:        resp.Term,
		VoteGranted: resp.VoteGranted,
	}, nil
}

// AppendEntries handles a Raft append/heartbeat request.
func (s *Service) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil AppendEntriesRequest")
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
			Key:   e.GetKey(),
			Value: e.GetValue(),
		})
	}
	resp, err := s.node.raft.HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         req.GetTerm(),
		LeaderID:     req.GetLeaderId(),
		PrevLogIndex: req.GetPrevLogIndex(),
		PrevLogTerm:  req.GetPrevLogTerm(),
		Entries:      entries,
		LeaderCommit: req.GetLeaderCommit(),
	})
	if err != nil {
		s.node.metrics.errorsTotal.Inc()
		return nil, status.Errorf(codes.Internal, "append entries: %v", err)
	}
	return &pb.AppendEntriesResponse{
		Term:          resp.Term,
		Success:       resp.Success,
		MatchIndex:    resp.MatchIndex,
		ConflictIndex: resp.ConflictIndex,
	}, nil
}

// Get performs a linearizable local read: it takes a Raft read barrier and
// only then reads from the KV state machine.
func (s *Service) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	n := s.node
	n.metrics.getTotal.Inc()
	n.ops.Add(1)

	leader := n.raft.Leader()
	if err := n.raft.ReadBarrier(ctx); err != nil {
		n.metrics.errorsTotal.Inc()
		n.logDebug("get read barrier failed", "error", err)
		return &pb.GetResponse{Leader: leader, Error: opErrorText(err)}, nil
	}
	value, ok := n.kv.Get(req.GetKey())
	if !ok {
		return &pb.GetResponse{Found: false, Leader: leader}, nil
	}
	return &pb.GetResponse{Found: true, Value: value, Leader: leader}, nil
}

// Put proposes a put command and returns once it is committed and applied.
func (s *Service) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	n := s.node
	n.metrics.putTotal.Inc()
	n.ops.Add(1)

	index, err := n.propose(ctx, model.Entry{
		Type:  model.CommandPut,
		Key:   req.GetKey(),
		Value: req.GetValue(),
	})
	leader := n.raft.Leader()
	if err != nil {
		n.metrics.errorsTotal.Inc()
		n.logDebug("put proposal failed", "error", err)
		return &pb.PutResponse{Success: false, Leader: leader, Error: opErrorText(err)}, nil
	}
	return &pb.PutResponse{Success: true, Leader: leader, Index: index}, nil
}

// Delete proposes a delete command and returns once it is committed and
// applied.
func (s *Service) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	n := s.node
	n.metrics.deleteTotal.Inc()
	n.ops.Add(1)

	index, err := n.propose(ctx, model.Entry{
		Type: model.CommandDelete,
		Key:  req.GetKey(),
	})
	leader := n.raft.Leader()
	if err != nil {
		n.metrics.errorsTotal.Inc()
		n.logDebug("delete proposal failed", "error", err)
		return &pb.DeleteResponse{Success: false, Leader: leader, Error: opErrorText(err)}, nil
	}
	return &pb.DeleteResponse{Success: true, Leader: leader, Index: index}, nil
}

// Status reports the Raft status plus this node's shard.
func (s *Service) Status(_ context.Context, _ *pb.StatusRequest) (*pb.StatusResponse, error) {
	n := s.node
	st := n.raft.Status()
	return &pb.StatusResponse{
		Id:          st.ID,
		Role:        string(st.Role),
		Term:        st.Term,
		Leader:      st.Leader,
		CommitIndex: st.CommitIndex,
		LastIndex:   st.LastIndex,
		LastApplied: st.LastApplied,
		Shard:       n.cfg.Shard,
	}, nil
}

// opErrorText maps internal Raft errors to the short error strings that are
// part of the wire contract. Internal details are never leaked to clients.
func opErrorText(err error) string {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return "not leader"
	case errors.Is(err, raft.ErrTimeout):
		return "timeout"
	case errors.Is(err, raft.ErrStopped):
		return "stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "internal error"
	}
}

func (n *Node) logDebug(msg string, args ...any) {
	n.log.Debug(msg, args...)
}
