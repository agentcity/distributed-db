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

	"distributed-db/internal/audit"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/metrics"
	"distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/rbac"
	"distributed-db/internal/storage"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Durability modes accepted by Config.DurabilityMode. Values are matched
// exactly (case-sensitive).
const (
	// DurabilitySync fsyncs every state-machine batch. It is the default and
	// the only production-safe profile.
	DurabilitySync = "sync"
	// DurabilityFast skips the per-batch state-machine fsync and relies on
	// replaying the durable Raft log after a crash. It is opt-in and not
	// production-safe until crash tests prove that replay always recovers the
	// state machine (see docs/PRODUCTION-PLAN.md, gate G1).
	DurabilityFast = "fast"
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
	Peers []string // other Raft members (their RaftAddr)
	// PeerAddrs optionally maps a logical Raft peer ID to the address the
	// transport should dial. When a peer is missing, its ID is used as the
	// address. Production normally leaves this nil; tests use it to keep
	// stable voter identities while routing Raft traffic through proxies.
	PeerAddrs  map[string]string
	RangeID    uint64
	RangeStart []byte
	RangeEnd   []byte
	RangeEpoch uint64

	// NoRanges starts the process without hosting any range. Ranges can then
	// be added through KVService.AddReplica (or a persisted range map can be
	// loaded). It exists for membership orchestration and tests; a normal data
	// node configures Ranges or the legacy fields above.
	NoRanges bool

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

	// LifecycleMetrics, when non-nil, receives the storage-lifecycle metrics:
	// snapshot runs/errors/age, MVCC GC runs/deleted versions/safe point and
	// the disk-full/free-space gauges. It is optional; logs are always written.
	LifecycleMetrics *metrics.Registry

	// AdminToken protects the destructive/admin KVService RPCs with an
	// "authorization: Bearer <token>" check. An empty value leaves admin auth
	// disabled unless AdminTokenRoles/AdminMTLSRoles configure credentials;
	// that default keeps development clusters working, but production clusters
	// must configure at least one credential.
	//
	// AdminToken always grants the admin role. Use AdminTokenRoles or
	// AdminMTLSRoles to issue operator or read-only credentials.
	//
	// Protected RPCs: SplitRange, MergeRanges, AddReplica, AddLearner,
	// PromoteReplica, RemoveVoter, RemoveReplica, TransferLeader and
	// UpdateRangeEpoch. Data-plane RPCs (Get, Scan, Prewrite, Commit,
	// CommitSingle, Rollback, CheckTxnStatus, ResolveLock), read-only RPCs
	// (Status, RangeStats, Membership) and every RaftService RPC never require
	// it. Rejections use gRPC Unauthenticated (no/bad credential) or
	// PermissionDenied (authenticated but insufficient role).
	//
	// The token is independent of TLS: it is checked by a unary interceptor on
	// both plaintext and TLS/mTLS listeners, while TLS still protects the token
	// in transit. With AdminMTLSRoles, a role can instead come from the Common
	// Name of a client certificate verified by the mTLS listener.
	//
	// AuditLogPath enables structured JSONL audit of protected RPC attempts
	// (allowed, denied and handler failures). See audit.Event for the format.
	// Empty config = historical behaviour: no admin auth, no audit.
	//
	// Outbound fan-out from metaserver to these KV admin RPCs is not wired yet;
	// see the exported WithAdminToken/AdminTokenDialOption helpers.
	AdminToken string

	// AdminTokenRoles maps additional bearer tokens to RBAC roles. Accepted
	// role values are admin, operator and read-only (case-insensitive, plus
	// readonly/read_only). An unknown role fails New rather than defaulting to
	// a permissive role. Tokens are stored as SHA-256 digests and are never
	// logged or included in errors.
	//
	// Permission mapping: admin RPCs require cluster_admin (admin and
	// operator); MetaService.SetRanges-style configuration is admin-only. On
	// the KV listener every protected RPC is cluster_admin, so an operator may
	// perform all of them and read-only may perform none.
	AdminTokenRoles map[string]string

	// AdminMTLSRoles maps the Common Name of a verified mTLS client
	// certificate to an RBAC role. It requires TLS.Server.ClientCAFile (mTLS);
	// New rejects the config otherwise so a plaintext peer can never spoof a
	// CN. Token credentials take precedence over a CN when both are present.
	AdminMTLSRoles map[string]string

	// AuditLogPath enables append-only JSON Lines audit of protected RPC
	// attempts at that path. The file is created with mode 0600 and the parent
	// directory is created if needed. There is no built-in rotation; rotate
	// externally and restrict access. The path requires at least one
	// configured credential (New rejects audit without auth).
	AuditLogPath string

	// AuditLogSync fsyncs the audit file after every record. It trades
	// throughput for durability; the default (false) relies on the OS page
	// cache. Audit write failures are logged but do not fail the RPC.
	AuditLogSync bool

	// Storage lifecycle. A completely zero lifecycle block selects
	// production-safe defaults so a node that does not configure maintenance
	// still compacts its Raft logs and collects old MVCC versions:
	//
	//   SnapshotInterval   DefaultSnapshotInterval (10m)
	//   GCInterval         DefaultGCInterval (10m)
	//   GCSafePointLag     DefaultGCSafePointLag (10m)
	//   DiskCheckInterval  DefaultDiskCheckInterval (1s)
	//   MinFreeDiskBytes   DefaultMinFreeDiskBytes (256 MiB)
	//
	// Setting any lifecycle field opts into explicit mode: a zero interval
	// then disables that loop (tests/dev), and a negative interval is also
	// treated as disabled. DisableBackgroundLifecycle disables all three loops
	// outright; the initial disk probe still runs.
	//
	// SnapshotInterval controls the existing Raft Snapshot/SnapshotToStore path
	// (persist the snapshot first, then truncate the log prefix). GCInterval
	// controls MVCC GC. One server-level goroutine runs both, visiting ranges
	// serially.
	SnapshotInterval  time.Duration
	GCInterval        time.Duration
	GCSafePointLag    time.Duration
	DiskCheckInterval time.Duration
	MinFreeDiskBytes  int64

	// DisableBackgroundLifecycle turns off the periodic snapshot, GC and disk
	// loops. It exists for tests and short-lived tools; production should
	// leave it false. An already-full disk is still detected at New/Start.
	DisableBackgroundLifecycle bool

	// GCSafePoint, when non-nil, supplies the MVCC GC safe point (a TSO
	// timestamp). It is the production hook for a real TSO/GC coordinator and
	// must return a conservative lower bound on the startTS of every
	// transaction that may still read, advancing monotonically. Returning 0
	// skips GC for that pass. When nil, the node uses the wall-clock lag
	// fallback documented on serverLifecycle.safePoint, capped by the oldest
	// live lock.
	GCSafePoint func() uint64

	// DurabilityMode selects how the state machine persists committed writes.
	// Accepted values are case-sensitive:
	//
	//   - "" or "sync": fsync every state-machine batch. This is the default
	//     and the production profile.
	//   - "fast": skip the per-batch state-machine fsync and rebuild committed
	//     state from the durable Raft log on restart. Opt-in only; it is not
	//     production-safe until crash tests prove that replay always recovers
	//     the state machine.
	//
	// Any other value makes New return an error, even when SyncStateMachine
	// is true, so a typo never silently changes durability.
	DurabilityMode string

	// SyncStateMachine forces a Pebble fsync on every state-machine batch,
	// independent of DurabilityMode. It is a backwards-compatible override:
	// true wins over DurabilityMode:"fast" (the node runs in sync mode), while
	// false (the zero value) leaves the choice to DurabilityMode, whose
	// default is sync.
	//
	// New code should prefer DurabilityMode; this field remains for existing
	// callers.
	SyncStateMachine bool

	// TLS configures TLS/mTLS for the gRPC listener (KVService and
	// RaftService) and for outbound Raft peer dials.
	//
	// The zero value keeps the historical plaintext transport. Setting
	// TLS.Server.CertFile and TLS.Server.KeyFile enables TLS on the listener
	// and makes every peer dial use TLS as well; there is no mixed mode and no
	// plaintext fallback. TLS.Server.ClientCAFile enables mTLS and rejects
	// clients without a certificate signed by that CA.
	//
	// Peer dials verify servers against TLS.Client.CAFile (or the system roots
	// when empty) and present the node certificate unless a distinct
	// TLS.Client.CertFile/KeyFile is configured. With mTLS enabled the node
	// certificate must be valid for client authentication too (ServerAuth and
	// ClientAuth extended key usages), or a distinct client certificate must be
	// supplied. TLS.Client.ServerName is required when peers are addressed by
	// IP address and the certificate has no IP SAN.
	TLS grpctls.Options
}

// resolveSyncWrites maps DurabilityMode and the SyncStateMachine override to
// the Pebble SyncWrites setting:
//
//	"" / "sync"          -> true
//	"fast"               -> false (unless SyncStateMachine is true)
//	anything else        -> error
//
// SyncStateMachine=true forces sync for every valid mode; its zero value never
// overrides the mode, so the default Config is sync.
func (c Config) resolveSyncWrites() (bool, error) {
	switch c.DurabilityMode {
	case "", DurabilitySync:
		return true, nil
	case DurabilityFast:
		return c.SyncStateMachine, nil
	default:
		return false, fmt.Errorf("kvserver: unknown durability mode %q (want %q or %q)",
			c.DurabilityMode, DurabilitySync, DurabilityFast)
	}
}

// Server is one v2 KV node: a RangeHost owning one or more independently
// replicated ranges plus the shared gRPC listener/server. All hosted ranges
// share one Pebble engine; each range sees its own keyspace through a
// storage.Prefix wrapper.
type Server struct {
	cfg Config
	log *slog.Logger

	// authz is the admin RBAC policy. It is disabled (Enabled() == false) when
	// no credential is configured, in which case the interceptor passes every
	// call through, preserving historical behaviour.
	authz *rbac.Policy

	// auditLog records protected admin attempts. It is nil when audit is
	// disabled or no server has started yet.
	auditLog *audit.Logger

	// engine is the one Pebble engine shared by every group. It is opened in
	// New and closed in Stop after all groups have stopped.
	engine storage.Engine

	// syncWrites is the resolved state-machine durability: true in sync mode
	// (the default) and false only for the explicit fast opt-in.
	syncWrites bool

	groups    map[uint64]*rangeGroup
	groupIDs  []uint64 // range order (ascending start key)
	defaultID uint64

	grpcSrv *grpc.Server
	lis     net.Listener

	// peerCreds is non-nil only when TLS is configured. It is handed to every
	// range group's peer transport so Raft traffic uses the same credentials
	// as the listener; a nil value means the node runs plaintext.
	peerCreds credentials.TransportCredentials

	// hostMu protects groups/groupIDs/defaultID/cfg.Ranges after Start.
	hostMu sync.RWMutex

	resolverCancel context.CancelFunc
	locksResolved  atomic.Uint64

	// lifecycle owns the single background snapshot/GC/disk-probe goroutine.
	// lifecycleCancel stops it and lifecycleDone is closed when it exits.
	lifecycle       *serverLifecycle
	lifecycleCancel context.CancelFunc
	lifecycleDone   <-chan struct{}

	// mu guards the lifecycle flags below and, via its read side, serializes
	// background snapshot/GC work with split/merge/remove (which take the
	// write side). It is not held by client reads or writes.
	mu       sync.RWMutex
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
	// Resolve durability before touching the filesystem so a bad mode fails
	// fast without creating or opening a data directory.
	syncWrites, err := cfg.resolveSyncWrites()
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Build and validate admin RBAC and open the audit sink before touching
	// storage, so a bad role configuration or audit path fails fast. An
	// unconfigured server gets a disabled policy and a nil audit logger.
	authz, err := buildKVAdminPolicy(cfg)
	if err != nil {
		return nil, err
	}
	auditLog, err := openKVAdminAudit(cfg)
	if err != nil {
		return nil, err
	}
	keepAudit := false
	defer func() {
		if !keepAudit && auditLog != nil {
			_ = auditLog.Close()
		}
	}()
	if cfg.ElectionTimeoutMin <= 0 {
		cfg.ElectionTimeoutMin = 300 * time.Millisecond
	}
	if cfg.ElectionTimeoutMax <= 0 {
		cfg.ElectionTimeoutMax = 600 * time.Millisecond
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 100 * time.Millisecond
	}
	// Resolve lifecycle defaults before opening storage. The disk guard needs
	// the free-space floor from the first write, and the initial probe runs in
	// New so an already-full disk is handled before Start.
	lifecycleCfg := resolveLifecycleConfig(cfg)
	// Resolve TLS before touching the filesystem so a half-configured
	// certificate/key pair fails fast. When TLS is configured there is no
	// plaintext fallback: either both server and peer credentials are built
	// or New returns an error.
	serverCreds, peerCreds, err := cfg.TLS.Credentials()
	if err != nil {
		return nil, fmt.Errorf("kvserver: TLS: %w", err)
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
		// SyncWrites is the resolved durability profile. It is true by
		// default (production): every state-machine batch is fsynced in
		// addition to the Raft log, so committed writes survive a crash even
		// if the log is unreadable. It is false only for the explicit
		// DurabilityMode:"fast" opt-in, where committed entries are replayed
		// from the durable Raft log on restart.
		SyncWrites: syncWrites,
	})
	if err != nil {
		return nil, fmt.Errorf("kvserver: open shared storage: %w", err)
	}

	// All ranges share this guarded engine. The wrapper is a no-op for reads
	// and rejects writes with ErrDiskFull while the disk guard is tripped.
	diskState := newDiskGuardState(cfg.DataDir, lifecycleCfg.minFreeDiskBytes, cfg.Logger)
	guarded := newDiskGuardEngine(shared, diskState)

	s := &Server{
		cfg:        cfg,
		log:        cfg.Logger.With("node", cfg.ID),
		authz:      authz,
		auditLog:   auditLog,
		engine:     guarded,
		syncWrites: syncWrites,
		peerCreds:  peerCreds,
		groups:     make(map[uint64]*rangeGroup, len(ranges)),
		groupIDs:   make([]uint64, 0, len(ranges)),
		defaultID:  0,
	}
	if len(ranges) > 0 {
		s.defaultID = ranges[0].ID
	}
	for _, rc := range ranges {
		g, err := newRangeGroup(cfg, guarded, rc, peerCreds)
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
	s.lifecycle = newServerLifecycle(s, lifecycleCfg, diskState)

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(16 << 20),
		grpc.MaxSendMsgSize(16 << 20),
	}
	if serverCreds != nil {
		opts = append(opts, grpc.Creds(serverCreds))
	}
	if s.cfg.Metrics != nil {
		opts = append(opts, grpc.ChainUnaryInterceptor(s.metricsInterceptor))
	}
	// Admin auth/RBAC runs before the disk guard so an unauthenticated or
	// forbidden destructive call is rejected as Unauthenticated /
	// PermissionDenied (not with a storage status) and before the handler so
	// protected RPCs never mutate state on a bad credential. A disabled policy
	// makes the interceptor a pass-through.
	opts = append(opts, grpc.ChainUnaryInterceptor(newKVAdminInterceptor(s.authz, s.auditLog)))
	// Reject mutating KV RPCs with a clear ResourceExhausted status while
	// storage is full. Reads keep working. The interceptor is cheap: one
	// atomic load per unary RPC.
	if s.lifecycle != nil {
		opts = append(opts, grpc.ChainUnaryInterceptor(s.diskGuardUnaryInterceptor))
	}
	s.grpcSrv = grpc.NewServer(opts...)
	RegisterKVServiceWithBackupScan(s.grpcSrv, s)
	pbv2.RegisterRaftServiceServer(s.grpcSrv, &raftService{s: s})
	keepAudit = true
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
	if s.stopped {
		s.mu.Unlock()
		cancel()
		return errors.New("kvserver: stopped")
	}
	s.resolverCancel = cancel
	if s.lifecycle != nil && s.lifecycle.enabled() {
		lifecycleCtx, lifecycleCancel := context.WithCancel(ctx)
		s.lifecycleCancel = lifecycleCancel
		s.lifecycleDone = s.lifecycle.start(lifecycleCtx)
	}
	s.mu.Unlock()
	go s.resolveLocksLoop(resolverCtx)
	var snapshotInterval, gcInterval time.Duration
	if s.lifecycle != nil {
		snapshotInterval = s.lifecycle.cfg.snapshotInterval
		gcInterval = s.lifecycle.cfg.gcInterval
	}
	s.log.Info("kvserver started",
		"raft_addr", lis.Addr().String(), "ranges", s.groupIDs, "default_range", s.defaultID,
		"sync_writes", s.syncWrites, "tls", s.cfg.TLS.Enabled(), "mtls", s.cfg.TLS.Server.ClientCAFile != "",
		"admin_auth", s.authz.Enabled(), "admin_audit", s.auditLog != nil,
		"snapshot_interval", snapshotInterval, "gc_interval", gcInterval)
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
	lifecycleCancel := s.lifecycleCancel
	lifecycleDone := s.lifecycleDone
	grpcSrv := s.grpcSrv
	lis := s.lis
	engine := s.engine
	s.engine = nil
	s.mu.Unlock()
	if lifecycleCancel != nil {
		lifecycleCancel()
	}
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

	// Wait for the single lifecycle goroutine before stopping groups or
	// closing storage. The loop takes Server.mu's read lock per operation, so
	// once it has exited no snapshot/GC can race shutdown. Waiting is
	// unconditional: closing storage under an in-flight snapshot/GC would be
	// the race this loop is meant to avoid. Cancellation is observed between
	// ranges and between ticks.
	if lifecycleDone != nil {
		<-lifecycleDone
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
	// gRPC has stopped above, so no interceptor can write concurrently.
	if s.auditLog != nil {
		if err := s.auditLog.Close(); err != nil {
			errs = append(errs, fmt.Errorf("kvserver: close admin audit log: %w", err))
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
