// Package node wires the Raft core and the KV state machine into a running
// database node: gRPC and HTTP servers, a gRPC peer transport, metrics and an
// optional coordinator load reporter.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"

	"distributed-db/internal/kv"
	"distributed-db/internal/model"
	"distributed-db/internal/pb"
	"distributed-db/internal/raft"
)

// ErrStopped is returned by Start when the node has already been stopped.
var ErrStopped = errors.New("node: stopped")

const (
	defaultElectionTimeoutMin = 300 * time.Millisecond
	defaultElectionTimeoutMax = 600 * time.Millisecond
	defaultHeartbeatInterval  = 100 * time.Millisecond

	// gracefulStopTimeout bounds each graceful shutdown stage (HTTP and gRPC)
	// so that a stuck client can never block Stop forever.
	gracefulStopTimeout = 5 * time.Second
	// reportInterval is how often the optional load reporter talks to the
	// coordinator.
	reportInterval = 5 * time.Second
	// maxRequestBody caps the size of a PUT value accepted over HTTP.
	maxRequestBody = 8 << 20
	// maxGRPCMessageSize raises the gRPC default (4 MiB) so that replicated
	// values accepted over HTTP also fit through Raft RPCs.
	maxGRPCMessageSize = 16 << 20

	httpReadHeaderTimeout = 10 * time.Second
	httpIdleTimeout       = 60 * time.Second
)

// Config configures a database node. See docs/ARCHITECTURE.md ("Node").
type Config struct {
	ID              string   // defaults to RaftAddr
	Shard           string   // logical shard this node belongs to
	RaftAddr        string   // gRPC listen address for NodeService
	HTTPAddr        string   // HTTP listen address; empty disables the HTTP server
	Peers           []string // other members of the Raft group (their RaftAddr)
	DataDir         string   // root data directory (raft/ and kv/ live below it)
	CoordinatorAddr string   // optional: periodic load report target

	AuthUser string
	AuthPass string
	// AuthHash is an optional bcrypt hash of AuthPass. When set it takes
	// precedence over AuthPass. Production deployments should always set a
	// hash instead of a plaintext password.
	AuthHash string

	TLSCert string
	TLSKey  string
	TLSCA   string // if empty and TLS is enabled, peer clients skip verification

	RateLimit float64 // requests/sec, 0 disables
	RateBurst int

	Logger *slog.Logger
}

// Node is a single database node process. Create it with New, run it with
// Start and shut it down with Stop. A Node must not be restarted after Stop.
type Node struct {
	cfg Config
	log *slog.Logger

	raft      *raft.Node
	fileStore *raft.FileStore
	kv        *kv.Store
	transport *PeerTransport
	reporter  *loadReporter
	metrics   *metrics
	limiter   *rate.Limiter

	grpcSrv *grpc.Server
	httpSrv *http.Server

	// mu guards the lifecycle fields below. It is an RWMutex so the metrics
	// collector can take a read lock while reading the Raft status.
	mu          sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
	grpcLn      net.Listener
	httpLn      net.Listener
	started     bool
	stopped     bool
	raftStarted bool

	stopOnce sync.Once
	stopErr  error
	wg       sync.WaitGroup // background reporters

	ops atomic.Uint64 // total KV operations, used by the load reporter
}

// New builds a node: it opens the Raft WAL and the KV state machine, creates
// the peer transport and the Raft node, and prepares (but does not start) the
// gRPC and HTTP servers.
func New(cfg Config) (*Node, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger

	// Validate TLS material up front so a bad certificate does not leave
	// stores open.
	serverCreds, err := serverTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	tr, err := NewPeerTransport(cfg)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		tr.Close()
		return nil, fmt.Errorf("node: create data dir: %w", err)
	}
	fileStore, err := raft.NewFileStore(filepath.Join(cfg.DataDir, "raft"))
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("node: open raft store: %w", err)
	}
	kvStore, err := kv.Open(filepath.Join(cfg.DataDir, "kv"))
	if err != nil {
		_ = fileStore.Close()
		tr.Close()
		return nil, fmt.Errorf("node: open kv store: %w", err)
	}

	n := &Node{
		cfg:       cfg,
		log:       log,
		fileStore: fileStore,
		kv:        kvStore,
		transport: tr,
	}
	n.metrics = newMetrics(n)

	raftCfg := raft.Config{
		ID:                 cfg.ID,
		Peers:              append([]string(nil), cfg.Peers...),
		DataDir:            filepath.Join(cfg.DataDir, "raft"),
		ElectionTimeoutMin: defaultElectionTimeoutMin,
		ElectionTimeoutMax: defaultElectionTimeoutMax,
		HeartbeatInterval:  defaultHeartbeatInterval,
		Logger:             log,
	}
	rn, err := raft.New(raftCfg, fileStore, kvStore, tr)
	if err != nil {
		_ = fileStore.Close()
		_ = kvStore.Close()
		tr.Close()
		return nil, fmt.Errorf("node: create raft node: %w", err)
	}
	n.raft = rn

	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxGRPCMessageSize),
		grpc.MaxSendMsgSize(maxGRPCMessageSize),
	}
	if serverCreds != nil {
		serverOpts = append(serverOpts, grpc.Creds(serverCreds))
	}
	n.grpcSrv = grpc.NewServer(serverOpts...)
	pb.RegisterNodeServiceServer(n.grpcSrv, NewService(n))

	if cfg.RateLimit > 0 {
		n.limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.RateBurst)
	}

	n.httpSrv = &http.Server{
		Handler:           n.buildHTTPHandler(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	if cfg.CoordinatorAddr != "" {
		rep, err := newLoadReporter(cfg, n)
		if err != nil {
			// Reporting is best effort: a bad coordinator address must not
			// prevent the node from serving.
			log.Warn("load reporter disabled", "addr", cfg.CoordinatorAddr, "error", err)
		} else {
			n.reporter = rep
		}
	}

	return n, nil
}

func normalizeConfig(cfg Config) (Config, error) {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.RaftAddr = strings.TrimSpace(cfg.RaftAddr)
	cfg.HTTPAddr = strings.TrimSpace(cfg.HTTPAddr)
	cfg.DataDir = strings.TrimSpace(cfg.DataDir)
	cfg.CoordinatorAddr = strings.TrimSpace(cfg.CoordinatorAddr)
	cfg.AuthUser = strings.TrimSpace(cfg.AuthUser)
	cfg.TLSCert = strings.TrimSpace(cfg.TLSCert)
	cfg.TLSKey = strings.TrimSpace(cfg.TLSKey)
	cfg.TLSCA = strings.TrimSpace(cfg.TLSCA)

	if cfg.RaftAddr == "" {
		return cfg, errors.New("node: RaftAddr is required")
	}
	if cfg.ID == "" {
		cfg.ID = cfg.RaftAddr
	}
	if cfg.DataDir == "" {
		return cfg, errors.New("node: DataDir is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Logger = cfg.Logger.With(slog.String("node", cfg.ID))

	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, errors.New("node: TLSCert and TLSKey must be set together")
	}
	if cfg.TLSCA != "" && cfg.TLSCert == "" {
		cfg.Logger.Warn("TLS CA configured without TLSCert/TLSKey; TLS is disabled")
	}

	if cfg.RateLimit < 0 || math.IsNaN(cfg.RateLimit) || math.IsInf(cfg.RateLimit, 0) {
		return cfg, fmt.Errorf("node: invalid RateLimit %v", cfg.RateLimit)
	}
	if cfg.RateLimit > 0 && cfg.RateBurst <= 0 {
		cfg.RateBurst = int(math.Ceil(cfg.RateLimit))
		if cfg.RateBurst < 1 {
			cfg.RateBurst = 1
		}
	}
	if cfg.AuthHash != "" {
		if _, err := bcrypt.Cost([]byte(cfg.AuthHash)); err != nil {
			return cfg, fmt.Errorf("node: invalid AuthHash: %w", err)
		}
	}

	// Peers are the *other* members: drop blanks, duplicates and self so a
	// sloppy CLI invocation cannot inflate the configured cluster.
	seen := make(map[string]struct{}, len(cfg.Peers))
	peers := make([]string, 0, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		peer = strings.TrimSpace(peer)
		if peer == "" || peer == cfg.ID {
			continue
		}
		if _, ok := seen[peer]; ok {
			continue
		}
		seen[peer] = struct{}{}
		peers = append(peers, peer)
	}
	cfg.Peers = peers

	return cfg, nil
}

// Start launches the gRPC server, the HTTP server (when HTTPAddr is set) and
// the Raft node. It returns once all listeners are bound and Raft has started.
// Cancelling ctx or calling Stop shuts the node down.
func (n *Node) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return ErrStopped
	}
	if n.started {
		n.mu.Unlock()
		return nil
	}

	n.ctx, n.cancel = context.WithCancel(ctx)
	grpcLn, err := net.Listen("tcp", n.cfg.RaftAddr)
	if err != nil {
		n.cancel()
		n.mu.Unlock()
		return fmt.Errorf("node: listen on %s: %w", n.cfg.RaftAddr, err)
	}
	n.grpcLn = grpcLn

	var httpLn net.Listener
	if n.cfg.HTTPAddr != "" {
		httpLn, err = net.Listen("tcp", n.cfg.HTTPAddr)
		if err != nil {
			_ = grpcLn.Close()
			n.grpcLn = nil
			n.cancel()
			n.mu.Unlock()
			return fmt.Errorf("node: listen on %s: %w", n.cfg.HTTPAddr, err)
		}
		n.httpLn = httpLn
	}
	n.started = true
	runCtx := n.ctx
	n.mu.Unlock()

	go n.serveGRPC(grpcLn)
	if httpLn != nil {
		go n.serveHTTP(httpLn)
	}

	if err := n.raft.Start(runCtx); err != nil {
		_ = n.Stop(context.Background())
		return fmt.Errorf("node: start raft: %w", err)
	}

	n.mu.Lock()
	n.raftStarted = true
	n.mu.Unlock()

	if n.reporter != nil {
		n.wg.Add(1)
		go n.reportLoop(runCtx)
	}
	// Tie the caller's context to the node lifecycle: cancelling it is
	// equivalent to Stop, mirroring raft.Node's contract.
	go n.watchContext()

	n.log.Info("node started",
		"shard", n.cfg.Shard,
		"raft_addr", grpcLn.Addr().String(),
		"http_addr", n.HTTPAddr(),
		"peers", n.cfg.Peers,
	)
	return nil
}

// Stop gracefully shuts the node down: HTTP, gRPC, the load reporter, Raft,
// the KV store and finally all peer connections. It is idempotent.
func (n *Node) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	n.stopOnce.Do(func() {
		n.stopErr = n.stop(ctx)
	})
	return n.stopErr
}

func (n *Node) stop(ctx context.Context) error {
	n.mu.Lock()
	n.stopped = true
	cancel := n.cancel
	started := n.started
	raftStarted := n.raftStarted
	httpSrv := n.httpSrv
	grpcSrv := n.grpcSrv
	n.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Give graceful shutdown a bounded amount of time even when the caller
	// passed a context without a deadline.
	stopCtx, stopCancel := context.WithTimeout(ctx, gracefulStopTimeout)
	defer stopCancel()

	var errs []error

	if httpSrv != nil && started {
		if err := httpSrv.Shutdown(stopCtx); err != nil {
			_ = httpSrv.Close()
			errs = append(errs, fmt.Errorf("node: http shutdown: %w", err))
		}
	}

	if grpcSrv != nil && started {
		done := make(chan struct{})
		go func() {
			grpcSrv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			// In-flight RPCs did not finish in time; force them closed.
			grpcSrv.Stop()
			<-done
			errs = append(errs, fmt.Errorf("node: grpc graceful stop: %w", stopCtx.Err()))
		}
	}

	if n.reporter != nil {
		n.reporter.Close()
	}
	n.wg.Wait()

	if raftStarted {
		n.raft.Stop()
	}
	if n.fileStore != nil {
		if err := n.fileStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("node: close raft store: %w", err))
		}
	}
	if n.kv != nil {
		if err := n.kv.Close(); err != nil {
			errs = append(errs, fmt.Errorf("node: close kv store: %w", err))
		}
	}
	if n.transport != nil {
		n.transport.Close()
	}

	n.log.Info("node stopped")
	return errors.Join(errs...)
}

func (n *Node) watchContext() {
	<-n.ctx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), gracefulStopTimeout)
	defer cancel()
	if err := n.Stop(ctx); err != nil {
		n.log.Debug("graceful shutdown reported errors", "error", err)
	}
}

func (n *Node) serveGRPC(ln net.Listener) {
	if err := n.grpcSrv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		n.log.Error("grpc server stopped unexpectedly", "error", err)
	}
}

// HTTPAddr returns the address the HTTP server is (or was configured to be)
// listening on. After Start it is the actual bound address, which is useful
// when the configured address used port 0.
func (n *Node) HTTPAddr() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.httpLn != nil {
		return n.httpLn.Addr().String()
	}
	return n.cfg.HTTPAddr
}

// RaftAddr returns the address the gRPC (NodeService/Raft) server is listening
// on.
func (n *Node) RaftAddr() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.grpcLn != nil {
		return n.grpcLn.Addr().String()
	}
	return n.cfg.RaftAddr
}

// Shard returns the configured shard name.
func (n *Node) Shard() string { return n.cfg.Shard }

// ID returns the node id (RaftAddr by default).
func (n *Node) ID() string { return n.cfg.ID }

// Status returns the current Raft status of this node.
func (n *Node) Status() raft.Status { return n.raft.Status() }

// IsLeader reports whether this node currently believes it is the leader.
func (n *Node) IsLeader() bool { return n.raft.IsLeader() }

// propose is the single place where the node proposes a command, so the error
// counter is updated consistently.
func (n *Node) propose(ctx context.Context, entry model.Entry) (uint64, error) {
	return n.raft.Propose(ctx, entry)
}
