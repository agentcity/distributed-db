package kvserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// tlsTestOptions builds a node TLS configuration backed by generated test
// certificates. mtls enables client-certificate verification on the listener.
func tlsTestOptions(t *testing.T, certs *testutil.Certs, mtls bool) grpctls.Options {
	t.Helper()
	opts := grpctls.Options{
		Server: grpctls.ServerOptions{
			CertFile: certs.ServerCert,
			KeyFile:  certs.ServerKey,
		},
		Client: grpctls.ClientOptions{
			CAFile:     certs.CAFile,
			ServerName: "localhost",
		},
	}
	if mtls {
		opts.Server.ClientCAFile = certs.CAFile
	}
	return opts
}

func tlsTestClientOptions(certs *testutil.Certs) grpctls.ClientOptions {
	return grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		CertFile:   certs.ClientCert,
		KeyFile:    certs.ClientKey,
		ServerName: "localhost",
	}
}

func TestNewRejectsHalfConfiguredTLS(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	cases := []struct {
		name string
		tls  grpctls.Options
		want string
	}{
		{
			name: "cert without key",
			tls:  grpctls.Options{Server: grpctls.ServerOptions{CertFile: certs.ServerCert}},
			want: "both CertFile and KeyFile",
		},
		{
			name: "client CA without server identity",
			tls:  grpctls.Options{Server: grpctls.ServerOptions{ClientCAFile: certs.CAFile}},
			want: "require server CertFile and KeyFile",
		},
		{
			name: "peer CA without server identity",
			tls:  grpctls.Options{Client: grpctls.ClientOptions{CAFile: certs.CAFile}},
			want: "require server CertFile and KeyFile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				RaftAddr: "127.0.0.1:0",
				DataDir:  t.TempDir(),
				NoRanges: true,
				Logger:   testLogger(),
				TLS:      tc.tls,
			}
			srv, err := New(cfg)
			if err == nil {
				_ = srv.Stop(context.Background())
				t.Fatal("New accepted a half-configured TLS configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %q, want substring %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "kvserver: TLS") {
				t.Fatalf("New error = %q, want a kvserver TLS context", err)
			}
		})
	}
}

func TestTLSServerRejectsPlaintextAndMissingClientCert(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	srv, err := New(Config{
		ID:       "tls-node",
		RaftAddr: "127.0.0.1:0",
		DataDir:  t.TempDir(),
		Ranges: []RangeConfig{{
			ID: 1, Start: nil, End: nil, Epoch: 1,
		}},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		Logger:             testLogger(),
		TLS:                tlsTestOptions(t, certs, true),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	waitNodeLeader(t, srv)
	addr := srv.Addr()

	// A TLS client with a certificate signed by the client CA succeeds.
	creds, err := grpctls.ClientCredentials(tlsTestClientOptions(certs))
	if err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial TLS: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := pbv2.NewKVServiceClient(cc).Status(ctx, &pbv2.StatusRequest{RangeId: 1})
	if err != nil {
		t.Fatalf("mTLS Status: %v", err)
	}
	if resp.GetRangeId() != 1 {
		t.Fatalf("Status range = %d, want 1", resp.GetRangeId())
	}

	// A TLS client without a certificate is rejected by the mTLS listener.
	noCertCreds, err := grpctls.ClientCredentials(grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client credentials without cert: %v", err)
	}
	noCertCC, err := grpc.NewClient(addr, grpc.WithTransportCredentials(noCertCreds))
	if err != nil {
		t.Fatalf("dial TLS without cert: %v", err)
	}
	t.Cleanup(func() { _ = noCertCC.Close() })
	callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer callCancel()
	if _, err := pbv2.NewKVServiceClient(noCertCC).Status(callCtx, &pbv2.StatusRequest{RangeId: 1}); err == nil {
		t.Fatal("mTLS server accepted a client without a certificate")
	}

	// A plaintext client cannot talk to a TLS listener.
	plainCC, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial plaintext: %v", err)
	}
	t.Cleanup(func() { _ = plainCC.Close() })
	plainCtx, plainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer plainCancel()
	if _, err := pbv2.NewKVServiceClient(plainCC).Status(plainCtx, &pbv2.StatusRequest{RangeId: 1}); err == nil {
		t.Fatal("plaintext client reached a TLS-only server")
	}
}

func TestRaftClusterOverMTLS(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	const nodes = 3
	addrs := make([]string, nodes)
	for i := range addrs {
		addrs[i] = testFreeAddr(t)
	}
	servers := make([]*Server, 0, nodes)
	for i, addr := range addrs {
		peers := make([]string, 0, nodes-1)
		for j, peer := range addrs {
			if i != j {
				peers = append(peers, peer)
			}
		}
		srv, err := New(Config{
			ID:       addr,
			RaftAddr: addr,
			DataDir:  t.TempDir(),
			Ranges: []RangeConfig{{
				ID: 1, Start: nil, End: nil, Peers: peers, Epoch: 1,
			}},
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 200 * time.Millisecond,
			HeartbeatInterval:  20 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             testLogger(),
			TLS:                tlsTestOptions(t, certs, true),
		})
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start %s: %v", addr, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(ctx)
		})
		servers = append(servers, srv)
	}

	creds, err := grpctls.ClientCredentials(tlsTestClientOptions(certs))
	if err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		leaders := 0
		for _, srv := range servers {
			if srv.Raft() != nil && srv.Raft().IsLeader() {
				leaders++
			}
		}
		if leaders == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Timeout: report each node's view through a TLS Status probe to make the
	// failure diagnosable.
	for _, srv := range servers {
		cc, derr := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(creds))
		if derr != nil {
			t.Logf("%s: dial: %v", srv.Addr(), derr)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, serr := pbv2.NewKVServiceClient(cc).Status(ctx, &pbv2.StatusRequest{RangeId: 1})
		cancel()
		_ = cc.Close()
		if serr != nil {
			t.Logf("%s: status: %v", srv.Addr(), serr)
			continue
		}
		t.Logf("%s: role=%s term=%d leader=%s", srv.Addr(), resp.GetRole(), resp.GetTerm(), resp.GetLeader())
	}
	t.Fatal("no single leader was elected with mTLS Raft traffic")
}

// waitNodeLeader waits until a single-range node reports leadership.
func waitNodeLeader(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if node := srv.Raft(); node != nil && node.IsLeader() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("node did not become leader")
}
