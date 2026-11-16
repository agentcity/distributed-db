package grpctls_test

import (
	"strings"
	"testing"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
)

func TestOptionsZeroIsPlaintext(t *testing.T) {
	var opts grpctls.Options
	if opts.Enabled() {
		t.Fatal("zero Options reports TLS enabled")
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("zero Options Validate: %v", err)
	}
	server, client, err := opts.Credentials()
	if err != nil {
		t.Fatalf("zero Options Credentials: %v", err)
	}
	if server != nil || client != nil {
		t.Fatalf("zero Options returned credentials: server=%v client=%v", server, client)
	}
}

func TestOptionsRejectsHalfConfiguration(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	cases := []struct {
		name string
		opts grpctls.Options
		want string
	}{
		{
			name: "cert without key",
			opts: grpctls.Options{Server: grpctls.ServerOptions{CertFile: certs.ServerCert}},
			want: "both CertFile and KeyFile",
		},
		{
			name: "key without cert",
			opts: grpctls.Options{Server: grpctls.ServerOptions{KeyFile: certs.ServerKey}},
			want: "both CertFile and KeyFile",
		},
		{
			name: "client CA without server identity",
			opts: grpctls.Options{Server: grpctls.ServerOptions{ClientCAFile: certs.CAFile}},
			want: "require server CertFile and KeyFile",
		},
		{
			name: "client CA without server cert",
			opts: grpctls.Options{
				Server: grpctls.ServerOptions{KeyFile: certs.ServerKey},
				Client: grpctls.ClientOptions{CAFile: certs.CAFile},
			},
			want: "both CertFile and KeyFile",
		},
		{
			name: "client trust without server identity",
			opts: grpctls.Options{Client: grpctls.ClientOptions{CAFile: certs.CAFile}},
			want: "require server CertFile and KeyFile",
		},
		{
			name: "client cert without client key",
			opts: grpctls.Options{
				Server: grpctls.ServerOptions{CertFile: certs.ServerCert, KeyFile: certs.ServerKey},
				Client: grpctls.ClientOptions{CertFile: certs.ClientCert},
			},
			want: "Client.CertFile and Client.KeyFile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if err == nil {
				t.Fatal("Validate accepted a half-configured TLS configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
			if _, _, err := tc.opts.Credentials(); err == nil {
				t.Fatal("Credentials accepted a half-configured TLS configuration")
			}
		})
	}
}

func TestOptionsBuildsServerAndClientCredentials(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	opts := grpctls.Options{
		Server: grpctls.ServerOptions{
			CertFile:     certs.ServerCert,
			KeyFile:      certs.ServerKey,
			ClientCAFile: certs.CAFile, // mTLS
		},
		Client: grpctls.ClientOptions{
			CAFile:     certs.CAFile,
			ServerName: "localhost",
		},
	}
	if !opts.Enabled() {
		t.Fatal("Options with a certificate reports TLS disabled")
	}
	server, client, err := opts.Credentials()
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if server == nil || client == nil {
		t.Fatalf("Credentials returned nil credentials: server=%v client=%v", server, client)
	}
	// The node identity doubles as the peer client identity for mTLS.
	peer := opts.ClientOptions()
	if peer.CertFile != certs.ServerCert || peer.KeyFile != certs.ServerKey {
		t.Fatalf("peer client identity = (%q, %q), want the node certificate",
			peer.CertFile, peer.KeyFile)
	}
	if peer.InsecureSkipVerify {
		t.Fatalf("peer client unexpectedly disables verification: %+v", peer)
	}
}

func TestOptionsRejectsUnreadableFiles(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	opts := grpctls.Options{
		Server: grpctls.ServerOptions{CertFile: certs.ServerCert, KeyFile: certs.ServerKey},
		Client: grpctls.ClientOptions{CAFile: certs.Dir + "/missing-ca.pem"},
	}
	if _, _, err := opts.Credentials(); err == nil {
		t.Fatal("Credentials accepted a missing CA file")
	}
}

func TestNewClientOptions(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	if opts, err := grpctls.NewClientOptions("", "", "", ""); err != nil || opts != nil {
		t.Fatalf("empty NewClientOptions = (%v, %v), want (nil, nil)", opts, err)
	}
	if _, err := grpctls.NewClientOptions("", certs.ClientCert, "", ""); err == nil {
		t.Fatal("NewClientOptions accepted a certificate without a key")
	}
	opts, err := grpctls.NewClientOptions(certs.CAFile, certs.ClientCert, certs.ClientKey, "localhost")
	if err != nil {
		t.Fatalf("NewClientOptions: %v", err)
	}
	if opts.CAFile != certs.CAFile || opts.CertFile != certs.ClientCert ||
		opts.KeyFile != certs.ClientKey || opts.ServerName != "localhost" {
		t.Fatalf("NewClientOptions returned %+v", opts)
	}
}
