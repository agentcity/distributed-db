package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"distributed-db/internal/metrics"
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

	// Ranges configures the ranges hosted by this process. Every range shares
	// one Pebble engine at <DataDir>/db/shared; each range still gets its own
	// Raft log under <DataDir>/raft/range-<id>.
	//
	// The shared-engine layout is new. A pre-existing <DataDir>/db/range-<id>
	// directory from the older per-range layout is not migrated or read; it
	// can be deleted by the operator.
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
	RangeEpoch uint64

	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration

	// Storage tuning. Zero values keep Pebble defaults.
	CacheSize    int64
	MemTableSize int64
	Logger       *slog.Logger

	// Metrics, when non-nil, records every unary KV/Raft RPC duration and
	// error through one SLO tracker.
	Metrics *metrics.SLO
}

// Server is one v2 KV node: a RangeHost owning one or more independently
// replicated ranges plus the shared gRPC listener/server. All hosted ranges
// share one Pebble engine; each range sees its own keyspace through a
// storage.Prefix wrapper.
type Server struct {
	cfg Config
	log *slog.Logger

	// engine is the one Pebble engine shared by every group. It is opened in
	// New and closed in Stop after all groups have stopped.
	engine storage.Engine

	groups    map[uint64]*rangeGroup
	groupIDs  []uint64 // range order (ascending start key)
	defaultID uint64

	grpcSrv *grpc.Server
	lis     net.Listener

	// hostMu protects groups/groupIDs/defaultID/cfg.Ranges after Start.
	hostMu sync.RWMutex

	resolverCancel context.CancelFunc
	locksResolved  atomic.Uint64

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
	if persisted, err := loadPersistedRanges(cfg.DataDir); err != nil {
		return nil, err
	} else if len(persisted) > 0 {
		cfg.Ranges = persisted
	}
	ranges, err := normalizeRanges(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Ranges = ranges
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("kvserver: create data dir: %w", err)
	}
	shared, err := storage.Open(sharedEngineDir(cfg.DataDir), storage.Options{
		CacheSize:    cfg.CacheSize,
		MemTableSize: cfg.MemTableSize,
		Compression:  "snappy",
		SyncWrites:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("kvserver: open shared storage: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		log:       cfg.Logger.With("node", cfg.ID),
		engine:    shared,
		groups:    make(map[uint64]*rangeGroup, len(ranges)),
		groupIDs:  make([]uint64, 0, len(ranges)),
		defaultID: ranges[0].ID,
	}
	for _, rc := range ranges {
		g, err := newRangeGroup(cfg, shared, rc)
		if err != nil {
			var errs []error
			for _, id := range s.groupIDs {
				errs = append(errs, s.groups[id].stop())
			}
			if cerr := shared.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("kvserver: close shared storage: %w", cerr))
			}
			return nil, errors.Join(append([]error{err}, errs...)...)
		}
		s.groups[rc.ID] = g
		s.groupIDs = append(s.groupIDs, rc.ID)
	}

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(16 << 20),
		grpc.MaxSendMsgSize(16 << 20),
	}
	if s.cfg.Metrics != nil {
		opts = append(opts, grpc.ChainUnaryInterceptor(s.metricsInterceptor))
	}
	s.grpcSrv = grpc.NewServer(opts...)
	pbv2.RegisterKVServiceServer(s.grpcSrv, &kvService{s: s})
	pbv2.RegisterRaftServiceServer(s.grpcSrv, &raftService{s: s})
	return s, nil
}

func (s *Server) metricsInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	done := s.cfg.Metrics.Begin()
	resp, err := handler(ctx, req)
	done(err)
	return resp, err
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
	resolverCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.resolverCancel = cancel
	s.mu.Unlock()
	go s.resolveLocksLoop(resolverCtx)
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
	cancel := s.resolverCancel
	grpcSrv := s.grpcSrv
	lis := s.lis
	engine := s.engine
	s.engine = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

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
	// Snapshot the group set under hostMu. No split/merge can add a group
	// after stopped was set above, but taking hostMu keeps this consistent
	// with readers such as Groups and groupForID.
	s.hostMu.RLock()
	groups := make([]*rangeGroup, 0, len(s.groupIDs))
	for _, id := range s.groupIDs {
		if g := s.groups[id]; g != nil {
			groups = append(groups, g)
		}
	}
	s.hostMu.RUnlock()
	for _, g := range groups {
		if err := g.stop(); err != nil {
			errs = append(errs, err)
		}
	}
	// The groups only use prefix wrappers, so the real engine must be closed
	// here, after every group has stopped. Close is idempotent and the engine
	// is detached from the Server above, so a concurrent second Stop cannot
	// close it twice.
	if engine != nil {
		if err := engine.Close(); err != nil {
			errs = append(errs, fmt.Errorf("kvserver: close shared storage: %w", err))
		}
	}
	s.log.Info("kvserver stopped")
	return errors.Join(errs...)
}

// ResolvedLocks reports how many abandoned locks the background resolver has
// rolled back. It is useful for tests and metrics.
func (s *Server) ResolvedLocks() uint64 { return s.locksResolved.Load() }

// lockPrimaryInRange reports whether a lock's primary key belongs to this
// group's range (or the lock carries no primary pointer).
//
// A false result identifies a cross-range secondary lock. Its commit or
// rollback depends on the primary range, which this group cannot decide from
// local state: the local MVCC store has no record for the primary key, and
// treating the lock's TTL as a rollback would make a committed cross-range
// transaction partially visible. Such locks are left for an explicit
// ResolveLock or for the client, which checks the primary range before
// resolving.
func (g *rangeGroup) lockPrimaryInRange(primary []byte) bool {
	return len(primary) == 0 || g.containsKey(primary)
}

func (s *Server) resolveLocksLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s.resolveExpiredLocks(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.resolveExpiredLocks(now)
		}
	}
}

func (s *Server) resolveExpiredLocks(now time.Time) {
	for _, id := range s.Groups() {
		g, err := s.groupForID(id)
		if err != nil || !g.node.IsLeader() || g.isFrozen() {
			continue
		}
		keys, err := g.store.ExpiredLockKeys(now)
		if err != nil {
			s.log.Warn("scan expired locks", "range", id, "error", err)
			continue
		}
		for _, key := range keys {
			st, err := g.store.CheckTxnStatus(key, 0, now)
			if err != nil || (!st.Committed && !st.RolledBack) {
				continue
			}
			if !g.lockPrimaryInRange(st.LockPrimary) {
				// The local range cannot decide this lock's fate; the primary
				// range may have committed the transaction. Leave it for the
				// client/ResolveLock instead of exposing a partial commit.
				s.log.Debug("skip cross-range secondary lock",
					"range", id, "key", key, "primary", st.LockPrimary, "lock_ts", st.LockTS)
				continue
			}
			if err := g.resolveLock(context.Background(), st, key); err != nil {
				s.log.Debug("resolve expired lock", "range", id, "error", err)
				continue
			}
			s.locksResolved.Add(1)
		}
	}
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

// loadPersistedRanges loads the range map written after split/merge. A missing
// file means "use the static config".
func loadPersistedRanges(dataDir string) ([]RangeConfig, error) {
	path := filepath.Join(dataDir, "ranges.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kvserver: read range map: %w", err)
	}
	var ranges []RangeConfig
	if err := json.Unmarshal(data, &ranges); err != nil {
		return nil, fmt.Errorf("kvserver: decode range map: %w", err)
	}
	return ranges, nil
}

// persistRangesLocked writes the current range map atomically. Caller holds
// hostMu.
func (s *Server) persistRangesLocked() error {
	data, err := json.Marshal(s.cfg.Ranges)
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.DataDir, "ranges.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
