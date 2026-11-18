package main

import (
	"flag"
	"strings"
	"testing"

	"distributed-db/internal/grpctls"
)

func TestAdminTokenFlagIsWired(t *testing.T) {
	fs := flag.NewFlagSet("kv2-test", flag.ContinueOnError)
	token := registerAdminTokenFlag(fs)
	if err := fs.Parse([]string{"-admin-token", "s3cret"}); err != nil {
		t.Fatalf("parse -admin-token: %v", err)
	}
	if *token != "s3cret" {
		t.Fatalf("parsed admin token = %q, want s3cret", *token)
	}
}

func TestAdminTokenFlagDefaultsToEmpty(t *testing.T) {
	fs := flag.NewFlagSet("kv2-test", flag.ContinueOnError)
	token := registerAdminTokenFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *token != "" {
		t.Fatalf("default admin token = %q, want empty (dev-disabled)", *token)
	}
}

func TestMetaServerConfigWiresAdminToken(t *testing.T) {
	cfg := metaServerConfig(":9090", "meta-1", "./data/meta", nil, nil, grpctls.Options{}, adminConfig{token: "s3cret"})
	if cfg.AdminToken != "s3cret" {
		t.Fatalf("AdminToken = %q, want s3cret", cfg.AdminToken)
	}
	if cfg.Replicated {
		t.Fatal("standalone meta config unexpectedly enabled replication")
	}
}

func TestMetaServerConfigPeersEnableReplication(t *testing.T) {
	cfg := metaServerConfig(":9090", "meta-1", "./data/meta", []string{"m2:9090"}, nil, grpctls.Options{}, adminConfig{})
	if !cfg.Replicated {
		t.Fatal("peer list did not enable replicated mode")
	}
	if cfg.AdminToken != "" {
		t.Fatalf("AdminToken = %q, want empty dev default", cfg.AdminToken)
	}
}

func TestTLSOptionsEmptyIsPlaintext(t *testing.T) {
	opts, err := tlsOptions("", "", "", "", "")
	if err != nil {
		t.Fatalf("tlsOptions: %v", err)
	}
	if opts.Enabled() {
		t.Fatal("empty flags enabled TLS")
	}
	if opts.Server.CertFile != "" || opts.Server.KeyFile != "" || opts.Client.CAFile != "" {
		t.Fatalf("unexpected options: %+v", opts)
	}
	server, client, err := opts.Credentials()
	if err != nil || server != nil || client != nil {
		t.Fatalf("plaintext Credentials = (%v, %v, %v), want nil, nil, nil", server, client, err)
	}
}

func TestTLSOptionsRejectsHalfConfiguredFlags(t *testing.T) {
	cases := []struct {
		name                             string
		cert, key, clientCA, ca, srvName string
		want                             string
	}{
		{
			name: "cert without key",
			cert: "server.pem",
			want: "both CertFile and KeyFile",
		},
		{
			name: "key without cert",
			key:  "server.key",
			want: "both CertFile and KeyFile",
		},
		{
			name:     "client CA without cert",
			clientCA: "clients.pem",
			want:     "require server CertFile and KeyFile",
		},
		{
			name: "peer CA without cert",
			ca:   "peers.pem",
			want: "require server CertFile and KeyFile",
		},
		{
			name:    "server name without cert",
			srvName: "kv.internal",
			want:    "require server CertFile and KeyFile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tlsOptions(tc.cert, tc.key, tc.clientCA, tc.ca, tc.srvName)
			if err == nil {
				t.Fatal("tlsOptions accepted a half-configured TLS flag set")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestTLSOptionsBuildsNodeTLS(t *testing.T) {
	opts, err := tlsOptions("server.pem", "server.key", "clients.pem", "peers.pem", "kv.internal")
	if err != nil {
		t.Fatalf("tlsOptions: %v", err)
	}
	if !opts.Enabled() {
		t.Fatal("TLS flags did not enable TLS")
	}
	if opts.Server.CertFile != "server.pem" || opts.Server.KeyFile != "server.key" {
		t.Fatalf("server identity = %+v", opts.Server)
	}
	if opts.Server.ClientCAFile != "clients.pem" {
		t.Fatalf("client CA = %q, want clients.pem", opts.Server.ClientCAFile)
	}
	if opts.Client.CAFile != "peers.pem" || opts.Client.ServerName != "kv.internal" {
		t.Fatalf("peer client options = %+v", opts.Client)
	}
	// Peer dials authenticate with the node identity unless a distinct client
	// certificate is configured.
	peer := opts.ClientOptions()
	if peer.CertFile != "server.pem" || peer.KeyFile != "server.key" {
		t.Fatalf("peer identity = %+v, want the node certificate", peer)
	}
}
