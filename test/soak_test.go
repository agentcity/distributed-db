package test

// Long-running soak harness for an EXTERNAL distributed-db cluster.
//
// This file never starts a meta service or a KV node. It is gated by
// SOAK_RUN=1 and skips otherwise, so the normal test suite and CI stay cheap.
//
// The harness is intended for multi-hour/multi-day runs against a deployed
// cluster:
//
//   - closed-loop mixed read/write transactions from one client host;
//   - per-worker key partitions, so the hot path needs no coordination;
//   - a global in-flight semaphore;
//   - p50/p95/p99 latency for all/read/write, throughput, errors and byte
//     counts, both for the whole run and per sampling bucket (latency drift);
//   - runtime.ReadMemStats heap fields, goroutine count and process RSS when
//     /proc exposes it;
//   - best-effort node Status probes for reachability/term/commit index;
//   - best-effort disk growth from the node RangeStats RPC, which reports the
//     storage-engine disk usage for each hosted range;
//   - optional WAL/compaction metrics scraped from Prometheus text endpoints.
//
// Failure gates cover p99 latency, error rate, client heap growth slope and
// WAL growth slope. A configured gate that cannot be evaluated is a failure,
// not a pass: a soak report must not silently claim evidence it does not have.
// Gate failures are reported with t.Errorf, so `go test` exits non-zero after
// the JSON report has been written.
//
// This harness does not prove a 72 h or 1 TB result by itself. It records the
// evidence from such a run; the run and its environment still have to be
// provisioned and reviewed. See docs/SOAK.md.
//
// Typical 72 h run:
//
//	SOAK_RUN=1 SOAK_META=10.0.0.10:9090 \
//	SOAK_NODES=10.0.0.11:7001,10.0.0.12:7001,10.0.0.13:7001 \
//	SOAK_DURATION=72h SOAK_PRELOAD_GB=1024 SOAK_REPORT_JSON=/abs/soak.json \
//	SOAK_P99_GATE_MS=250 SOAK_MEMORY_GROWTH_MIB_PER_HOUR=64 \
//	go test -run '^TestSoakMultiHost$' -v -timeout 96h ./test
//
// See scripts/run-soak.sh for a validated wrapper that sets the test timeout.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

const (
	soakEnvRun  = "SOAK_RUN"
	soakHarness = "soak-72h-v1"

	soakGiB int64 = 1 << 30

	soakDefaultDuration       = 72 * time.Hour
	soakDefaultClients        = 16
	soakDefaultValueSize      = 1024
	soakDefaultKeyspace       = 10_000_000
	soakDefaultReadRatio      = 0.8
	soakDefaultOpTimeout      = 10 * time.Second
	soakDefaultSampleInterval = time.Minute
	soakDefaultVerifyRate     = 0.001
	soakDefaultVerifySample   = 64
	soakDefaultPreloadWorkers = 4
	soakDefaultPreloadBatch   = 2048
	soakDefaultMaxErrorRate   = 0.001
	soakDefaultDiskInterval   = 5 * time.Minute

	soakMaxClients           = 1024
	soakMaxInflight          = 8192
	soakMaxValueSize         = 16 << 20
	soakMaxKeyspace          = 1 << 40
	soakMaxVerifySample      = 10_000
	soakMaxPreloadGB         = 1 << 20
	soakMaxPreloadWorkers    = 64
	soakMaxPreloadBatch      = 10_000
	soakMaxGateMilliseconds  = 86_400_000
	soakMaxThroughputGate    = 1e12
	soakMaxGrowthMiBPerHour  = 1e9
	soakMaxMetricsBody       = 16 << 20
	soakPreloadBatchBytes    = 4 << 20
	soakStatusTimeout        = 3 * time.Second
	soakMetricsTimeout       = 10 * time.Second
	soakDiskProbeTimeout     = 20 * time.Second
	soakPreloadCommitTimeout = 2 * time.Minute

	// Bounded log-linear latency histogram: 16 linear nanosecond buckets, then
	// 8 sub-buckets per power of two. Each worker owns its own histogram so the
	// hot path never contends on a shared structure; the sampler merges the
	// per-worker snapshots.
	soakHistBuckets = 512
	soakHistLinear  = 16
)

const (
	soakWarmupKey       = "soak/warmup/0"
	soakWorkloadKeyBase = "soak/workload/w"
	soakPreloadKeyBase  = "soak/preload/"
)

// soakConfig is the fully validated harness configuration.
type soakConfig struct {
	Meta          string
	Nodes         []string
	Duration      time.Duration
	Clients       int
	MaxInflight   int
	ReadRatio     float64
	ValueSize     int
	Keyspace      uint64
	PerWorkerKeys uint64

	OpTimeout      time.Duration
	SampleInterval time.Duration
	Seed           uint64

	VerifyRate   float64
	VerifySample int

	PreloadGB        float64
	PreloadBytes     int64
	PreloadKeys      int64
	PreloadWorkers   int
	PreloadBatchKeys int

	P99GateMS                 float64
	MaxErrorRate              float64
	MemoryGrowthMaxMiBPerHour float64
	WALGrowthMaxMiBPerHour    float64
	MinThroughput             float64

	MetricsURLs           []string
	WALMetric             string
	DiskMetric            string
	CompactionMetric      string
	CompactionBytesMetric string
	DiskProbe             bool
	DiskProbeInterval     time.Duration
	ReportPath            string
}

// soakConfigFromEnv reads SOAK_* from the process environment and fails the
// test with a precise message when a value is invalid.
func soakConfigFromEnv(tb testing.TB) soakConfig {
	tb.Helper()
	cfg, err := soakConfigFrom(os.Getenv)
	if err != nil {
		tb.Fatalf("SOAK config: %v", err)
	}
	return cfg
}

// soakConfigFrom builds and validates the configuration from getenv. It is
// pure with respect to the process environment so TestSoakSelf can exercise it.
func soakConfigFrom(getenv func(string) string) (soakConfig, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := soakConfig{
		Duration:          soakDefaultDuration,
		Clients:           soakDefaultClients,
		ReadRatio:         soakDefaultReadRatio,
		ValueSize:         soakDefaultValueSize,
		Keyspace:          soakDefaultKeyspace,
		OpTimeout:         soakDefaultOpTimeout,
		SampleInterval:    soakDefaultSampleInterval,
		VerifyRate:        soakDefaultVerifyRate,
		VerifySample:      soakDefaultVerifySample,
		PreloadWorkers:    soakDefaultPreloadWorkers,
		PreloadBatchKeys:  soakDefaultPreloadBatch,
		MaxErrorRate:      soakDefaultMaxErrorRate,
		DiskProbe:         true,
		DiskProbeInterval: soakDefaultDiskInterval,
	}

	cfg.Meta = get("SOAK_META")
	if cfg.Meta == "" {
		return cfg, fmt.Errorf("SOAK_META is required (meta/TSO gRPC host:port)")
	}
	rawNodes := get("SOAK_NODES")
	if rawNodes == "" {
		return cfg, fmt.Errorf("SOAK_NODES is required (comma-separated KV gRPC host:port list)")
	}
	nodes, err := soakParseNodes(rawNodes)
	if err != nil {
		return cfg, fmt.Errorf("SOAK_NODES: %w", err)
	}
	cfg.Nodes = nodes

	if raw := get("SOAK_CLIENTS"); raw != "" {
		cfg.Clients, err = soakParseIntRange(raw, 1, soakMaxClients)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_CLIENTS: %w", err)
		}
	}
	if raw := get("SOAK_MAX_INFLIGHT"); raw != "" {
		cfg.MaxInflight, err = soakParseIntRange(raw, 1, soakMaxInflight)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_MAX_INFLIGHT: %w", err)
		}
	} else {
		cfg.MaxInflight = cfg.Clients
	}

	if raw := get("SOAK_DURATION"); raw != "" {
		cfg.Duration, err = soakParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_DURATION: %w", err)
		}
		if cfg.Duration < time.Second {
			return cfg, fmt.Errorf("SOAK_DURATION must be at least 1s (got %s)", cfg.Duration)
		}
	}

	if raw := get("SOAK_VALUE_SIZE"); raw != "" {
		cfg.ValueSize, err = soakParseIntRange(raw, 1, soakMaxValueSize)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_VALUE_SIZE: %w", err)
		}
	}

	if raw := get("SOAK_KEYSPACE"); raw != "" {
		cfg.Keyspace, err = soakParseUintRange(raw, 1, soakMaxKeyspace)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_KEYSPACE: %w", err)
		}
	}
	if cfg.Keyspace < uint64(cfg.Clients) {
		return cfg, fmt.Errorf("SOAK_KEYSPACE=%d must be >= SOAK_CLIENTS=%d", cfg.Keyspace, cfg.Clients)
	}
	cfg.PerWorkerKeys = cfg.Keyspace / uint64(cfg.Clients)

	if raw := get("SOAK_READ_RATIO"); raw != "" {
		cfg.ReadRatio, err = soakParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_READ_RATIO: %w", err)
		}
	}

	if raw := get("SOAK_OP_TIMEOUT"); raw != "" {
		cfg.OpTimeout, err = soakParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_OP_TIMEOUT: %w", err)
		}
		if cfg.OpTimeout < time.Millisecond {
			return cfg, fmt.Errorf("SOAK_OP_TIMEOUT must be at least 1ms (got %s)", cfg.OpTimeout)
		}
	}

	if raw := get("SOAK_SAMPLE_INTERVAL"); raw != "" {
		cfg.SampleInterval, err = soakParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_SAMPLE_INTERVAL: %w", err)
		}
		if cfg.SampleInterval < 5*time.Second || cfg.SampleInterval > time.Hour {
			return cfg, fmt.Errorf("SOAK_SAMPLE_INTERVAL must be in [5s,1h] (got %s)", cfg.SampleInterval)
		}
	}

	if raw := get("SOAK_SEED"); raw != "" {
		cfg.Seed, err = soakParseUintRange(raw, 0, math.MaxUint64)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_SEED: %w", err)
		}
	}

	if raw := get("SOAK_VERIFY_RATE"); raw != "" {
		cfg.VerifyRate, err = soakParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_VERIFY_RATE: %w", err)
		}
	}
	if raw := get("SOAK_VERIFY_SAMPLE"); raw != "" {
		cfg.VerifySample, err = soakParseIntRange(raw, 0, soakMaxVerifySample)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_VERIFY_SAMPLE: %w", err)
		}
	}

	if raw := get("SOAK_PRELOAD_GB"); raw != "" {
		cfg.PreloadGB, err = soakParseFloatRange(raw, 0, soakMaxPreloadGB)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_PRELOAD_GB: %w", err)
		}
	}
	if cfg.PreloadGB > 0 {
		bytesFloat := cfg.PreloadGB * float64(soakGiB)
		if bytesFloat > float64(math.MaxInt64) {
			return cfg, fmt.Errorf("SOAK_PRELOAD_GB=%g is too large", cfg.PreloadGB)
		}
		cfg.PreloadBytes = int64(bytesFloat)
		if cfg.PreloadBytes < int64(cfg.ValueSize) {
			cfg.PreloadBytes = int64(cfg.ValueSize)
		}
		cfg.PreloadKeys = cfg.PreloadBytes / int64(cfg.ValueSize)
		if cfg.PreloadKeys < 1 {
			cfg.PreloadKeys = 1
		}
	}
	if raw := get("SOAK_PRELOAD_WORKERS"); raw != "" {
		cfg.PreloadWorkers, err = soakParseIntRange(raw, 1, soakMaxPreloadWorkers)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_PRELOAD_WORKERS: %w", err)
		}
	}
	if raw := get("SOAK_PRELOAD_BATCH_KEYS"); raw != "" {
		cfg.PreloadBatchKeys, err = soakParseIntRange(raw, 1, soakMaxPreloadBatch)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_PRELOAD_BATCH_KEYS: %w", err)
		}
	}
	if cfg.PreloadKeys > 0 && int64(cfg.PreloadWorkers) > cfg.PreloadKeys {
		cfg.PreloadWorkers = int(cfg.PreloadKeys)
	}

	if raw := get("SOAK_P99_GATE_MS"); raw != "" {
		cfg.P99GateMS, err = soakParseFloatRange(raw, 0, soakMaxGateMilliseconds)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_P99_GATE_MS: %w", err)
		}
	}
	if raw := get("SOAK_MAX_ERROR_RATE"); raw != "" {
		cfg.MaxErrorRate, err = soakParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_MAX_ERROR_RATE: %w", err)
		}
	}
	if raw := get("SOAK_MEMORY_GROWTH_MIB_PER_HOUR"); raw != "" {
		cfg.MemoryGrowthMaxMiBPerHour, err = soakParseFloatRange(raw, 0, soakMaxGrowthMiBPerHour)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_MEMORY_GROWTH_MIB_PER_HOUR: %w", err)
		}
	}
	if raw := get("SOAK_WAL_GROWTH_MIB_PER_HOUR"); raw != "" {
		cfg.WALGrowthMaxMiBPerHour, err = soakParseFloatRange(raw, 0, soakMaxGrowthMiBPerHour)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_WAL_GROWTH_MIB_PER_HOUR: %w", err)
		}
	}
	if raw := get("SOAK_MIN_THROUGHPUT"); raw != "" {
		cfg.MinThroughput, err = soakParseFloatRange(raw, 0, soakMaxThroughputGate)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_MIN_THROUGHPUT: %w", err)
		}
	}

	if raw := get("SOAK_METRICS_URLS"); raw != "" {
		cfg.MetricsURLs, err = soakParseURLList(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_METRICS_URLS: %w", err)
		}
	}
	if raw := get("SOAK_WAL_METRIC"); raw != "" {
		if !soakValidMetricName(raw) {
			return cfg, fmt.Errorf("SOAK_WAL_METRIC: invalid Prometheus metric name %q", raw)
		}
		cfg.WALMetric = raw
	}
	if raw := get("SOAK_DISK_METRIC"); raw != "" {
		if !soakValidMetricName(raw) {
			return cfg, fmt.Errorf("SOAK_DISK_METRIC: invalid Prometheus metric name %q", raw)
		}
		cfg.DiskMetric = raw
	}
	if raw := get("SOAK_COMPACTION_METRIC"); raw != "" {
		if !soakValidMetricName(raw) {
			return cfg, fmt.Errorf("SOAK_COMPACTION_METRIC: invalid Prometheus metric name %q", raw)
		}
		cfg.CompactionMetric = raw
	}
	if raw := get("SOAK_COMPACTION_BYTES_METRIC"); raw != "" {
		if !soakValidMetricName(raw) {
			return cfg, fmt.Errorf("SOAK_COMPACTION_BYTES_METRIC: invalid Prometheus metric name %q", raw)
		}
		cfg.CompactionBytesMetric = raw
	}

	if raw := get("SOAK_DISK_PROBE"); raw != "" {
		cfg.DiskProbe, err = soakParseBool(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_DISK_PROBE: %w", err)
		}
	}
	if raw := get("SOAK_DISK_PROBE_INTERVAL"); raw != "" {
		cfg.DiskProbeInterval, err = soakParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SOAK_DISK_PROBE_INTERVAL: %w", err)
		}
		if cfg.DiskProbeInterval < 10*time.Second || cfg.DiskProbeInterval > 24*time.Hour {
			return cfg, fmt.Errorf("SOAK_DISK_PROBE_INTERVAL must be in [10s,24h] (got %s)", cfg.DiskProbeInterval)
		}
	}

	cfg.ReportPath = get("SOAK_REPORT_JSON")
	return cfg, nil
}

func soakParseNodes(raw string) ([]string, error) {
	nodes := make([]string, 0, 5)
	seen := make(map[string]struct{}, 5)
	for _, part := range strings.Split(raw, ",") {
		node := strings.TrimSpace(part)
		if node == "" {
			return nil, fmt.Errorf("contains an empty address in %q", raw)
		}
		if _, ok := seen[node]; ok {
			return nil, fmt.Errorf("contains duplicate address %q", node)
		}
		seen[node] = struct{}{}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("is empty")
	}
	return nodes, nil
}

func soakParseURLList(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	urls := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		u := strings.TrimSpace(part)
		if u == "" {
			return nil, fmt.Errorf("contains an empty URL in %q", raw)
		}
		parsed, err := url.Parse(u)
		if err != nil {
			return nil, fmt.Errorf("invalid URL %q: %w", u, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("URL %q must use http or https", u)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("URL %q has no host", u)
		}
		if _, ok := seen[u]; ok {
			return nil, fmt.Errorf("contains duplicate URL %q", u)
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	return urls, nil
}

func soakValidMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		ok := c == '_' || c == ':' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func soakParseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("want 0/1, true/false, yes/no or on/off, got %q", raw)
	}
}

func soakParseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", raw)
		}
		return d, nil
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) && secs > 0 {
		d := time.Duration(secs * float64(time.Second))
		if d <= 0 {
			return 0, fmt.Errorf("duration %q is too small", raw)
		}
		return d, nil
	}
	// Go durations stop at hours. Accept a leading day component too, so
	// operators can write 3d or 1d12h instead of 72h or 36h.
	if idx := strings.IndexByte(raw, 'd'); idx > 0 {
		days, derr := strconv.ParseFloat(raw[:idx], 64)
		if derr == nil && !math.IsNaN(days) && !math.IsInf(days, 0) && days > 0 {
			tail := time.Duration(0)
			if rest := raw[idx+1:]; rest != "" {
				var terr error
				tail, terr = time.ParseDuration(rest)
				if terr != nil {
					return 0, fmt.Errorf("want a positive Go duration (for example 30s, 72h, 3d) or seconds, got %q", raw)
				}
			}
			ns := days*24*float64(time.Hour) + float64(tail)
			if ns <= 0 || ns > float64(math.MaxInt64) {
				return 0, fmt.Errorf("duration %q is out of range", raw)
			}
			return time.Duration(ns), nil
		}
	}
	return 0, fmt.Errorf("want a positive Go duration (for example 30s, 72h, 3d) or seconds, got %q", raw)
}

func soakParseIntRange(raw string, min, max int) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("want an integer in [%d,%d], got %q", min, max, raw)
	}
	return v, nil
}

func soakParseUintRange(raw string, min, max uint64) (uint64, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("want an integer in [%d,%d], got %q", min, max, raw)
	}
	return v, nil
}

func soakParseFloatRange(raw string, min, max float64) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
		return 0, fmt.Errorf("want a number in [%g,%g], got %q", min, max, raw)
	}
	return v, nil
}

// soakCheckTestTimeout refuses to start a run whose configured duration
// cannot fit in `go test -timeout`. A 72 h run under the default 10 m timeout
// would panic halfway through and lose the report, so fail early with the
// exact command the operator needs.
func soakCheckTestTimeout(t *testing.T, cfg soakConfig) {
	t.Helper()
	f := flag.Lookup("test.timeout")
	if f == nil {
		return
	}
	timeout, err := time.ParseDuration(f.Value.String())
	if err != nil || timeout <= 0 {
		return
	}
	grace := 10 * time.Minute
	if cfg.Duration < 10*time.Minute {
		grace = 2 * time.Minute
	}
	if timeout < cfg.Duration+grace {
		t.Fatalf("SOAK: go test -timeout %s is too short for SOAK_DURATION=%s; use -timeout %s (and add preload time, or 0 for none)",
			timeout, cfg.Duration, cfg.Duration+grace)
	}
	if cfg.PreloadBytes > 0 {
		t.Logf("SOAK: preload of %.3f GiB is additional to the %s measured duration; ensure -timeout covers it", cfg.PreloadGB, cfg.Duration)
	}
}

// TestSoakMultiHost is the external-cluster soak run. It skips unless
// SOAK_RUN=1 and refuses to run under -short.
func TestSoakMultiHost(t *testing.T) {
	if os.Getenv(soakEnvRun) != "1" {
		t.Skip("soak harness disabled: set SOAK_RUN=1, SOAK_META and SOAK_NODES (see docs/SOAK.md)")
	}
	if testing.Short() {
		t.Skip("soak harness skipped in -short")
	}

	cfg := soakConfigFromEnv(t)
	if cfg.Seed == 0 {
		cfg.Seed = uint64(time.Now().UnixNano())
	}
	soakCheckTestTimeout(t, cfg)
	host, _ := os.Hostname()

	client, err := v2client.New(cfg.Meta, cfg.Nodes)
	if err != nil {
		t.Fatalf("SOAK: create client: %v", err)
	}
	defer func() { _ = client.Close() }()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 30*time.Second)
	ranges, err := client.ListRanges(pingCtx)
	cancelPing()
	if err != nil {
		t.Fatalf("SOAK: cannot reach meta %s: %v", cfg.Meta, err)
	}
	if len(ranges) == 0 {
		t.Fatalf("SOAK: meta %s returned no ranges; is the cluster initialized?", cfg.Meta)
	}

	soakLogConfig(t, cfg, host, len(ranges))
	soakWarmup(t, client, cfg)

	var preload soakPreloadStats
	if cfg.PreloadBytes > 0 {
		preload = soakPreload(t, client, cfg)
	}

	workers := soakNewWorkers(cfg)
	soakSeedWorkers(t, client, cfg, workers)

	state := newSoakRunState(t, cfg, client, workers, len(ranges))
	defer state.close()

	workload := state.runWorkload()
	verify := soakVerifySamples(t, client, cfg, workers)
	memory := soakBuildMemorySummary(cfg, state.buckets)
	storage := soakBuildStorageSummary(cfg, state.buckets, state.probeStats())
	cluster := soakBuildClusterSummary(cfg, state.latestStatus, state.ranges, state.leaderChanges)
	drift := soakBuildDrift(state.buckets)
	gates := soakEvaluateGates(soakGateInput{
		cfg:      cfg,
		workload: workload,
		verify:   verify,
		memory:   memory,
		storage:  storage,
	})
	summary := soakBuildSummary(soakSummaryInput{
		cfg:      cfg,
		host:     host,
		preload:  preload,
		workload: workload,
		verify:   verify,
		buckets:  state.buckets,
		drift:    drift,
		memory:   memory,
		storage:  storage,
		cluster:  cluster,
		gates:    gates,
	})
	if cfg.ReportPath != "" {
		if err := soakWriteReport(cfg.ReportPath, summary); err != nil {
			t.Errorf("SOAK: write report %s: %v", cfg.ReportPath, err)
		} else {
			t.Logf("SOAK report: %s", cfg.ReportPath)
		}
	}
	soakLogSummary(t, summary)
	for _, failure := range gates.Failures {
		t.Errorf("SOAK gate failed: %s", failure)
	}
}

func soakLogConfig(t *testing.T, cfg soakConfig, host string, ranges int) {
	t.Helper()
	t.Logf("SOAK config: host=%s meta=%s nodes=%d ranges=%d clients=%d max_inflight=%d duration=%s sample_interval=%s value_size=%d keyspace=%d per_worker_keys=%d read_ratio=%.3f op_timeout=%s seed=%d verify_rate=%.6f verify_sample=%d preload_gb=%.3f preload_keys=%d preload_workers=%d p99_gate_ms=%.1f max_error_rate=%.6f memory_growth_gate_mib_per_hour=%.1f wal_growth_gate_mib_per_hour=%.1f min_throughput=%.1f metrics_targets=%d disk_probe=%t disk_probe_interval=%s report=%s",
		host, cfg.Meta, len(cfg.Nodes), ranges, cfg.Clients, cfg.MaxInflight, cfg.Duration,
		cfg.SampleInterval, cfg.ValueSize, cfg.Keyspace, cfg.PerWorkerKeys, cfg.ReadRatio,
		cfg.OpTimeout, cfg.Seed, cfg.VerifyRate, cfg.VerifySample, cfg.PreloadGB,
		cfg.PreloadKeys, cfg.PreloadWorkers, cfg.P99GateMS, cfg.MaxErrorRate,
		cfg.MemoryGrowthMaxMiBPerHour, cfg.WALGrowthMaxMiBPerHour, cfg.MinThroughput,
		len(cfg.MetricsURLs), cfg.DiskProbe, cfg.DiskProbeInterval, cfg.ReportPath)
}

// soakWarmup writes and reads one key outside the measured region. It proves
// that both the meta and at least one KV route are reachable before a long run
// starts, and surfaces an empty/misconfigured keyspace early.
func soakWarmup(t *testing.T, client *v2client.Client, cfg soakConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
	defer cancel()

	key := []byte(soakWarmupKey)
	value := soakValueFor(key, cfg.ValueSize, nil)
	txn, err := client.Begin(ctx)
	if err != nil {
		t.Fatalf("SOAK warmup: begin write: %v", err)
	}
	txn.Set(key, value)
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("SOAK warmup: commit write: %v", err)
	}

	txn, err = client.Begin(ctx)
	if err != nil {
		t.Fatalf("SOAK warmup: begin read: %v", err)
	}
	got, err := txn.Get(ctx, key)
	if err != nil {
		t.Fatalf("SOAK warmup: read back: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("SOAK warmup: read back %d bytes, want %d deterministic bytes", len(got), len(value))
	}
}

// soakPreloadStats summarizes the optional dataset preload.
type soakPreloadStats struct {
	Keys     int64
	Txns     int64
	Bytes    int64
	Duration time.Duration
}

func soakPreloadBatchKeys(cfg soakConfig) int {
	n := soakPreloadBatchBytes / cfg.ValueSize
	if n < 1 {
		n = 1
	}
	if n > cfg.PreloadBatchKeys {
		n = cfg.PreloadBatchKeys
	}
	return n
}

// soakPreload streams SOAK_PRELOAD_GB of value payload in bounded batches.
// Keys are derived from their ordinal and values are deterministic functions
// of the key, so a 1 TiB dataset is never materialized in memory and a later
// sample can verify a value without retaining it.
func soakPreload(t *testing.T, client *v2client.Client, cfg soakConfig) soakPreloadStats {
	t.Helper()
	if cfg.PreloadKeys < 1 {
		return soakPreloadStats{}
	}
	t.Logf("SOAK preload: starting target_gb=%.3f target_bytes=%d keys=%d value_size=%d workers=%d batch_keys=%d",
		cfg.PreloadGB, cfg.PreloadBytes, cfg.PreloadKeys, cfg.ValueSize, cfg.PreloadWorkers, soakPreloadBatchKeys(cfg))

	batchKeys := soakPreloadBatchKeys(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		next     atomic.Uint64
		doneKeys atomic.Int64
		doneTxns atomic.Int64
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < cfg.PreloadWorkers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			keyBuf := make([]byte, 0, 64)
			valueBuf := make([]byte, 0, cfg.ValueSize)
			for ctx.Err() == nil {
				start := next.Add(uint64(batchKeys)) - uint64(batchKeys)
				if start >= uint64(cfg.PreloadKeys) {
					return
				}
				n := uint64(batchKeys)
				if remaining := uint64(cfg.PreloadKeys) - start; n > remaining {
					n = remaining
				}
				beginCtx, cancelBegin := context.WithTimeout(ctx, soakPreloadCommitTimeout)
				txn, err := client.Begin(beginCtx)
				cancelBegin()
				if err != nil {
					fail(fmt.Errorf("worker %d begin at key %d: %w", worker, start, err))
					return
				}
				for j := uint64(0); j < n; j++ {
					key := soakAppendPreloadKey(keyBuf, start+j)
					valueBuf = soakValueFor(key, cfg.ValueSize, valueBuf)
					txn.Set(key, valueBuf)
				}
				commitCtx, cancelCommit := context.WithTimeout(context.Background(), soakPreloadCommitTimeout)
				err = txn.Commit(commitCtx)
				cancelCommit()
				if err != nil {
					fail(fmt.Errorf("worker %d commit keys [%d,%d): %w", worker, start, start+n, err))
					return
				}
				doneKeys.Add(int64(n))
				doneTxns.Add(1)
			}
		}(i)
	}

	done := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				keys := doneKeys.Load()
				elapsed := now.Sub(started)
				rate := 0.0
				if elapsed > 0 {
					rate = float64(keys*int64(cfg.ValueSize)) / elapsed.Seconds() / float64(1<<20)
				}
				t.Logf("SOAK preload: keys=%d/%d (%.1f%%) elapsed=%s rate=%.1f MiB/s",
					keys, cfg.PreloadKeys, 100*float64(keys)/float64(cfg.PreloadKeys),
					elapsed.Round(time.Second), rate)
			}
		}
	}()
	wg.Wait()
	close(done)
	progressWG.Wait()

	elapsed := time.Since(started)
	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err != nil {
		t.Fatalf("SOAK preload failed after %d/%d keys: %v", doneKeys.Load(), cfg.PreloadKeys, err)
	}
	keys := doneKeys.Load()
	if keys != cfg.PreloadKeys {
		t.Fatalf("SOAK preload wrote %d keys, want %d", keys, cfg.PreloadKeys)
	}
	bytesWritten := keys * int64(cfg.ValueSize)
	rate := 0.0
	if elapsed > 0 {
		rate = float64(bytesWritten) / elapsed.Seconds() / float64(1<<20)
	}
	t.Logf("SOAK preload: complete keys=%d bytes=%d (%.3f GiB) txns=%d elapsed=%s rate=%.1f MiB/s",
		keys, bytesWritten, float64(bytesWritten)/float64(soakGiB), doneTxns.Load(),
		elapsed.Round(time.Millisecond), rate)
	return soakPreloadStats{Keys: keys, Txns: doneTxns.Load(), Bytes: bytesWritten, Duration: elapsed}
}

// soakWorker is one client goroutine. Fields without atomic types are touched
// by a single goroutine; the sampler reads only the atomics while the workload
// is running and everything else after wg.Wait.
type soakWorker struct {
	id    int
	start uint64
	span  uint64

	writeSeq    uint64
	initialized uint64

	rng      *rand.Rand
	keyBuf   []byte
	valueBuf []byte
	firstErr string

	reads         atomic.Int64
	writes        atomic.Int64
	errors        atomic.Int64
	verifyChecked atomic.Int64
	verifyFailed  atomic.Int64
	bytesRead     atomic.Int64
	bytesWritten  atomic.Int64

	histAll   soakAtomicHist
	histRead  soakAtomicHist
	histWrite soakAtomicHist
}

func soakNewWorkers(cfg soakConfig) []*soakWorker {
	workers := make([]*soakWorker, cfg.Clients)
	for i := range workers {
		seed := cfg.Seed + uint64(i+1)*0x9e3779b97f4a7c15
		w := &soakWorker{
			id:     i,
			start:  uint64(i) * cfg.PerWorkerKeys,
			span:   cfg.PerWorkerKeys,
			rng:    rand.New(rand.NewSource(int64(seed))),
			keyBuf: make([]byte, 0, 64),
		}
		w.histAll.init()
		w.histRead.init()
		w.histWrite.init()
		workers[i] = w
	}
	return workers
}

// soakSeedWorkers prepares one key per worker outside the measured region, so
// a read-only mix has data and post-run verification always has a sample.
func soakSeedWorkers(t *testing.T, client *v2client.Client, cfg soakConfig, workers []*soakWorker) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, len(workers))
	for _, w := range workers {
		wg.Add(1)
		go func(w *soakWorker) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
			defer cancel()
			key := soakAppendWorkloadKey(w.keyBuf, w.id, w.start)
			w.valueBuf = soakValueFor(key, cfg.ValueSize, w.valueBuf)
			txn, err := client.Begin(ctx)
			if err == nil {
				txn.Set(key, w.valueBuf)
				err = txn.Commit(ctx)
			}
			if err != nil {
				errCh <- fmt.Errorf("worker %d seed key %d: %w", w.id, w.start, err)
				return
			}
			w.writeSeq = 1
			w.initialized = 1
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("SOAK warmup: %v", err)
	}
}

// soakWorkerOp performs exactly one read or write transaction and returns its
// contribution to the metrics.
func soakWorkerOp(client *v2client.Client, cfg soakConfig, w *soakWorker) (isRead bool, nbytes int, elapsed time.Duration, verifyChecked, verifyFailed bool, opErr error) {
	started := time.Now()
	isRead = w.rng.Float64() < cfg.ReadRatio
	if w.initialized == 0 {
		isRead = false
	}

	opCtx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
	defer cancel()

	if isRead {
		idx := w.rng.Uint64() % w.initialized
		key := soakAppendWorkloadKey(w.keyBuf, w.id, w.start+idx)
		txn, err := client.Begin(opCtx)
		if err != nil {
			opErr = err
		} else {
			got, err := txn.Get(opCtx, key)
			switch {
			case err != nil:
				opErr = err
				if errors.Is(err, v2client.ErrNotFound) {
					// Every read targets a key this worker committed. A miss is
					// data loss, not an ordinary request error.
					verifyChecked = true
					verifyFailed = true
				}
			case len(got) != cfg.ValueSize:
				verifyChecked = true
				verifyFailed = true
				opErr = fmt.Errorf("read key %q: got %d value bytes, want %d", key, len(got), cfg.ValueSize)
			default:
				nbytes = len(got)
				if cfg.VerifyRate > 0 && w.rng.Float64() < cfg.VerifyRate {
					verifyChecked = true
					w.valueBuf = soakValueFor(key, cfg.ValueSize, w.valueBuf)
					if !bytes.Equal(got, w.valueBuf) {
						verifyFailed = true
						opErr = fmt.Errorf("read key %q: deterministic value mismatch", key)
					}
				}
			}
		}
	} else {
		idx := w.writeSeq % w.span
		key := soakAppendWorkloadKey(w.keyBuf, w.id, w.start+idx)
		w.valueBuf = soakValueFor(key, cfg.ValueSize, w.valueBuf)
		txn, err := client.Begin(opCtx)
		if err != nil {
			opErr = err
		} else {
			txn.Set(key, w.valueBuf)
			opErr = txn.Commit(opCtx)
		}
		if opErr == nil {
			nbytes = len(w.valueBuf)
			w.writeSeq++
			if w.initialized < w.span {
				w.initialized++
			}
		}
	}
	elapsed = time.Since(started)
	return isRead, nbytes, elapsed, verifyChecked, verifyFailed, opErr
}

// soakWorkloadResult is the merged outcome of the measured phase.
type soakWorkloadResult struct {
	Duration      time.Duration
	Ops           int64
	Reads         int64
	Writes        int64
	Errors        int64
	VerifyChecked int64
	VerifyFailed  int64
	BytesRead     int64
	BytesWritten  int64
	FirstError    string
	All           soakHistogram
	Read          soakHistogram
	Write         soakHistogram
}

// soakWorkerCursor holds the previous cumulative snapshot for one worker. The
// sampler subtracts it to produce per-bucket deltas without resetting atomics.
type soakWorkerCursor struct {
	reads         int64
	writes        int64
	errors        int64
	verifyChecked int64
	verifyFailed  int64
	bytesRead     int64
	bytesWritten  int64

	all   soakHistCursor
	read  soakHistCursor
	write soakHistCursor
}

// soakRunState owns the workload, sampler and probe lifecycle for one run.
type soakRunState struct {
	t      *testing.T
	cfg    soakConfig
	client *v2client.Client
	probe  *soakProbe

	workers []*soakWorker
	cursors []soakWorkerCursor

	start        time.Time
	lastSnapshot time.Time
	lastDisk     time.Time

	buckets []soakBucketJSON

	latestStatus  []soakNodeStatusJSON
	prevLeaders   map[string]string
	leaderChanges int

	ranges           int
	diskRangesTotal  int
	diskRangesProbed int
	diskPartial      bool
	diskLastError    string
	metricsLastError string

	samplerCancel context.CancelFunc
	samplerDone   chan struct{}
}

func newSoakRunState(t *testing.T, cfg soakConfig, client *v2client.Client, workers []*soakWorker, ranges int) *soakRunState {
	return &soakRunState{
		t:           t,
		cfg:         cfg,
		client:      client,
		probe:       newSoakProbe(client, cfg.Nodes),
		workers:     workers,
		cursors:     make([]soakWorkerCursor, len(workers)),
		prevLeaders: make(map[string]string, len(cfg.Nodes)),
		ranges:      ranges,
		samplerDone: make(chan struct{}),
	}
}

func (s *soakRunState) close() {
	s.stopSampler()
	if s.probe != nil {
		s.probe.close()
	}
}

func (s *soakRunState) startSampler() {
	ctx, cancel := context.WithCancel(context.Background())
	s.samplerCancel = cancel
	s.samplerDone = make(chan struct{})
	go func() {
		defer close(s.samplerDone)
		ticker := time.NewTicker(s.cfg.SampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.snapshot(ctx, now)
			}
		}
	}()
}

func (s *soakRunState) stopSampler() {
	if s.samplerCancel == nil {
		return
	}
	s.samplerCancel()
	<-s.samplerDone
	s.samplerCancel = nil
}

func (s *soakRunState) runWorkload() soakWorkloadResult {
	sem := make(chan struct{}, s.cfg.MaxInflight)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start = time.Now()
	s.lastSnapshot = s.start
	s.startSampler()

	stopTimer := time.AfterFunc(s.cfg.Duration, cancel)
	defer stopTimer.Stop()

	var wg sync.WaitGroup
	for _, w := range s.workers {
		wg.Add(1)
		go func(w *soakWorker) {
			defer wg.Done()
			for {
				if runCtx.Err() != nil {
					return
				}
				select {
				case sem <- struct{}{}:
				case <-runCtx.Done():
					return
				}
				isRead, nbytes, elapsed, checked, failed, err := soakWorkerOp(s.client, s.cfg, w)
				<-sem

				w.histAll.record(elapsed)
				if isRead {
					w.reads.Add(1)
					w.histRead.record(elapsed)
					w.bytesRead.Add(int64(nbytes))
				} else {
					w.writes.Add(1)
					w.histWrite.record(elapsed)
					w.bytesWritten.Add(int64(nbytes))
				}
				if checked {
					w.verifyChecked.Add(1)
				}
				if failed {
					w.verifyFailed.Add(1)
				}
				if err != nil {
					w.errors.Add(1)
					if w.firstErr == "" {
						w.firstErr = soakTruncate(err.Error(), 512)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	cancel()
	s.stopSampler()

	finished := time.Now()
	s.snapshot(context.Background(), finished)
	return s.finalWorkload(finished.Sub(s.start))
}

func (s *soakRunState) finalWorkload(duration time.Duration) soakWorkloadResult {
	var result soakWorkloadResult
	result.Duration = duration
	for _, w := range s.workers {
		result.Reads += w.reads.Load()
		result.Writes += w.writes.Load()
		result.Errors += w.errors.Load()
		result.VerifyChecked += w.verifyChecked.Load()
		result.VerifyFailed += w.verifyFailed.Load()
		result.BytesRead += w.bytesRead.Load()
		result.BytesWritten += w.bytesWritten.Load()

		var h soakHistogram
		w.histAll.snapshot(&h)
		result.All.merge(&h)
		var rh soakHistogram
		w.histRead.snapshot(&rh)
		result.Read.merge(&rh)
		var wh soakHistogram
		w.histWrite.snapshot(&wh)
		result.Write.merge(&wh)

		if result.FirstError == "" {
			result.FirstError = w.firstErr
		}
	}
	result.Ops = result.Reads + result.Writes
	return result
}

func (s *soakRunState) snapshot(ctx context.Context, now time.Time) {
	elapsed := now.Sub(s.start)
	bucketStart := s.lastSnapshot
	bucketDur := now.Sub(bucketStart)
	if bucketDur <= 0 {
		bucketDur = time.Nanosecond
	}

	var (
		reads         int64
		writes        int64
		errorsTotal   int64
		verifyChecked int64
		verifyFailed  int64
		bytesRead     int64
		bytesWritten  int64
		histAll       soakHistogram
		histRead      soakHistogram
		histWrite     soakHistogram
	)
	for i, w := range s.workers {
		c := &s.cursors[i]

		cur := w.reads.Load()
		reads += soakCounterDelta(cur, c.reads)
		c.reads = cur
		cur = w.writes.Load()
		writes += soakCounterDelta(cur, c.writes)
		c.writes = cur
		cur = w.errors.Load()
		errorsTotal += soakCounterDelta(cur, c.errors)
		c.errors = cur
		cur = w.verifyChecked.Load()
		verifyChecked += soakCounterDelta(cur, c.verifyChecked)
		c.verifyChecked = cur
		cur = w.verifyFailed.Load()
		verifyFailed += soakCounterDelta(cur, c.verifyFailed)
		c.verifyFailed = cur
		cur = w.bytesRead.Load()
		bytesRead += soakCounterDelta(cur, c.bytesRead)
		c.bytesRead = cur
		cur = w.bytesWritten.Load()
		bytesWritten += soakCounterDelta(cur, c.bytesWritten)
		c.bytesWritten = cur

		d := c.all.delta(&w.histAll)
		histAll.merge(&d)
		d = c.read.delta(&w.histRead)
		histRead.merge(&d)
		d = c.write.delta(&w.histWrite)
		histWrite.merge(&d)
	}

	ops := reads + writes
	durationSeconds := bucketDur.Seconds()
	throughput := 0.0
	if durationSeconds > 0 {
		throughput = float64(ops) / durationSeconds
	}
	errorRate := 0.0
	if ops > 0 {
		errorRate = float64(errorsTotal) / float64(ops)
	}
	mibPerSec := 0.0
	if durationSeconds > 0 {
		mibPerSec = float64(bytesRead+bytesWritten) / durationSeconds / float64(1<<20)
	}

	cluster := soakClusterSampleJSON{NodesTotal: len(s.cfg.Nodes)}
	if s.probe != nil {
		statuses := s.probe.status(ctx)
		s.latestStatus = statuses
		reachable := 0
		var termMax, commitMax uint64
		for _, st := range statuses {
			if st.Reachable {
				reachable++
			}
			if st.Term > termMax {
				termMax = st.Term
			}
			if st.CommitIndex > commitMax {
				commitMax = st.CommitIndex
			}
			if st.Reachable && st.Leader != "" {
				if previous, ok := s.prevLeaders[st.Addr]; ok && previous != "" && previous != st.Leader {
					s.leaderChanges++
				}
				s.prevLeaders[st.Addr] = st.Leader
			}
		}
		cluster.NodesReachable = reachable
		cluster.TermMax = termMax
		cluster.CommitIndexMax = commitMax
		cluster.LeaderChanges = s.leaderChanges
	}

	storage := soakStorageSampleJSON{}
	if s.probe != nil {
		sample, err := s.probe.metrics(ctx, s.cfg)
		if err != nil {
			s.metricsLastError = soakTruncate(err.Error(), 512)
		} else {
			s.metricsLastError = ""
		}
		storage.WALBytes = sample.WALBytes
		storage.CompactionsTotal = sample.CompactionCount
		storage.CompactionBytesTotal = sample.CompactionBytes

		if s.cfg.DiskProbe && (s.lastDisk.IsZero() || now.Sub(s.lastDisk) >= s.cfg.DiskProbeInterval) {
			disk := s.probe.disk(ctx, s.cfg)
			s.lastDisk = now
			s.diskRangesTotal = disk.Ranges
			if disk.Ranges > 0 {
				// Splits/merges can change the range count during a long run.
				s.ranges = disk.Ranges
			}
			s.diskRangesProbed = disk.Probed
			s.diskPartial = disk.Partial
			if disk.Error != "" {
				s.diskLastError = soakTruncate(disk.Error, 512)
			} else if disk.Probed > 0 {
				s.diskLastError = ""
			}
			if disk.Probed > 0 {
				bytes := disk.Bytes
				keys := disk.Keys
				storage.DiskBytes = &bytes
				storage.DiskKeys = &keys
			}
		}
		if storage.DiskBytes == nil && sample.DiskBytes != nil {
			storage.DiskBytes = sample.DiskBytes
		}
	}

	memory := soakMemoryNow()
	latencyAll := soakLatencyFromHist(&histAll)
	latencyRead := soakLatencyFromHist(&histRead)
	latencyWrite := soakLatencyFromHist(&histWrite)

	bucket := soakBucketJSON{
		Index:           len(s.buckets),
		Start:           bucketStart.UTC().Format(time.RFC3339),
		End:             now.UTC().Format(time.RFC3339),
		ElapsedSeconds:  elapsed.Seconds(),
		DurationSeconds: durationSeconds,
		Ops:             ops,
		Reads:           reads,
		Writes:          writes,
		Errors:          errorsTotal,
		ErrorRate:       errorRate,
		Throughput:      throughput,
		ReadThroughput:  soakRate(reads, durationSeconds),
		WriteThroughput: soakRate(writes, durationSeconds),
		BytesRead:       bytesRead,
		BytesWritten:    bytesWritten,
		MiBPerSec:       mibPerSec,
		VerifyChecked:   verifyChecked,
		VerifyFailed:    verifyFailed,
		Latency:         latencyAll,
		ReadLatency:     latencyRead,
		WriteLatency:    latencyWrite,
		Memory:          memory.json(),
		Cluster:         cluster,
		Storage:         storage,
	}
	s.buckets = append(s.buckets, bucket)
	s.lastSnapshot = now
	if s.t != nil {
		s.t.Logf("SOAK bucket %d: elapsed=%.0fs dur=%.1fs ops=%d thr=%.1f/s reads=%d writes=%d errors=%d p99=%.2f/%.2f/%.2fms heap=%.1fMiB goroutines=%d rss=%s wal=%s disk=%s nodes=%d/%d",
			bucket.Index, elapsed.Seconds(), durationSeconds, ops, throughput, reads, writes,
			errorsTotal, latencyAll.P99MS, latencyRead.P99MS, latencyWrite.P99MS,
			float64(memory.heapInuse)/(1<<20), memory.goroutines,
			soakFormatOptionalBytes(memory.rss), soakFormatOptionalBytes(storage.WALBytes),
			soakFormatOptionalBytes(storage.DiskBytes), cluster.NodesReachable, len(s.cfg.Nodes))
	}
}

func soakRate(n int64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(n) / seconds
}

func soakCounterDelta(current, previous int64) int64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func (s *soakRunState) probeStats() soakStorageProbeStats {
	return soakStorageProbeStats{
		RangesTotal:      s.diskRangesTotal,
		RangesProbed:     s.diskRangesProbed,
		DiskPartial:      s.diskPartial,
		DiskLastError:    s.diskLastError,
		MetricsLastError: s.metricsLastError,
	}
}

// soakVerifyResult is the post-run verification outcome.
type soakVerifyResult struct {
	Checked    int64
	Failed     int64
	FirstError string
}

// soakVerifySamples re-reads a deterministic spread of workload and preloaded
// keys and compares the full payload. This is a sampled data-integrity check,
// not a durability or crash-consistency test.
func soakVerifySamples(t *testing.T, client *v2client.Client, cfg soakConfig, workers []*soakWorker) soakVerifyResult {
	t.Helper()
	var result soakVerifyResult
	if cfg.VerifySample <= 0 {
		return result
	}

	rng := rand.New(rand.NewSource(0x5EED))
	keyBuf := make([]byte, 0, 64)
	valueBuf := make([]byte, 0, cfg.ValueSize)
	check := func(key []byte) {
		result.Checked++
		ctx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
		defer cancel()
		txn, err := client.Begin(ctx)
		if err != nil {
			result.Failed++
			if result.FirstError == "" {
				result.FirstError = fmt.Sprintf("begin: %v", err)
			}
			return
		}
		got, err := txn.Get(ctx, key)
		if err != nil {
			result.Failed++
			if result.FirstError == "" {
				result.FirstError = fmt.Sprintf("get %q: %v", key, err)
			}
			return
		}
		want := soakValueFor(key, cfg.ValueSize, valueBuf)
		if !bytes.Equal(got, want) {
			result.Failed++
			if result.FirstError == "" {
				result.FirstError = fmt.Sprintf("key %q: %d bytes, payload mismatch", key, len(got))
			}
		}
	}

	if len(workers) > 0 {
		for i := 0; i < cfg.VerifySample; i++ {
			w := workers[rng.Intn(len(workers))]
			if w.initialized == 0 {
				continue
			}
			idx := uint64(0)
			if w.initialized > 1 {
				idx = uint64(rng.Int63n(int64(w.initialized)))
			}
			check(soakAppendWorkloadKey(keyBuf, w.id, w.start+idx))
		}
	}

	if cfg.PreloadKeys > 0 {
		n := int64(cfg.VerifySample)
		if n > cfg.PreloadKeys {
			n = cfg.PreloadKeys
		}
		step := uint64(cfg.PreloadKeys) / uint64(n)
		if step == 0 {
			step = 1
		}
		for i := int64(0); i < n; i++ {
			check(soakAppendPreloadKey(keyBuf, uint64(i)*step))
		}
	}
	return result
}

// ---- latency histogram ----------------------------------------------------

// soakHistogram is a bounded log-linear latency histogram. Recording is O(1)
// and the structure is 512 uint64 counters. Each worker records privately; the
// sampler merges snapshots. Quantiles are conservative bucket upper bounds
// clamped to the observed maximum.
type soakHistogram struct {
	counts [soakHistBuckets]uint64
	total  uint64
	min    time.Duration
	max    time.Duration
	sum    time.Duration
}

func soakHistIndex(v uint64) int {
	if v < soakHistLinear {
		return int(v)
	}
	e := uint(bits.Len64(v) - 1)
	frac := (v >> (e - 3)) & 0x7
	return soakHistLinear + int(e-4)*8 + int(frac)
}

func soakHistUpper(idx int) time.Duration {
	if idx < soakHistLinear {
		return time.Duration(idx + 1)
	}
	if idx >= soakHistBuckets {
		idx = soakHistBuckets - 1
	}
	p := idx - soakHistLinear
	e := uint(p/8) + 4
	frac := p % 8
	upper := (uint64(1) << e) + (uint64(frac)+1)<<(e-3)
	if upper > math.MaxInt64 {
		upper = math.MaxInt64
	}
	return time.Duration(upper)
}

func soakHistLower(idx int) time.Duration {
	if idx < soakHistLinear {
		return time.Duration(idx)
	}
	if idx >= soakHistBuckets {
		idx = soakHistBuckets - 1
	}
	p := idx - soakHistLinear
	e := uint(p/8) + 4
	frac := p % 8
	lower := (uint64(1) << e) + uint64(frac)<<(e-3)
	if lower > math.MaxInt64 {
		lower = math.MaxInt64
	}
	return time.Duration(lower)
}

func (h *soakHistogram) record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	idx := soakHistIndex(uint64(d))
	if idx >= soakHistBuckets {
		idx = soakHistBuckets - 1
	}
	h.counts[idx]++
	if h.total == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.total++
	h.sum += d
}

func (h *soakHistogram) merge(other *soakHistogram) {
	if other == nil || other.total == 0 {
		return
	}
	for i, n := range other.counts {
		h.counts[i] += n
	}
	if h.total == 0 || other.min < h.min {
		h.min = other.min
	}
	if other.max > h.max {
		h.max = other.max
	}
	h.total += other.total
	h.sum += other.sum
}

func (h *soakHistogram) quantile(q float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	if q <= 0 {
		return h.min
	}
	if q >= 1 {
		return h.max
	}
	rank := uint64(math.Ceil(q * float64(h.total)))
	if rank < 1 {
		rank = 1
	}
	var cumulative uint64
	for i, n := range h.counts {
		cumulative += n
		if cumulative >= rank {
			d := soakHistUpper(i)
			if d > h.max {
				d = h.max
			}
			return d
		}
	}
	return h.max
}

func (h *soakHistogram) mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return h.sum / time.Duration(h.total)
}

// soakAtomicHist is the per-worker hot-path histogram. One atomic add per
// bucket, total and sum, plus two CAS loops for min/max.
type soakAtomicHist struct {
	counts [soakHistBuckets]atomic.Uint64
	total  atomic.Uint64
	sum    atomic.Uint64
	min    atomic.Uint64
	max    atomic.Uint64
}

func (h *soakAtomicHist) init() {
	h.min.Store(math.MaxUint64)
}

func (h *soakAtomicHist) record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	raw := uint64(d)
	idx := soakHistIndex(raw)
	if idx >= soakHistBuckets {
		idx = soakHistBuckets - 1
	}
	h.counts[idx].Add(1)
	h.total.Add(1)
	h.sum.Add(raw)

	for {
		old := h.max.Load()
		if raw <= old || h.max.CompareAndSwap(old, raw) {
			break
		}
	}
	for {
		old := h.min.Load()
		if raw >= old || h.min.CompareAndSwap(old, raw) {
			break
		}
	}
}

func (h *soakAtomicHist) snapshot(dst *soakHistogram) {
	dst.total = h.total.Load()
	dst.sum = time.Duration(h.sum.Load())
	for i := 0; i < soakHistBuckets; i++ {
		dst.counts[i] = h.counts[i].Load()
	}
	if dst.total == 0 {
		dst.min = 0
		dst.max = 0
		return
	}
	dst.min = time.Duration(h.min.Load())
	dst.max = time.Duration(h.max.Load())
}

// soakHistCursor stores the previous cumulative counter snapshot so the
// sampler can produce a per-bucket delta without stopping the workers.
type soakHistCursor struct {
	counts [soakHistBuckets]uint64
	total  uint64
	sum    uint64
}

func (c *soakHistCursor) delta(h *soakAtomicHist) soakHistogram {
	var d soakHistogram
	var countTotal uint64
	var curSum uint64
	for i := 0; i < soakHistBuckets; i++ {
		cur := h.counts[i].Load()
		if cur > c.counts[i] {
			d.counts[i] = cur - c.counts[i]
		} else if cur < c.counts[i] {
			// A counter never decreases; be defensive if a future change
			// reuses a worker, and count the new value.
			d.counts[i] = cur
		}
		countTotal += d.counts[i]
		c.counts[i] = cur
	}
	curSum = h.sum.Load()
	if curSum >= c.sum {
		d.sum = time.Duration(curSum - c.sum)
	} else {
		d.sum = time.Duration(curSum)
	}
	c.sum = curSum
	c.total = h.total.Load()

	d.total = countTotal
	if d.total == 0 {
		return d
	}
	first, last := -1, -1
	for i := 0; i < soakHistBuckets; i++ {
		if d.counts[i] == 0 {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first >= 0 {
		d.min = soakHistLower(first)
		d.max = soakHistUpper(last)
	}
	return d
}

func soakLatencyFromHist(h *soakHistogram) soakLatencyJSON {
	ms := func(d time.Duration) float64 {
		return float64(d) / float64(time.Millisecond)
	}
	return soakLatencyJSON{
		P50MS:  ms(h.quantile(0.50)),
		P95MS:  ms(h.quantile(0.95)),
		P99MS:  ms(h.quantile(0.99)),
		MeanMS: ms(h.mean()),
		MinMS:  ms(h.min),
		MaxMS:  ms(h.max),
	}
}

// Key and value helpers. The returned key aliases dst; Txn.Set copies it.

func soakAppendWorkloadKey(dst []byte, worker int, id uint64) []byte {
	dst = append(dst[:0], soakWorkloadKeyBase...)
	dst = strconv.AppendInt(dst, int64(worker), 10)
	dst = append(dst, '/')
	return strconv.AppendUint(dst, id, 10)
}

func soakAppendPreloadKey(dst []byte, id uint64) []byte {
	dst = append(dst[:0], soakPreloadKeyBase...)
	return strconv.AppendUint(dst, id, 10)
}

// soakValueFor fills buf with a deterministic payload derived from key and n.
// The same key and size always produce the same bytes, so a verification read
// does not need to retain the original value.
func soakValueFor(key []byte, n int, buf []byte) []byte {
	if n < 0 {
		n = 0
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	h := uint64(14695981039346656037) // FNV-1a offset basis
	for _, b := range key {
		h ^= uint64(b)
		h *= 1099511628211
	}
	h ^= uint64(n) * 0x9e3779b97f4a7c15

	var word [8]byte
	for i := 0; i < n; i += 8 {
		h += 0x9e3779b97f4a7c15
		z := h
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		binary.LittleEndian.PutUint64(word[:], z)
		copy(buf[i:], word[:])
	}
	return buf
}

// ---- runtime memory and process RSS --------------------------------------

type soakMemoryJSON struct {
	HeapAllocBytes  int64   `json:"heap_alloc_bytes"`
	HeapInuseBytes  int64   `json:"heap_inuse_bytes"`
	HeapSysBytes    int64   `json:"heap_sys_bytes"`
	SysBytes        int64   `json:"sys_bytes"`
	StackInuseBytes int64   `json:"stack_inuse_bytes"`
	HeapObjects     uint64  `json:"heap_objects"`
	NumGC           uint32  `json:"num_gc"`
	GCPauseTotalMS  float64 `json:"gc_pause_total_ms"`
	NumGoroutine    int     `json:"num_goroutine"`
	RSSBytes        *int64  `json:"rss_bytes,omitempty"`
}

type soakMemorySample struct {
	heapAlloc   uint64
	heapInuse   uint64
	heapSys     uint64
	sys         uint64
	stackInuse  uint64
	heapObjects uint64
	numGC       uint32
	pauseTotal  uint64
	goroutines  int
	rss         *int64
}

func soakMemoryNow() soakMemorySample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss, ok := soakReadRSS()
	var rssPtr *int64
	if ok {
		rssPtr = &rss
	}
	return soakMemorySample{
		heapAlloc:   ms.HeapAlloc,
		heapInuse:   ms.HeapInuse,
		heapSys:     ms.HeapSys,
		sys:         ms.Sys,
		stackInuse:  ms.StackInuse,
		heapObjects: ms.HeapObjects,
		numGC:       ms.NumGC,
		pauseTotal:  ms.PauseTotalNs,
		goroutines:  runtime.NumGoroutine(),
		rss:         rssPtr,
	}
}

func (m soakMemorySample) json() soakMemoryJSON {
	return soakMemoryJSON{
		HeapAllocBytes:  int64(m.heapAlloc),
		HeapInuseBytes:  int64(m.heapInuse),
		HeapSysBytes:    int64(m.heapSys),
		SysBytes:        int64(m.sys),
		StackInuseBytes: int64(m.stackInuse),
		HeapObjects:     m.heapObjects,
		NumGC:           m.numGC,
		GCPauseTotalMS:  float64(m.pauseTotal) / float64(time.Millisecond),
		NumGoroutine:    m.goroutines,
		RSSBytes:        m.rss,
	}
}

// soakReadRSS reads the current resident set size from /proc when the host
// exposes it. Linux is the supported soak runner; other platforms simply omit
// RSS rather than guessing from ru_maxrss (which is a high-water mark, not a
// current value).
func soakReadRSS() (int64, bool) {
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseInt(fields[1], 10, 64); err == nil && pages > 0 {
				return pages * int64(os.Getpagesize()), true
			}
		}
	}
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "VmRSS:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb >= 0 {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

// ---- external cluster probes ---------------------------------------------

type soakNodeStatusJSON struct {
	Addr        string `json:"addr"`
	ID          string `json:"id,omitempty"`
	Role        string `json:"role,omitempty"`
	Leader      string `json:"leader,omitempty"`
	Term        uint64 `json:"term,omitempty"`
	CommitIndex uint64 `json:"commit_index,omitempty"`
	LastApplied uint64 `json:"last_applied,omitempty"`
	RangeID     uint64 `json:"range_id,omitempty"`
	Reachable   bool   `json:"reachable"`
	Error       string `json:"error,omitempty"`
}

type soakMetricSample struct {
	WALBytes        *int64
	DiskBytes       *int64
	CompactionCount *int64
	CompactionBytes *int64
}

func (m *soakMetricSample) merge(other soakMetricSample) {
	if other.WALBytes != nil {
		m.WALBytes = soakAddInt64Ptr(m.WALBytes, *other.WALBytes)
	}
	if other.DiskBytes != nil {
		m.DiskBytes = soakAddInt64Ptr(m.DiskBytes, *other.DiskBytes)
	}
	if other.CompactionCount != nil {
		m.CompactionCount = soakAddInt64Ptr(m.CompactionCount, *other.CompactionCount)
	}
	if other.CompactionBytes != nil {
		m.CompactionBytes = soakAddInt64Ptr(m.CompactionBytes, *other.CompactionBytes)
	}
}

func soakAddInt64Ptr(dst *int64, v int64) *int64 {
	if dst == nil {
		value := v
		return &value
	}
	sum := *dst + v
	return &sum
}

type soakDiskSample struct {
	Ranges  int
	Probed  int
	Bytes   int64
	Keys    int64
	Partial bool
	Error   string
}

type soakProbe struct {
	client *v2client.Client
	nodes  []string

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
	kv    map[string]pbv2.KVServiceClient

	httpClient *http.Client
}

func newSoakProbe(client *v2client.Client, nodes []string) *soakProbe {
	return &soakProbe{
		client: client,
		nodes:  nodes,
		conns:  make(map[string]*grpc.ClientConn, len(nodes)),
		kv:     make(map[string]pbv2.KVServiceClient, len(nodes)),
		httpClient: &http.Client{
			Timeout: soakMetricsTimeout,
		},
	}
}

func (p *soakProbe) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cc := range p.conns {
		_ = cc.Close()
	}
	p.conns = nil
	p.kv = nil
}

func (p *soakProbe) kvFor(addr string) (pbv2.KVServiceClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kv, ok := p.kv[addr]; ok {
		return kv, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	kv := pbv2.NewKVServiceClient(cc)
	p.conns[addr] = cc
	p.kv[addr] = kv
	return kv, nil
}

func (p *soakProbe) status(ctx context.Context) []soakNodeStatusJSON {
	out := make([]soakNodeStatusJSON, len(p.nodes))
	var wg sync.WaitGroup
	for i, addr := range p.nodes {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			out[i] = p.statusOne(ctx, addr)
		}(i, addr)
	}
	wg.Wait()
	return out
}

func (p *soakProbe) statusOne(ctx context.Context, addr string) soakNodeStatusJSON {
	callCtx, cancel := context.WithTimeout(ctx, soakStatusTimeout)
	defer cancel()
	status := soakNodeStatusJSON{Addr: addr}
	kv, err := p.kvFor(addr)
	if err != nil {
		status.Error = soakTruncate(err.Error(), 256)
		return status
	}
	resp, err := kv.Status(callCtx, &pbv2.StatusRequest{})
	if err != nil {
		status.Error = soakTruncate(err.Error(), 256)
		return status
	}
	status.Reachable = true
	status.ID = resp.GetId()
	status.Role = resp.GetRole()
	status.Leader = resp.GetLeader()
	status.Term = resp.GetTerm()
	status.CommitIndex = resp.GetCommitIndex()
	status.LastApplied = resp.GetLastApplied()
	status.RangeID = resp.GetRangeId()
	return status
}

func (p *soakProbe) metrics(ctx context.Context, cfg soakConfig) (soakMetricSample, error) {
	var agg soakMetricSample
	if len(cfg.MetricsURLs) == 0 {
		return agg, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, soakMetricsTimeout)
	defer cancel()
	var errs []string
	for _, target := range cfg.MetricsURLs {
		body, err := p.fetchMetrics(callCtx, target)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", soakSanitizeURL(target), err))
			continue
		}
		sample := soakParseMetricsText(body, cfg)
		agg.merge(sample)
	}
	if len(errs) > 0 {
		return agg, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return agg, nil
}

func (p *soakProbe) fetchMetrics(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, soakSanitizeError(err, target)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, soakSanitizeError(err, target)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, soakMaxMetricsBody+1))
	if err != nil {
		return nil, soakSanitizeError(err, target)
	}
	if len(body) > soakMaxMetricsBody {
		return nil, fmt.Errorf("metrics body exceeds %d bytes", soakMaxMetricsBody)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// soakSanitizeURL removes embedded credentials before a URL reaches a log or
// report. The endpoint identity is still useful; the password/token is not.
func soakSanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	return u.String()
}

func soakSanitizeError(err error, target string) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), target, soakSanitizeURL(target))
	return errors.New(msg)
}

func (p *soakProbe) disk(ctx context.Context, cfg soakConfig) soakDiskSample {
	var out soakDiskSample
	callCtx, cancel := context.WithTimeout(ctx, soakDiskProbeTimeout)
	defer cancel()

	ranges, err := p.client.ListRanges(callCtx)
	if err != nil {
		out.Error = fmt.Sprintf("ListRanges: %v", err)
		return out
	}
	out.Ranges = len(ranges)
	var firstErr string
	for _, r := range ranges {
		if callCtx.Err() != nil {
			out.Partial = true
			break
		}
		candidates := soakRangeCandidates(r, p.nodes)
		if len(candidates) == 0 {
			if firstErr == "" {
				firstErr = fmt.Sprintf("range %d has no replicas", r.ID)
			}
			out.Partial = true
			continue
		}
		ok := false
		for _, addr := range candidates {
			if callCtx.Err() != nil {
				break
			}
			resp, err := p.rangeStats(callCtx, addr, r.ID)
			if err != nil {
				if firstErr == "" {
					firstErr = fmt.Sprintf("range %d on %s: %v", r.ID, addr, err)
				}
				continue
			}
			if !resp.GetSuccess() {
				if msg := resp.GetError(); msg != "" && firstErr == "" {
					firstErr = fmt.Sprintf("range %d on %s: %s", r.ID, addr, msg)
				}
				continue
			}
			if !bytes.Equal(resp.GetStart(), r.Start) || !bytes.Equal(resp.GetEnd(), r.End) {
				if firstErr == "" {
					firstErr = fmt.Sprintf("range %d on %s: stale bounds", r.ID, addr)
				}
				continue
			}
			out.Bytes += int64(resp.GetSizeBytes())
			out.Keys += int64(resp.GetKeyCount())
			out.Probed++
			ok = true
			break
		}
		if !ok {
			out.Partial = true
		}
	}
	if firstErr != "" {
		// Keep a partial failure visible. The summary records Probed/Ranges
		// and Partial, so one bad range does not hide the successful ones.
		out.Error = firstErr
	}
	return out
}

func (p *soakProbe) rangeStats(ctx context.Context, addr string, rangeID uint64) (*pbv2.RangeStatsResponse, error) {
	kv, err := p.kvFor(addr)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, soakStatusTimeout)
	defer cancel()
	// Epoch 0 skips the range-epoch check. The scheduler's own probe uses the
	// meta version, but a soak observer must not fail just because a split
	// advanced the epoch between ListRanges and this call.
	return kv.RangeStats(callCtx, &pbv2.RangeStatsRequest{RangeId: rangeID})
}

func soakRangeCandidates(r v2client.RangeInfo, fallback []string) []string {
	out := make([]string, 0, len(r.Replicas)+1)
	seen := make(map[string]struct{}, len(r.Replicas)+1)
	add := func(addr string) {
		if addr == "" {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	add(r.Leader)
	for _, addr := range r.Replicas {
		add(addr)
	}
	if len(out) == 0 {
		for _, addr := range fallback {
			add(addr)
		}
	}
	return out
}

// ---- Prometheus text metrics ---------------------------------------------

var (
	soakWALMetricAliases = []string{
		"pebble_wal_physical_size_bytes",
		"pebble_wal_size_bytes",
		"rocksdb_wal_size_bytes",
		"kvserver_wal_size_bytes",
		"db_wal_size_bytes",
		"wal_size_bytes",
	}
	soakDiskMetricAliases = []string{
		"pebble_disk_space_usage_bytes",
		"pebble_disk_usage_bytes",
		"kvserver_disk_used_bytes",
		"db_disk_size_bytes",
		"disk_usage_bytes",
	}
	soakCompactionCountAliases = []string{
		"pebble_compactions_total",
		"pebble_compaction_count",
		"kvserver_compactions_total",
		"compactions_total",
	}
	soakCompactionBytesAliases = []string{
		"pebble_compaction_bytes_written_total",
		"pebble_compact_bytes_written_total",
		"compaction_bytes_written_total",
	}
)

const (
	soakMetricWAL = iota
	soakMetricDisk
	soakMetricCompaction
	soakMetricCompactionBytes
)

func soakParseMetricsText(body []byte, cfg soakConfig) soakMetricSample {
	kinds := make(map[string]int, 16)
	register := func(names []string, explicit string, kind int) {
		if explicit != "" {
			kinds[explicit] = kind
			return
		}
		for _, name := range names {
			kinds[name] = kind
		}
	}
	register(soakWALMetricAliases, cfg.WALMetric, soakMetricWAL)
	register(soakDiskMetricAliases, cfg.DiskMetric, soakMetricDisk)
	register(soakCompactionCountAliases, cfg.CompactionMetric, soakMetricCompaction)
	register(soakCompactionBytesAliases, cfg.CompactionBytesMetric, soakMetricCompactionBytes)

	var sums [4]float64
	var found [4]bool
	for _, line := range strings.Split(string(body), "\n") {
		name, raw, ok := soakPromLine(line)
		if !ok {
			continue
		}
		kind, ok := kinds[name]
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		sums[kind] += value
		found[kind] = true
	}

	var out soakMetricSample
	if found[soakMetricWAL] {
		out.WALBytes = soakFloatBytes(sums[soakMetricWAL])
	}
	if found[soakMetricDisk] {
		out.DiskBytes = soakFloatBytes(sums[soakMetricDisk])
	}
	if found[soakMetricCompaction] {
		out.CompactionCount = soakFloatBytes(sums[soakMetricCompaction])
	}
	if found[soakMetricCompactionBytes] {
		out.CompactionBytes = soakFloatBytes(sums[soakMetricCompactionBytes])
	}
	return out
}

// soakPromLine parses one Prometheus text exposition sample. It handles labels
// (including quoted spaces and escaped quotes) and ignores timestamps.
func soakPromLine(line string) (name, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", false
	}
	i := 0
	for i < len(line) {
		c := line[i]
		if c == '_' || c == ':' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", "", false
	}
	name = line[:i]
	if i < len(line) && line[i] == '{' {
		inQuote := false
		escaped := false
		for i < len(line) {
			c := line[i]
			if inQuote {
				switch {
				case escaped:
					escaped = false
				case c == '\\':
					escaped = true
				case c == '"':
					inQuote = false
				}
			} else {
				switch c {
				case '"':
					inQuote = true
				case '}':
					i++
					goto labelsDone
				}
			}
			i++
		}
		return "", "", false
	}
labelsDone:
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return "", "", false
	}
	j := i
	for j < len(line) && line[j] != ' ' && line[j] != '\t' {
		j++
	}
	if j == i {
		return "", "", false
	}
	return name, line[i:j], true
}

func soakFloatBytes(v float64) *int64 {
	if v < 0 || v > float64(math.MaxInt64) {
		return nil
	}
	n := int64(v)
	return &n
}

// ---- JSON report types ----------------------------------------------------

type soakLatencyJSON struct {
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MeanMS float64 `json:"mean_ms"`
	MinMS  float64 `json:"min_ms"`
	MaxMS  float64 `json:"max_ms"`
}

type soakStorageSampleJSON struct {
	DiskBytes            *int64 `json:"disk_bytes,omitempty"`
	DiskKeys             *int64 `json:"disk_key_count,omitempty"`
	WALBytes             *int64 `json:"wal_bytes,omitempty"`
	CompactionsTotal     *int64 `json:"compactions_total,omitempty"`
	CompactionBytesTotal *int64 `json:"compaction_bytes_total,omitempty"`
}

type soakClusterSampleJSON struct {
	NodesTotal     int    `json:"nodes_total"`
	NodesReachable int    `json:"nodes_reachable"`
	TermMax        uint64 `json:"term_max,omitempty"`
	CommitIndexMax uint64 `json:"commit_index_max,omitempty"`
	LeaderChanges  int    `json:"leader_changes"`
}

type soakBucketJSON struct {
	Index           int                   `json:"index"`
	Start           string                `json:"start"`
	End             string                `json:"end"`
	ElapsedSeconds  float64               `json:"elapsed_seconds"`
	DurationSeconds float64               `json:"duration_seconds"`
	Ops             int64                 `json:"ops"`
	Reads           int64                 `json:"reads"`
	Writes          int64                 `json:"writes"`
	Errors          int64                 `json:"errors"`
	ErrorRate       float64               `json:"error_rate"`
	Throughput      float64               `json:"throughput_ops_per_sec"`
	ReadThroughput  float64               `json:"read_ops_per_sec"`
	WriteThroughput float64               `json:"write_ops_per_sec"`
	BytesRead       int64                 `json:"bytes_read"`
	BytesWritten    int64                 `json:"bytes_written"`
	MiBPerSec       float64               `json:"mib_per_sec"`
	VerifyChecked   int64                 `json:"verify_checked"`
	VerifyFailed    int64                 `json:"verify_failed"`
	Latency         soakLatencyJSON       `json:"latency"`
	ReadLatency     soakLatencyJSON       `json:"read_latency"`
	WriteLatency    soakLatencyJSON       `json:"write_latency"`
	Memory          soakMemoryJSON        `json:"memory"`
	Cluster         soakClusterSampleJSON `json:"cluster"`
	Storage         soakStorageSampleJSON `json:"storage"`
}

type soakPreloadJSON struct {
	Enabled     bool    `json:"enabled"`
	TargetGB    float64 `json:"target_gb"`
	TargetBytes int64   `json:"target_bytes"`
	Keys        int64   `json:"keys"`
	Bytes       int64   `json:"bytes"`
	Workers     int     `json:"workers"`
	BatchKeys   int     `json:"batch_keys"`
	Seconds     float64 `json:"seconds"`
	MiBPerSec   float64 `json:"mib_per_sec"`
}

type soakWorkloadJSON struct {
	DurationSeconds float64         `json:"duration_seconds"`
	Ops             int64           `json:"ops"`
	Reads           int64           `json:"reads"`
	Writes          int64           `json:"writes"`
	Errors          int64           `json:"errors"`
	ErrorRate       float64         `json:"error_rate"`
	Throughput      float64         `json:"throughput_ops_per_sec"`
	ReadThroughput  float64         `json:"read_ops_per_sec"`
	WriteThroughput float64         `json:"write_ops_per_sec"`
	BytesRead       int64           `json:"bytes_read"`
	BytesWritten    int64           `json:"bytes_written"`
	MiBPerSec       float64         `json:"mib_per_sec"`
	FirstError      string          `json:"first_error,omitempty"`
	Latency         soakLatencyJSON `json:"latency"`
	ReadLatency     soakLatencyJSON `json:"read_latency"`
	WriteLatency    soakLatencyJSON `json:"write_latency"`
}

type soakVerificationJSON struct {
	InlineChecked int64  `json:"inline_checked"`
	InlineFailed  int64  `json:"inline_failed"`
	SampleChecked int64  `json:"sample_checked"`
	SampleFailed  int64  `json:"sample_failed"`
	TotalFailed   int64  `json:"total_failed"`
	FirstError    string `json:"first_error,omitempty"`
}

type soakDriftSeriesJSON struct {
	Points         int     `json:"points"`
	FirstMS        float64 `json:"first_ms"`
	LastMS         float64 `json:"last_ms"`
	MinMS          float64 `json:"min_ms"`
	MaxMS          float64 `json:"max_ms"`
	SlopeMSPerHour float64 `json:"slope_ms_per_hour"`
	SlopeEvaluated bool    `json:"slope_evaluated"`
}

type soakDriftJSON struct {
	Buckets  int                 `json:"buckets"`
	P99      soakDriftSeriesJSON `json:"p99"`
	ReadP99  soakDriftSeriesJSON `json:"read_p99"`
	WriteP99 soakDriftSeriesJSON `json:"write_p99"`
}

type soakMemorySummaryJSON struct {
	Samples                  int     `json:"samples"`
	WarmupSeconds            float64 `json:"warmup_seconds"`
	HeapInuseStartBytes      int64   `json:"heap_inuse_start_bytes"`
	HeapInuseEndBytes        int64   `json:"heap_inuse_end_bytes"`
	HeapInuseMaxBytes        int64   `json:"heap_inuse_max_bytes"`
	HeapInuseSlopeMiBPerHour float64 `json:"heap_inuse_slope_mib_per_hour"`
	HeapInuseSlopeEvaluated  bool    `json:"heap_inuse_slope_evaluated"`
	HeapAllocSlopeMiBPerHour float64 `json:"heap_alloc_slope_mib_per_hour"`
	HeapAllocSlopeEvaluated  bool    `json:"heap_alloc_slope_evaluated"`
	RSSAvailable             bool    `json:"rss_available"`
	RSSStartBytes            int64   `json:"rss_start_bytes,omitempty"`
	RSSEndBytes              int64   `json:"rss_end_bytes,omitempty"`
	RSSMaxBytes              int64   `json:"rss_max_bytes,omitempty"`
	RSSSlopeMiBPerHour       float64 `json:"rss_slope_mib_per_hour"`
	RSSSlopeEvaluated        bool    `json:"rss_slope_evaluated"`
	GoroutinesStart          int     `json:"goroutines_start"`
	GoroutinesEnd            int     `json:"goroutines_end"`
	GoroutinesMax            int     `json:"goroutines_max"`
	GCPauseTotalMS           float64 `json:"gc_pause_total_ms"`
	NumGCEnd                 uint32  `json:"num_gc_end"`
}

type soakStorageJSON struct {
	DiskProbeEnabled        bool    `json:"disk_probe_enabled"`
	DiskProbeIntervalSecs   float64 `json:"disk_probe_interval_seconds"`
	DiskAvailable           bool    `json:"disk_available"`
	DiskPartial             bool    `json:"disk_partial"`
	DiskSamples             int     `json:"disk_samples"`
	DiskRangesTotal         int     `json:"disk_ranges_total"`
	DiskRangesProbed        int     `json:"disk_ranges_probed"`
	DiskStartBytes          int64   `json:"disk_start_bytes,omitempty"`
	DiskEndBytes            int64   `json:"disk_end_bytes,omitempty"`
	DiskMaxBytes            int64   `json:"disk_max_bytes,omitempty"`
	DiskGrowthMiBPerHour    float64 `json:"disk_growth_mib_per_hour"`
	DiskGrowthEvaluated     bool    `json:"disk_growth_evaluated"`
	DiskKeyCountStart       int64   `json:"disk_key_count_start,omitempty"`
	DiskKeyCountEnd         int64   `json:"disk_key_count_end,omitempty"`
	WALMetricsConfigured    bool    `json:"wal_metrics_configured"`
	WALMetric               string  `json:"wal_metric,omitempty"`
	WALAvailable            bool    `json:"wal_available"`
	WALSamples              int     `json:"wal_samples"`
	WALStartBytes           int64   `json:"wal_start_bytes,omitempty"`
	WALEndBytes             int64   `json:"wal_end_bytes,omitempty"`
	WALMaxBytes             int64   `json:"wal_max_bytes,omitempty"`
	WALGrowthMiBPerHour     float64 `json:"wal_growth_mib_per_hour"`
	WALGrowthEvaluated      bool    `json:"wal_growth_evaluated"`
	CompactionAvailable     bool    `json:"compaction_available"`
	CompactionSamples       int     `json:"compaction_samples"`
	CompactionsStart        int64   `json:"compactions_start"`
	CompactionsEnd          int64   `json:"compactions_end"`
	CompactionsDelta        int64   `json:"compactions_delta"`
	CompactionCounterResets int     `json:"compaction_counter_resets"`
	CompactionBytesStart    int64   `json:"compaction_bytes_start"`
	CompactionBytesEnd      int64   `json:"compaction_bytes_end"`
	CompactionBytesDelta    int64   `json:"compaction_bytes_delta"`
	MetricsTargets          int     `json:"metrics_targets"`
	MetricsLastError        string  `json:"metrics_last_error,omitempty"`
	DiskLastError           string  `json:"disk_last_error,omitempty"`
}

type soakClusterJSON struct {
	NodesTotal     int                  `json:"nodes_total"`
	NodesReachable int                  `json:"nodes_reachable"`
	Ranges         int                  `json:"ranges"`
	LeaderChanges  int                  `json:"leader_changes"`
	Nodes          []soakNodeStatusJSON `json:"nodes"`
}

type soakGateResult struct {
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Configured bool    `json:"configured"`
	Evaluated  bool    `json:"evaluated"`
	Passed     bool    `json:"passed"`
	Limit      float64 `json:"limit"`
	Observed   float64 `json:"observed"`
	Detail     string  `json:"detail,omitempty"`
}

type soakGatesJSON struct {
	P99          soakGateResult `json:"p99"`
	ErrorRate    soakGateResult `json:"error_rate"`
	MemoryGrowth soakGateResult `json:"memory_growth"`
	WALGrowth    soakGateResult `json:"wal_growth"`
	Integrity    soakGateResult `json:"integrity"`
	Throughput   soakGateResult `json:"throughput"`
	Passed       bool           `json:"passed"`
	Failures     []string       `json:"failures"`
}

type soakGateEvaluation struct {
	P99          soakGateResult
	ErrorRate    soakGateResult
	MemoryGrowth soakGateResult
	WALGrowth    soakGateResult
	Integrity    soakGateResult
	Throughput   soakGateResult
	Failures     []string
}

func (g soakGateEvaluation) json() soakGatesJSON {
	return soakGatesJSON{
		P99:          g.P99,
		ErrorRate:    g.ErrorRate,
		MemoryGrowth: g.MemoryGrowth,
		WALGrowth:    g.WALGrowth,
		Integrity:    g.Integrity,
		Throughput:   g.Throughput,
		Passed:       len(g.Failures) == 0,
		Failures:     g.Failures,
	}
}

type soakBuildJSON struct {
	GoVersion string `json:"go_version"`
	Module    string `json:"module,omitempty"`
	Revision  string `json:"revision,omitempty"`
	VCSTime   string `json:"vcs_time,omitempty"`
	Dirty     bool   `json:"dirty,omitempty"`
}

type soakRunConfigJSON struct {
	Clients                    int     `json:"clients"`
	MaxInflight                int     `json:"max_inflight"`
	ReadRatio                  float64 `json:"read_ratio"`
	ValueSize                  int     `json:"value_size"`
	Keyspace                   uint64  `json:"keyspace"`
	PerWorkerKeys              uint64  `json:"per_worker_keys"`
	DurationSeconds            float64 `json:"duration_seconds"`
	SampleIntervalSeconds      float64 `json:"sample_interval_seconds"`
	OpTimeoutSeconds           float64 `json:"op_timeout_seconds"`
	Seed                       uint64  `json:"seed"`
	VerifyRate                 float64 `json:"verify_rate"`
	VerifySample               int     `json:"verify_sample"`
	PreloadTargetGB            float64 `json:"preload_target_gb"`
	PreloadWorkers             int     `json:"preload_workers"`
	PreloadBatchKeys           int     `json:"preload_batch_keys"`
	P99GateMS                  float64 `json:"p99_gate_ms"`
	MaxErrorRate               float64 `json:"max_error_rate"`
	MemoryGrowthGateMiBPerHour float64 `json:"memory_growth_gate_mib_per_hour"`
	WALGrowthGateMiBPerHour    float64 `json:"wal_growth_gate_mib_per_hour"`
	MinThroughput              float64 `json:"min_throughput_ops_per_sec"`
	MetricsTargets             int     `json:"metrics_targets"`
	DiskProbe                  bool    `json:"disk_probe"`
	DiskProbeIntervalSeconds   float64 `json:"disk_probe_interval_seconds"`
}

type soakSummary struct {
	Harness      string                `json:"harness"`
	Timestamp    string                `json:"timestamp"`
	ClientHost   string                `json:"client_host,omitempty"`
	Build        soakBuildJSON         `json:"build"`
	Meta         string                `json:"meta"`
	Nodes        []string              `json:"nodes"`
	Config       soakRunConfigJSON     `json:"config"`
	Preload      soakPreloadJSON       `json:"preload"`
	Workload     soakWorkloadJSON      `json:"workload"`
	Buckets      []soakBucketJSON      `json:"buckets"`
	LatencyDrift soakDriftJSON         `json:"latency_drift"`
	Memory       soakMemorySummaryJSON `json:"memory"`
	Storage      soakStorageJSON       `json:"storage"`
	Cluster      soakClusterJSON       `json:"cluster"`
	Verification soakVerificationJSON  `json:"verification"`
	Gates        soakGatesJSON         `json:"gates"`
}

type soakSummaryInput struct {
	cfg      soakConfig
	host     string
	preload  soakPreloadStats
	workload soakWorkloadResult
	verify   soakVerifyResult
	buckets  []soakBucketJSON
	drift    soakDriftJSON
	memory   soakMemorySummaryJSON
	storage  soakStorageJSON
	cluster  soakClusterJSON
	gates    soakGateEvaluation
}

func soakBuildSummary(in soakSummaryInput) *soakSummary {
	cfg := in.cfg
	durationSeconds := in.workload.Duration.Seconds()
	deriveRate := func(n int64) float64 {
		if durationSeconds <= 0 {
			return 0
		}
		return float64(n) / durationSeconds
	}
	errorRate := 0.0
	if in.workload.Ops > 0 {
		errorRate = float64(in.workload.Errors) / float64(in.workload.Ops)
	}
	preloadSeconds := in.preload.Duration.Seconds()
	preloadRate := 0.0
	if preloadSeconds > 0 {
		preloadRate = float64(in.preload.Bytes) / preloadSeconds / float64(1<<20)
	}

	return &soakSummary{
		Harness:    soakHarness,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		ClientHost: in.host,
		Build:      soakBuildInfo(),
		Meta:       cfg.Meta,
		Nodes:      append([]string(nil), cfg.Nodes...),
		Config: soakRunConfigJSON{
			Clients:                    cfg.Clients,
			MaxInflight:                cfg.MaxInflight,
			ReadRatio:                  cfg.ReadRatio,
			ValueSize:                  cfg.ValueSize,
			Keyspace:                   cfg.Keyspace,
			PerWorkerKeys:              cfg.PerWorkerKeys,
			DurationSeconds:            cfg.Duration.Seconds(),
			SampleIntervalSeconds:      cfg.SampleInterval.Seconds(),
			OpTimeoutSeconds:           cfg.OpTimeout.Seconds(),
			Seed:                       cfg.Seed,
			VerifyRate:                 cfg.VerifyRate,
			VerifySample:               cfg.VerifySample,
			PreloadTargetGB:            cfg.PreloadGB,
			PreloadWorkers:             cfg.PreloadWorkers,
			PreloadBatchKeys:           cfg.PreloadBatchKeys,
			P99GateMS:                  cfg.P99GateMS,
			MaxErrorRate:               cfg.MaxErrorRate,
			MemoryGrowthGateMiBPerHour: cfg.MemoryGrowthMaxMiBPerHour,
			WALGrowthGateMiBPerHour:    cfg.WALGrowthMaxMiBPerHour,
			MinThroughput:              cfg.MinThroughput,
			MetricsTargets:             len(cfg.MetricsURLs),
			DiskProbe:                  cfg.DiskProbe,
			DiskProbeIntervalSeconds:   cfg.DiskProbeInterval.Seconds(),
		},
		Preload: soakPreloadJSON{
			Enabled:     cfg.PreloadBytes > 0,
			TargetGB:    cfg.PreloadGB,
			TargetBytes: cfg.PreloadBytes,
			Keys:        in.preload.Keys,
			Bytes:       in.preload.Bytes,
			Workers:     cfg.PreloadWorkers,
			BatchKeys:   soakPreloadBatchKeys(cfg),
			Seconds:     preloadSeconds,
			MiBPerSec:   preloadRate,
		},
		Workload: soakWorkloadJSON{
			DurationSeconds: durationSeconds,
			Ops:             in.workload.Ops,
			Reads:           in.workload.Reads,
			Writes:          in.workload.Writes,
			Errors:          in.workload.Errors,
			ErrorRate:       errorRate,
			Throughput:      deriveRate(in.workload.Ops),
			ReadThroughput:  deriveRate(in.workload.Reads),
			WriteThroughput: deriveRate(in.workload.Writes),
			BytesRead:       in.workload.BytesRead,
			BytesWritten:    in.workload.BytesWritten,
			MiBPerSec:       deriveRate(in.workload.BytesRead+in.workload.BytesWritten) / float64(1<<20),
			FirstError:      in.workload.FirstError,
			Latency:         soakLatencyFromHist(&in.workload.All),
			ReadLatency:     soakLatencyFromHist(&in.workload.Read),
			WriteLatency:    soakLatencyFromHist(&in.workload.Write),
		},
		Buckets:      in.buckets,
		LatencyDrift: in.drift,
		Memory:       in.memory,
		Storage:      in.storage,
		Cluster:      in.cluster,
		Verification: soakVerificationJSON{
			InlineChecked: in.workload.VerifyChecked,
			InlineFailed:  in.workload.VerifyFailed,
			SampleChecked: in.verify.Checked,
			SampleFailed:  in.verify.Failed,
			TotalFailed:   in.workload.VerifyFailed + in.verify.Failed,
			FirstError:    in.verify.FirstError,
		},
		Gates: in.gates.json(),
	}
}

func soakLogSummary(t *testing.T, s *soakSummary) {
	t.Helper()
	w := s.Workload
	t.Logf("SOAK workload: elapsed=%.3fs ops=%d reads=%d writes=%d errors=%d error_rate=%.6f throughput=%.2f ops/s read=%.2f write=%.2f mib/s=%.2f",
		w.DurationSeconds, w.Ops, w.Reads, w.Writes, w.Errors, w.ErrorRate,
		w.Throughput, w.ReadThroughput, w.WriteThroughput, w.MiBPerSec)
	t.Logf("SOAK latency all: %s", soakFormatLatency(w.Latency))
	t.Logf("SOAK latency read: %s", soakFormatLatency(w.ReadLatency))
	t.Logf("SOAK latency write: %s", soakFormatLatency(w.WriteLatency))
	if s.Preload.Enabled {
		t.Logf("SOAK preload: keys=%d bytes=%d (%.3f GiB) workers=%d elapsed=%.3fs rate=%.1f MiB/s",
			s.Preload.Keys, s.Preload.Bytes, float64(s.Preload.Bytes)/float64(soakGiB),
			s.Preload.Workers, s.Preload.Seconds, s.Preload.MiBPerSec)
	} else {
		t.Logf("SOAK preload: disabled")
	}
	t.Logf("SOAK drift p99: buckets=%d first=%.2fms last=%.2fms min=%.2fms max=%.2fms slope=%.2fms/h evaluated=%t",
		s.LatencyDrift.P99.Points, s.LatencyDrift.P99.FirstMS, s.LatencyDrift.P99.LastMS,
		s.LatencyDrift.P99.MinMS, s.LatencyDrift.P99.MaxMS, s.LatencyDrift.P99.SlopeMSPerHour,
		s.LatencyDrift.P99.SlopeEvaluated)
	mem := s.Memory
	t.Logf("SOAK memory: samples=%d heap_inuse_start=%.1fMiB end=%.1fMiB max=%.1fMiB slope=%.2fMiB/h evaluated=%t rss_available=%t rss_slope=%.2fMiB/h goroutines_start=%d end=%d max=%d",
		mem.Samples, float64(mem.HeapInuseStartBytes)/(1<<20), float64(mem.HeapInuseEndBytes)/(1<<20),
		float64(mem.HeapInuseMaxBytes)/(1<<20), mem.HeapInuseSlopeMiBPerHour, mem.HeapInuseSlopeEvaluated,
		mem.RSSAvailable, mem.RSSSlopeMiBPerHour, mem.GoroutinesStart, mem.GoroutinesEnd, mem.GoroutinesMax)
	st := s.Storage
	fmtStorageBytes := func(v int64, available bool) string {
		if !available {
			return "n/a"
		}
		return fmt.Sprintf("%.3fGiB", float64(v)/float64(soakGiB))
	}
	t.Logf("SOAK storage: disk_available=%t disk_partial=%t disk_start=%s disk_end=%s disk_growth=%.2fMiB/h wal_available=%t wal_start=%s wal_end=%s wal_growth=%.2fMiB/h compaction_available=%t compactions_delta=%d compaction_bytes_delta=%d",
		st.DiskAvailable, st.DiskPartial, fmtStorageBytes(st.DiskStartBytes, st.DiskAvailable),
		fmtStorageBytes(st.DiskEndBytes, st.DiskAvailable), st.DiskGrowthMiBPerHour,
		st.WALAvailable, fmtStorageBytes(st.WALStartBytes, st.WALAvailable),
		fmtStorageBytes(st.WALEndBytes, st.WALAvailable), st.WALGrowthMiBPerHour,
		st.CompactionAvailable, st.CompactionsDelta, st.CompactionBytesDelta)
	if st.WALMetric != "" {
		t.Logf("SOAK storage metric: wal=%s", st.WALMetric)
	}
	if st.MetricsLastError != "" {
		t.Logf("SOAK storage metrics error: %s", st.MetricsLastError)
	}
	if st.DiskLastError != "" {
		t.Logf("SOAK storage disk error: %s", st.DiskLastError)
	}
	t.Logf("SOAK cluster: nodes=%d reachable=%d ranges=%d leader_changes=%d",
		s.Cluster.NodesTotal, s.Cluster.NodesReachable, s.Cluster.Ranges, s.Cluster.LeaderChanges)
	t.Logf("SOAK verification: inline_checked=%d inline_failed=%d sample_checked=%d sample_failed=%d total_failed=%d",
		s.Verification.InlineChecked, s.Verification.InlineFailed,
		s.Verification.SampleChecked, s.Verification.SampleFailed, s.Verification.TotalFailed)
	if s.Verification.FirstError != "" {
		t.Logf("SOAK verification first error: %s", s.Verification.FirstError)
	}
	if w.FirstError != "" {
		t.Logf("SOAK workload first error: %s", w.FirstError)
	}
	if s.Gates.Passed {
		t.Logf("SOAK gate: PASS")
	} else {
		t.Logf("SOAK gate: FAIL failures=%d", len(s.Gates.Failures))
		for _, failure := range s.Gates.Failures {
			t.Logf("SOAK gate: %s", failure)
		}
	}
}

func soakFormatLatency(l soakLatencyJSON) string {
	return fmt.Sprintf("p50=%.3fms p95=%.3fms p99=%.3fms mean=%.3fms min=%.3fms max=%.3fms",
		l.P50MS, l.P95MS, l.P99MS, l.MeanMS, l.MinMS, l.MaxMS)
}

func soakFormatOptionalBytes(p *int64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3fGiB", float64(*p)/float64(soakGiB))
}

func soakInt64Ptr(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func soakTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func soakBuildInfo() soakBuildJSON {
	info := soakBuildJSON{GoVersion: runtime.Version()}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.Module = bi.Main.Path
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = setting.Value
		case "vcs.time":
			info.VCSTime = setting.Value
		case "vcs.modified":
			info.Dirty = setting.Value == "true"
		}
	}
	return info
}

func soakWriteReport(path string, summary *soakSummary) error {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---- summaries ------------------------------------------------------------

type soakPoint struct {
	Seconds float64
	Value   float64
}

func soakSlopePerHour(points []soakPoint) (float64, bool) {
	if len(points) < 3 {
		return 0, false
	}
	minX, maxX := points[0].Seconds, points[0].Seconds
	var sumX, sumY float64
	for _, p := range points {
		if p.Seconds < minX {
			minX = p.Seconds
		}
		if p.Seconds > maxX {
			maxX = p.Seconds
		}
		sumX += p.Seconds
		sumY += p.Value
	}
	if maxX-minX < 60 {
		return 0, false
	}
	n := float64(len(points))
	meanX := sumX / n
	meanY := sumY / n
	var num, den float64
	for _, p := range points {
		dx := p.Seconds - meanX
		num += dx * (p.Value - meanY)
		den += dx * dx
	}
	if den == 0 {
		return 0, false
	}
	slopePerSecond := num / den
	return slopePerSecond * 3600, true
}

func soakBuildMemorySummary(cfg soakConfig, buckets []soakBucketJSON) soakMemorySummaryJSON {
	warmup := cfg.Duration.Seconds() / 10
	if warmup > 3600 {
		warmup = 3600
	}
	if warmup < 60 {
		warmup = 0
	}
	out := soakMemorySummaryJSON{WarmupSeconds: warmup}
	var heapPoints, allocPoints, rssPoints []soakPoint
	var allHeapPoints, allRSSPoints []soakPoint
	var haveRSS bool
	for _, b := range buckets {
		out.Samples++
		m := b.Memory
		allHeapPoints = append(allHeapPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(m.HeapInuseBytes)})
		if m.RSSBytes != nil {
			allRSSPoints = append(allRSSPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*m.RSSBytes)})
		}
		if b.ElapsedSeconds >= warmup {
			heapPoints = append(heapPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(m.HeapInuseBytes)})
			allocPoints = append(allocPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(m.HeapAllocBytes)})
			if m.RSSBytes != nil {
				rssPoints = append(rssPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*m.RSSBytes)})
			}
		}
		if m.RSSBytes != nil {
			out.RSSAvailable = true
			haveRSS = true
			if int64(*m.RSSBytes) > out.RSSMaxBytes {
				out.RSSMaxBytes = *m.RSSBytes
			}
		}
		if m.HeapInuseBytes > out.HeapInuseMaxBytes {
			out.HeapInuseMaxBytes = m.HeapInuseBytes
		}
		if b.Index == 0 {
			out.GoroutinesStart = m.NumGoroutine
		}
		out.GoroutinesEnd = m.NumGoroutine
		if m.NumGoroutine > out.GoroutinesMax {
			out.GoroutinesMax = m.NumGoroutine
		}
		out.GCPauseTotalMS = m.GCPauseTotalMS
		out.NumGCEnd = m.NumGC
	}
	if len(allHeapPoints) > 0 {
		out.HeapInuseStartBytes = int64(allHeapPoints[0].Value)
		out.HeapInuseEndBytes = int64(allHeapPoints[len(allHeapPoints)-1].Value)
	}
	if slope, ok := soakSlopePerHour(heapPoints); ok {
		out.HeapInuseSlopeMiBPerHour = slope / float64(1<<20)
		out.HeapInuseSlopeEvaluated = true
	}
	if slope, ok := soakSlopePerHour(allocPoints); ok {
		out.HeapAllocSlopeMiBPerHour = slope / float64(1<<20)
		out.HeapAllocSlopeEvaluated = true
	}
	if haveRSS {
		if len(allRSSPoints) > 0 {
			out.RSSStartBytes = int64(allRSSPoints[0].Value)
			out.RSSEndBytes = int64(allRSSPoints[len(allRSSPoints)-1].Value)
		}
		if slope, ok := soakSlopePerHour(rssPoints); ok {
			out.RSSSlopeMiBPerHour = slope / float64(1<<20)
			out.RSSSlopeEvaluated = true
		}
	}
	return out
}

type soakStorageProbeStats struct {
	RangesTotal      int
	RangesProbed     int
	DiskPartial      bool
	DiskLastError    string
	MetricsLastError string
}

func soakBuildStorageSummary(cfg soakConfig, buckets []soakBucketJSON, stats soakStorageProbeStats) soakStorageJSON {
	out := soakStorageJSON{
		DiskProbeEnabled:      cfg.DiskProbe,
		DiskProbeIntervalSecs: cfg.DiskProbeInterval.Seconds(),
		DiskRangesTotal:       stats.RangesTotal,
		DiskRangesProbed:      stats.RangesProbed,
		DiskPartial:           stats.DiskPartial,
		DiskLastError:         stats.DiskLastError,
		MetricsLastError:      stats.MetricsLastError,
		MetricsTargets:        len(cfg.MetricsURLs),
		WALMetricsConfigured:  len(cfg.MetricsURLs) > 0,
	}
	if len(cfg.MetricsURLs) > 0 {
		out.WALMetric = "auto"
	}
	if cfg.WALMetric != "" {
		out.WALMetric = cfg.WALMetric
	}

	var diskPoints, walPoints []soakPoint
	var diskKeys []soakPoint
	var compactionPoints, compactionBytesPoints []soakPoint
	for _, b := range buckets {
		if b.Storage.DiskBytes != nil {
			diskPoints = append(diskPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*b.Storage.DiskBytes)})
			if b.Storage.DiskKeys != nil {
				diskKeys = append(diskKeys, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*b.Storage.DiskKeys)})
			}
		}
		if b.Storage.WALBytes != nil {
			walPoints = append(walPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*b.Storage.WALBytes)})
		}
		if b.Storage.CompactionsTotal != nil {
			compactionPoints = append(compactionPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*b.Storage.CompactionsTotal)})
		}
		if b.Storage.CompactionBytesTotal != nil {
			compactionBytesPoints = append(compactionBytesPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: float64(*b.Storage.CompactionBytesTotal)})
		}
	}

	if len(diskPoints) > 0 {
		out.DiskAvailable = true
		out.DiskSamples = len(diskPoints)
		out.DiskStartBytes = int64(diskPoints[0].Value)
		out.DiskEndBytes = int64(diskPoints[len(diskPoints)-1].Value)
		for _, p := range diskPoints {
			if int64(p.Value) > out.DiskMaxBytes {
				out.DiskMaxBytes = int64(p.Value)
			}
		}
		if slope, ok := soakSlopePerHour(diskPoints); ok {
			out.DiskGrowthMiBPerHour = slope / float64(1<<20)
			out.DiskGrowthEvaluated = true
		}
	}
	if len(diskKeys) > 0 {
		out.DiskKeyCountStart = int64(diskKeys[0].Value)
		out.DiskKeyCountEnd = int64(diskKeys[len(diskKeys)-1].Value)
	}

	if len(walPoints) > 0 {
		out.WALAvailable = true
		out.WALSamples = len(walPoints)
		out.WALStartBytes = int64(walPoints[0].Value)
		out.WALEndBytes = int64(walPoints[len(walPoints)-1].Value)
		for _, p := range walPoints {
			if int64(p.Value) > out.WALMaxBytes {
				out.WALMaxBytes = int64(p.Value)
			}
		}
		if slope, ok := soakSlopePerHour(walPoints); ok {
			out.WALGrowthMiBPerHour = slope / float64(1<<20)
			out.WALGrowthEvaluated = true
		}
	}

	if len(compactionPoints) > 0 {
		out.CompactionAvailable = true
		out.CompactionSamples = len(compactionPoints)
		out.CompactionsStart = int64(compactionPoints[0].Value)
		out.CompactionsEnd = int64(compactionPoints[len(compactionPoints)-1].Value)
		var previous int64
		for i, p := range compactionPoints {
			cur := int64(p.Value)
			if i > 0 {
				if cur >= previous {
					out.CompactionsDelta += cur - previous
				} else {
					out.CompactionCounterResets++
					out.CompactionsDelta += cur
				}
			}
			previous = cur
		}
	}
	if len(compactionBytesPoints) > 0 {
		out.CompactionAvailable = true
		if out.CompactionSamples == 0 {
			out.CompactionSamples = len(compactionBytesPoints)
		}
		previous := int64(compactionBytesPoints[0].Value)
		for i, p := range compactionBytesPoints {
			cur := int64(p.Value)
			if i == 0 {
				out.CompactionBytesStart = cur
				continue
			}
			if cur >= previous {
				out.CompactionBytesDelta += cur - previous
			} else {
				out.CompactionBytesDelta += cur
			}
			previous = cur
		}
		out.CompactionBytesEnd = int64(compactionBytesPoints[len(compactionBytesPoints)-1].Value)
	}
	return out
}

func soakBuildDrift(buckets []soakBucketJSON) soakDriftJSON {
	var allPoints, readPoints, writePoints []soakPoint
	for _, b := range buckets {
		if b.Ops > 0 {
			allPoints = append(allPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: b.Latency.P99MS})
		}
		if b.Reads > 0 {
			readPoints = append(readPoints, soakPoint{Seconds: b.ElapsedSeconds, Value: b.ReadLatency.P99MS})
		}
		if b.Writes > 0 {
			writePoints = append(writePoints, soakPoint{Seconds: b.ElapsedSeconds, Value: b.WriteLatency.P99MS})
		}
	}
	return soakDriftJSON{
		Buckets:  len(buckets),
		P99:      soakDriftSeries(allPoints),
		ReadP99:  soakDriftSeries(readPoints),
		WriteP99: soakDriftSeries(writePoints),
	}
}

func soakDriftSeries(points []soakPoint) soakDriftSeriesJSON {
	out := soakDriftSeriesJSON{Points: len(points)}
	if len(points) == 0 {
		return out
	}
	out.FirstMS = points[0].Value
	out.LastMS = points[len(points)-1].Value
	out.MinMS = points[0].Value
	out.MaxMS = points[0].Value
	for _, p := range points {
		if p.Value < out.MinMS {
			out.MinMS = p.Value
		}
		if p.Value > out.MaxMS {
			out.MaxMS = p.Value
		}
	}
	if slope, ok := soakSlopePerHour(points); ok {
		out.SlopeMSPerHour = slope
		out.SlopeEvaluated = true
	}
	return out
}

func soakBuildClusterSummary(cfg soakConfig, statuses []soakNodeStatusJSON, ranges, leaderChanges int) soakClusterJSON {
	out := soakClusterJSON{
		NodesTotal:    len(cfg.Nodes),
		Ranges:        ranges,
		LeaderChanges: leaderChanges,
		Nodes:         append([]soakNodeStatusJSON(nil), statuses...),
	}
	for _, st := range statuses {
		if st.Reachable {
			out.NodesReachable++
		}
	}
	return out
}

// ---- gate evaluation ------------------------------------------------------

type soakGateInput struct {
	cfg      soakConfig
	workload soakWorkloadResult
	verify   soakVerifyResult
	memory   soakMemorySummaryJSON
	storage  soakStorageJSON
}

func soakEvaluateGates(in soakGateInput) soakGateEvaluation {
	cfg := in.cfg
	g := soakGateEvaluation{Failures: make([]string, 0, 8)}
	fail := func(format string, args ...any) {
		g.Failures = append(g.Failures, fmt.Sprintf(format, args...))
	}

	if in.workload.Ops == 0 {
		fail("no measured operations completed")
	}

	p99All := in.workload.All.quantile(0.99)
	p99Read := in.workload.Read.quantile(0.99)
	p99Write := in.workload.Write.quantile(0.99)
	p99Max := p99All
	if p99Read > p99Max {
		p99Max = p99Read
	}
	if p99Write > p99Max {
		p99Max = p99Write
	}
	g.P99 = soakGateResult{
		Name:       "p99",
		Unit:       "ms",
		Configured: cfg.P99GateMS > 0,
		Limit:      cfg.P99GateMS,
		Observed:   float64(p99Max) / float64(time.Millisecond),
	}
	if cfg.P99GateMS > 0 {
		if in.workload.Ops == 0 {
			g.P99.Detail = "no measured operations"
		} else {
			g.P99.Evaluated = true
			g.P99.Passed = g.P99.Observed <= cfg.P99GateMS+1e-9
			g.P99.Detail = fmt.Sprintf("all=%.3fms read=%.3fms write=%.3fms",
				float64(p99All)/float64(time.Millisecond),
				float64(p99Read)/float64(time.Millisecond),
				float64(p99Write)/float64(time.Millisecond))
			if !g.P99.Passed {
				fail("p99 %.3fms exceeds SOAK_P99_GATE_MS=%.1f (%s)",
					g.P99.Observed, cfg.P99GateMS, g.P99.Detail)
			}
		}
	} else {
		g.P99.Passed = true
		g.P99.Detail = "disabled"
	}

	errorRate := 0.0
	if in.workload.Ops > 0 {
		errorRate = float64(in.workload.Errors) / float64(in.workload.Ops)
	}
	g.ErrorRate = soakGateResult{
		Name:       "error_rate",
		Unit:       "ratio",
		Configured: true,
		Limit:      cfg.MaxErrorRate,
		Observed:   errorRate,
	}
	if in.workload.Ops > 0 {
		g.ErrorRate.Evaluated = true
		g.ErrorRate.Passed = errorRate <= cfg.MaxErrorRate+1e-12
		if !g.ErrorRate.Passed {
			fail("error rate %.6f exceeds SOAK_MAX_ERROR_RATE=%.6f (%d/%d)",
				errorRate, cfg.MaxErrorRate, in.workload.Errors, in.workload.Ops)
		}
	} else {
		g.ErrorRate.Detail = "no measured operations"
	}

	g.MemoryGrowth = soakGateResult{
		Name:       "memory_growth",
		Unit:       "MiB/h",
		Configured: cfg.MemoryGrowthMaxMiBPerHour > 0,
		Limit:      cfg.MemoryGrowthMaxMiBPerHour,
		Observed:   in.memory.HeapInuseSlopeMiBPerHour,
	}
	if cfg.MemoryGrowthMaxMiBPerHour > 0 {
		if in.memory.HeapInuseSlopeEvaluated {
			g.MemoryGrowth.Evaluated = true
			g.MemoryGrowth.Passed = in.memory.HeapInuseSlopeMiBPerHour <= cfg.MemoryGrowthMaxMiBPerHour+1e-9
			g.MemoryGrowth.Detail = fmt.Sprintf("heap_inuse samples=%d warmup=%.0fs",
				in.memory.Samples, in.memory.WarmupSeconds)
			if !g.MemoryGrowth.Passed {
				fail("heap in-use growth %.2f MiB/h exceeds SOAK_MEMORY_GROWTH_MIB_PER_HOUR=%.1f",
					in.memory.HeapInuseSlopeMiBPerHour, cfg.MemoryGrowthMaxMiBPerHour)
			}
		} else {
			g.MemoryGrowth.Detail = "insufficient heap samples for a slope"
			fail("memory growth gate configured but not evaluated: %s", g.MemoryGrowth.Detail)
		}
	} else {
		g.MemoryGrowth.Passed = true
		g.MemoryGrowth.Detail = "disabled"
	}

	g.WALGrowth = soakGateResult{
		Name:       "wal_growth",
		Unit:       "MiB/h",
		Configured: cfg.WALGrowthMaxMiBPerHour > 0,
		Limit:      cfg.WALGrowthMaxMiBPerHour,
		Observed:   in.storage.WALGrowthMiBPerHour,
	}
	if cfg.WALGrowthMaxMiBPerHour > 0 {
		if in.storage.WALGrowthEvaluated {
			g.WALGrowth.Evaluated = true
			g.WALGrowth.Passed = in.storage.WALGrowthMiBPerHour <= cfg.WALGrowthMaxMiBPerHour+1e-9
			g.WALGrowth.Detail = fmt.Sprintf("metric=%s samples=%d", in.storage.WALMetric, in.storage.WALSamples)
			if !g.WALGrowth.Passed {
				fail("WAL growth %.2f MiB/h exceeds SOAK_WAL_GROWTH_MIB_PER_HOUR=%.1f",
					in.storage.WALGrowthMiBPerHour, cfg.WALGrowthMaxMiBPerHour)
			}
		} else {
			g.WALGrowth.Detail = "no WAL metric samples; set SOAK_METRICS_URLS/SOAK_WAL_METRIC"
			fail("WAL growth gate configured but not evaluated: %s", g.WALGrowth.Detail)
		}
	} else {
		g.WALGrowth.Passed = true
		g.WALGrowth.Detail = "disabled"
	}

	verifyFailed := in.workload.VerifyFailed + in.verify.Failed
	g.Integrity = soakGateResult{
		Name:       "integrity",
		Unit:       "failures",
		Configured: true,
		Evaluated:  true,
		Limit:      0,
		Observed:   float64(verifyFailed),
		Passed:     verifyFailed == 0,
		Detail:     fmt.Sprintf("inline=%d sample=%d", in.workload.VerifyFailed, in.verify.Failed),
	}
	if verifyFailed > 0 {
		fail("integrity failures=%d (inline=%d post_sample=%d)",
			verifyFailed, in.workload.VerifyFailed, in.verify.Failed)
	}

	throughput := 0.0
	if in.workload.Duration > 0 {
		throughput = float64(in.workload.Ops) / in.workload.Duration.Seconds()
	}
	g.Throughput = soakGateResult{
		Name:       "throughput",
		Unit:       "ops/s",
		Configured: cfg.MinThroughput > 0,
		Limit:      cfg.MinThroughput,
		Observed:   throughput,
	}
	if cfg.MinThroughput > 0 {
		g.Throughput.Evaluated = true
		g.Throughput.Passed = throughput >= cfg.MinThroughput
		if !g.Throughput.Passed {
			fail("throughput %.2f ops/s is below SOAK_MIN_THROUGHPUT=%.2f",
				throughput, cfg.MinThroughput)
		}
	} else {
		g.Throughput.Passed = true
		g.Throughput.Detail = "disabled"
	}

	return g
}

// TestSoakSelf is the always-on, cluster-free smoke test for this harness. It
// exercises configuration parsing, deterministic keys/values, histogram
// quantiles, drift/memory/storage summary math and gate evaluation. It runs
// under -short; TestSoakMultiHost does not.
func TestSoakSelf(t *testing.T) {
	t.Run("config-defaults", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		if cfg.Duration != 72*time.Hour {
			t.Fatalf("duration = %s, want 72h", cfg.Duration)
		}
		if cfg.Clients != soakDefaultClients {
			t.Fatalf("clients = %d, want %d", cfg.Clients, soakDefaultClients)
		}
		if cfg.MaxInflight != cfg.Clients {
			t.Fatalf("max inflight = %d, want clients %d", cfg.MaxInflight, cfg.Clients)
		}
		if cfg.SampleInterval != time.Minute {
			t.Fatalf("sample interval = %s, want 1m", cfg.SampleInterval)
		}
		if cfg.P99GateMS != 0 || cfg.MemoryGrowthMaxMiBPerHour != 0 || cfg.WALGrowthMaxMiBPerHour != 0 {
			t.Fatalf("trend gates should default to off: %+v", cfg)
		}
		if cfg.MaxErrorRate != soakDefaultMaxErrorRate {
			t.Fatalf("max error rate = %g, want %g", cfg.MaxErrorRate, soakDefaultMaxErrorRate)
		}
		if !cfg.DiskProbe {
			t.Fatalf("disk probe should default on")
		}
		if cfg.ReportPath != "" {
			t.Fatalf("report path = %q, want empty", cfg.ReportPath)
		}
	})

	t.Run("config-overrides", func(t *testing.T) {
		env := soakSelfEnv()
		env["SOAK_DURATION"] = "90m"
		env["SOAK_CLIENTS"] = "4"
		env["SOAK_MAX_INFLIGHT"] = "8"
		env["SOAK_VALUE_SIZE"] = "4096"
		env["SOAK_KEYSPACE"] = "1000"
		env["SOAK_READ_RATIO"] = "0.5"
		env["SOAK_SAMPLE_INTERVAL"] = "30s"
		env["SOAK_PRELOAD_GB"] = "0.5"
		env["SOAK_PRELOAD_WORKERS"] = "2"
		env["SOAK_P99_GATE_MS"] = "250"
		env["SOAK_MAX_ERROR_RATE"] = "0"
		env["SOAK_MEMORY_GROWTH_MIB_PER_HOUR"] = "64"
		env["SOAK_WAL_GROWTH_MIB_PER_HOUR"] = "128"
		env["SOAK_METRICS_URLS"] = "http://127.0.0.1:9100/metrics,https://metrics.example:9100/metrics"
		env["SOAK_WAL_METRIC"] = "custom_wal_bytes"
		env["SOAK_DISK_PROBE"] = "0"
		env["SOAK_DISK_PROBE_INTERVAL"] = "10m"
		env["SOAK_REPORT_JSON"] = "/tmp/soak.json"
		env["SOAK_SEED"] = "42"
		cfg, err := soakConfigFrom(mapGet(env))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		if cfg.Duration != 90*time.Minute {
			t.Fatalf("duration = %s, want 90m", cfg.Duration)
		}
		if cfg.Clients != 4 || cfg.MaxInflight != 8 || cfg.PerWorkerKeys != 250 {
			t.Fatalf("clients/inflight/span = %d/%d/%d, want 4/8/250", cfg.Clients, cfg.MaxInflight, cfg.PerWorkerKeys)
		}
		if cfg.PreloadBytes != int64(0.5*float64(soakGiB)) {
			t.Fatalf("preload bytes = %d, want %.0f", cfg.PreloadBytes, 0.5*float64(soakGiB))
		}
		if cfg.PreloadKeys != cfg.PreloadBytes/4096 {
			t.Fatalf("preload keys = %d, want %d", cfg.PreloadKeys, cfg.PreloadBytes/4096)
		}
		if len(cfg.MetricsURLs) != 2 {
			t.Fatalf("metrics urls = %d, want 2", len(cfg.MetricsURLs))
		}
		if cfg.WALMetric != "custom_wal_bytes" {
			t.Fatalf("wal metric = %q", cfg.WALMetric)
		}
		if cfg.DiskProbe {
			t.Fatalf("disk probe should be disabled")
		}
		if cfg.DiskProbeInterval != 10*time.Minute {
			t.Fatalf("disk interval = %s, want 10m", cfg.DiskProbeInterval)
		}
		if cfg.ReportPath != "/tmp/soak.json" {
			t.Fatalf("report path = %q", cfg.ReportPath)
		}
		if cfg.Seed != 42 {
			t.Fatalf("seed = %d, want 42", cfg.Seed)
		}
	})

	t.Run("config-errors", func(t *testing.T) {
		cases := []struct {
			name  string
			key   string
			value string
			unset bool
		}{
			{"missing-meta", "SOAK_META", "", true},
			{"missing-nodes", "SOAK_NODES", "", true},
			{"empty-node", "SOAK_NODES", "a:1,,b:1", false},
			{"duplicate-node", "SOAK_NODES", "a:1,a:1", false},
			{"bad-duration", "SOAK_DURATION", "soon", false},
			{"zero-duration", "SOAK_DURATION", "0", false},
			{"keyspace-below-clients", "SOAK_KEYSPACE", "3", false},
			{"bad-clients", "SOAK_CLIENTS", "0", false},
			{"bad-inflight", "SOAK_MAX_INFLIGHT", "999999", false},
			{"bad-ratio", "SOAK_READ_RATIO", "1.5", false},
			{"bad-value-size", "SOAK_VALUE_SIZE", "0", false},
			{"bad-sample", "SOAK_SAMPLE_INTERVAL", "1s", false},
			{"bad-metrics-url", "SOAK_METRICS_URLS", "ftp://host/metrics", false},
			{"bad-metric-name", "SOAK_WAL_METRIC", "bad-name!", false},
			{"bad-bool", "SOAK_DISK_PROBE", "maybe", false},
			{"bad-gate", "SOAK_P99_GATE_MS", "-1", false},
			{"bad-growth", "SOAK_MEMORY_GROWTH_MIB_PER_HOUR", "-1", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env := soakSelfEnv()
				if tc.unset {
					delete(env, tc.key)
				} else {
					env[tc.key] = tc.value
				}
				if _, err := soakConfigFrom(mapGet(env)); err == nil {
					t.Fatalf("config with %s=%q succeeded, want error", tc.key, tc.value)
				}
			})
		}
	})

	t.Run("parse-duration", func(t *testing.T) {
		cases := map[string]time.Duration{
			"72h":   72 * time.Hour,
			"90m":   90 * time.Minute,
			"300":   300 * time.Second,
			"1.5h":  90 * time.Minute,
			"500ms": 500 * time.Millisecond,
			"3d":    72 * time.Hour,
			"1d12h": 36 * time.Hour,
		}
		for raw, want := range cases {
			got, err := soakParseDuration(raw)
			if err != nil || got != want {
				t.Fatalf("parse %q = %s, %v; want %s", raw, got, err, want)
			}
		}
		for _, raw := range []string{"", "soon", "0", "-1s"} {
			if _, err := soakParseDuration(raw); err == nil {
				t.Fatalf("parse %q succeeded, want error", raw)
			}
		}
	})

	t.Run("histogram", func(t *testing.T) {
		var h soakHistogram
		for i := 0; i < 100; i++ {
			h.record(10 * time.Millisecond)
		}
		if h.total != 100 {
			t.Fatalf("histogram total = %d, want 100", h.total)
		}
		p50 := h.quantile(0.50)
		if p50 < 10*time.Millisecond || p50 > 12*time.Millisecond {
			t.Fatalf("p50 = %s, want [10ms,12ms] conservative bucket", p50)
		}
		if p95, p99 := h.quantile(0.95), h.quantile(0.99); !(p50 <= p95 && p95 <= p99) {
			t.Fatalf("quantiles not monotonic: p50=%s p95=%s p99=%s", p50, p95, p99)
		}
		if h.mean() != 10*time.Millisecond {
			t.Fatalf("mean = %s, want 10ms", h.mean())
		}

		var other soakHistogram
		for i := 0; i < 50; i++ {
			other.record(100 * time.Millisecond)
		}
		h.merge(&other)
		if h.total != 150 {
			t.Fatalf("merged total = %d, want 150", h.total)
		}
		if p99 := h.quantile(0.99); p99 < 100*time.Millisecond || p99 > 110*time.Millisecond {
			t.Fatalf("merged p99 = %s, want [100ms,110ms]", p99)
		}
	})

	t.Run("atomic-histogram-delta", func(t *testing.T) {
		var h soakAtomicHist
		h.init()
		var cursor soakHistCursor
		for i := 0; i < 100; i++ {
			h.record(10 * time.Millisecond)
		}
		d1 := cursor.delta(&h)
		if d1.total != 100 {
			t.Fatalf("first delta total = %d, want 100", d1.total)
		}
		for i := 0; i < 50; i++ {
			h.record(100 * time.Millisecond)
		}
		d2 := cursor.delta(&h)
		if d2.total != 50 {
			t.Fatalf("second delta total = %d, want 50", d2.total)
		}
		if p99 := d2.quantile(0.99); p99 < 100*time.Millisecond || p99 > 110*time.Millisecond {
			t.Fatalf("second delta p99 = %s, want [100ms,110ms]", p99)
		}
		var full soakHistogram
		h.snapshot(&full)
		if full.total != 150 {
			t.Fatalf("full total = %d, want 150", full.total)
		}
	})

	t.Run("key-value-determinism", func(t *testing.T) {
		key := soakAppendWorkloadKey(nil, 7, 42)
		if string(key) != "soak/workload/w7/42" {
			t.Fatalf("key = %q", key)
		}
		preloadKey := soakAppendPreloadKey(nil, 99)
		if string(preloadKey) != "soak/preload/99" {
			t.Fatalf("preload key = %q", preloadKey)
		}
		v1 := soakValueFor(key, 1024, nil)
		v2 := soakValueFor(key, 1024, make([]byte, 0, 1024))
		if !bytes.Equal(v1, v2) {
			t.Fatalf("deterministic value mismatch")
		}
		if len(v1) != 1024 {
			t.Fatalf("value length = %d, want 1024", len(v1))
		}
		if bytes.Equal(v1, soakValueFor(soakAppendWorkloadKey(nil, 7, 43), 1024, nil)) {
			t.Fatalf("different keys produced the same value")
		}
	})

	t.Run("prom-line", func(t *testing.T) {
		name, value, ok := soakPromLine(`pebble_wal_physical_size_bytes{node="n1",path="a b"} 12345`)
		if !ok || name != "pebble_wal_physical_size_bytes" || value != "12345" {
			t.Fatalf("parsed %q %q %t", name, value, ok)
		}
		name, value, ok = soakPromLine(`metric_without_labels 42`)
		if !ok || name != "metric_without_labels" || value != "42" {
			t.Fatalf("parsed %q %q %t", name, value, ok)
		}
		if _, _, ok := soakPromLine("# HELP metric help"); ok {
			t.Fatalf("comment parsed as a sample")
		}
	})

	t.Run("sanitize-url", func(t *testing.T) {
		if got := soakSanitizeURL("http://user:pass@metrics.example:9100/metrics"); got != "http://metrics.example:9100/metrics" {
			t.Fatalf("sanitized URL = %q", got)
		}
		err := soakSanitizeError(errors.New("Get http://user:pass@metrics.example:9100/metrics: refused"), "http://user:pass@metrics.example:9100/metrics")
		if strings.Contains(err.Error(), "pass") {
			t.Fatalf("sanitized error leaks credentials: %v", err)
		}
	})

	t.Run("metrics-text", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		body := []byte(`
# HELP pebble_wal_physical_size_bytes WAL size.
pebble_wal_physical_size_bytes{node="a"} 1024
pebble_wal_physical_size_bytes{node="b",path="x y"} 2048
pebble_compactions_total 7
pebble_compaction_bytes_written_total 4096
pebble_disk_space_usage_bytes 1e9
`)
		sample := soakParseMetricsText(body, cfg)
		if sample.WALBytes == nil || *sample.WALBytes != 3072 {
			t.Fatalf("wal bytes = %v, want 3072", sample.WALBytes)
		}
		if sample.CompactionCount == nil || *sample.CompactionCount != 7 {
			t.Fatalf("compactions = %v, want 7", sample.CompactionCount)
		}
		if sample.CompactionBytes == nil || *sample.CompactionBytes != 4096 {
			t.Fatalf("compaction bytes = %v, want 4096", sample.CompactionBytes)
		}
		if sample.DiskBytes == nil || *sample.DiskBytes != 1_000_000_000 {
			t.Fatalf("disk bytes = %v, want 1e9", sample.DiskBytes)
		}

		cfg.WALMetric = "custom_wal_bytes"
		sample = soakParseMetricsText([]byte("custom_wal_bytes 77\npebble_wal_physical_size_bytes 99\n"), cfg)
		if sample.WALBytes == nil || *sample.WALBytes != 77 {
			t.Fatalf("explicit metric = %v, want 77", sample.WALBytes)
		}
	})

	t.Run("memory-now", func(t *testing.T) {
		sample := soakMemoryNow()
		if sample.heapInuse == 0 {
			t.Fatalf("heap in use = 0")
		}
		if sample.goroutines < 1 {
			t.Fatalf("goroutines = %d", sample.goroutines)
		}
		m := sample.json()
		if m.HeapInuseBytes <= 0 {
			t.Fatalf("json heap in use = %d", m.HeapInuseBytes)
		}
	})

	t.Run("drift", func(t *testing.T) {
		buckets := []soakBucketJSON{
			{Index: 0, ElapsedSeconds: 60, Ops: 10, Reads: 8, Writes: 2, Latency: soakLatencyJSON{P99MS: 10}, ReadLatency: soakLatencyJSON{P99MS: 12}, WriteLatency: soakLatencyJSON{P99MS: 20}},
			{Index: 1, ElapsedSeconds: 120, Ops: 10, Reads: 8, Writes: 2, Latency: soakLatencyJSON{P99MS: 20}, ReadLatency: soakLatencyJSON{P99MS: 22}, WriteLatency: soakLatencyJSON{P99MS: 30}},
			{Index: 2, ElapsedSeconds: 180, Ops: 10, Reads: 8, Writes: 2, Latency: soakLatencyJSON{P99MS: 30}, ReadLatency: soakLatencyJSON{P99MS: 32}, WriteLatency: soakLatencyJSON{P99MS: 40}},
		}
		drift := soakBuildDrift(buckets)
		if drift.P99.Points != 3 || !drift.P99.SlopeEvaluated {
			t.Fatalf("p99 drift = %+v", drift.P99)
		}
		if drift.P99.MaxMS != 30 || drift.P99.MinMS != 10 {
			t.Fatalf("p99 min/max = %g/%g", drift.P99.MinMS, drift.P99.MaxMS)
		}
		if drift.P99.SlopeMSPerHour <= 0 {
			t.Fatalf("p99 slope = %g, want positive", drift.P99.SlopeMSPerHour)
		}
	})

	t.Run("memory-summary", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cfg.Duration = 10 * time.Hour
		// 100 MiB at t=0, growing 10 MiB/h. The first hour is warmup.
		buckets := make([]soakBucketJSON, 0, 8)
		for i := 0; i <= 8; i++ {
			hours := float64(i) * 0.5
			buckets = append(buckets, soakBucketJSON{
				Index:          i,
				ElapsedSeconds: hours * 3600,
				Memory: soakMemoryJSON{
					HeapInuseBytes: int64(100<<20) + int64(hours*10*float64(1<<20)),
					NumGoroutine:   20 + i,
					RSSBytes:       soakInt64Ptr(int64(200<<20) + int64(hours*20*float64(1<<20))),
				},
			})
		}
		summary := soakBuildMemorySummary(cfg, buckets)
		if summary.Samples != 9 {
			t.Fatalf("samples = %d, want 9", summary.Samples)
		}
		if summary.WarmupSeconds != 3600 {
			t.Fatalf("warmup = %g, want 3600", summary.WarmupSeconds)
		}
		if !summary.HeapInuseSlopeEvaluated {
			t.Fatalf("heap slope not evaluated")
		}
		if math.Abs(summary.HeapInuseSlopeMiBPerHour-10) > 0.5 {
			t.Fatalf("heap slope = %g, want ~10", summary.HeapInuseSlopeMiBPerHour)
		}
		if !summary.RSSAvailable || math.Abs(summary.RSSSlopeMiBPerHour-20) > 0.5 {
			t.Fatalf("rss summary = %+v", summary)
		}
		if summary.GoroutinesStart != 20 || summary.GoroutinesEnd != 28 {
			t.Fatalf("goroutines = %d..%d", summary.GoroutinesStart, summary.GoroutinesEnd)
		}
	})

	t.Run("storage-summary", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cfg.MetricsURLs = []string{"http://localhost:9100/metrics"}
		cfg.WALMetric = "wal_size_bytes"
		buckets := make([]soakBucketJSON, 0, 6)
		for i := 0; i < 6; i++ {
			seconds := float64(i) * 600
			wal := int64(1<<20) + int64(seconds/3600*10*float64(1<<20))
			disk := int64(1<<30) + int64(seconds/3600*20*float64(1<<20))
			keys := int64(1000 + i*100)
			comp := int64(i * 5)
			compBytes := int64(i * 1000)
			buckets = append(buckets, soakBucketJSON{
				Index:          i,
				ElapsedSeconds: seconds,
				Storage: soakStorageSampleJSON{
					DiskBytes:            &disk,
					DiskKeys:             &keys,
					WALBytes:             &wal,
					CompactionsTotal:     &comp,
					CompactionBytesTotal: &compBytes,
				},
			})
		}
		storage := soakBuildStorageSummary(cfg, buckets, soakStorageProbeStats{RangesTotal: 3, RangesProbed: 3})
		if !storage.DiskAvailable || !storage.WALAvailable || !storage.CompactionAvailable {
			t.Fatalf("availability = %+v", storage)
		}
		if math.Abs(storage.WALGrowthMiBPerHour-10) > 0.5 {
			t.Fatalf("wal slope = %g, want ~10", storage.WALGrowthMiBPerHour)
		}
		if math.Abs(storage.DiskGrowthMiBPerHour-20) > 0.5 {
			t.Fatalf("disk slope = %g, want ~20", storage.DiskGrowthMiBPerHour)
		}
		if storage.CompactionsDelta != 25 {
			t.Fatalf("compactions delta = %d, want 25", storage.CompactionsDelta)
		}
		if storage.CompactionBytesDelta != 5000 {
			t.Fatalf("compaction bytes delta = %d, want 5000", storage.CompactionBytesDelta)
		}
		if storage.WALMetric != "wal_size_bytes" {
			t.Fatalf("wal metric = %q", storage.WALMetric)
		}
	})

	t.Run("gates", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		workload := soakSelfWorkload(1000, 10*time.Millisecond)
		memory := soakMemorySummaryJSON{HeapInuseSlopeEvaluated: true, HeapInuseSlopeMiBPerHour: 1}
		storage := soakStorageJSON{WALGrowthEvaluated: true, WALGrowthMiBPerHour: 1, WALMetric: "auto"}

		g := soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if len(g.Failures) != 0 {
			t.Fatalf("baseline failures = %v", g.Failures)
		}
		if !g.P99.Passed || g.P99.Evaluated || !g.ErrorRate.Evaluated || !g.Integrity.Passed {
			t.Fatalf("baseline gates = %+v", g)
		}

		cfg.P99GateMS = 1
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if g.P99.Passed || len(g.Failures) == 0 {
			t.Fatalf("p99 gate should fail: %+v", g.Failures)
		}

		cfg.P99GateMS = 0
		cfg.MaxErrorRate = 0.0001
		workload.Errors = 10
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if g.ErrorRate.Passed || len(g.Failures) == 0 {
			t.Fatalf("error gate should fail: %+v", g.Failures)
		}

		cfg.MaxErrorRate = 0.01
		workload.Errors = 0
		memory.HeapInuseSlopeMiBPerHour = 10
		cfg.MemoryGrowthMaxMiBPerHour = 5
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if g.MemoryGrowth.Passed || len(g.Failures) == 0 {
			t.Fatalf("memory gate should fail: %+v", g.Failures)
		}

		memory.HeapInuseSlopeMiBPerHour = 2
		storage.WALGrowthEvaluated = false
		cfg.WALGrowthMaxMiBPerHour = 10
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if g.WALGrowth.Passed || len(g.Failures) == 0 {
			t.Fatalf("missing WAL metric should fail a configured WAL gate: %+v", g.Failures)
		}

		storage.WALGrowthEvaluated = true
		storage.WALGrowthMiBPerHour = 2
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		if !g.MemoryGrowth.Passed || !g.WALGrowth.Passed || len(g.Failures) != 0 {
			t.Fatalf("growth gates should pass: %+v", g.Failures)
		}

		verify := soakVerifyResult{Checked: 1, Failed: 1}
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, verify: verify, memory: memory, storage: storage})
		if g.Integrity.Passed || len(g.Failures) == 0 {
			t.Fatalf("integrity gate should fail: %+v", g.Failures)
		}

		empty := soakWorkloadResult{}
		g = soakEvaluateGates(soakGateInput{cfg: cfg, workload: empty, memory: memory, storage: storage})
		if len(g.Failures) == 0 {
			t.Fatalf("zero operations should fail")
		}
	})

	t.Run("report-json", func(t *testing.T) {
		cfg, err := soakConfigFrom(mapGet(soakSelfEnv()))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		workload := soakSelfWorkload(100, 5*time.Millisecond)
		workload.Duration = 60 * time.Second
		memory := soakMemorySummaryJSON{Samples: 2, HeapInuseSlopeEvaluated: true}
		storage := soakStorageJSON{WALMetric: "auto"}
		gates := soakEvaluateGates(soakGateInput{cfg: cfg, workload: workload, memory: memory, storage: storage})
		summary := soakBuildSummary(soakSummaryInput{
			cfg:      cfg,
			host:     "self",
			workload: workload,
			buckets:  []soakBucketJSON{{Index: 0, ElapsedSeconds: 60, Ops: 100}},
			drift:    soakDriftJSON{Buckets: 1},
			memory:   memory,
			storage:  storage,
			cluster:  soakClusterJSON{NodesTotal: 3},
			gates:    gates,
		})
		if summary.Harness != soakHarness {
			t.Fatalf("harness = %q", summary.Harness)
		}
		data, err := json.Marshal(summary)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, want := range []string{`"harness":"soak-72h-v1"`, `"buckets"`, `"gates"`, `"latency_drift"`} {
			if !bytes.Contains(data, []byte(want)) {
				t.Fatalf("report JSON missing %s", want)
			}
		}
		path := filepath.Join(t.TempDir(), "reports", "soak.json")
		if err := soakWriteReport(path, summary); err != nil {
			t.Fatalf("write report: %v", err)
		}
		var decoded map[string]any
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read report: %v", err)
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal report: %v", err)
		}
		if decoded["harness"] != soakHarness {
			t.Fatalf("decoded harness = %v", decoded["harness"])
		}
	})
}

func soakSelfEnv() map[string]string {
	return map[string]string{
		"SOAK_META":  "meta.example:9090",
		"SOAK_NODES": "node1.example:7001,node2.example:7001,node3.example:7001",
	}
}

func mapGet(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func soakSelfWorkload(ops int, d time.Duration) soakWorkloadResult {
	var result soakWorkloadResult
	for i := 0; i < ops; i++ {
		result.All.record(d)
		if i%5 == 0 {
			result.Writes++
			result.Write.record(d)
		} else {
			result.Reads++
			result.Read.record(d)
		}
	}
	result.Ops = result.Reads + result.Writes
	result.Duration = time.Minute
	// Treat the synthetic run as having performed one inline verification so
	// the always-on integrity gate is evaluated in the self check.
	result.VerifyChecked = 1
	return result
}
