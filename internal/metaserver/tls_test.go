package metaserver

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/meta"
	"distributed-db/internal/v2client"
)

func tlsTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func metaTLSOptions(certs *testutil.Certs, mtls bool) grpctls.Options {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Config{Addr: "127.0.0.1:0", TLS: tc.tls})
			if err == nil {
				_ = s.Stop()
				t.Fatal("New accepted a half-configured TLS configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestTLSMetaServerListRanges(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	s, err := New(Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil,
			Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 3,
		}},
		Logger: tlsTestLogger(),
		TLS:    metaTLSOptions(certs, false),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	cl, err := v2client.NewMetaOnlyWithTLS(s.Addr(), &grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		CertFile:   certs.ClientCert,
		KeyFile:    certs.ClientKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithTLS: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := cl.ListRanges(ctx)
	if err != nil {
		t.Fatalf("ListRanges over TLS: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 1 || rows[0].Version != 3 {
		t.Fatalf("ListRanges = %+v, want one range with id=1 version=3", rows)
	}
}

func TestTLSMetaServerRequiresClientCert(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	s, err := New(Config{
		Addr:   "127.0.0.1:0",
		Ranges: []meta.Range{{ID: 1, Replicas: []string{"127.0.0.1:7001"}}},
		Logger: tlsTestLogger(),
		TLS:    metaTLSOptions(certs, true),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	cl, err := v2client.NewMetaOnlyWithTLS(s.Addr(), &grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithTLS: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.ListRanges(ctx); err == nil {
		t.Fatal("mTLS meta server answered a client without a certificate")
	}
}

func TestPlaintextClientToTLSMetaServerFails(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	s, err := New(Config{
		Addr:   "127.0.0.1:0",
		Ranges: []meta.Range{{ID: 1, Replicas: []string{"127.0.0.1:7001"}}},
		Logger: tlsTestLogger(),
		TLS:    metaTLSOptions(certs, false),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	cl, err := v2client.NewMetaOnly(s.Addr())
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.ListRanges(ctx); err == nil {
		t.Fatal("plaintext meta client reached a TLS-only server")
	}
}
