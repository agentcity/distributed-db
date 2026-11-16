// Command metricsrv runs a small dependency-free Prometheus and SLO HTTP
// server.
//
// Routes:
//
//	GET /metrics          Prometheus text exposition (local plus optional peers)
//	GET /debug/slo        SLO report as JSON or text (?format=text)
//	GET /healthz          liveness probe
//	GET /debug/snapshot   JSON snapshot used for optional HTTP federation
//
// The server does not instrument KV or SQL by itself. Those packages can be
// wired later by accepting internal/metrics.Recorder or metrics.Tracker; see
// package metrics for the adapter helpers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/metrics"
)

func main() {
	addr := flag.String("addr", ":9100", "HTTP listen address")
	prefix := flag.String("prefix", metrics.DefaultPrefix, "SLO metric name prefix")
	snapshotFile := flag.String("snapshot-file", "", "optional JSON snapshot file to aggregate")
	upstream := flag.String("upstream", "", "optional peer /debug/snapshot URL to aggregate")
	upstreamName := flag.String("upstream-name", "upstream", "source label for aggregated upstream metrics")
	refresh := flag.Duration("refresh", 15*time.Second, "upstream refresh interval; 0 fetches only at startup")
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := metrics.NewRegistry()
	slo := metrics.NewSLO(reg, metrics.SLOConfig{Prefix: *prefix})
	aggregator := NewAggregator()

	if *snapshotFile != "" {
		if err := aggregator.LoadFile("file", *snapshotFile); err != nil {
			fatal(err)
		}
		log.Info("loaded snapshot file", "path", *snapshotFile)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	if *upstream != "" {
		name := strings.TrimSpace(*upstreamName)
		if name == "" {
			name = "upstream"
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := aggregator.Fetch(fetchCtx, client, name, *upstream)
		cancel()
		if err != nil {
			// A temporarily unavailable peer must not prevent local serving.
			log.Warn("initial upstream fetch failed", "url", *upstream, "error", err)
		} else {
			log.Info("aggregated upstream snapshot", "url", *upstream, "source", name)
		}
		if *refresh > 0 {
			go refreshLoop(ctx, aggregator, client, name, *upstream, *refresh, log)
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           NewServer(reg, slo, aggregator).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("metricsrv started", "addr", *addr, "prefix", *prefix)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
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
