package kvserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"

	"google.golang.org/grpc"
)

// Config configures one v2 KV node.
type Config struct {
	ID       string // defaults to RaftAddr
	RaftAddr string // gRPC listen address for KVService and RaftService
	DataDir  string
	Peers    []string // other Raft members (their RaftAddr)

	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration

	// Storage tuning. Zero values keep Pebble defaults.
	CacheSize    int64
	MemTableSize int64
	Logger       *slog.Logger
}

// Server is one v2 KV node: Raft + MVCC + Pebble + gRPC.
type Server struct {
	cfg Config
	log *slog.Logger

	engine    storage.Engine
	store     *mvcc.Store
	logStore  *raft.FileStore
	raft      *raft.Node
	transport *peerTransport
	grpcSrv   *grpc.Server
	lis       net.Listener

	mu       sync.Mutex
	started  bool
	stopped  bool
	stopOnce sync.Once
	stopErr  error
}

// New builds a server and opens its storage and Raft log. Start binds the
// listener and starts Raft.
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
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("kvserver: create data dir: %w", err)
	}

	engine, err := storage.Open(filepath.Join(cfg.DataDir, "db"), storage.Options{
		CacheSize:    cfg.CacheSize,
		MemTableSize: cfg.MemTableSize,
		Compression:  "snappy",
		SyncWrites:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("kvserver: open storage: %w", err)
	}
	logStore, err := raft.NewFileStore(filepath.Join(cfg.DataDir, "raft"))
	if err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("kvserver: open raft log: %w", err)
	}

	tr := newPeerTransport(cfg.Logger)
	s := &Server{
		cfg:       cfg,
		log:       cfg.Logger.With("node", cfg.ID),
		engine:    engine,
		store:     mvcc.New(engine),
		logStore:  logStore,
		transport: tr,
	}

	sm := &stateMachine{store: s.store}
	rn, err := raft.New(raft.Config{
		ID:                 cfg.ID,
		Peers:              append([]string(nil), cfg.Peers...),
		DataDir:            filepath.Join(cfg.DataDir, "raft"),
		ElectionTimeoutMin: cfg.ElectionTimeoutMin,
		ElectionTimeoutMax: cfg.ElectionTimeoutMax,
		HeartbeatInterval:  cfg.HeartbeatInterval,
		Logger:             cfg.Logger,
	}, logStore, sm, tr)
	if err != nil {
		_ = logStore.Close()
		_ = engine.Close()
		tr.Close()
		return nil, fmt.Errorf("kvserver: create raft: %w", err)
	}
	s.raft = rn

	s.grpcSrv = grpc.NewServer(
		grpc.MaxRecvMsgSize(16<<20),
		grpc.MaxSendMsgSize(16<<20),
	)
	pbv2.RegisterKVServiceServer(s.grpcSrv, &kvService{s: s})
	pbv2.RegisterRaftServiceServer(s.grpcSrv, &raftService{s: s})
	return s, nil
}

// Start binds the listener, serves gRPC and starts Raft.
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

	go func() {
		if err := s.grpcSrv.Serve(lis); err != nil {
			s.log.Error("grpc server stopped", "error", err)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.raft.Start(ctx); err != nil {
		_ = s.Stop(context.Background())
		return fmt.Errorf("kvserver: start raft: %w", err)
	}
	s.log.Info("kvserver started", "raft_addr", lis.Addr().String(), "peers", s.cfg.Peers)
	return nil
}

// Stop stops Raft, gRPC, the log store and Pebble. It is idempotent.
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
	if s.raft != nil {
		s.raft.Stop()
	}
	var errs []error
	if s.logStore != nil {
		if err := s.logStore.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.engine != nil {
		if err := s.engine.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.transport != nil {
		s.transport.Close()
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

// Raft exposes the Raft node for status/tests.
func (s *Server) Raft() *raft.Node { return s.raft }
