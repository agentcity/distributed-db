package v2client

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
)

func tlsTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startTLSTestCluster starts one KV node and one standalone meta service, both
// mTLS-only, and returns their addresses.
func startTLSTestCluster(t *testing.T, certs *testutil.Certs) (metaAddr, nodeAddr string) {
	t.Helper()
	log := tlsTestLogger()
	serverTLS := grpctls.Options{
		Server: grpctls.ServerOptions{
			CertFile:     certs.ServerCert,
			KeyFile:      certs.ServerKey,
			ClientCAFile: certs.CAFile,
		},
		Client: grpctls.ClientOptions{
			CAFile:     certs.CAFile,
			ServerName: "localhost",
		},
	}

	nodeAddr = freeAddr(t)
	node, err := kvserver.New(kvserver.Config{
		ID:                 nodeAddr,
		RaftAddr:           nodeAddr,
		DataDir:            t.TempDir(),
		Ranges:             []kvserver.RangeConfig{{ID: 1, Epoch: 1}},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		Logger:             log,
		TLS:                serverTLS,
	})
	if err != nil {
		t.Fatalf("kvserver.New: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("kvserver.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = node.Stop(ctx)
	})
	waitForLeader(t, node)
	if got := node.Addr(); got != nodeAddr {
		t.Fatalf("node bound %s, want %s", got, nodeAddr)
	}

	ms, err := metaserver.New(metaserver.Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil,
			Replicas: []string{nodeAddr}, Leader: nodeAddr, Version: 1,
		}},
		Logger: log,
		TLS:    serverTLS,
	})
	if err != nil {
		t.Fatalf("metaserver.New: %v", err)
	}
	if err := ms.Start(context.Background()); err != nil {
		t.Fatalf("metaserver.Start: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	return ms.Addr(), nodeAddr
}

// freeAddr reserves and releases a loopback port so a node can advertise a
// stable, dialable address before it starts.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("release addr: %v", err)
	}
	return addr
}

func waitForLeader(t *testing.T, node *kvserver.Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n := node.Raft(); n != nil && n.IsLeader() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("KV node did not become leader")
}

func tlsClientOptions(certs *testutil.Certs, withCert bool) *grpctls.ClientOptions {
	opts := &grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		ServerName: "localhost",
	}
	if withCert {
		opts.CertFile = certs.ClientCert
		opts.KeyFile = certs.ClientKey
	}
	return opts
}

func TestTLSClientPutGet(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	metaAddr, nodeAddr := startTLSTestCluster(t, certs)

	cl, err := NewWithTLS(metaAddr, []string{nodeAddr}, tlsClientOptions(certs, true))
	if err != nil {
		t.Fatalf("NewWithTLS: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	txn, err := cl.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin over mTLS: %v", err)
	}
	txn.Set([]byte("tls-key"), []byte("tls-value"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("Commit over mTLS: %v", err)
	}

	read, err := cl.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin read over mTLS: %v", err)
	}
	defer func() { _ = read.Rollback(ctx) }()
	got, err := read.Get(ctx, []byte("tls-key"))
	if err != nil {
		t.Fatalf("Get over mTLS: %v", err)
	}
	if string(got) != "tls-value" {
		t.Fatalf("Get = %q, want %q", got, "tls-value")
	}
}

func TestTLSClientWithoutCertRejected(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	metaAddr, nodeAddr := startTLSTestCluster(t, certs)

	cl, err := NewWithTLS(metaAddr, []string{nodeAddr}, tlsClientOptions(certs, false))
	if err != nil {
		t.Fatalf("NewWithTLS: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.Begin(ctx); err == nil {
		t.Fatal("mTLS cluster served a client without a certificate")
	}
}

func TestPlaintextClientToTLSClusterFails(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	metaAddr, nodeAddr := startTLSTestCluster(t, certs)

	cl, err := New(metaAddr, []string{nodeAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.Begin(ctx); err == nil {
		t.Fatal("plaintext client reached a TLS-only cluster")
	}
}

func TestNewWithTLSRejectsBadOptions(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	if _, err := NewWithTLS("127.0.0.1:1", []string{"127.0.0.1:2"}, &grpctls.ClientOptions{
		CertFile: certs.ClientCert,
	}); err == nil {
		t.Fatal("NewWithTLS accepted a certificate without a key")
	}
	if _, err := NewMetaOnlyWithTLS("127.0.0.1:1", &grpctls.ClientOptions{
		CAFile: certs.Dir + "/missing.pem",
	}); err == nil {
		t.Fatal("NewMetaOnlyWithTLS accepted a missing CA file")
	}
}
