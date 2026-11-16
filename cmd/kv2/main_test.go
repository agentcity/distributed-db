package main

import (
	"strings"
	"testing"
)

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
