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

	"distributed-db/internal/sqlengine"
	"distributed-db/internal/sqlhttp"
	"distributed-db/internal/v2client"
)

func main() {
	httpAddr := flag.String("http-addr", ":8080", "SQL HTTP listen address")
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
	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           sqlhttp.New(engine),
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

	log.Info("sqlsrv started", "http", *httpAddr, "meta", *metaAddr, "nodes", *nodes)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
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
