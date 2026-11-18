package main

import (
	"bytes"
	"flag"
	"io"
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

func newSQLFlagSet(t *testing.T) (*flag.FlagSet, *options) {
	t.Helper()
	fs := flag.NewFlagSet("sqlsrv-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, registerFlags(fs)
}

// TestSQLFlagsExposeNoClusterAdminSurface guards the design decision that SQL
// bearer auth is not cluster admin auth: sqlsrv serves no admin RPC and must
// not grow admin RBAC or audit flags by accident.
func TestSQLFlagsExposeNoClusterAdminSurface(t *testing.T) {
	fs, _ := newSQLFlagSet(t)
	for _, name := range []string{
		"admin-token", "admin-token-roles", "admin-token-roles-file",
		"admin-mtls-roles", "audit-log", "audit-log-sync", "audit-required",
	} {
		if fs.Lookup(name) != nil {
			t.Fatalf("sqlsrv registered cluster admin flag -%s", name)
		}
	}
	if fs.Lookup("auth-token") == nil {
		t.Fatal("sqlsrv is missing its SQL -auth-token flag")
	}
}

func TestAuthTokenEnvFallback(t *testing.T) {
	t.Setenv(authTokenEnv, "env-sql-token")
	fs, opts := newSQLFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts.resolveEnv(fs)
	if opts.authToken != "env-sql-token" {
		t.Fatalf("auth token = %q, want environment fallback", opts.authToken)
	}
}

func TestAuthTokenFlagWinsOverEnv(t *testing.T) {
	t.Setenv(authTokenEnv, "env-sql-token")
	fs, opts := newSQLFlagSet(t)
	if err := fs.Parse([]string{"-auth-token", "flag-sql-token"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts.resolveEnv(fs)
	if opts.authToken != "flag-sql-token" {
		t.Fatalf("auth token = %q, want the explicit flag", opts.authToken)
	}
}

func TestExplicitEmptyAuthTokenDisablesEnv(t *testing.T) {
	t.Setenv(authTokenEnv, "env-sql-token")
	fs, opts := newSQLFlagSet(t)
	if err := fs.Parse([]string{"-auth-token", ""}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts.resolveEnv(fs)
	if opts.authToken != "" {
		t.Fatalf("auth token = %q, want explicit empty to win over the environment", opts.authToken)
	}
}

func TestAuthTokenHelpDoesNotLeakEnvValue(t *testing.T) {
	const token = "sql-help-leak-token"
	t.Setenv(authTokenEnv, token)
	fs, _ := newSQLFlagSet(t)
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	if strings.Contains(buf.String(), token) {
		t.Fatalf("sqlsrv help leaked the token:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), authTokenEnv) {
		t.Fatalf("sqlsrv help does not document %s:\n%s", authTokenEnv, buf.String())
	}
}

func TestEnvAuthTokenStillRequiresTLS(t *testing.T) {
	const token = "sql-env-secret-token"
	t.Setenv(authTokenEnv, token)
	fs, opts := newSQLFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts.resolveEnv(fs)
	err := opts.httpConfig().Validate()
	if err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("httpConfig.Validate = %v, want an auth-requires-TLS error", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("validation error leaked the token: %v", err)
	}
}
