package raftgrpc

import (
	"context"
	"net"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/model"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
)

// tlsRaftServer is a minimal RaftService that accepts AppendEntries and echoes
// the term so the test can prove the RPC crossed a TLS connection.
type tlsRaftServer struct {
	pbv2.UnimplementedRaftServiceServer
}

func (s *tlsRaftServer) AppendEntries(_ context.Context, req *pbv2.AppendEntriesRequest) (*pbv2.AppendEntriesResponse, error) {
	return &pbv2.AppendEntriesResponse{
		Term:       req.GetTerm(),
		Success:    true,
		MatchIndex: uint64(len(req.GetEntries())),
	}, nil
}

func startTLSRaftServer(t *testing.T, creds *testutil.Certs, mtls bool) string {
	t.Helper()
	serverOpts := grpctls.ServerOptions{CertFile: creds.ServerCert, KeyFile: creds.ServerKey}
	if mtls {
		serverOpts.ClientCAFile = creds.CAFile
	}
	transportCreds, err := grpctls.ServerCredentials(serverOpts)
	if err != nil {
		t.Fatalf("server credentials: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(transportCreds))
	pbv2.RegisterRaftServiceServer(srv, &tlsRaftServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestTransportTLSAppendEntries(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	addr := startTLSRaftServer(t, certs, true)

	tr, err := NewWithTLS(grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		CertFile:   certs.ClientCert,
		KeyFile:    certs.ClientKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("NewWithTLS: %v", err)
	}
	t.Cleanup(tr.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := tr.AppendEntries(ctx, addr, &raft.AppendEntriesRequest{
		Term:     7,
		LeaderID: "leader-1",
		Entries:  []model.Entry{{Term: 7, Index: 1}},
	})
	if err != nil {
		t.Fatalf("AppendEntries over TLS: %v", err)
	}
	if !resp.Success || resp.Term != 7 {
		t.Fatalf("AppendEntries response = %+v, want success at term 7", resp)
	}
}

func TestTransportPlaintextToTLSServerFails(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	addr := startTLSRaftServer(t, certs, false)

	tr := New()
	t.Cleanup(tr.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := tr.AppendEntries(ctx, addr, &raft.AppendEntriesRequest{Term: 1}); err == nil {
		t.Fatal("plaintext raft transport reached a TLS-only server")
	}
}

func TestTransportTLSRejectsBadOptions(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	if _, err := NewWithTLS(grpctls.ClientOptions{CertFile: certs.ClientCert}); err == nil {
		t.Fatal("NewWithTLS accepted a certificate without a key")
	}
	if _, err := NewWithTLS(grpctls.ClientOptions{CAFile: certs.Dir + "/missing.pem"}); err == nil {
		t.Fatal("NewWithTLS accepted a missing CA file")
	}
}
