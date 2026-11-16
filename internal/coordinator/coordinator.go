// Package coordinator implements the stateless cluster entry point: it maps a
// key to a shard, discovers that shard's Raft leader, proxies KV operations to
// it and exposes the same surface over HTTP and gRPC.
package coordinator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"distributed-db/internal/pb"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Config is the coordinator configuration. All fields mirror the command line
// flags documented in docs/ARCHITECTURE.md.
type Config struct {
	HTTPAddr  string
	GRPCAddr  string
	Shards    map[string][]string // shard -> node raft addresses
	AuthUser  string
	AuthPass  string
	TLSCert   string
	TLSKey    string
	TLSCA     string
	RateLimit float64 // requests/sec, 0 disables the limiter
	RateBurst int
	Logger    *slog.Logger
}

// gracefulStopTimeout caps Stop even when the caller passed a context without
// a deadline.
const gracefulStopTimeout = 10 * time.Second

// loadKey identifies a load sample: one node in one shard.
type loadKey struct {
	node  string
	shard string
}

// loadSample is the last load report received from a node.
type loadSample struct {
	load      float64
	ops       uint64
	updatedAt time.Time
}

// Coordinator is the cluster entry point. It implements
// pb.CoordinatorServiceServer.
type Coordinator struct {
	pb.UnimplementedCoordinatorServiceServer

	cfg    Config
	logger *slog.Logger

	shardNames []string
	shards     map[string][]string

	metrics *coordinatorMetrics
	conns   *connPool
	limiter *rate.Limiter

	// newNodeClient is overridable in tests. Production uses the pooled
	// gRPC connections via dialNodeClient.
	newNodeClient func(addr string) (pb.NodeServiceClient, error)

	leadersMu sync.Mutex
	leaders   map[string]cachedLeader

	loadsMu sync.RWMutex
	loads   map[loadKey]loadSample

	httpHandler http.Handler

	mu         sync.Mutex
	started    bool
	stopped    bool
	httpServer *http.Server
	grpcServer *grpc.Server

	stopOnce sync.Once
	stopErr  error
}

// New validates cfg and builds a coordinator. Servers are not started until
// Start is called.
func New(cfg Config) (*Coordinator, error) {
	if strings.TrimSpace(cfg.HTTPAddr) == "" {
		return nil, errors.New("coordinator: HTTPAddr is required")
	}
	if strings.TrimSpace(cfg.GRPCAddr) == "" {
		return nil, errors.New("coordinator: GRPCAddr is required")
	}
	if len(cfg.Shards) == 0 {
		return nil, errNoShards
	}
	if cfg.RateLimit < 0 {
		return nil, errors.New("coordinator: RateLimit must not be negative")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, errors.New("coordinator: TLSCert and TLSKey must be set together")
	}

	shards := make(map[string][]string, len(cfg.Shards))
	for name, members := range cfg.Shards {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("coordinator: shard name must not be empty")
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("coordinator: shard %q has no members", name)
		}
		copied := make([]string, 0, len(members))
		for _, addr := range members {
			if strings.TrimSpace(addr) == "" {
				return nil, fmt.Errorf("coordinator: shard %q contains an empty node address", name)
			}
			copied = append(copied, addr)
		}
		shards[name] = copied
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	c := &Coordinator{
		cfg:        cfg,
		logger:     logger,
		shardNames: sortedShardNames(shards),
		shards:     shards,
		metrics:    newCoordinatorMetrics(),
		conns:      newConnPool(),
		leaders:    make(map[string]cachedLeader),
		loads:      make(map[loadKey]loadSample),
	}
	if cfg.RateLimit > 0 {
		burst := cfg.RateBurst
		if burst <= 0 {
			burst = 1
		}
		c.limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), burst)
	}
	c.newNodeClient = c.dialNodeClient
	c.httpHandler = c.buildHTTPHandler()
	return c, nil
}

// Start binds the HTTP and gRPC listeners and serves in the background. It
// returns once both listeners are bound. ctx is the base context of HTTP
// requests; it does not stop the coordinator by itself, call Stop instead.
func (c *Coordinator) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("coordinator: already started")
	}
	if c.stopped {
		return errors.New("coordinator: already stopped")
	}

	serverTLS, clientCreds, err := buildTLS(c.cfg)
	if err != nil {
		return err
	}
	c.conns.setCredentials(clientCreds)

	httpLn, err := net.Listen("tcp", c.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("coordinator: listen HTTP on %s: %w", c.cfg.HTTPAddr, err)
	}
	grpcLn, err := net.Listen("tcp", c.cfg.GRPCAddr)
	if err != nil {
		_ = httpLn.Close()
		return fmt.Errorf("coordinator: listen gRPC on %s: %w", c.cfg.GRPCAddr, err)
	}

	httpSrv := &http.Server{
		Handler:           c.httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         serverTLS,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	var grpcOpts []grpc.ServerOption
	if serverTLS != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	pb.RegisterCoordinatorServiceServer(grpcSrv, c)

	c.httpServer = httpSrv
	c.grpcServer = grpcSrv
	c.started = true

	go func() {
		var err error
		if serverTLS != nil {
			// The certificate chain is already in TLSConfig.
			err = httpSrv.ServeTLS(httpLn, "", "")
		} else {
			err = httpSrv.Serve(httpLn)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.logger.Error("coordinator HTTP server stopped", "error", err)
		}
	}()
	go func() {
		if err := grpcSrv.Serve(grpcLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			c.logger.Error("coordinator gRPC server stopped", "error", err)
		}
	}()
	c.logger.Info("coordinator started",
		"http_addr", httpLn.Addr().String(),
		"grpc_addr", grpcLn.Addr().String(),
		"shards", len(c.shardNames),
		"tls", serverTLS != nil,
	)

	return nil
}

// Stop gracefully shuts the servers down, closes all node connections and is
// safe to call more than once. The supplied context bounds the shutdown.
func (c *Coordinator) Stop(ctx context.Context) error {
	c.stopOnce.Do(func() {
		c.stopErr = c.doStop(ctx)
	})
	return c.stopErr
}

func (c *Coordinator) doStop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	httpSrv, grpcSrv := c.httpServer, c.grpcServer
	c.mu.Unlock()

	// Bound graceful shutdown even when the caller gave no deadline.
	ctx, cancel := context.WithTimeout(ctx, gracefulStopTimeout)
	defer cancel()

	var errs []error
	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("coordinator: HTTP shutdown: %w", err))
			_ = httpSrv.Close()
		}
	}
	if grpcSrv != nil {
		if err := gracefulStopGRPC(ctx, grpcSrv); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.conns.closeAll(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		c.logger.Warn("coordinator stopped with errors", "error", err)
		return err
	}
	c.logger.Info("coordinator stopped")
	return nil
}

// gracefulStopGRPC waits for in-flight RPCs unless ctx expires first, in which
// case it force-stops the server.
func gracefulStopGRPC(ctx context.Context, srv *grpc.Server) error {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		srv.Stop()
		<-done
		return fmt.Errorf("coordinator: gRPC graceful stop: %w", ctx.Err())
	}
}

// buildTLS loads the optional server certificate and the credentials used to
// reach nodes. TLS for nodes is enabled when a client certificate or a CA is
// configured.
func buildTLS(cfg Config) (serverTLS *tls.Config, clientCreds credentials.TransportCredentials, err error) {
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, nil, errors.New("coordinator: TLSCert and TLSKey must be set together")
	}

	var cert tls.Certificate
	haveCert := cfg.TLSCert != "" && cfg.TLSKey != ""
	if haveCert {
		cert, err = tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, nil, fmt.Errorf("coordinator: load TLS certificate: %w", err)
		}
		serverTLS = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	clientCreds = insecure.NewCredentials()
	if !haveCert && cfg.TLSCA == "" {
		return serverTLS, clientCreds, nil
	}

	clientTLS := &tls.Config{MinVersion: tls.VersionTLS12}
	if haveCert {
		// Reuse the server certificate for mutual TLS. Nodes that do not ask
		// for a client certificate simply ignore it.
		clientTLS.Certificates = []tls.Certificate{cert}
	}
	if cfg.TLSCA != "" {
		pem, err := os.ReadFile(cfg.TLSCA)
		if err != nil {
			return nil, nil, fmt.Errorf("coordinator: read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("coordinator: no certificates found in %s", cfg.TLSCA)
		}
		clientTLS.RootCAs = pool
	} else {
		// Dev-only fallback: no CA was configured, so node certificates
		// cannot be verified. Do not use this in production.
		clientTLS.InsecureSkipVerify = true //nolint:gosec // documented fallback
	}
	clientCreds = credentials.NewTLS(clientTLS)
	return serverTLS, clientCreds, nil
}

// coordinatorStatus is the JSON body of GET /v1/status.
type coordinatorStatus struct {
	Shards  map[string][]string `json:"shards"`
	Leaders map[string]string   `json:"leaders"`
	Loads   []loadStatus        `json:"loads"`
}

type loadStatus struct {
	Node      string    `json:"node"`
	Shard     string    `json:"shard"`
	Load      float64   `json:"load"`
	Ops       uint64    `json:"ops"`
	UpdatedAt time.Time `json:"updated_at"`
}

// statusSnapshot assembles the current shard map, cached leaders and load
// samples. Loads are sorted for stable JSON output.
func (c *Coordinator) statusSnapshot() coordinatorStatus {
	shards := make(map[string][]string, len(c.shards))
	for name, members := range c.shards {
		shards[name] = append([]string(nil), members...)
	}

	c.loadsMu.RLock()
	loads := make([]loadStatus, 0, len(c.loads))
	for key, sample := range c.loads {
		loads = append(loads, loadStatus{
			Node:      key.node,
			Shard:     key.shard,
			Load:      sample.load,
			Ops:       sample.ops,
			UpdatedAt: sample.updatedAt,
		})
	}
	c.loadsMu.RUnlock()
	sort.Slice(loads, func(i, j int) bool {
		if loads[i].Node != loads[j].Node {
			return loads[i].Node < loads[j].Node
		}
		return loads[i].Shard < loads[j].Shard
	})

	return coordinatorStatus{
		Shards:  shards,
		Leaders: c.cachedLeaders(),
		Loads:   loads,
	}
}

// storeLoad records a node load sample and updates the load metric.
func (c *Coordinator) storeLoad(node, shard string, load float64, ops uint64) {
	if node == "" {
		node = "unknown"
	}
	if shard == "" {
		shard = "unknown"
	}
	c.loadsMu.Lock()
	c.loads[loadKey{node: node, shard: shard}] = loadSample{
		load:      load,
		ops:       ops,
		updatedAt: time.Now(),
	}
	c.loadsMu.Unlock()
	c.metrics.load.WithLabelValues(node, shard).Set(load)
}
