// Command sqlsrv runs the P0 SQL server over the v2 transactional KV cluster.
//
// The SQL HTTP listener is plaintext without authentication by default, which
// is intended for local development only. Production deployments should set
// -tls-cert and -tls-key together with -auth-token. -auth-token without a TLS
// listener is rejected at startup so a Bearer token can never cross the
// network in cleartext.
//
// # SQL auth is not cluster admin auth
//
// -auth-token (or SQLSRV_AUTH_TOKEN) authenticates SQL HTTP clients only. It is
// deliberately not accepted as a cluster admin credential and is never forwarded
// to kv2/metaserver admin RPCs: sqlsrv serves no admin RPC, and reusing the SQL
// bearer token for control-plane RBAC would couple a data-plane credential to
// topology permissions. Admin RBAC (roles, mTLS CN mapping, audit log) is
// configured on the servers with kv2's -admin-token-roles, -admin-mtls-roles and
// -audit-log flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlhttp"
	"distributed-db/internal/v2client"
)

// authTokenEnv supplies the SQL bearer token when -auth-token is not set on the
// command line. It is read after flag parsing, so the value never appears in
// -help output or a flag parse error. It is a SQL transport credential and is
// never used as a cluster admin credential.
const authTokenEnv = "SQLSRV_AUTH_TOKEN"

// options holds the sqlsrv command-line flags. It is a separate type so tests
// can assert flag/env precedence and that no cluster admin flags are exposed
// without starting a listener.
type options struct {
	httpAddr      string
	metaAddr      string
	nodes         string
	tlsCA         string
	tlsCert       string
	tlsKey        string
	tlsServerName string
	authToken     string
	logLevel      string
}

// registerFlags registers the sqlsrv flags on fs. There are intentionally no
// -admin-token/-admin-mtls-roles/-audit-log flags here: sqlsrv has no admin
// surface. See the package comment.
func registerFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.httpAddr, "http-addr", ":8080", "SQL HTTP listen address (also serves /metrics, /debug/slo, and /healthz)")
	fs.StringVar(&o.metaAddr, "meta", "127.0.0.1:9090", "meta/TSO gRPC address")
	fs.StringVar(&o.nodes, "nodes", "", "comma-separated KV node gRPC addresses")
	fs.StringVar(&o.tlsCA, "tls-ca", "", "PEM CA bundle used to verify the meta/KV servers (default: system roots); setting it or -tls-server-name enables cluster TLS")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "PEM TLS certificate for the SQL HTTP listener (requires -tls-key); also presented to meta/KV when -tls-ca or -tls-server-name enables cluster TLS")
	fs.StringVar(&o.tlsKey, "tls-key", "", "PEM private key for -tls-cert (requires -tls-cert)")
	fs.StringVar(&o.tlsServerName, "tls-server-name", "", "expected server name for meta/KV certificate verification; setting it or -tls-ca enables cluster TLS")
	fs.StringVar(&o.authToken, "auth-token", "",
		"Bearer token required for /v1/sql, /metrics, and /debug/slo (requires -tls-cert/-tls-key; "+
			"env "+authTokenEnv+"); /health and /healthz stay public. This is SQL auth only, not a cluster admin credential")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn, error")
	return o
}

// resolveEnv applies the SQLSRV_AUTH_TOKEN fallback unless -auth-token was set
// explicitly. An explicit empty flag therefore disables SQL auth even when the
// environment is set.
func (o *options) resolveEnv(fs *flag.FlagSet) {
	if flagWasSet(fs, "auth-token") {
		return
	}
	if v := strings.TrimSpace(os.Getenv(authTokenEnv)); v != "" {
		o.authToken = v
	}
}

// httpConfig assembles the validated SQL HTTP configuration.
func (o *options) httpConfig() sqlhttp.Config {
	return sqlhttp.Config{
		TLSCertFile: o.tlsCert,
		TLSKeyFile:  o.tlsKey,
		AuthToken:   o.authToken,
	}
}

// flagWasSet reports whether name appeared on the command line.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			found = true
		}
	})
	return found
}

func main() {
	opts := registerFlags(flag.CommandLine)
	flag.Parse()
	opts.resolveEnv(flag.CommandLine)

	// Validate the security flags before touching the cluster so an unsafe
	// combination (for example auth without TLS) fails immediately.
	httpCfg := opts.httpConfig()
	if err := httpCfg.Validate(); err != nil {
		fatal(err)
	}
	if opts.nodes == "" {
		fmt.Fprintln(os.Stderr, "fatal: -nodes is required (comma-separated KV addresses)")
		os.Exit(2)
	}
	log := newLogger(opts.logLevel)
	// Cluster TLS is opt-in through -tls-ca or -tls-server-name so that
	// enabling TLS on the SQL HTTP listener alone keeps dialing an existing
	// plaintext cluster. Once enabled, the listener certificate is also
	// presented for mTLS when it is configured.
	clientTLS, err := clusterTLSOptions(opts.tlsCA, opts.tlsServerName, opts.tlsCert, opts.tlsKey)
	if err != nil {
		fatal(err)
	}
	client, err := v2client.NewWithTLS(opts.metaAddr, splitList(opts.nodes), clientTLS)
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	engine := sqlengine.New(v2client.NewSQLClient(client))
	reg := metrics.NewRegistry()
	sqlMetrics := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlMetrics)
	httpCfg.Engine = engine
	httpCfg.Metrics = reg
	httpCfg.SLO = sqlMetrics.SLO
	srv, err := newHTTPServer(opts.httpAddr, httpCfg)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	tlsEnabled := srv.TLSConfig != nil
	authEnabled := httpCfg.AuthEnabled()
	if !tlsEnabled {
		log.Warn("SQL HTTP listener is plaintext; use -tls-cert/-tls-key in production")
	}
	if !authEnabled {
		log.Warn("SQL HTTP authentication is disabled; use -auth-token in production")
	}
	log.Info("sqlsrv started",
		"http", opts.httpAddr,
		"tls", tlsEnabled,
		"auth", authEnabled,
		"metrics", "/metrics",
		"slo", "/debug/slo",
		"health", "/healthz",
		"meta", opts.metaAddr,
		"nodes", opts.nodes,
		"cluster_tls", clientTLS != nil,
		"cluster_admin", false)

	var serveErr error
	if tlsEnabled {
		// Certificates are already loaded into srv.TLSConfig by
		// sqlhttp.NewServer; empty file arguments keep them in place.
		serveErr = srv.ListenAndServeTLS("", "")
	} else {
		serveErr = srv.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		fatal(serveErr)
	}
}

// newHTTPServer builds the SQL HTTP server from the validated SQL config. It
// is a small seam so tests can exercise startup policy without binding a port.
func newHTTPServer(addr string, cfg sqlhttp.Config) (*http.Server, error) {
	return sqlhttp.NewServer(cfg, addr)
}

// clientTLSOptions builds the optional client TLS configuration used for both
// meta and KV dials. No options means plaintext. A certificate without its key
// is rejected. A client certificate is optional: when it is absent the client
// only verifies the server (TLS without mTLS).
func clientTLSOptions(caFile, certFile, keyFile, serverName string) (*grpctls.ClientOptions, error) {
	return grpctls.NewClientOptions(caFile, certFile, keyFile, serverName)
}

// clusterTLSOptions enables TLS for meta/KV dials only when a CA bundle or a
// server name is configured. The SQL listener's certificate/key flags alone do
// not turn on cluster TLS; when cluster TLS is enabled, the same certificate is
// presented for mTLS if it is configured.
func clusterTLSOptions(caFile, serverName, certFile, keyFile string) (*grpctls.ClientOptions, error) {
	if caFile == "" && serverName == "" {
		return nil, nil
	}
	return clientTLSOptions(caFile, certFile, keyFile, serverName)
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
	os.Exit(1)
}
