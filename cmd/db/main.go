package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"distributed-db/internal/coordinator"
	"distributed-db/internal/node"
)

const usage = `distributed-db — шардированное key/value хранилище на Raft

Usage:
  db node        [flags]
  db coordinator [flags]

Run "db node -h" or "db coordinator -h" for the full flag list.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "node":
		runNode(os.Args[2:])
	case "coordinator":
		runCoordinator(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	var (
		id        = fs.String("id", "", "node id, defaults to -raft-addr")
		shard     = fs.String("shard", "shard-1", "shard name this node belongs to")
		raftAddr  = fs.String("raft-addr", ":7001", "gRPC listen address for the node service")
		httpAddr  = fs.String("http-addr", ":8001", "HTTP listen address")
		peers     = fs.String("peers", "", "comma-separated addresses of the other raft members")
		dataDir   = fs.String("data-dir", "./data/node", "directory for raft and kv WAL files")
		coordAddr = fs.String("coordinator", "", "optional coordinator gRPC address for load reports")
		authUser  = fs.String("auth-user", "", "HTTP basic auth user (empty disables auth)")
		authPass  = fs.String("auth-pass", "", "HTTP basic auth password")
		tlsCert   = fs.String("tls-cert", "", "TLS certificate file (enables TLS)")
		tlsKey    = fs.String("tls-key", "", "TLS private key file")
		tlsCA     = fs.String("tls-ca", "", "CA bundle for peer TLS verification")
		rateLimit = fs.Float64("rate-limit", 0, "global HTTP requests per second (0 disables)")
		rateBurst = fs.Int("rate-burst", 200, "HTTP rate limiter burst")
		logLevel  = fs.String("log-level", "info", "debug, info, warn or error")
		logFormat = fs.String("log-format", "text", "text or json")
	)
	_ = fs.Parse(args)

	if *id == "" {
		*id = *raftAddr
	}
	if err := validateCommon(*authUser, *authPass, *tlsCert, *tlsKey, *rateLimit, *rateBurst); err != nil {
		fatal(err)
	}
	log := newLogger(*logLevel, *logFormat)

	peerList := splitList(*peers)
	if len(peerList) > 1 {
		sort.Strings(peerList)
	}

	n, err := node.New(node.Config{
		ID:              *id,
		Shard:           *shard,
		RaftAddr:        *raftAddr,
		HTTPAddr:        *httpAddr,
		Peers:           peerList,
		DataDir:         *dataDir,
		CoordinatorAddr: *coordAddr,
		AuthUser:        *authUser,
		AuthPass:        *authPass,
		TLSCert:         *tlsCert,
		TLSKey:          *tlsKey,
		TLSCA:           *tlsCA,
		RateLimit:       *rateLimit,
		RateBurst:       *rateBurst,
		Logger:          log,
	})
	if err != nil {
		fatal(fmt.Errorf("create node: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := n.Start(ctx); err != nil {
		fatal(fmt.Errorf("start node: %w", err))
	}
	log.Info("node started", "id", *id, "shard", *shard, "raft", *raftAddr, "http", *httpAddr)

	<-ctx.Done()
	log.Info("shutting down node", "id", *id)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Stop(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("node stopped", "id", *id)
}

func runCoordinator(args []string) {
	fs := flag.NewFlagSet("coordinator", flag.ExitOnError)
	var (
		httpAddr  = fs.String("http-addr", ":8080", "HTTP listen address for clients")
		grpcAddr  = fs.String("grpc-addr", ":9090", "gRPC listen address")
		shards    = fs.String("shards", "", "shard map: shard-1=addr1,addr2;shard-2=addr3,addr4")
		authUser  = fs.String("auth-user", "", "HTTP basic auth user (empty disables auth)")
		authPass  = fs.String("auth-pass", "", "HTTP basic auth password")
		tlsCert   = fs.String("tls-cert", "", "TLS certificate file (enables TLS)")
		tlsKey    = fs.String("tls-key", "", "TLS private key file")
		tlsCA     = fs.String("tls-ca", "", "CA bundle for node TLS verification")
		rateLimit = fs.Float64("rate-limit", 0, "global HTTP requests per second (0 disables)")
		rateBurst = fs.Int("rate-burst", 500, "HTTP rate limiter burst")
		logLevel  = fs.String("log-level", "info", "debug, info, warn or error")
		logFormat = fs.String("log-format", "text", "text or json")
	)
	_ = fs.Parse(args)

	if err := validateCommon(*authUser, *authPass, *tlsCert, *tlsKey, *rateLimit, *rateBurst); err != nil {
		fatal(err)
	}
	shardMap, err := parseShards(*shards)
	if err != nil {
		fatal(err)
	}
	log := newLogger(*logLevel, *logFormat)

	c, err := coordinator.New(coordinator.Config{
		HTTPAddr:  *httpAddr,
		GRPCAddr:  *grpcAddr,
		Shards:    shardMap,
		AuthUser:  *authUser,
		AuthPass:  *authPass,
		TLSCert:   *tlsCert,
		TLSKey:    *tlsKey,
		TLSCA:     *tlsCA,
		RateLimit: *rateLimit,
		RateBurst: *rateBurst,
		Logger:    log,
	})
	if err != nil {
		fatal(fmt.Errorf("create coordinator: %w", err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := c.Start(ctx); err != nil {
		fatal(fmt.Errorf("start coordinator: %w", err))
	}
	log.Info("coordinator started", "http", *httpAddr, "grpc", *grpcAddr,
		"shards", len(shardMap))

	<-ctx.Done()
	log.Info("shutting down coordinator")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stop(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("coordinator stopped")
}

func validateCommon(authUser, authPass, tlsCert, tlsKey string, rateLimit float64, rateBurst int) error {
	if (authUser == "") != (authPass == "") {
		return fmt.Errorf("-auth-user and -auth-pass must be set together")
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("-tls-cert and -tls-key must be set together")
	}
	if rateLimit < 0 {
		return fmt.Errorf("-rate-limit must be >= 0")
	}
	if rateLimit > 0 && rateBurst <= 0 {
		return fmt.Errorf("-rate-burst must be > 0 when rate limiting is enabled")
	}
	return nil
}

func parseShards(spec string) (map[string][]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("-shards is required, e.g. shard-1=127.0.0.1:7001,127.0.0.1:7002")
	}
	out := make(map[string][]string)
	for _, group := range strings.Split(spec, ";") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		name, addrs, ok := strings.Cut(group, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid shard entry %q: want name=addr,addr", group)
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("duplicate shard %q", name)
		}
		list := splitList(addrs)
		if len(list) == 0 {
			return nil, fmt.Errorf("shard %q has no nodes", name)
		}
		out[name] = list
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-shards contains no shards")
	}
	return out, nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "unknown log level %q, using info\n", level)
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
	os.Exit(1)
}
