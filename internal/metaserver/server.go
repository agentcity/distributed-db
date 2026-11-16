// Package metaserver exposes the v2 metadata and timestamp oracle over gRPC.
//
// Standalone mode (no Peers/Replicated) keeps the M1 in-memory behaviour. In
// replicated mode a 3/5-node Raft group stores the range map and the TSO
// high-water mark; AllocTS is a replicated command, so timestamps survive
// leader failover and never go backwards.
package metaserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"distributed-db/internal/meta"
	"distributed-db/internal/model"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/raftgrpc"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Config configures the meta server.
type Config struct {
	Addr   string
	Ranges []meta.Range
	Logger *slog.Logger

	// Replicated enables the Raft meta group. ID defaults to Addr, Peers are
	// the other meta gRPC addresses, DataDir holds the Raft WAL/snapshots.
	Replicated         bool
	ID                 string
	Peers              []string
	DataDir            string
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration
}

// Server is a gRPC MetaService backed by meta.Cluster, optionally replicated.
type Server struct {
	pbv2.UnimplementedMetaServiceServer

	cfg     Config
	log     *slog.Logger
	cluster *meta.Cluster

	raft      *raft.Node
	transport *raftgrpc.Transport
	logStore  *raft.FileStore

	mu      sync.Mutex
	grpc    *grpc.Server
	lis     net.Listener
	started bool
	stopped bool

	// tsMu serializes TSO reservations so two proposals cannot reserve the
	// same timestamp on one leader.
	tsMu sync.Mutex
}

// New creates a meta server. Start binds the listener and, in replicated mode,
// starts Raft.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("metaserver: Addr is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ID == "" {
		cfg.ID = cfg.Addr
	}
	if len(cfg.Peers) > 0 {
		cfg.Replicated = true
	}
	if cfg.Replicated {
		if cfg.DataDir == "" {
			return nil, errors.New("metaserver: DataDir is required in replicated mode")
		}
		if cfg.ElectionTimeoutMin <= 0 {
			cfg.ElectionTimeoutMin = 150 * time.Millisecond
		}
		if cfg.ElectionTimeoutMax <= 0 {
			cfg.ElectionTimeoutMax = 300 * time.Millisecond
		}
		if cfg.HeartbeatInterval <= 0 {
			cfg.HeartbeatInterval = 30 * time.Millisecond
		}
	}
	cluster := meta.New()
	if len(cfg.Ranges) > 0 {
		if err := cluster.SetRanges(cfg.Ranges); err != nil {
			return nil, err
		}
	}
	return &Server{cfg: cfg, log: cfg.Logger, cluster: cluster}, nil
}

// Start binds Addr, serves MetaService and starts the meta Raft group.
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("metaserver: already started")
	}
	lis, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.lis = lis
	s.grpc = grpc.NewServer()
	pbv2.RegisterMetaServiceServer(s.grpc, s)
	if s.cfg.Replicated {
		pbv2.RegisterRaftServiceServer(s.grpc, &raftService{s: s})
	}
	s.started = true
	s.mu.Unlock()

	go func() {
		if err := s.grpc.Serve(lis); err != nil {
			s.log.Error("meta grpc server stopped", "error", err)
		}
	}()

	if s.cfg.Replicated {
		if err := s.startRaft(ctx); err != nil {
			_ = s.Stop()
			return err
		}
	}

	s.log.Info("meta server started", "addr", lis.Addr().String(), "replicated", s.cfg.Replicated)

	go func() {
		<-ctx.Done()
		_ = s.Stop()
	}()
	return nil
}

func (s *Server) startRaft(ctx context.Context) error {
	dir := filepath.Join(s.cfg.DataDir, "raft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	store, err := raft.NewFileStore(dir)
	if err != nil {
		return err
	}
	tr := raftgrpc.New()
	sm := &stateMachine{cluster: s.cluster}
	node, err := raft.New(raft.Config{
		ID:                 s.cfg.ID,
		Peers:              append([]string(nil), s.cfg.Peers...),
		ElectionTimeoutMin: s.cfg.ElectionTimeoutMin,
		ElectionTimeoutMax: s.cfg.ElectionTimeoutMax,
		HeartbeatInterval:  s.cfg.HeartbeatInterval,
	}, store, sm, tr)
	if err != nil {
		tr.Close()
		return err
	}
	if err := node.Start(ctx); err != nil {
		tr.Close()
		return err
	}
	s.mu.Lock()
	s.logStore = store
	s.transport = tr
	s.raft = node
	s.mu.Unlock()
	return nil
}

// Stop stops Raft and the gRPC server. It is idempotent.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	grpcSrv := s.grpc
	lis := s.lis
	node := s.raft
	tr := s.transport
	s.mu.Unlock()

	if node != nil {
		node.Stop()
	}
	if tr != nil {
		tr.Close()
	}
	if grpcSrv != nil {
		grpcSrv.Stop()
	}
	if lis != nil {
		_ = lis.Close()
	}
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lis != nil {
		return s.lis.Addr().String()
	}
	return s.cfg.Addr
}

// Cluster exposes the local replicated state for tests/admin APIs.
func (s *Server) Cluster() *meta.Cluster { return s.cluster }

// Raft exposes the meta Raft node (nil in standalone mode).
func (s *Server) Raft() *raft.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raft
}

func (s *Server) requireLeader(ctx context.Context) error {
	if !s.cfg.Replicated {
		return nil
	}
	s.mu.Lock()
	node := s.raft
	s.mu.Unlock()
	if node == nil {
		return status.Error(codes.Unavailable, "meta raft is not started")
	}
	if leader := node.Leader(); leader != "" && leader != s.cfg.ID {
		return status.Errorf(codes.FailedPrecondition, "not meta leader; leader=%s", leader)
	}
	if err := node.ReadBarrier(ctx); err != nil {
		return status.Errorf(codes.Unavailable, "meta read barrier: %v", err)
	}
	return nil
}

// Leader implements pbv2.MetaServiceServer. A follower can answer with the
// current leader so clients can redirect without a second discovery service.
func (s *Server) Leader(context.Context, *pbv2.LeaderRequest) (*pbv2.LeaderResponse, error) {
	s.mu.Lock()
	node := s.raft
	s.mu.Unlock()
	if node == nil {
		return &pbv2.LeaderResponse{}, nil
	}
	st := node.Status()
	return &pbv2.LeaderResponse{Leader: st.Leader, Term: st.Term}, nil
}

// GetRange implements pbv2.MetaServiceServer.
func (s *Server) GetRange(ctx context.Context, req *pbv2.GetRangeRequest) (*pbv2.RangeInfo, error) {
	if err := s.requireLeader(ctx); err != nil {
		return nil, err
	}
	r, err := s.cluster.RangeForKey(req.GetKey())
	if err != nil {
		return nil, err
	}
	return rangeInfo(r), nil
}

// ListRanges implements pbv2.MetaServiceServer.
func (s *Server) ListRanges(ctx context.Context, _ *pbv2.ListRangesRequest) (*pbv2.ListRangesResponse, error) {
	if err := s.requireLeader(ctx); err != nil {
		return nil, err
	}
	rows := s.cluster.Snapshot()
	out := make([]*pbv2.RangeInfo, 0, len(rows))
	for i := range rows {
		out = append(out, rangeInfo(&rows[i]))
	}
	return &pbv2.ListRangesResponse{Ranges: out}, nil
}

func rangeInfo(r *meta.Range) *pbv2.RangeInfo {
	return &pbv2.RangeInfo{
		Id: r.ID, Start: r.Start, End: r.End,
		Replicas: append([]string(nil), r.Replicas...),
		Leader:   r.Leader, Version: r.Version,
	}
}

// AllocTS implements pbv2.MetaServiceServer. In replicated mode the leader
// reserves timestamps locally and commits the high-water mark through Raft.
func (s *Server) AllocTS(ctx context.Context, req *pbv2.AllocTSRequest) (*pbv2.AllocTSResponse, error) {
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	if !s.cfg.Replicated {
		start, count := s.cluster.AllocTS(n)
		return &pbv2.AllocTSResponse{Start: start, Count: int32(count)}, nil
	}
	if err := s.requireLeader(ctx); err != nil {
		return nil, err
	}

	s.tsMu.Lock()
	defer s.tsMu.Unlock()
	start, count := s.cluster.AllocTS(n)
	if count == 0 {
		return nil, status.Error(codes.ResourceExhausted, "TSO exhausted")
	}
	last := start + uint64(count-1)
	payload, err := json.Marshal(metaCommand{Last: last})
	if err != nil {
		return nil, err
	}
	if _, err := s.raft.Propose(ctx, model.Entry{Type: model.CommandMetaAllocTS, Value: payload}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "replicate TSO: %v", err)
	}
	return &pbv2.AllocTSResponse{Start: start, Count: int32(count)}, nil
}

// SetRanges applies a new range map through Raft. It is used by tests and the
// future admin API.
func (s *Server) SetRanges(ctx context.Context, ranges []meta.Range) error {
	if !s.cfg.Replicated {
		return s.cluster.SetRanges(ranges)
	}
	if err := s.requireLeader(ctx); err != nil {
		return err
	}
	payload, err := json.Marshal(metaCommand{Ranges: ranges})
	if err != nil {
		return err
	}
	_, err = s.raft.Propose(ctx, model.Entry{Type: model.CommandMetaSetRange, Value: payload})
	return err
}

// snapshotForTest is a small helper used by tests to force a meta snapshot.
func (s *Server) snapshotForTest() error {
	s.mu.Lock()
	node := s.raft
	s.mu.Unlock()
	if node == nil {
		return nil
	}
	_, err := node.Snapshot()
	return err
}
