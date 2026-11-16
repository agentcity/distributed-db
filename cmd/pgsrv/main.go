// Command pgsrv runs a PostgreSQL wire-protocol front end for the P0 SQL
// engine over the v2 transactional KV cluster. It supports simple Query and
// the extended query protocol (Parse/Bind/Describe/Execute/Sync/Close/Flush)
// with INT/TEXT parameters and text/binary row formats.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/pgwire"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/v2client"
)

func main() {
	addr := flag.String("addr", ":5432", "PostgreSQL wire listen address")
	metricsAddr := flag.String("metrics-addr", "", "optional HTTP listen address for /metrics, /debug/slo, and /healthz (empty disables)")
	metaAddr := flag.String("meta", "127.0.0.1:9090", "meta/TSO gRPC address")
	nodes := flag.String("nodes", "", "comma-separated KV node gRPC addresses")
	tlsCert := flag.String("tls-cert", "", "PEM TLS server certificate file (requires -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM TLS server private key file (requires -tls-cert)")
	authUser := flag.String("auth-user", "", "require password authentication for this user (requires TLS)")
	authPassword := flag.String("auth-password", "", "password for -auth-user (requires TLS; visible in the process list)")
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	flag.Parse()

	cfg, err := pgwireConfig(*tlsCert, *tlsKey, *authUser, *authPassword)
	if err != nil {
		fatal(err)
	}
	if *nodes == "" {
		fmt.Fprintln(os.Stderr, "fatal: -nodes is required (comma-separated KV addresses)")
		os.Exit(2)
	}
	log := newLogger(*logLevel)
	client, err := v2client.New(*metaAddr, splitList(*nodes))
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	engine := sqlengine.New(v2client.NewSQLClient(client))
	var reg *metrics.Registry
	var querySLO *metrics.SLO
	if *metricsAddr != "" {
		reg = metrics.NewRegistry()
		sqlMetrics := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
		engine.SetMetrics(sqlMetrics)
		querySLO = metrics.NewSLO(reg, metrics.SLOConfig{Prefix: "pgwire"})
	}
	srv, err := pgwire.NewWithConfig(engine, log, cfg)
	if err != nil {
		fatal(err)
	}
	srv.SetMetrics(reg, querySLO)
	actual, err := srv.Start(*addr)
	if err != nil {
		fatal(err)
	}

	var metricsSrv *http.Server
	if *metricsAddr != "" {
		ln, err := net.Listen("tcp", *metricsAddr)
		if err != nil {
			srv.Stop()
			fatal(fmt.Errorf("metrics listen %q: %w", *metricsAddr, err))
		}
		metricsSrv = &http.Server{
			Handler:           metrics.NewHTTPHandler(reg, querySLO),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := metricsSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Error("pgsrv metrics server stopped", "error", err)
			}
		}()
		log.Info("pgsrv metrics enabled",
			"addr", ln.Addr().String(),
			"endpoints", "/metrics /debug/slo /healthz")
	}
	log.Info("pgsrv started",
		"addr", actual,
		"meta", *metaAddr,
		"nodes", *nodes,
		"tls", cfg.TLSEnabled(),
		"auth", cfg.AuthEnabled())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("pgsrv stopping")
	srv.Stop()
	if metricsSrv != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = metricsSrv.Shutdown(shutdown)
		cancel()
	}
	log.Info("pgsrv stopped")
}

// pgwireConfig builds and structurally validates the pgwire configuration.
// Certificate files are loaded later by pgwire.NewWithConfig, so this function
// fails fast on flag combinations that can never be secure (for example
// password auth without TLS) before dialing the cluster.
func pgwireConfig(tlsCert, tlsKey, authUser, authPassword string) (pgwire.Config, error) {
	cfg := pgwire.Config{
		TLSCertFile:  tlsCert,
		TLSKeyFile:   tlsKey,
		AuthUser:     authUser,
		AuthPassword: authPassword,
	}
	if err := cfg.Validate(); err != nil {
		return pgwire.Config{}, err
	}
	return cfg, nil
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
