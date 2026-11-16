package grpctls_test

import (
	"crypto/tls"
	"path/filepath"
	"testing"

	"distributed-db/internal/grpctls"
)

func TestServerConfigDefaults(t *testing.T) {
	f := newTestFiles(t)
	cfg, err := grpctls.ServerConfig(grpctls.ServerOptions{
		CertFile: f.serverCert,
		KeyFile:  f.serverKey,
	})
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs != nil {
		t.Error("ClientCAs is non-nil without ClientCAFile")
	}
	if got := len(cfg.Certificates); got != 1 {
		t.Errorf("len(Certificates) = %d, want 1", got)
	}
	if cfg.ServerName != "" {
		t.Errorf("ServerName = %q, want empty", cfg.ServerName)
	}
}

func TestServerConfigMTLS(t *testing.T) {
	f := newTestFiles(t)
	cfg, err := grpctls.ServerConfig(grpctls.ServerOptions{
		CertFile:     f.serverCert,
		KeyFile:      f.serverKey,
		ClientCAFile: f.caFile,
		ServerName:   "kv.internal",
	})
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil with ClientCAFile set")
	}
	if cfg.ServerName != "kv.internal" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "kv.internal")
	}
}

func TestServerConfigExplicitTLS12(t *testing.T) {
	f := newTestFiles(t)
	cfg, err := grpctls.ServerConfig(grpctls.ServerOptions{
		CertFile:   f.serverCert,
		KeyFile:    f.serverKey,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestServerConfigErrors(t *testing.T) {
	f := newTestFiles(t)
	missing := filepath.Join(f.dir, "does-not-exist.pem")
	tests := []struct {
		name string
		opts grpctls.ServerOptions
		want string
	}{
		{
			name: "missing key",
			opts: grpctls.ServerOptions{CertFile: f.serverCert},
			want: "CertFile and KeyFile are required",
		},
		{
			name: "missing cert",
			opts: grpctls.ServerOptions{KeyFile: f.serverKey},
			want: "CertFile and KeyFile are required",
		},
		{
			name: "missing cert file",
			opts: grpctls.ServerOptions{CertFile: missing, KeyFile: f.serverKey},
			want: "load certificate/key pair",
		},
		{
			name: "missing key file",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: missing},
			want: "load certificate/key pair",
		},
		{
			name: "malformed cert",
			opts: grpctls.ServerOptions{CertFile: f.malformed, KeyFile: f.serverKey},
			want: "load certificate/key pair",
		},
		{
			name: "malformed key",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: f.malformed},
			want: "load certificate/key pair",
		},
		{
			name: "mismatched key",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: f.clientKey},
			want: "load certificate/key pair",
		},
		{
			name: "missing client CA file",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: f.serverKey, ClientCAFile: missing},
			want: "read ClientCAFile",
		},
		{
			name: "malformed client CA",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: f.serverKey, ClientCAFile: f.malformed},
			want: "parse ClientCAFile",
		},
		{
			name: "empty client CA",
			opts: grpctls.ServerOptions{CertFile: f.serverCert, KeyFile: f.serverKey, ClientCAFile: f.empty},
			want: "parse ClientCAFile",
		},
		{
			name: "unsupported min version",
			opts: grpctls.ServerOptions{
				CertFile:   f.serverCert,
				KeyFile:    f.serverKey,
				MinVersion: tls.VersionTLS11,
			},
			want: "unsupported MinVersion",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := grpctls.ServerConfig(tt.opts)
			wantError(t, err, tt.want)
		})
	}
}

func TestClientConfigDefaults(t *testing.T) {
	f := newTestFiles(t)
	cfg, err := grpctls.ClientConfig(grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is true by default")
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil with CAFile set")
	}
	if cfg.ServerName != "localhost" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "localhost")
	}
	if got := len(cfg.Certificates); got != 0 {
		t.Errorf("len(Certificates) = %d, want 0 without client cert", got)
	}
}

func TestClientConfigMutualTLS(t *testing.T) {
	f := newTestFiles(t)
	cfg, err := grpctls.ClientConfig(grpctls.ClientOptions{
		CAFile:     f.caFile,
		CertFile:   f.clientCert,
		KeyFile:    f.clientKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if got := len(cfg.Certificates); got != 1 {
		t.Errorf("len(Certificates) = %d, want 1", got)
	}
}

func TestClientConfigSystemRoots(t *testing.T) {
	cfg, err := grpctls.ClientConfig(grpctls.ClientOptions{})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs should be nil so the system roots are used")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is true by default")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
}

func TestClientConfigInsecureSkipVerifyExplicit(t *testing.T) {
	cfg, err := grpctls.ClientConfig(grpctls.ClientOptions{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want explicit true")
	}
}

func TestClientConfigErrors(t *testing.T) {
	f := newTestFiles(t)
	missing := filepath.Join(f.dir, "does-not-exist.pem")
	tests := []struct {
		name string
		opts grpctls.ClientOptions
		want string
	}{
		{
			name: "cert without key",
			opts: grpctls.ClientOptions{CertFile: f.clientCert},
			want: "must be provided together",
		},
		{
			name: "key without cert",
			opts: grpctls.ClientOptions{KeyFile: f.clientKey},
			want: "must be provided together",
		},
		{
			name: "missing CA file",
			opts: grpctls.ClientOptions{CAFile: missing},
			want: "read CAFile",
		},
		{
			name: "malformed CA",
			opts: grpctls.ClientOptions{CAFile: f.malformed},
			want: "parse CAFile",
		},
		{
			name: "empty CA",
			opts: grpctls.ClientOptions{CAFile: f.empty},
			want: "parse CAFile",
		},
		{
			name: "malformed client cert",
			opts: grpctls.ClientOptions{CertFile: f.malformed, KeyFile: f.clientKey},
			want: "load certificate/key pair",
		},
		{
			name: "mismatched client key",
			opts: grpctls.ClientOptions{CertFile: f.clientCert, KeyFile: f.serverKey},
			want: "load certificate/key pair",
		},
		{
			name: "unsupported min version",
			opts: grpctls.ClientOptions{MinVersion: tls.VersionTLS10},
			want: "unsupported MinVersion",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := grpctls.ClientConfig(tt.opts)
			wantError(t, err, tt.want)
		})
	}
}

func TestCredentials(t *testing.T) {
	f := newTestFiles(t)

	serverCreds, err := grpctls.ServerCredentials(grpctls.ServerOptions{
		CertFile: f.serverCert,
		KeyFile:  f.serverKey,
	})
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	if serverCreds == nil {
		t.Fatal("ServerCredentials returned nil credentials")
	}
	if got := serverCreds.Info().SecurityProtocol; got != "tls" {
		t.Errorf("server SecurityProtocol = %q, want tls", got)
	}

	clientCreds, err := grpctls.ClientCredentials(grpctls.ClientOptions{
		CAFile:     f.caFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}
	if clientCreds == nil {
		t.Fatal("ClientCredentials returned nil credentials")
	}
	if got := clientCreds.Info().SecurityProtocol; got != "tls" {
		t.Errorf("client SecurityProtocol = %q, want tls", got)
	}
}

func TestCredentialsPropagateErrors(t *testing.T) {
	if _, err := grpctls.ServerCredentials(grpctls.ServerOptions{}); err == nil {
		t.Fatal("ServerCredentials accepted missing certificate and key")
	}
	if _, err := grpctls.ClientCredentials(grpctls.ClientOptions{MinVersion: tls.VersionTLS11}); err == nil {
		t.Fatal("ClientCredentials accepted an unsupported TLS version")
	}
}
