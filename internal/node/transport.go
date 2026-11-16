package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/pb"
	"distributed-db/internal/raft"
)

// PeerTransport implements raft.Transport over gRPC. A client connection is
// created lazily on the first RPC to a peer and reused afterwards; Close
// releases every connection.
type PeerTransport struct {
	log   *slog.Logger
	creds credentials.TransportCredentials

	mu     sync.Mutex
	conns  map[string]*peerConn
	closed bool
}

type peerConn struct {
	cc     *grpc.ClientConn
	client pb.NodeServiceClient
}

// NewPeerTransport builds a transport that dials peers with the TLS settings
// from cfg. No connection is opened until the first RPC.
func NewPeerTransport(cfg Config) (*PeerTransport, error) {
	creds, err := clientTransportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &PeerTransport{
		log:   log,
		creds: creds,
		conns: make(map[string]*peerConn),
	}, nil
}

func (t *PeerTransport) client(peer string) (pb.NodeServiceClient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, errors.New("node: peer transport is closed")
	}
	if pc, ok := t.conns[peer]; ok {
		return pc.client, nil
	}

	// grpc.NewClient is lazy: it does not perform I/O or block on connect.
	cc, err := grpc.NewClient(peer,
		grpc.WithTransportCredentials(t.creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxGRPCMessageSize),
			grpc.MaxCallSendMsgSize(maxGRPCMessageSize),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("node: create client for peer %q: %w", peer, err)
	}
	client := pb.NewNodeServiceClient(cc)
	t.conns[peer] = &peerConn{cc: cc, client: client}
	t.log.Debug("peer connection created", "peer", peer)
	return client, nil
}

// RequestVote implements raft.Transport.
func (t *PeerTransport) RequestVote(ctx context.Context, peer string, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	if req == nil {
		return nil, errors.New("node: nil RequestVoteRequest")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := client.RequestVote(ctx, &pb.RequestVoteRequest{
		Term:         req.Term,
		CandidateId:  req.CandidateID,
		LastLogIndex: req.LastLogIndex,
		LastLogTerm:  req.LastLogTerm,
	})
	if err != nil {
		return nil, fmt.Errorf("node: RequestVote to %s: %w", peer, err)
	}
	return &raft.RequestVoteResponse{
		Term:        resp.GetTerm(),
		VoteGranted: resp.GetVoteGranted(),
	}, nil
}

// AppendEntries implements raft.Transport.
func (t *PeerTransport) AppendEntries(ctx context.Context, peer string, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if req == nil {
		return nil, errors.New("node: nil AppendEntriesRequest")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := t.client(peer)
	if err != nil {
		return nil, err
	}
	entries := make([]*pb.LogEntry, 0, len(req.Entries))
	for _, entry := range req.Entries {
		entries = append(entries, &pb.LogEntry{
			Term:  entry.Term,
			Index: entry.Index,
			Type:  string(entry.Type),
			Key:   entry.Key,
			Value: entry.Value,
		})
	}
	resp, err := client.AppendEntries(ctx, &pb.AppendEntriesRequest{
		Term:         req.Term,
		LeaderId:     req.LeaderID,
		PrevLogIndex: req.PrevLogIndex,
		PrevLogTerm:  req.PrevLogTerm,
		Entries:      entries,
		LeaderCommit: req.LeaderCommit,
	})
	if err != nil {
		return nil, fmt.Errorf("node: AppendEntries to %s: %w", peer, err)
	}
	return &raft.AppendEntriesResponse{
		Term:          resp.GetTerm(),
		Success:       resp.GetSuccess(),
		MatchIndex:    resp.GetMatchIndex(),
		ConflictIndex: resp.GetConflictIndex(),
	}, nil
}

// Close closes all peer connections. It is safe to call more than once.
func (t *PeerTransport) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for addr, pc := range t.conns {
		if err := pc.cc.Close(); err != nil {
			t.log.Debug("closing peer connection failed", "peer", addr, "error", err)
		}
	}
	t.conns = nil
}

// clientTransportCredentials builds the credentials used to call peers and the
// coordinator.
func clientTransportCredentials(cfg Config) (credentials.TransportCredentials, error) {
	if cfg.TLSCert == "" && cfg.TLSKey == "" {
		return insecure.NewCredentials(), nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("node: load TLS client certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if cfg.TLSCA != "" {
		pool, err := loadCertPool(cfg.TLSCA)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	} else {
		// TLS is enabled but no CA was supplied, so the peer certificate
		// cannot be verified. This is the documented development fallback;
		// production deployments must set TLSCA.
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit, documented fallback
	}
	return credentials.NewTLS(tlsCfg), nil
}

// serverTransportCredentials builds the credentials for the node's gRPC
// server. It returns (nil, nil) when TLS is disabled.
func serverTransportCredentials(cfg Config) (credentials.TransportCredentials, error) {
	if cfg.TLSCert == "" && cfg.TLSKey == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("node: load TLS server certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if cfg.TLSCA != "" {
		pool, err := loadCertPool(cfg.TLSCA)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientCAs = pool
		// Verify a client certificate when one is presented, but allow
		// clients without a certificate (for example a coordinator doing
		// status probes) to connect.
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return credentials.NewTLS(tlsCfg), nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("node: read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("node: no certificates found in CA file %q", path)
	}
	return pool, nil
}

// compile-time assertion: the transport must satisfy the Raft contract.
var _ raft.Transport = (*PeerTransport)(nil)
