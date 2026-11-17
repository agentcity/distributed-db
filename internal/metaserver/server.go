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
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"distributed-db/internal/audit"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/meta"
	"distributed-db/internal/model"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/raftgrpc"
	"distributed-db/internal/rbac"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Config configures the meta server.
type Config struct {
	Addr   string
	Ranges []meta.Range
	Logger *slog.Logger

	// AdminToken protects the meta admin RPCs (SetRanges, SplitRange,
	// MergeRanges) with an "authorization: Bearer <token>" check. An empty
	// value leaves admin auth disabled unless AdminTokenRoles/AdminMTLSRoles
	// configure credentials; that default keeps development clusters working.
	//
	// AdminToken always grants the admin role. Use AdminTokenRoles or
	// AdminMTLSRoles to issue operator or read-only credentials.
	//
	// The token is independent of TLS: it is checked by a gRPC unary
	// interceptor and works on both plaintext and TLS/mTLS listeners. It only
	// authenticates callers; TLS still protects the token in transit. With
	// AdminMTLSRoles, a role can instead come from the Common Name of a client
	// certificate verified by the mTLS listener.
	//
	// Role policy: SetRanges is configuration (admin only); SplitRange and
	// MergeRanges are cluster operations (admin and operator); read-only is
	// denied every meta admin RPC. Read-only RPCs (AllocTS, GetRange,
	// ListRanges, Leader) never require a credential. KVService admin RPCs are
	// served by the kvserver listener and are not covered here.
	AdminToken string

	// AdminTokenRoles maps additional bearer tokens to RBAC roles. Accepted
	// role values are admin, operator and read-only (case-insensitive, plus
	// readonly/read_only). An unknown role fails New. Tokens are stored only as
	// SHA-256 digests and never logged or included in errors.
	AdminTokenRoles map[string]string

	// AdminMTLSRoles maps the Common Name of a verified mTLS client certificate
	// to an RBAC role. It requires TLS.Server.ClientCAFile (mTLS); New rejects
	// the configuration otherwise. Token credentials take precedence over a CN
	// when both are present.
	AdminMTLSRoles map[string]string

	// AuditLogPath enables append-only JSON Lines audit of protected RPC
	// attempts at that path (mode 0600, parent directory created if needed).
	// There is no built-in rotation; rotate externally. The path requires at
	// least one configured credential.
	AuditLogPath string

	// AuditLogSync fsyncs the audit file after every record. Audit write
	// failures are logged but do not fail the RPC.
	AuditLogSync bool

	// Replicated enables the Raft meta group. ID defaults to Addr, Peers are
	// the other meta gRPC addresses, DataDir holds the Raft WAL/snapshots.
	Replicated         bool
	ID                 string
	Peers              []string
	DataDir            string
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration

	// TLS configures TLS/mTLS for the meta gRPC listener and for every
	// outbound dial the server makes: Raft peer transport, KV scheduler,
	// membership, leader rebalance and snapshot RPCs.
	//
	// The zero value keeps the historical plaintext transport. Setting
	// TLS.Server.CertFile and TLS.Server.KeyFile enables TLS on the listener
	// and on all outbound dials; there is no plaintext fallback.
	// TLS.Server.ClientCAFile enables mTLS, and TLS.Client.CAFile selects the
	// trust root used to verify KV nodes and meta peers. Outbound dials present
	// the meta node certificate unless TLS.Client.CertFile/KeyFile is set, so
	// with mTLS on the KV side that certificate must also be valid for client
	// authentication.
	TLS grpctls.Options
}

// Server is a gRPC MetaService backed by meta.Cluster, optionally replicated.
type Server struct {
	pbv2.UnimplementedMetaServiceServer

	cfg     Config
	log     *slog.Logger
	cluster *meta.Cluster

	// authz is the meta admin RBAC policy. It is disabled (Enabled() == false)
	// when no credential is configured, in which case the interceptor passes
	// every call through, preserving historical behaviour.
	authz *rbac.Policy

	// auditLog records protected admin attempts. It is nil when audit is
	// disabled.
	auditLog *audit.Logger

	raft      *raft.Node
	transport *raftgrpc.Transport
	logStore  *raft.FileStore

	// serverCreds and peerCreds are non-nil only when TLS is configured.
	// serverCreds secures the listener; peerCreds is shared by every outbound
	// dial so the server never mixes TLS and plaintext transports.
	serverCreds credentials.TransportCredentials
	peerCreds   credentials.TransportCredentials

	mu      sync.Mutex
	grpc    *grpc.Server
	lis     net.Listener
	started bool
	stopped bool

	// autoSplit tracks the optional background auto-split loop. Access is
	// guarded by mu and Stop cancels it.
	autoSplit *autoSplitLoop

	// lifecycle tracks the optional combined auto-split + auto-merge loop.
	// Access is guarded by mu and Stop cancels and waits for it.
	lifecycle *lifecycleLoop

	// rebalance tracks the optional leader-rebalancing loop. Access is guarded
	// by mu and Stop cancels and waits for it.
	rebalance *rebalanceLoop

	// tsMu serializes TSO reservations so two proposals cannot reserve the
	// same timestamp on one leader.
	tsMu sync.Mutex
}

// autoSplitLoop is the cancel handle for one StartAutoSplit goroutine.
type autoSplitLoop struct {
	cancel context.CancelFunc
}

// lifecycleLoop is the cancel/join handle for one StartLifecycle goroutine.
type lifecycleLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// rebalanceLoop is the cancel/join handle for one StartLeaderRebalance
// goroutine.
type rebalanceLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
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
	// Resolve TLS before any listener is bound so a half-configured
	// certificate/key pair fails fast and never silently degrades to plaintext.
	serverCreds, peerCreds, err := cfg.TLS.Credentials()
	if err != nil {
		return nil, fmt.Errorf("metaserver: TLS: %w", err)
	}
	cluster := meta.New()
	if len(cfg.Ranges) > 0 {
		if err := cluster.SetRanges(cfg.Ranges); err != nil {
			return nil, err
		}
	}
	// Validate admin RBAC and open the audit sink before returning the server,
	// so a bad role configuration or audit path fails New rather than Start.
	authz, err := buildMetaAdminPolicy(cfg)
	if err != nil {
		return nil, err
	}
	auditLog, err := openMetaAdminAudit(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:         cfg,
		log:         cfg.Logger,
		cluster:     cluster,
		authz:       authz,
		auditLog:    auditLog,
		serverCreds: serverCreds,
		peerCreds:   peerCreds,
	}, nil
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
	var serverOpts []grpc.ServerOption
	if s.serverCreds != nil {
		serverOpts = append(serverOpts, grpc.Creds(s.serverCreds))
	}
	// The admin auth/RBAC check is independent of the transport: it is
	// installed even on plaintext listeners, and a no-op when no credential is
	// configured. Verification of an mTLS CN still requires the listener's
	// client CA.
	serverOpts = append(serverOpts, grpc.UnaryInterceptor(newMetaAdminInterceptor(s.authz, s.auditLog)))
	s.grpc = grpc.NewServer(serverOpts...)
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

	s.log.Info("meta server started",
		"addr", lis.Addr().String(), "replicated", s.cfg.Replicated,
		"tls", s.cfg.TLS.Enabled(), "mtls", s.cfg.TLS.Server.ClientCAFile != "",
		"admin_auth", s.authz.Enabled(), "admin_audit", s.auditLog != nil)

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
	var tr *raftgrpc.Transport
	if s.cfg.TLS.Enabled() {
		tr, err = raftgrpc.NewWithTLS(s.cfg.TLS.ClientOptions())
		if err != nil {
			return err
		}
	} else {
		tr = raftgrpc.New()
	}
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
	autoSplit := s.autoSplit
	s.autoSplit = nil
	lifecycle := s.lifecycle
	s.lifecycle = nil
	rebalance := s.rebalance
	s.rebalance = nil
	grpcSrv := s.grpc
	lis := s.lis
	node := s.raft
	tr := s.transport
	s.mu.Unlock()

	if autoSplit != nil {
		autoSplit.cancel()
	}
	if lifecycle != nil {
		lifecycle.cancel()
		<-lifecycle.done
	}
	if rebalance != nil {
		rebalance.cancel()
		<-rebalance.done
	}
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
	// gRPC has stopped above, so no interceptor can write concurrently.
	if s.auditLog != nil {
		return s.auditLog.Close()
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

// SetRanges implements the admin RPC. The range map is committed through Raft;
// callers must update it only after the data movement has completed.
func (s *Server) SetRanges(ctx context.Context, req *pbv2.SetRangesRequest) (*pbv2.SetRangesResponse, error) {
	ranges := make([]meta.Range, 0, len(req.GetRanges()))
	for _, r := range req.GetRanges() {
		if r == nil {
			return &pbv2.SetRangesResponse{Error: "nil range"}, nil
		}
		ranges = append(ranges, meta.Range{
			ID: r.GetId(), Start: r.GetStart(), End: r.GetEnd(),
			Replicas: append([]string(nil), r.GetReplicas()...),
			Leader:   r.GetLeader(), Version: r.GetVersion(),
		})
	}
	if err := s.ApplyRanges(ctx, ranges); err != nil {
		return &pbv2.SetRangesResponse{Error: err.Error()}, nil
	}
	return &pbv2.SetRangesResponse{Success: true}, nil
}

// ApplyRanges applies a new range map through Raft. It is used by the admin
// RPC and by split/merge orchestration.
func (s *Server) ApplyRanges(ctx context.Context, ranges []meta.Range) error {
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
