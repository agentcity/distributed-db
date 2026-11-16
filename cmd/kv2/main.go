// Command kv2 runs the v2 transactional KV plane.
//
//	kv2 -mode=meta -addr=:9090
//	kv2 -mode=node -id=127.0.0.1:7001 -raft-addr=127.0.0.1:7001 \
//	    -data-dir=./data/n1 -peers=127.0.0.1:7002,127.0.0.1:7003
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
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
)

func main() {
	mode := flag.String("mode", "node", "node or meta")
	id := flag.String("id", "", "node id, defaults to raft-addr")
	raftAddr := flag.String("raft-addr", ":7001", "gRPC listen address for KVService/RaftService")
	addr := flag.String("addr", ":9090", "meta gRPC listen address")
	dataDir := flag.String("data-dir", "./data/kv2", "node data directory")
	peers := flag.String("peers", "", "comma-separated Raft peer addresses")
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	switch *mode {
	case "meta":
		srv, err := metaserver.New(metaserver.Config{Addr: *addr, Logger: log})
		if err != nil {
			fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Start(ctx); err != nil {
			fatal(err)
		}
		log.Info("kv2 meta started", "addr", srv.Addr())
		<-ctx.Done()
		_ = srv.Stop()

	case "node":
		if *id == "" {
			*id = *raftAddr
		}
		srv, err := kvserver.New(kvserver.Config{
			ID:       *id,
			RaftAddr: *raftAddr,
			DataDir:  *dataDir,
			Peers:    splitList(*peers),
			Logger:   log,
		})
		if err != nil {
			fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Start(ctx); err != nil {
			fatal(err)
		}
		log.Info("kv2 node started", "id", *id, "addr", srv.Addr())
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Stop(shutdownCtx); err != nil {
			log.Error("shutdown failed", "error", err)
			os.Exit(1)
		}

	default:
		fatal(fmt.Errorf("unknown mode %q (want node or meta)", *mode))
	}
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
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
