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

	"distributed-db/internal/grpctls"
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
	metaID := flag.String("meta-id", "", "meta node id, defaults to -addr")
	metaPeers := flag.String("meta-peers", "", "comma-separated other meta gRPC addresses")
	metaDataDir := flag.String("meta-data-dir", "./data/meta", "meta Raft WAL/snapshot directory")
	adminToken := registerAdminTokenFlag(flag.CommandLine)
	logLevel := flag.String("log-level", "info", "debug, info, warn, error")
	durability := flag.String("durability", kvserver.DurabilitySync,
		`state-machine durability for -mode=node: "sync" (fsync every batch; production default) `+
			`or "fast" (skip state-machine fsyncs and rely on Raft log replay; `+
			`NOT production-safe until crash tests pass)`)
	tlsCert := flag.String("tls-cert", "", "PEM certificate for the gRPC listener; enables TLS (requires -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert (requires -tls-cert)")
	tlsClientCA := flag.String("tls-client-ca", "", "PEM CA bundle that signs client certificates; enables mTLS")
	tlsCA := flag.String("tls-ca", "", "PEM CA bundle used to verify KV/meta peers (default: system roots)")
	tlsServerName := flag.String("tls-server-name", "", "expected server name for peer certificate verification")
	flag.Parse()

	log := newLogger(*logLevel)
	tlsCfg, err := tlsOptions(*tlsCert, *tlsKey, *tlsClientCA, *tlsCA, *tlsServerName)
	if err != nil {
		fatal(err)
	}

	switch *mode {
	case "meta":
		metaIDVal := *metaID
		if metaIDVal == "" {
			metaIDVal = *addr
		}
		metaCfg := metaServerConfig(*addr, metaIDVal, *metaDataDir, splitList(*metaPeers), log, tlsCfg, *adminToken)
		srv, err := metaserver.New(metaCfg)
		if err != nil {
			fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Start(ctx); err != nil {
			fatal(err)
		}
		log.Info("kv2 meta started", "addr", srv.Addr(), "tls", tlsCfg.Enabled(),
			"mtls", *tlsClientCA != "", "admin_auth", *adminToken != "")
		<-ctx.Done()
		_ = srv.Stop()

	case "node":
		if *id == "" {
			*id = *raftAddr
		}
		if *durability == kvserver.DurabilityFast {
			log.Warn("durability=fast: state-machine batches are not fsynced; " +
				"not production-safe until crash tests pass")
		}
		srv, err := kvserver.New(kvserver.Config{
			ID:             *id,
			RaftAddr:       *raftAddr,
			DataDir:        *dataDir,
			Peers:          splitList(*peers),
			DurabilityMode: *durability,
			Logger:         log,
			TLS:            tlsCfg,
		})
		if err != nil {
			fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Start(ctx); err != nil {
			fatal(err)
		}
		log.Info("kv2 node started", "id", *id, "addr", srv.Addr(),
			"tls", tlsCfg.Enabled(), "mtls", *tlsClientCA != "")
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

// registerAdminTokenFlag registers the meta admin-token flag. It is split out
// so a test can assert the flag name and parsing without starting a server.
func registerAdminTokenFlag(fs *flag.FlagSet) *string {
	return fs.String("admin-token", "",
		"meta mode: Bearer token required on meta admin RPCs "+
			"(SetRanges/SplitRange/MergeRanges); empty disables the check (development only)")
}

// metaServerConfig assembles the meta server configuration from flags. It is a
// helper so tests can assert that -admin-token and the peer list are wired
// into metaserver.Config without starting a listener.
func metaServerConfig(addr, id, dataDir string, peers []string, log *slog.Logger, tlsCfg grpctls.Options, adminToken string) metaserver.Config {
	cfg := metaserver.Config{
		Addr: addr, ID: id, Peers: peers,
		DataDir: dataDir, Logger: log, TLS: tlsCfg,
		AdminToken: adminToken,
	}
	if len(cfg.Peers) > 0 {
		cfg.Replicated = true
	}
	return cfg
}

// tlsOptions builds and validates the cluster TLS configuration from the
// command-line flags. The zero value (no flags) keeps plaintext; a certificate
// without its key, or client-only flags without a server certificate, is
// rejected instead of silently disabling TLS.
func tlsOptions(certFile, keyFile, clientCAFile, caFile, serverName string) (grpctls.Options, error) {
	opts := grpctls.Options{
		Server: grpctls.ServerOptions{
			CertFile:     certFile,
			KeyFile:      keyFile,
			ClientCAFile: clientCAFile,
		},
		Client: grpctls.ClientOptions{
			CAFile:     caFile,
			ServerName: serverName,
		},
	}
	if err := opts.Validate(); err != nil {
		return grpctls.Options{}, err
	}
	return opts, nil
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
