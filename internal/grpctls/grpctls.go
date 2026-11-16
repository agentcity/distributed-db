// Package grpctls provides reusable, hardened TLS/mTLS configuration and gRPC
// transport credentials for the KV, Raft, and meta servers and clients.
//
// # Security defaults
//
//   - TLS 1.3 is the minimum accepted protocol version unless a caller
//     explicitly selects TLS 1.2 through MinVersion.
//   - Server certificate verification is enabled by default on clients.
//     InsecureSkipVerify must be set explicitly and is intended only for
//     isolated tests and local development.
//   - A server requests and verifies client certificates only when
//     ServerOptions.ClientCAFile is set. That option therefore enables mTLS;
//     there is no way to enable mTLS without a client CA.
//
// The package deliberately has no plaintext fallback. Callers must propagate
// errors from ServerConfig/ClientConfig (or ServerCredentials/
// ClientCredentials) and must not fall back to credentials/insecure.
//
// # Integration points
//
// KV, Raft, and meta servers pass ServerCredentials to grpc.NewServer:
//
//	creds, err := grpctls.ServerCredentials(grpctls.ServerOptions{
//		CertFile:     cfg.TLSCertFile,
//		KeyFile:      cfg.TLSKeyFile,
//		ClientCAFile: cfg.TLSClientCAFile, // non-empty enables mTLS
//	})
//	if err != nil {
//		return nil, err
//	}
//	srv := grpc.NewServer(grpc.Creds(creds))
//
// v2client and meta peers pass ClientCredentials to grpc.NewClient:
//
//	creds, err := grpctls.ClientCredentials(grpctls.ClientOptions{
//		CAFile:     cfg.TLSCAFile,
//		CertFile:   cfg.TLSCertFile, // optional client certificate
//		KeyFile:    cfg.TLSKeyFile,
//		ServerName: cfg.TLSServerName,
//	})
//	if err != nil {
//		return nil, err
//	}
//	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
//
// Raft transports and meta leader/rebalance dials should use the same
// ClientOptions as v2client so that all inter-node RPCs share one trust root.
package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// ServerOptions configures a TLS server.
//
// CertFile and KeyFile are required and must be a matching X.509 certificate
// and private key pair. Setting ClientCAFile enables mTLS: the server then
// requires every client to present a certificate signed by that CA.
//
// MinVersion is the minimum accepted TLS version. The zero value selects TLS
// 1.3; only tls.VersionTLS12 and tls.VersionTLS13 are accepted.
//
// ServerName is optional and copied to tls.Config.ServerName. Servers normally
// do not need it; it exists for callers that share one option shape with
// clients. Certificate and hostname verification happens on the client.
type ServerOptions struct {
	// CertFile is the PEM-encoded server certificate (leaf first).
	CertFile string
	// KeyFile is the PEM-encoded private key for CertFile.
	KeyFile string
	// ClientCAFile is an optional PEM bundle of CAs used to verify client
	// certificates. A non-empty value enables mutual TLS.
	ClientCAFile string
	// MinVersion is the minimum accepted TLS version. Zero means TLS 1.3.
	MinVersion uint16
	// ServerName is optional; see the type documentation.
	ServerName string
}

// ClientOptions configures a TLS client.
//
// CAFile is the PEM bundle used to verify the server certificate. If it is
// empty, the host's system roots are used. CertFile and KeyFile are optional
// and must be provided together; when set, the client presents a certificate
// for mTLS.
//
// ServerName is the hostname checked against the server certificate. gRPC
// normally derives it from the dial target when it is empty, but setting it
// explicitly is recommended when nodes are addressed by IP address or through
// a load balancer.
//
// InsecureSkipVerify disables server certificate and hostname verification. It
// is for explicit tests and local development only; production callers must
// leave it false.
//
// MinVersion is the minimum accepted TLS version. The zero value selects TLS
// 1.3; only tls.VersionTLS12 and tls.VersionTLS13 are accepted.
type ClientOptions struct {
	// CAFile is an optional PEM bundle used to verify the server. Empty
	// means use the system roots.
	CAFile string
	// CertFile is the optional PEM-encoded client certificate.
	CertFile string
	// KeyFile is the PEM-encoded private key for CertFile.
	KeyFile string
	// ServerName is the expected server name. Empty lets gRPC derive it
	// from the dial target.
	ServerName string
	// InsecureSkipVerify disables server certificate verification. Set it
	// only for explicit tests or local development.
	InsecureSkipVerify bool
	// MinVersion is the minimum accepted TLS version. Zero means TLS 1.3.
	MinVersion uint16
}

// ServerConfig builds a server tls.Config from opts.
//
// It rejects incomplete certificate/key pairs, unreadable or malformed
// certificate, key, and client CA files, and TLS versions older than TLS 1.2.
// When ClientCAFile is empty, client authentication is disabled
// (tls.NoClientCert); otherwise it is set to tls.RequireAndVerifyClientCert.
func ServerConfig(opts ServerOptions) (*tls.Config, error) {
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, fmt.Errorf("grpctls: server: CertFile and KeyFile are required")
	}
	minVersion, err := resolveMinVersion("server", opts.MinVersion)
	if err != nil {
		return nil, err
	}
	cert, err := loadKeyPair("server", opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
		ServerName:   opts.ServerName,
	}
	if opts.ClientCAFile != "" {
		pool, err := loadCertPool("server", "ClientCAFile", opts.ClientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientConfig builds a client tls.Config from opts.
//
// It rejects incomplete client certificate/key pairs, unreadable or malformed
// CA, certificate, and key files, and TLS versions older than TLS 1.2.
// Certificate verification stays enabled unless InsecureSkipVerify is set
// explicitly.
func ClientConfig(opts ClientOptions) (*tls.Config, error) {
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return nil, fmt.Errorf("grpctls: client: CertFile and KeyFile must be provided together")
	}
	minVersion, err := resolveMinVersion("client", opts.MinVersion)
	if err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		MinVersion: minVersion,
		ServerName: opts.ServerName,
		// Explicit opt-in only. ClientOptions documents that this is for
		// tests and local development, never for production.
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // explicit opt-in
	}
	if opts.CAFile != "" {
		pool, err := loadCertPool("client", "CAFile", opts.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if opts.CertFile != "" {
		cert, err := loadKeyPair("client", opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// ServerCredentials returns gRPC transport credentials for a TLS server.
//
// It is a convenience wrapper around ServerConfig. Callers must propagate its
// error and must never fall back to plaintext credentials.
func ServerCredentials(opts ServerOptions) (credentials.TransportCredentials, error) {
	cfg, err := ServerConfig(opts)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

// ClientCredentials returns gRPC transport credentials for a TLS client.
//
// It is a convenience wrapper around ClientConfig. Callers must propagate its
// error and must never fall back to plaintext credentials.
func ClientCredentials(opts ClientOptions) (credentials.TransportCredentials, error) {
	cfg, err := ClientConfig(opts)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func resolveMinVersion(role string, minVersion uint16) (uint16, error) {
	switch minVersion {
	case 0:
		return tls.VersionTLS13, nil
	case tls.VersionTLS12, tls.VersionTLS13:
		return minVersion, nil
	default:
		return 0, fmt.Errorf(
			"grpctls: %s: unsupported MinVersion %s (0x%04x): use tls.VersionTLS12 or tls.VersionTLS13 (zero means TLS 1.3)",
			role, tls.VersionName(minVersion), minVersion,
		)
	}
}

func loadKeyPair(role, certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf(
			"grpctls: %s: load certificate/key pair (CertFile=%q KeyFile=%q): %w",
			role, certFile, keyFile, err,
		)
	}
	return cert, nil
}

func loadCertPool(role, field, path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("grpctls: %s: read %s %q: %w", role, field, path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("grpctls: %s: parse %s %q: no valid PEM certificates found", role, field, path)
	}
	return pool, nil
}
