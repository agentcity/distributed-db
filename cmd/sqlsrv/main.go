// Command sqlsrv runs the P0 SQL server over the v2 transactional KV cluster.
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

func main() {
	httpAddr := flag.String("http-addr", ":8080", "SQL HTTP listen address (also serves /metrics, /debug/slo, and /healthz)")
	metaAddr := flag.String("meta", "127.0.0.1:9090", "meta/TSO gRPC address")
	nodes := flag.String("nodes", "", "comma-separated KV node gRPC addresses")
	tlsCA := flag.String("tls-ca", "", "PEM CA bundle used to verify the meta/KV servers (default: system roots)")
	tlsCert := flag.String("tls-cert", "", "PEM client certificate for mTLS (requires -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert (requires -tls-cert)")
	tlsServerName := flag.String("tls-server-name", "", "expected server name for meta/KV certificate verification")
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	flag.Parse()

	if *nodes == "" {
		fmt.Fprintln(os.Stderr, "fatal: -nodes is required (comma-separated KV addresses)")
		os.Exit(2)
	}
	log := newLogger(*logLevel)
	clientTLS, err := clientTLSOptions(*tlsCA, *tlsCert, *tlsKey, *tlsServerName)
	if err != nil {
		fatal(err)
	}
	client, err := v2client.NewWithTLS(*metaAddr, splitList(*nodes), clientTLS)
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	engine := sqlengine.New(v2client.NewSQLClient(client))
	reg := metrics.NewRegistry()
	sqlMetrics := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlMetrics)
	srv := &http.Server{
		Addr: *httpAddr,
		Handler: sqlhttp.NewWithConfig(sqlhttp.Config{
			Engine:  engine,
			Metrics: reg,
			SLO:     sqlMetrics.SLO,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("sqlsrv started",
		"http", *httpAddr,
		"metrics", "/metrics",
		"slo", "/debug/slo",
		"meta", *metaAddr,
		"nodes", *nodes,
		"cluster_tls", clientTLS != nil)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

// clientTLSOptions builds the optional client TLS configuration used for both
// meta and KV dials. No flags means plaintext. A certificate without its key is
// rejected. A client certificate is optional: when it is absent the client only
// verifies the server (TLS without mTLS).
func clientTLSOptions(caFile, certFile, keyFile, serverName string) (*grpctls.ClientOptions, error) {
	return grpctls.NewClientOptions(caFile, certFile, keyFile, serverName)
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
