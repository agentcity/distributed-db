// Command pgsrv runs a minimal PostgreSQL wire-protocol front end for the P0
// SQL engine over the v2 transactional KV cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"distributed-db/internal/pgwire"
	"distributed-db/internal/sqlengine"
	"distributed-db/internal/v2client"
)

func main() {
	addr := flag.String("addr", ":5432", "PostgreSQL wire listen address")
	metaAddr := flag.String("meta", "127.0.0.1:9090", "meta/TSO gRPC address")
	nodes := flag.String("nodes", "", "comma-separated KV node gRPC addresses")
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	flag.Parse()

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
	srv := pgwire.New(engine, log)
	actual, err := srv.Start(*addr)
	if err != nil {
		fatal(err)
	}
	log.Info("pgsrv started", "addr", actual, "meta", *metaAddr, "nodes", *nodes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("pgsrv stopping")
	srv.Stop()
	log.Info("pgsrv stopped")
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
