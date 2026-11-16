package kvserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
)

// Config configures one v2 KV node.
type Config struct {
	ID       string // defaults to RaftAddr
	RaftAddr string // gRPC listen address for KVService and RaftService
	DataDir  string

	// Ranges configures the ranges hosted by this process. Each entry gets its
	// own Pebble engine, Raft log and state machine under
	// <DataDir>/db/range-<id> and <DataDir>/raft/range-<id>.
	//
	// When Ranges is empty, one range is built from the legacy RangeID,
	// RangeStart, RangeEnd and Peers fields (RangeID defaults to 1).
	Ranges []RangeConfig

	// Legacy single-range configuration. These fields remain supported and are
	// ignored when Ranges is non-empty.
	Peers      []string // other Raft members (their RaftAddr)
	RangeID    uint64
	RangeStart []byte
	RangeEnd   []byte

	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration

	// Storage tuning. Zero values keep Pebble defaults.
	CacheSize    int64
	MemTableSize int64
	Logger       *slog.Logger
}

// Server is one v2 KV node: a RangeHost owning one or more independently
// replicated ranges plus the shared gRPC listener/server.
type Server struct {
	cfg Config
	log *slog.Logger

	groups    map[uint64]*rangeGroup
	groupIDs  []uint64 // range order (ascending start key)
	defaultID uint64

	grpcSrv *grpc.Server
	lis     net.Listener

	mu       sync.Mutex
	started  bool
	stopped  bool
	stopOnce sync.Once
	stopErr  error
}

// New builds a server and opens storage and Raft logs for every configured
// range. Start binds the listener and starts all Raft groups.
func New(cfg Config) (*Server, error) {
	if cfg.RaftAddr == "" {
		return nil, errors.New("kvserver: RaftAddr is required")
	}
	if cfg.ID == "" {
		cfg.ID = cfg.RaftAddr
	}
	if cfg.DataDir == "" {
		return nil, errors.New("kvserver: DataDir is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ElectionTimeoutMin <= 0 {
		cfg.ElectionTimeoutMin = 300 * time.Millisecond
	}
	if cfg.ElectionTimeoutMax <= 0 {
		cfg.ElectionTimeoutMax = 600 * time.Millisecond
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 100 * time.Millisecond
	}
	ranges, err := normalizeRanges(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Ranges = ranges
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("kvserver: create data dir: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		log:       cfg.Logger.With("node", cfg.ID),
		groups:    make(map[uint64]*rangeGroup, len(ranges)),
		groupIDs:  make([]uint64, 0, len(ranges)),
		defaultID: ranges[0].ID,
	}
	for _, rc := range ranges {
		g, err := newRangeGroup(cfg, rc)
		if err != nil {
			var errs []error
			for _, id := range s.groupIDs {
				errs = append(errs, s.groups[id].stop())
			}
			return nil, errors.Join(append([]error{err}, errs...)...)
		}
		s.groups[rc.ID] = g
		s.groupIDs = append(s.groupIDs, rc.ID)
	}

	s.grpcSrv = grpc.NewServer(
		grpc.MaxRecvMsgSize(16<<20),
		grpc.MaxSendMsgSize(16<<20),
	)
	pbv2.RegisterKVServiceServer(s.grpcSrv, &kvService{s: s})
	pbv2.RegisterRaftServiceServer(s.grpcSrv, &raftService{s: s})
	return s, nil
}

// Start binds the listener once, serves gRPC and then starts every Raft group.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return errors.New("kvserver: stopped")
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	lis, err := net.Listen("tcp", s.cfg.RaftAddr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("kvserver: listen %s: %w", s.cfg.RaftAddr, err)
	}
	s.lis = lis
	s.started = true
	s.mu.Unlock()

	// Serve before starting Raft: groups may send RPCs to peers immediately
	// after their election timers fire.
	go func() {
		if err := s.grpcSrv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.log.Error("grpc server stopped", "error", err)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	for _, id := range s.groupIDs {
		if err := s.groups[id].start(ctx); err != nil {
			_ = s.Stop(context.Background())
			return err
		}
	}
	s.log.Info("kvserver started",
		"raft_addr", lis.Addr().String(), "ranges", s.groupIDs, "default_range", s.defaultID)
	return nil
}

// Stop stops all Raft groups, gRPC, the log stores and Pebble engines. It is
// idempotent.
func (s *Server) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.stopErr = s.stop(ctx)
	})
	return s.stopErr
}

func (s *Server) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	grpcSrv := s.grpcSrv
	lis := s.lis
	s.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		if grpcSrv != nil {
			grpcSrv.GracefulStop()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if grpcSrv != nil {
			grpcSrv.Stop()
		}
	}
	if lis != nil {
		_ = lis.Close()
	}

	var errs []error
	for _, id := range s.groupIDs {
		if err := s.groups[id].stop(); err != nil {
			errs = append(errs, err)
		}
	}
	s.log.Info("kvserver stopped")
	return errors.Join(errs...)
}

// Addr returns the bound gRPC address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lis != nil {
		return s.lis.Addr().String()
	}
	return s.cfg.RaftAddr
}

// Raft exposes the default group's Raft node for status/tests.
func (s *Server) Raft() *raft.Node {
	g := s.defaultGroup()
	if g == nil {
		return nil
	}
	return g.node
}

// Snapshot takes a state-machine snapshot of the default range and compacts
// its local log. It is used by tests and by an operator-triggered maintenance
// path.
func (s *Server) Snapshot() (raft.Snapshot, error) {
	g := s.defaultGroup()
	if g == nil {
		return raft.Snapshot{}, fmt.Errorf("%w: no default range", ErrRangeNotFound)
	}
	return g.node.Snapshot()
}
