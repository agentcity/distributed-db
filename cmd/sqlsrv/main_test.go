package main

import (
	"strings"
	"testing"

	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlhttp"
)

func TestClientTLSOptionsEmptyIsPlaintext(t *testing.T) {
	opts, err := clientTLSOptions("", "", "", "")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts != nil {
		t.Fatalf("empty flags = %+v, want nil (plaintext)", opts)
	}
}

func TestClientTLSOptionsRejectsCertWithoutKey(t *testing.T) {
	_, err := clientTLSOptions("", "client.pem", "", "")
	if err == nil || !strings.Contains(err.Error(), "both CertFile and KeyFile") {
		t.Fatalf("error = %v, want a certificate/key pair error", err)
	}
}

func TestClientTLSOptionsBuildsClientTLS(t *testing.T) {
	opts, err := clientTLSOptions("ca.pem", "client.pem", "client.key", "meta.internal")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts == nil || opts.CAFile != "ca.pem" || opts.CertFile != "client.pem" ||
		opts.KeyFile != "client.key" || opts.ServerName != "meta.internal" {
		t.Fatalf("clientTLSOptions = %+v", opts)
	}
}

func TestClientTLSOptionsServerNameOnlyEnablesTLS(t *testing.T) {
	opts, err := clientTLSOptions("", "", "", "meta.internal")
	if err != nil {
		t.Fatalf("clientTLSOptions: %v", err)
	}
	if opts == nil || opts.ServerName != "meta.internal" {
		t.Fatalf("server-name-only options = %+v, want TLS with system roots", opts)
	}
}

func TestNewHTTPServerPlaintextWithoutAuth(t *testing.T) {
	srv, err := newHTTPServer("127.0.0.1:0", sqlhttp.Config{Engine: sqlengine.New(nil)})
	if err != nil {
		t.Fatalf("newHTTPServer: %v", err)
	}
	if srv.TLSConfig != nil {
		t.Fatalf("TLSConfig = %+v, want nil for development plaintext mode", srv.TLSConfig)
	}
}

func TestNewHTTPServerRejectsAuthWithoutTLS(t *testing.T) {
	_, err := newHTTPServer("127.0.0.1:0", sqlhttp.Config{
		Engine:    sqlengine.New(nil),
		AuthToken: "super-secret-token",
	})
	if err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("newHTTPServer error = %v, want an auth-requires-TLS error", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("startup error leaked the auth token: %v", err)
	}
}

func TestNewHTTPServerRejectsPartialCertificatePair(t *testing.T) {
	_, err := newHTTPServer("127.0.0.1:0", sqlhttp.Config{
		Engine:      sqlengine.New(nil),
		TLSCertFile: "server.crt",
	})
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("newHTTPServer error = %v, want a certificate/key pair error", err)
	}
}

func TestNewHTTPServerTLSWithAuth(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certificates: %v", err)
	}
	srv, err := newHTTPServer("127.0.0.1:0", sqlhttp.Config{
		Engine:      sqlengine.New(nil),
		TLSCertFile: certs.ServerCert,
		TLSKeyFile:  certs.ServerKey,
		AuthToken:   "tok",
	})
	if err != nil {
		t.Fatalf("newHTTPServer: %v", err)
	}
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil, want a TLS listener")
	}
	if got := len(srv.TLSConfig.Certificates); got != 1 {
		t.Fatalf("len(Certificates) = %d, want 1", got)
	}
}

func TestClusterTLSOptionsStayPlaintextForListenerTLSOnly(t *testing.T) {
	opts, err := clusterTLSOptions("", "", "server.crt", "server.key")
	if err != nil {
		t.Fatalf("clusterTLSOptions: %v", err)
	}
	if opts != nil {
		t.Fatalf("listener certificate alone enabled cluster TLS: %+v", opts)
	}
}

func TestClusterTLSOptionsEnabledByCA(t *testing.T) {
	opts, err := clusterTLSOptions("ca.pem", "", "server.crt", "server.key")
	if err != nil {
		t.Fatalf("clusterTLSOptions: %v", err)
	}
	if opts == nil || opts.CAFile != "ca.pem" || opts.CertFile != "server.crt" || opts.KeyFile != "server.key" {
		t.Fatalf("clusterTLSOptions = %+v", opts)
	}
}
