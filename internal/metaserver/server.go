// Package metaserver exposes the v2 timestamp oracle over gRPC.
//
// For M1 the TSO is a single in-memory meta.Cluster: it is enough for a
// single-region, non-replicated test cluster. Moving it into a Raft-replicated
// meta group is M2 work.
package metaserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
)

// Config configures the meta server.
type Config struct {
	Addr   string
	Logger *slog.Logger
}

// Server is a gRPC MetaService backed by meta.Cluster.
type Server struct {
	pbv2.UnimplementedMetaServiceServer

	cfg     Config
	log     *slog.Logger
	cluster *meta.Cluster

	mu      sync.Mutex
	grpc    *grpc.Server
	lis     net.Listener
	started bool
	stopped bool
}

// New creates a meta server. Start binds the listener.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("metaserver: Addr is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg, log: cfg.Logger, cluster: meta.New()}, nil
}

// Start binds Addr and serves MetaService in the background.
func (s *Server) Start(ctx context.Context) error {
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
	s.started = true
	s.mu.Unlock()

	go func() {
		if err := s.grpc.Serve(lis); err != nil {
			s.log.Error("meta grpc server stopped", "error", err)
		}
	}()
	s.log.Info("meta server started", "addr", lis.Addr().String())

	if ctx != nil {
		go func() {
			<-ctx.Done()
			_ = s.Stop()
		}()
	}
	return nil
}

// Stop stops the gRPC server. It is idempotent.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	grpcSrv := s.grpc
	lis := s.lis
	s.mu.Unlock()

	if grpcSrv != nil {
		grpcSrv.GracefulStop()
	}
	if lis != nil {
		_ = lis.Close()
	}
	return nil
}

// Addr returns the bound address (useful with :0 in tests).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lis != nil {
		return s.lis.Addr().String()
	}
	return s.cfg.Addr
}

// Cluster exposes the in-memory range/TSO state for tests and future admin
// APIs. Callers must not mutate it concurrently with RPCs.
func (s *Server) Cluster() *meta.Cluster { return s.cluster }

// AllocTS implements pbv2.MetaServiceServer.
func (s *Server) AllocTS(_ context.Context, req *pbv2.AllocTSRequest) (*pbv2.AllocTSResponse, error) {
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	start, count := s.cluster.AllocTS(n)
	return &pbv2.AllocTSResponse{Start: start, Count: int32(count)}, nil
}
