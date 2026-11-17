package test

// Multi-host SLO harness for an EXTERNAL cluster.
//
// Everything in this file talks to a cluster that is already running: it never
// starts a meta service or a KV node. The harness is gated by SLO_RUN=1 and
// skips otherwise, so it cannot run as part of the normal test suite or CI.
//
// The workload is a closed-loop mix of single-key Begin/Get transactions and
// Begin/Set/Commit transactions. Every worker owns a disjoint key partition,
// so keys stay unique per worker without coordination on the hot path. A
// global semaphore bounds the number of in-flight operations.
//
// Reported metrics:
//   - ops, reads, writes, errors and error rate;
//   - aggregate/read/write throughput and MiB/s;
//   - p50/p95/p99 latency from a bounded, fixed-array log-linear histogram
//     (reported quantiles are conservative bucket upper bounds);
//   - read verification: every read checks the value length, a configurable
//     sample also checks the full deterministic payload;
//   - a post-run sample that re-reads workload keys and (when preloaded)
//     spread-out dataset keys;
//   - optional preload throughput when SLO_PRELOAD_GB is set;
//   - a machine-readable JSON report when SLO_REPORT_JSON is set.
//
// Environment:
//
//	SLO_RUN=1                required gate; any other value skips
//	SLO_META                 required meta/TSO gRPC host:port
//	SLO_NODES                required comma-separated KV gRPC host:port list
//	SLO_CLIENTS              worker goroutines (default 8, max 1024)
//	SLO_MAX_INFLIGHT         global in-flight operation cap (default clients)
//	SLO_DURATION             measured duration, Go duration or seconds (default 60s)
//	SLO_VALUE_SIZE           value payload bytes (default 1024, max 16 MiB)
//	SLO_KEYSPACE             total logical key budget, partitioned by worker
//	                         (default 1000000, must be >= SLO_CLIENTS)
//	SLO_READ_RATIO           fraction of reads in the mix, 0..1 (default 0.8)
//	SLO_OP_TIMEOUT           per-operation timeout (default 10s)
//	SLO_SEED                 optional uint64 seed; 0/empty derives one from clock
//	SLO_VERIFY_RATE          fraction of reads fully verified, 0..1 (default 0.01)
//	SLO_VERIFY_SAMPLE        post-run verification reads (default 32, max 10000)
//	SLO_PRELOAD_GB           optional value-payload target in GiB (default 0)
//	SLO_PRELOAD_WORKERS      concurrent preload transactions (default 4)
//	SLO_P95_GATE_MS          optional p95 gate in ms for all/read/write (0 = off)
//	SLO_P99_GATE_MS          optional p99 gate in ms for all/read/write (0 = off)
//	SLO_MAX_ERROR_RATE       allowed workload error fraction (default 0)
//	SLO_MIN_THROUGHPUT       minimum aggregate ops/sec (default 0 = off)
//	SLO_REPORT_JSON          optional path for the JSON report
//
// SLO_PRELOAD_GB counts value payload only. Key bytes, transaction metadata,
// Raft log, WAL/SST and replication overhead are additional. The preload
// streams bounded batches and never holds the dataset in RAM: keys are derived
// from their ordinal and values are generated deterministically from the key.
// Preloaded keys live under "slo/preload/" and are not deleted by this harness.
//
// This file is not a substitute for a production SLO run: it measures one
// client host against an already-deployed cluster. No 1 TB result is claimed
// by this harness or its runbook.
//
// Typical use:
//
//	SLO_RUN=1 SLO_META=10.0.0.10:9090 \
//	SLO_NODES=10.0.0.11:7001,10.0.0.12:7001,10.0.0.13:7001 \
//	SLO_CLIENTS=16 SLO_DURATION=10m SLO_P99_GATE_MS=50 \
//	go test -run '^TestSLOMultiHost$' -v -timeout 24h ./test
//
// See docs/SLO-RUNBOOK.md and scripts/bench-multihost.sh.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/v2client"
)

const (
	sloEnvRun = "SLO_RUN"

	sloGiB = int64(1) << 30

	sloDefaultClients        = 8
	sloDefaultDuration       = 60 * time.Second
	sloDefaultValueSize      = 1024
	sloDefaultKeyspace       = 1_000_000
	sloDefaultReadRatio      = 0.8
	sloDefaultOpTimeout      = 10 * time.Second
	sloDefaultVerifyRate     = 0.01
	sloDefaultVerifySample   = 32
	sloDefaultPreloadWorkers = 4

	sloMaxClients           = 1024
	sloMaxInflight          = 8192
	sloMaxValueSize         = 16 << 20
	sloMaxKeyspace          = 1 << 40
	sloMaxVerifySample      = 10_000
	sloMaxPreloadGB         = 1 << 20
	sloMaxPreloadWorkers    = 64
	sloMaxGateMilliseconds  = 86_400_000 // one day
	sloMaxThroughputGate    = 1e12
	sloPreloadBatchBytes    = 4 << 20
	sloPreloadMaxBatchKeys  = 2048
	sloPreloadCommitTimeout = 2 * time.Minute

	// Bounded log-linear histogram: values below 16ns use linear buckets;
	// larger values use 8 sub-buckets per power of two. Each worker owns its
	// own histogram (512 uint64 counters) so the hot path has no atomics.
	sloHistBuckets = 512
	sloHistLinear  = 16
)

const (
	sloWarmupKey        = "slo/warmup/0"
	sloWorkloadKeyStart = "slo/workload/w"
	sloPreloadKeyStart  = "slo/preload/"
)

// sloConfig is the fully validated harness configuration.
type sloConfig struct {
	Meta           string
	Nodes          []string
	Clients        int
	Duration       time.Duration
	ValueSize      int
	Keyspace       uint64
	PerWorkerKeys  uint64
	ReadRatio      float64
	MaxInflight    int
	OpTimeout      time.Duration
	Seed           uint64
	VerifyRate     float64
	VerifySample   int
	PreloadGB      float64
	PreloadBytes   int64
	PreloadKeys    int64
	PreloadWorkers int
	MaxErrorRate   float64
	MinThroughput  float64
	P95GateMS      float64
	P99GateMS      float64
	ReportPath     string
}

// sloConfigFromEnv reads SLO_* from the process environment and fails the test
// with a precise message when a value is invalid.
func sloConfigFromEnv(tb testing.TB) sloConfig {
	tb.Helper()
	cfg, err := sloConfigFrom(os.Getenv)
	if err != nil {
		tb.Fatalf("SLO config: %v", err)
	}
	return cfg
}

// sloConfigFrom builds and validates the configuration from getenv. It is pure
// with respect to the process environment so TestSLOSelfCheck can exercise it.
func sloConfigFrom(getenv func(string) string) (sloConfig, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := sloConfig{
		Clients:        sloDefaultClients,
		Duration:       sloDefaultDuration,
		ValueSize:      sloDefaultValueSize,
		Keyspace:       sloDefaultKeyspace,
		ReadRatio:      sloDefaultReadRatio,
		OpTimeout:      sloDefaultOpTimeout,
		VerifyRate:     sloDefaultVerifyRate,
		VerifySample:   sloDefaultVerifySample,
		PreloadWorkers: sloDefaultPreloadWorkers,
	}

	cfg.Meta = get("SLO_META")
	if cfg.Meta == "" {
		return cfg, fmt.Errorf("SLO_META is required (meta/TSO gRPC host:port)")
	}
	rawNodes := get("SLO_NODES")
	if rawNodes == "" {
		return cfg, fmt.Errorf("SLO_NODES is required (comma-separated KV gRPC host:port list)")
	}
	nodes, err := sloParseNodes(rawNodes)
	if err != nil {
		return cfg, fmt.Errorf("SLO_NODES: %w", err)
	}
	cfg.Nodes = nodes

	if raw := get("SLO_CLIENTS"); raw != "" {
		cfg.Clients, err = sloParseIntRange(raw, 1, sloMaxClients)
		if err != nil {
			return cfg, fmt.Errorf("SLO_CLIENTS: %w", err)
		}
	}
	if raw := get("SLO_MAX_INFLIGHT"); raw != "" {
		cfg.MaxInflight, err = sloParseIntRange(raw, 1, sloMaxInflight)
		if err != nil {
			return cfg, fmt.Errorf("SLO_MAX_INFLIGHT: %w", err)
		}
	} else {
		// Default to the worker count: bounded in-flight by construction.
		cfg.MaxInflight = cfg.Clients
	}

	if raw := get("SLO_DURATION"); raw != "" {
		cfg.Duration, err = sloParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SLO_DURATION: %w", err)
		}
		if cfg.Duration < time.Second {
			return cfg, fmt.Errorf("SLO_DURATION must be at least 1s (got %s)", cfg.Duration)
		}
	}

	if raw := get("SLO_VALUE_SIZE"); raw != "" {
		cfg.ValueSize, err = sloParseIntRange(raw, 1, sloMaxValueSize)
		if err != nil {
			return cfg, fmt.Errorf("SLO_VALUE_SIZE: %w", err)
		}
	}

	if raw := get("SLO_KEYSPACE"); raw != "" {
		cfg.Keyspace, err = sloParseUintRange(raw, 1, sloMaxKeyspace)
		if err != nil {
			return cfg, fmt.Errorf("SLO_KEYSPACE: %w", err)
		}
	}
	if cfg.Keyspace < uint64(cfg.Clients) {
		return cfg, fmt.Errorf("SLO_KEYSPACE=%d must be >= SLO_CLIENTS=%d", cfg.Keyspace, cfg.Clients)
	}
	cfg.PerWorkerKeys = cfg.Keyspace / uint64(cfg.Clients)

	if raw := get("SLO_READ_RATIO"); raw != "" {
		cfg.ReadRatio, err = sloParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SLO_READ_RATIO: %w", err)
		}
	}

	if raw := get("SLO_OP_TIMEOUT"); raw != "" {
		cfg.OpTimeout, err = sloParseDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("SLO_OP_TIMEOUT: %w", err)
		}
		if cfg.OpTimeout < time.Millisecond {
			return cfg, fmt.Errorf("SLO_OP_TIMEOUT must be at least 1ms (got %s)", cfg.OpTimeout)
		}
	}

	if raw := get("SLO_SEED"); raw != "" {
		cfg.Seed, err = sloParseUintRange(raw, 0, math.MaxUint64)
		if err != nil {
			return cfg, fmt.Errorf("SLO_SEED: %w", err)
		}
	}

	if raw := get("SLO_VERIFY_RATE"); raw != "" {
		cfg.VerifyRate, err = sloParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SLO_VERIFY_RATE: %w", err)
		}
	}
	if raw := get("SLO_VERIFY_SAMPLE"); raw != "" {
		cfg.VerifySample, err = sloParseIntRange(raw, 0, sloMaxVerifySample)
		if err != nil {
			return cfg, fmt.Errorf("SLO_VERIFY_SAMPLE: %w", err)
		}
	}

	if raw := get("SLO_PRELOAD_GB"); raw != "" {
		cfg.PreloadGB, err = sloParseFloatRange(raw, 0, sloMaxPreloadGB)
		if err != nil {
			return cfg, fmt.Errorf("SLO_PRELOAD_GB: %w", err)
		}
	}
	if cfg.PreloadGB > 0 {
		bytesFloat := cfg.PreloadGB * float64(sloGiB)
		if bytesFloat > float64(math.MaxInt64) {
			return cfg, fmt.Errorf("SLO_PRELOAD_GB=%g is too large", cfg.PreloadGB)
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
	if raw := get("SLO_PRELOAD_WORKERS"); raw != "" {
		cfg.PreloadWorkers, err = sloParseIntRange(raw, 1, sloMaxPreloadWorkers)
		if err != nil {
			return cfg, fmt.Errorf("SLO_PRELOAD_WORKERS: %w", err)
		}
	}
	if cfg.PreloadKeys > 0 && int64(cfg.PreloadWorkers) > cfg.PreloadKeys {
		cfg.PreloadWorkers = int(cfg.PreloadKeys)
	}

	if raw := get("SLO_MAX_ERROR_RATE"); raw != "" {
		cfg.MaxErrorRate, err = sloParseFloatRange(raw, 0, 1)
		if err != nil {
			return cfg, fmt.Errorf("SLO_MAX_ERROR_RATE: %w", err)
		}
	}
	if raw := get("SLO_MIN_THROUGHPUT"); raw != "" {
		cfg.MinThroughput, err = sloParseFloatRange(raw, 0, sloMaxThroughputGate)
		if err != nil {
			return cfg, fmt.Errorf("SLO_MIN_THROUGHPUT: %w", err)
		}
	}
	if raw := get("SLO_P95_GATE_MS"); raw != "" {
		cfg.P95GateMS, err = sloParseFloatRange(raw, 0, sloMaxGateMilliseconds)
		if err != nil {
			return cfg, fmt.Errorf("SLO_P95_GATE_MS: %w", err)
		}
	}
	if raw := get("SLO_P99_GATE_MS"); raw != "" {
		cfg.P99GateMS, err = sloParseFloatRange(raw, 0, sloMaxGateMilliseconds)
		if err != nil {
			return cfg, fmt.Errorf("SLO_P99_GATE_MS: %w", err)
		}
	}
	cfg.ReportPath = get("SLO_REPORT_JSON")

	return cfg, nil
}

func sloParseNodes(raw string) ([]string, error) {
	nodes := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
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

func sloParseDuration(raw string) (time.Duration, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", raw)
		}
		return d, nil
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs <= 0 {
		return 0, fmt.Errorf("want a positive Go duration (for example 30s or 5m) or seconds, got %q", raw)
	}
	d := time.Duration(secs * float64(time.Second))
	if d <= 0 {
		return 0, fmt.Errorf("duration %q is too small", raw)
	}
	return d, nil
}

func sloParseIntRange(raw string, min, max int) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("want an integer in [%d,%d], got %q", min, max, raw)
	}
	return v, nil
}

func sloParseUintRange(raw string, min, max uint64) (uint64, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("want an integer in [%d,%d], got %q", min, max, raw)
	}
	return v, nil
}

func sloParseFloatRange(raw string, min, max float64) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
		return 0, fmt.Errorf("want a number in [%g,%g], got %q", min, max, raw)
	}
	return v, nil
}

// TestSLOMultiHost is the external-cluster SLO run. It skips unless SLO_RUN=1
// and refuses to run under -short so a normal `go test -short ./test` stays
// cheap.
func TestSLOMultiHost(t *testing.T) {
	if os.Getenv(sloEnvRun) != "1" {
		t.Skip("multi-host SLO harness disabled: set SLO_RUN=1, SLO_META and SLO_NODES (see docs/SLO-RUNBOOK.md)")
	}
	if testing.Short() {
		t.Skip("multi-host SLO harness skipped in -short")
	}

	cfg := sloConfigFromEnv(t)
	if cfg.Seed == 0 {
		cfg.Seed = uint64(time.Now().UnixNano())
	}
	host, _ := os.Hostname()

	client, err := v2client.New(cfg.Meta, cfg.Nodes)
	if err != nil {
		t.Fatalf("SLO: create client: %v", err)
	}
	defer func() { _ = client.Close() }()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 30*time.Second)
	ranges, err := client.ListRanges(pingCtx)
	cancelPing()
	if err != nil {
		t.Fatalf("SLO: cannot reach meta %s: %v", cfg.Meta, err)
	}
	if len(ranges) == 0 {
		t.Fatalf("SLO: meta %s returned no ranges; is the cluster initialized?", cfg.Meta)
	}

	sloLogConfig(t, cfg, host, len(ranges))
	sloWarmup(t, client, cfg)

	var preload sloPreloadStats
	if cfg.PreloadBytes > 0 {
		preload = sloPreload(t, client, cfg)
	}

	workers := sloNewWorkers(cfg)
	sloSeedWorkers(t, client, cfg, workers)
	workload := sloRunWorkload(t, client, cfg, workers)
	verify := sloVerifySamples(t, client, cfg, workers)
	passed, failures := sloEvaluateGates(cfg, workload, verify)
	summary := sloBuildSummary(cfg, host, preload, workload, verify, passed, failures)
	sloLogSummary(t, summary)
	if cfg.ReportPath != "" {
		if err := sloWriteReport(cfg.ReportPath, summary); err != nil {
			t.Errorf("SLO: write report %s: %v", cfg.ReportPath, err)
		} else {
			t.Logf("SLO report: %s", cfg.ReportPath)
		}
	}
	for _, failure := range failures {
		t.Errorf("SLO gate failed: %s", failure)
	}
}

func sloLogConfig(t *testing.T, cfg sloConfig, host string, ranges int) {
	t.Helper()
	t.Logf("SLO config: host=%s meta=%s nodes=%d ranges=%d clients=%d inflight=%d duration=%s value_size=%d keyspace=%d per_worker_keys=%d read_ratio=%.3f op_timeout=%s seed=%d verify_rate=%.4f verify_sample=%d preload_gb=%.3f preload_keys=%d p95_gate_ms=%.1f p99_gate_ms=%.1f max_error_rate=%.6f min_throughput=%.1f",
		host, cfg.Meta, len(cfg.Nodes), ranges, cfg.Clients, cfg.MaxInflight, cfg.Duration,
		cfg.ValueSize, cfg.Keyspace, cfg.PerWorkerKeys, cfg.ReadRatio, cfg.OpTimeout, cfg.Seed,
		cfg.VerifyRate, cfg.VerifySample, cfg.PreloadGB, cfg.PreloadKeys,
		cfg.P95GateMS, cfg.P99GateMS, cfg.MaxErrorRate, cfg.MinThroughput)
}

// sloWarmup writes and reads one key outside the measured region. It proves
// that both the meta and at least one KV route are reachable before a long run
// starts, and it surfaces an empty/misconfigured keyspace early.
func sloWarmup(t *testing.T, client *v2client.Client, cfg sloConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
	defer cancel()

	key := []byte(sloWarmupKey)
	value := sloValueFor(key, cfg.ValueSize, nil)
	txn, err := client.Begin(ctx)
	if err != nil {
		t.Fatalf("SLO warmup: begin write: %v", err)
	}
	txn.Set(key, value)
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("SLO warmup: commit write: %v", err)
	}

	txn, err = client.Begin(ctx)
	if err != nil {
		t.Fatalf("SLO warmup: begin read: %v", err)
	}
	got, err := txn.Get(ctx, key)
	if err != nil {
		t.Fatalf("SLO warmup: read back: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("SLO warmup: read back %d bytes, want %d deterministic bytes", len(got), len(value))
	}
}

// sloPreloadStats summarizes the optional dataset preload.
type sloPreloadStats struct {
	Keys     int64
	Txns     int64
	Bytes    int64
	Duration time.Duration
}

// sloPreloadBatchKeys bounds one preload transaction by both key count and
// approximate value bytes.
func sloPreloadBatchKeys(valueSize int) int {
	n := sloPreloadBatchBytes / valueSize
	if n < 1 {
		n = 1
	}
	if n > sloPreloadMaxBatchKeys {
		n = sloPreloadMaxBatchKeys
	}
	return n
}

// sloPreload streams SLO_PRELOAD_GB worth of value payload in bounded batches.
// Keys are derived from their ordinal and values are deterministic functions
// of the key, so the dataset is never materialized in memory and a later
// sample can verify a value without keeping it around.
func sloPreload(t *testing.T, client *v2client.Client, cfg sloConfig) sloPreloadStats {
	t.Helper()
	if cfg.PreloadKeys < 1 {
		return sloPreloadStats{}
	}
	t.Logf("SLO preload: starting target_gb=%.3f target_bytes=%d keys=%d value_size=%d workers=%d",
		cfg.PreloadGB, cfg.PreloadBytes, cfg.PreloadKeys, cfg.ValueSize, cfg.PreloadWorkers)

	batchKeys := sloPreloadBatchKeys(cfg.ValueSize)
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
			valueBuf := make([]byte, cfg.ValueSize)
			for ctx.Err() == nil {
				start := next.Add(uint64(batchKeys)) - uint64(batchKeys)
				if start >= uint64(cfg.PreloadKeys) {
					return
				}
				n := uint64(batchKeys)
				if remaining := uint64(cfg.PreloadKeys) - start; n > remaining {
					n = remaining
				}
				beginCtx, cancelBegin := context.WithTimeout(ctx, sloPreloadCommitTimeout)
				txn, err := client.Begin(beginCtx)
				cancelBegin()
				if err != nil {
					fail(fmt.Errorf("worker %d begin at key %d: %w", worker, start, err))
					return
				}
				for j := uint64(0); j < n; j++ {
					key := sloAppendPreloadKey(keyBuf, start+j)
					txn.Set(key, sloValueFor(key, cfg.ValueSize, valueBuf))
				}
				commitCtx, cancelCommit := context.WithTimeout(context.Background(), sloPreloadCommitTimeout)
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
		ticker := time.NewTicker(10 * time.Second)
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
				t.Logf("SLO preload: keys=%d/%d (%.1f%%) elapsed=%s rate=%.1f MiB/s",
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
		t.Fatalf("SLO preload failed after %d/%d keys: %v", doneKeys.Load(), cfg.PreloadKeys, err)
	}
	keys := doneKeys.Load()
	if keys != cfg.PreloadKeys {
		t.Fatalf("SLO preload wrote %d keys, want %d", keys, cfg.PreloadKeys)
	}
	bytesWritten := keys * int64(cfg.ValueSize)
	rate := 0.0
	if elapsed > 0 {
		rate = float64(bytesWritten) / elapsed.Seconds() / float64(1<<20)
	}
	t.Logf("SLO preload: complete keys=%d bytes=%d (%.3f GiB) txns=%d elapsed=%s rate=%.1f MiB/s",
		keys, bytesWritten, float64(bytesWritten)/float64(sloGiB), doneTxns.Load(),
		elapsed.Round(time.Millisecond), rate)
	return sloPreloadStats{Keys: keys, Txns: doneTxns.Load(), Bytes: bytesWritten, Duration: elapsed}
}

// sloWorkerState is one client goroutine. All mutable fields are touched by a
// single goroutine; the measured histograms are merged after the run.
type sloWorkerState struct {
	id    int
	start uint64
	span  uint64

	writeSeq    uint64
	initialized uint64

	rng      *rand.Rand
	keyBuf   []byte
	valueBuf []byte

	res sloWorkerResult
}

type sloWorkerResult struct {
	reads         int64
	writes        int64
	errors        int64
	verifyChecked int64
	verifyFailed  int64
	bytesRead     int64
	bytesWritten  int64
	firstErr      string
	allHist       sloHistogram
	readHist      sloHistogram
	writeHist     sloHistogram
}

func sloNewWorkers(cfg sloConfig) []*sloWorkerState {
	workers := make([]*sloWorkerState, cfg.Clients)
	for i := range workers {
		// A per-worker seed keeps the mixes independent while still being
		// reproducible from SLO_SEED.
		seed := cfg.Seed + uint64(i+1)*0x9e3779b97f4a7c15
		workers[i] = &sloWorkerState{
			id:       i,
			start:    uint64(i) * cfg.PerWorkerKeys,
			span:     cfg.PerWorkerKeys,
			rng:      rand.New(rand.NewSource(int64(seed))),
			keyBuf:   make([]byte, 0, 64),
			valueBuf: nil, // allocated on first write/verification
		}
	}
	return workers
}

// sloSeedWorkers prepares one key per worker outside the measured region. This
// guarantees that a read-only mix has data to read and that post-run
// verification always has at least one key per worker to sample.
func sloSeedWorkers(t *testing.T, client *v2client.Client, cfg sloConfig, workers []*sloWorkerState) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, len(workers))
	for _, w := range workers {
		wg.Add(1)
		go func(w *sloWorkerState) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
			defer cancel()
			key := sloAppendWorkloadKey(w.keyBuf, w.id, w.start)
			w.valueBuf = sloValueFor(key, cfg.ValueSize, w.valueBuf)
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
		t.Fatalf("SLO warmup: %v", err)
	}
}

// sloWorkerOp performs exactly one read or write transaction and returns its
// contribution to the metrics. A read on a worker that has no initialized key
// falls back to a write; sloSeedWorkers makes that a safety net rather than
// the normal path.
func sloWorkerOp(client *v2client.Client, cfg sloConfig, w *sloWorkerState) (isRead bool, nbytes int, elapsed time.Duration, verifyChecked, verifyFailed bool, opErr error) {
	started := time.Now()
	isRead = w.rng.Float64() < cfg.ReadRatio
	if w.initialized == 0 {
		isRead = false
	}

	opCtx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
	defer cancel()

	if isRead {
		idx := w.rng.Uint64() % w.initialized
		key := sloAppendWorkloadKey(w.keyBuf, w.id, w.start+idx)
		txn, err := client.Begin(opCtx)
		if err != nil {
			opErr = err
		} else {
			got, err := txn.Get(opCtx, key)
			switch {
			case err != nil:
				opErr = err
				if errors.Is(err, v2client.ErrNotFound) {
					// Every read targets a key this worker committed. A miss
					// is data loss, not an ordinary request error.
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
					w.valueBuf = sloValueFor(key, cfg.ValueSize, w.valueBuf)
					if !bytes.Equal(got, w.valueBuf) {
						verifyFailed = true
						opErr = fmt.Errorf("read key %q: deterministic value mismatch", key)
					}
				}
			}
		}
	} else {
		idx := w.writeSeq % w.span
		key := sloAppendWorkloadKey(w.keyBuf, w.id, w.start+idx)
		w.valueBuf = sloValueFor(key, cfg.ValueSize, w.valueBuf)
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

// sloWorkloadResult is the merged outcome of the measured phase.
type sloWorkloadResult struct {
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
	All           sloHistogram
	Read          sloHistogram
	Write         sloHistogram
}

// sloRunWorkload runs the closed-loop mixed workload for SLO_DURATION. In-flight
// operations are capped by a global semaphore; each worker is synchronous, so
// each permit corresponds to exactly one request in flight.
func sloRunWorkload(t *testing.T, client *v2client.Client, cfg sloConfig, workers []*sloWorkerState) sloWorkloadResult {
	t.Helper()
	sem := make(chan struct{}, cfg.MaxInflight)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopTimer := time.AfterFunc(cfg.Duration, cancel)
	defer stopTimer.Stop()

	started := time.Now()
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *sloWorkerState) {
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
				isRead, nbytes, elapsed, checked, failed, err := sloWorkerOp(client, cfg, w)
				<-sem

				w.res.allHist.record(elapsed)
				if isRead {
					w.res.reads++
					w.res.readHist.record(elapsed)
					w.res.bytesRead += int64(nbytes)
				} else {
					w.res.writes++
					w.res.writeHist.record(elapsed)
					w.res.bytesWritten += int64(nbytes)
				}
				if checked {
					w.res.verifyChecked++
				}
				if failed {
					w.res.verifyFailed++
				}
				if err != nil {
					w.res.errors++
					if w.res.firstErr == "" {
						w.res.firstErr = err.Error()
					}
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(started)

	var result sloWorkloadResult
	result.Duration = elapsed
	for _, w := range workers {
		result.Reads += w.res.reads
		result.Writes += w.res.writes
		result.Errors += w.res.errors
		result.VerifyChecked += w.res.verifyChecked
		result.VerifyFailed += w.res.verifyFailed
		result.BytesRead += w.res.bytesRead
		result.BytesWritten += w.res.bytesWritten
		result.All.merge(&w.res.allHist)
		result.Read.merge(&w.res.readHist)
		result.Write.merge(&w.res.writeHist)
		if result.FirstError == "" {
			result.FirstError = w.res.firstErr
		}
	}
	result.Ops = result.Reads + result.Writes
	return result
}

// sloVerifyResult is the post-run verification outcome.
type sloVerifyResult struct {
	Checked    int64
	Failed     int64
	FirstError string
}

// sloVerifySamples re-reads a deterministic spread of workload keys (and, when
// preloaded, dataset keys) and compares the full payload. This is a sampled
// data-integrity check, not a durability or crash-consistency test.
func sloVerifySamples(t *testing.T, client *v2client.Client, cfg sloConfig, workers []*sloWorkerState) sloVerifyResult {
	t.Helper()
	var result sloVerifyResult
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
		want := sloValueFor(key, cfg.ValueSize, valueBuf)
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
			check(sloAppendWorkloadKey(keyBuf, w.id, w.start+idx))
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
			check(sloAppendPreloadKey(keyBuf, uint64(i)*step))
		}
	}
	return result
}

func sloEvaluateGates(cfg sloConfig, workload sloWorkloadResult, verify sloVerifyResult) (bool, []string) {
	failures := make([]string, 0, 8)
	if workload.Ops == 0 {
		failures = append(failures, "no measured operations completed")
	}
	verifyFailed := workload.VerifyFailed + verify.Failed
	if verifyFailed > 0 {
		failures = append(failures, fmt.Sprintf("verification failures=%d (inline=%d post_sample=%d)",
			verifyFailed, workload.VerifyFailed, verify.Failed))
	}
	errorRate := 0.0
	if workload.Ops > 0 {
		errorRate = float64(workload.Errors) / float64(workload.Ops)
	}
	if errorRate > cfg.MaxErrorRate+1e-12 {
		failures = append(failures, fmt.Sprintf("error rate %.6f exceeds SLO_MAX_ERROR_RATE=%.6f (%d/%d)",
			errorRate, cfg.MaxErrorRate, workload.Errors, workload.Ops))
	}
	if cfg.MinThroughput > 0 && workload.Duration > 0 {
		throughput := float64(workload.Ops) / workload.Duration.Seconds()
		if throughput < cfg.MinThroughput {
			failures = append(failures, fmt.Sprintf("throughput %.2f ops/s is below SLO_MIN_THROUGHPUT=%.2f",
				throughput, cfg.MinThroughput))
		}
	}
	checkLatency := func(name string, hist *sloHistogram) {
		if hist.total == 0 {
			return
		}
		p95 := hist.quantile(0.95)
		p99 := hist.quantile(0.99)
		if cfg.P95GateMS > 0 && float64(p95)/float64(time.Millisecond) > cfg.P95GateMS {
			failures = append(failures, fmt.Sprintf("%s p95=%s exceeds SLO_P95_GATE_MS=%.1f",
				name, p95.Round(time.Microsecond), cfg.P95GateMS))
		}
		if cfg.P99GateMS > 0 && float64(p99)/float64(time.Millisecond) > cfg.P99GateMS {
			failures = append(failures, fmt.Sprintf("%s p99=%s exceeds SLO_P99_GATE_MS=%.1f",
				name, p99.Round(time.Microsecond), cfg.P99GateMS))
		}
	}
	checkLatency("all", &workload.All)
	checkLatency("read", &workload.Read)
	checkLatency("write", &workload.Write)
	return len(failures) == 0, failures
}

// sloHistogram is a bounded log-linear latency histogram. Recording is O(1)
// and the structure is 512 uint64 counters, so it stays cheap over long runs.
// Each worker records privately; the harness merges once at the end.
type sloHistogram struct {
	counts [sloHistBuckets]uint64
	total  uint64
	min    time.Duration
	max    time.Duration
	sum    time.Duration
}

func sloHistIndex(v uint64) int {
	if v < sloHistLinear {
		return int(v)
	}
	e := uint(bits.Len64(v) - 1)
	frac := (v >> (e - 3)) & 0x7
	return sloHistLinear + int(e-4)*8 + int(frac)
}

// sloHistUpper returns the exclusive upper bound of a bucket in nanoseconds.
// Quantiles report this conservative upper bound, clamped to the observed max.
func sloHistUpper(idx int) time.Duration {
	if idx < sloHistLinear {
		return time.Duration(idx + 1)
	}
	if idx >= sloHistBuckets {
		idx = sloHistBuckets - 1
	}
	p := idx - sloHistLinear
	e := uint(p/8) + 4
	frac := p % 8
	upper := (uint64(1) << e) + (uint64(frac)+1)<<(e-3)
	if upper > math.MaxInt64 {
		upper = math.MaxInt64
	}
	return time.Duration(upper)
}

func (h *sloHistogram) record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	idx := sloHistIndex(uint64(d))
	if idx >= sloHistBuckets {
		idx = sloHistBuckets - 1
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

func (h *sloHistogram) merge(other *sloHistogram) {
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

func (h *sloHistogram) quantile(q float64) time.Duration {
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
			d := sloHistUpper(i)
			if d > h.max {
				d = h.max
			}
			return d
		}
	}
	return h.max
}

func (h *sloHistogram) mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return h.sum / time.Duration(h.total)
}

// Key helpers. The returned slice aliases dst; callers that retain a key must
// copy it (Txn.Set and Txn.Get copy as needed for this harness).

func sloAppendWorkloadKey(dst []byte, worker int, id uint64) []byte {
	dst = append(dst[:0], sloWorkloadKeyStart...)
	dst = strconv.AppendInt(dst, int64(worker), 10)
	dst = append(dst, '/')
	return strconv.AppendUint(dst, id, 10)
}

func sloAppendPreloadKey(dst []byte, id uint64) []byte {
	dst = append(dst[:0], sloPreloadKeyStart...)
	return strconv.AppendUint(dst, id, 10)
}

// sloValueFor fills buf with a deterministic payload derived from key and n.
// The same key and size always produce the same bytes on every platform, so a
// verification read does not need to retain the original value. Txn.Set copies
// the returned slice, so buf may be reused between calls.
func sloValueFor(key []byte, n int, buf []byte) []byte {
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

// JSON report types. Latencies are milliseconds so reports stay readable for
// both microsecond and second-scale values.

type sloLatencyJSON struct {
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MeanMS float64 `json:"mean_ms"`
	MinMS  float64 `json:"min_ms"`
	MaxMS  float64 `json:"max_ms"`
}

type sloPreloadJSON struct {
	Enabled     bool    `json:"enabled"`
	TargetGB    float64 `json:"target_gb"`
	TargetBytes int64   `json:"target_bytes"`
	Keys        int64   `json:"keys"`
	Bytes       int64   `json:"bytes"`
	Workers     int     `json:"workers"`
	Seconds     float64 `json:"seconds"`
	MiBPerSec   float64 `json:"mib_per_sec"`
}

type sloWorkloadJSON struct {
	DurationSeconds float64        `json:"duration_seconds"`
	Ops             int64          `json:"ops"`
	Reads           int64          `json:"reads"`
	Writes          int64          `json:"writes"`
	Errors          int64          `json:"errors"`
	ErrorRate       float64        `json:"error_rate"`
	Throughput      float64        `json:"throughput_ops_per_sec"`
	ReadThroughput  float64        `json:"read_ops_per_sec"`
	WriteThroughput float64        `json:"write_ops_per_sec"`
	BytesRead       int64          `json:"bytes_read"`
	BytesWritten    int64          `json:"bytes_written"`
	MiBPerSec       float64        `json:"mib_per_sec"`
	FirstError      string         `json:"first_error,omitempty"`
	Latency         sloLatencyJSON `json:"latency"`
	ReadLatency     sloLatencyJSON `json:"read_latency"`
	WriteLatency    sloLatencyJSON `json:"write_latency"`
}

type sloVerificationJSON struct {
	InlineChecked int64  `json:"inline_checked"`
	InlineFailed  int64  `json:"inline_failed"`
	SampleChecked int64  `json:"sample_checked"`
	SampleFailed  int64  `json:"sample_failed"`
	TotalFailed   int64  `json:"total_failed"`
	FirstError    string `json:"first_error,omitempty"`
}

type sloGatesJSON struct {
	MaxErrorRate  float64  `json:"max_error_rate"`
	P95GateMS     float64  `json:"p95_gate_ms"`
	P99GateMS     float64  `json:"p99_gate_ms"`
	MinThroughput float64  `json:"min_throughput_ops_per_sec"`
	Passed        bool     `json:"passed"`
	Failures      []string `json:"failures"`
}

type sloSummary struct {
	Harness         string              `json:"harness"`
	Timestamp       string              `json:"timestamp"`
	ClientHost      string              `json:"client_host,omitempty"`
	Meta            string              `json:"meta"`
	Nodes           []string            `json:"nodes"`
	Clients         int                 `json:"clients"`
	MaxInflight     int                 `json:"max_inflight"`
	ValueSize       int                 `json:"value_size"`
	Keyspace        uint64              `json:"keyspace"`
	PerWorkerKeys   uint64              `json:"per_worker_keys"`
	ReadRatio       float64             `json:"read_ratio"`
	DurationSeconds float64             `json:"duration_seconds"`
	Seed            uint64              `json:"seed"`
	Preload         sloPreloadJSON      `json:"preload"`
	Workload        sloWorkloadJSON     `json:"workload"`
	Verification    sloVerificationJSON `json:"verification"`
	Gates           sloGatesJSON        `json:"gates"`
}

func sloLatencyFromHist(h *sloHistogram) sloLatencyJSON {
	ms := func(d time.Duration) float64 {
		return float64(d) / float64(time.Millisecond)
	}
	return sloLatencyJSON{
		P50MS:  ms(h.quantile(0.50)),
		P95MS:  ms(h.quantile(0.95)),
		P99MS:  ms(h.quantile(0.99)),
		MeanMS: ms(h.mean()),
		MinMS:  ms(h.min),
		MaxMS:  ms(h.max),
	}
}

func sloBuildSummary(cfg sloConfig, host string, preload sloPreloadStats, workload sloWorkloadResult, verify sloVerifyResult, passed bool, failures []string) *sloSummary {
	durationSeconds := workload.Duration.Seconds()
	deriveRate := func(n int64) float64 {
		if durationSeconds <= 0 {
			return 0
		}
		return float64(n) / durationSeconds
	}
	errorRate := 0.0
	if workload.Ops > 0 {
		errorRate = float64(workload.Errors) / float64(workload.Ops)
	}
	preloadSeconds := preload.Duration.Seconds()
	preloadRate := 0.0
	if preloadSeconds > 0 {
		preloadRate = float64(preload.Bytes) / preloadSeconds / float64(1<<20)
	}
	return &sloSummary{
		Harness:         "slo-multihost-v1",
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		ClientHost:      host,
		Meta:            cfg.Meta,
		Nodes:           append([]string(nil), cfg.Nodes...),
		Clients:         cfg.Clients,
		MaxInflight:     cfg.MaxInflight,
		ValueSize:       cfg.ValueSize,
		Keyspace:        cfg.Keyspace,
		PerWorkerKeys:   cfg.PerWorkerKeys,
		ReadRatio:       cfg.ReadRatio,
		DurationSeconds: cfg.Duration.Seconds(),
		Seed:            cfg.Seed,
		Preload: sloPreloadJSON{
			Enabled:     cfg.PreloadBytes > 0,
			TargetGB:    cfg.PreloadGB,
			TargetBytes: cfg.PreloadBytes,
			Keys:        preload.Keys,
			Bytes:       preload.Bytes,
			Workers:     cfg.PreloadWorkers,
			Seconds:     preloadSeconds,
			MiBPerSec:   preloadRate,
		},
		Workload: sloWorkloadJSON{
			DurationSeconds: durationSeconds,
			Ops:             workload.Ops,
			Reads:           workload.Reads,
			Writes:          workload.Writes,
			Errors:          workload.Errors,
			ErrorRate:       errorRate,
			Throughput:      deriveRate(workload.Ops),
			ReadThroughput:  deriveRate(workload.Reads),
			WriteThroughput: deriveRate(workload.Writes),
			BytesRead:       workload.BytesRead,
			BytesWritten:    workload.BytesWritten,
			MiBPerSec:       deriveRate(workload.BytesRead+workload.BytesWritten) / float64(1<<20),
			FirstError:      workload.FirstError,
			Latency:         sloLatencyFromHist(&workload.All),
			ReadLatency:     sloLatencyFromHist(&workload.Read),
			WriteLatency:    sloLatencyFromHist(&workload.Write),
		},
		Verification: sloVerificationJSON{
			InlineChecked: workload.VerifyChecked,
			InlineFailed:  workload.VerifyFailed,
			SampleChecked: verify.Checked,
			SampleFailed:  verify.Failed,
			TotalFailed:   workload.VerifyFailed + verify.Failed,
			FirstError:    verify.FirstError,
		},
		Gates: sloGatesJSON{
			MaxErrorRate:  cfg.MaxErrorRate,
			P95GateMS:     cfg.P95GateMS,
			P99GateMS:     cfg.P99GateMS,
			MinThroughput: cfg.MinThroughput,
			Passed:        passed,
			Failures:      failures,
		},
	}
}

func sloLogSummary(t *testing.T, s *sloSummary) {
	t.Helper()
	w := s.Workload
	t.Logf("SLO workload: elapsed=%.3fs ops=%d reads=%d writes=%d errors=%d error_rate=%.6f throughput=%.2f ops/s read=%.2f write=%.2f mib/s=%.2f",
		w.DurationSeconds, w.Ops, w.Reads, w.Writes, w.Errors, w.ErrorRate,
		w.Throughput, w.ReadThroughput, w.WriteThroughput, w.MiBPerSec)
	t.Logf("SLO latency all: %s", sloFormatLatency(w.Latency))
	t.Logf("SLO latency read: %s", sloFormatLatency(w.ReadLatency))
	t.Logf("SLO latency write: %s", sloFormatLatency(w.WriteLatency))
	if s.Preload.Enabled {
		t.Logf("SLO preload: keys=%d bytes=%d (%.3f GiB) workers=%d elapsed=%.3fs rate=%.1f MiB/s",
			s.Preload.Keys, s.Preload.Bytes, float64(s.Preload.Bytes)/float64(sloGiB),
			s.Preload.Workers, s.Preload.Seconds, s.Preload.MiBPerSec)
	} else {
		t.Logf("SLO preload: disabled")
	}
	t.Logf("SLO verification: inline_checked=%d inline_failed=%d sample_checked=%d sample_failed=%d total_failed=%d",
		s.Verification.InlineChecked, s.Verification.InlineFailed,
		s.Verification.SampleChecked, s.Verification.SampleFailed, s.Verification.TotalFailed)
	if s.Verification.FirstError != "" {
		t.Logf("SLO verification first error: %s", s.Verification.FirstError)
	}
	if w.FirstError != "" {
		t.Logf("SLO workload first error: %s", w.FirstError)
	}
	if s.Gates.Passed {
		t.Logf("SLO gate: PASS (p95_gate_ms=%.1f p99_gate_ms=%.1f max_error_rate=%.6f min_throughput=%.1f)",
			s.Gates.P95GateMS, s.Gates.P99GateMS, s.Gates.MaxErrorRate, s.Gates.MinThroughput)
	} else {
		t.Logf("SLO gate: FAIL failures=%d", len(s.Gates.Failures))
	}
}

func sloFormatLatency(l sloLatencyJSON) string {
	return fmt.Sprintf("p50=%.3fms p95=%.3fms p99=%.3fms mean=%.3fms min=%.3fms max=%.3fms",
		l.P50MS, l.P95MS, l.P99MS, l.MeanMS, l.MinMS, l.MaxMS)
}

func sloWriteReport(path string, summary *sloSummary) error {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// TestSLOSelfCheck is the always-on, cluster-free smoke test for this harness.
// It exercises configuration parsing, deterministic keys/values and histogram
// quantiles. It runs under -short; TestSLOMultiHost does not.
func TestSLOSelfCheck(t *testing.T) {
	t.Run("histogram", func(t *testing.T) {
		var h sloHistogram
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

		var other sloHistogram
		other.record(20 * time.Millisecond)
		h.merge(&other)
		if h.total != 101 {
			t.Fatalf("merged total = %d, want 101", h.total)
		}
		if h.min != 10*time.Millisecond || h.max != 20*time.Millisecond {
			t.Fatalf("merged min/max = %s/%s, want 10ms/20ms", h.min, h.max)
		}
		var empty sloHistogram
		if empty.quantile(0.99) != 0 || empty.mean() != 0 {
			t.Fatalf("empty histogram returned non-zero quantile/mean")
		}
	})

	t.Run("keys", func(t *testing.T) {
		seen := make(map[string]struct{}, 2048)
		buf := make([]byte, 0, 64)
		for worker := 0; worker < 4; worker++ {
			for i := 0; i < 500; i++ {
				key := string(sloAppendWorkloadKey(buf, worker, uint64(worker*500+i)))
				if _, ok := seen[key]; ok {
					t.Fatalf("duplicate workload key %q", key)
				}
				seen[key] = struct{}{}
				if !strings.HasPrefix(key, "slo/workload/w") {
					t.Fatalf("unexpected workload key prefix: %q", key)
				}
			}
		}
		if got := string(sloAppendPreloadKey(buf, 42)); got != "slo/preload/42" {
			t.Fatalf("preload key = %q", got)
		}
	})

	t.Run("values", func(t *testing.T) {
		keyA := []byte("slo/workload/w1/1")
		keyB := []byte("slo/workload/w1/2")
		first := append([]byte(nil), sloValueFor(keyA, 64, nil)...)
		again := sloValueFor(keyA, 64, nil)
		if !bytes.Equal(first, again) {
			t.Fatalf("value generation is not deterministic")
		}
		if bytes.Equal(first, sloValueFor(keyB, 64, nil)) {
			t.Fatalf("different keys produced identical values")
		}
		for _, n := range []int{1, 7, 8, 17, 1024} {
			v := sloValueFor(keyA, n, nil)
			if len(v) != n {
				t.Fatalf("value length = %d, want %d", len(v), n)
			}
		}
		reuse := make([]byte, 0, 32)
		if got := sloValueFor(keyA, 32, reuse); len(got) != 32 {
			t.Fatalf("reused buffer length = %d, want 32", len(got))
		}
	})

	t.Run("duration", func(t *testing.T) {
		for _, tc := range []struct {
			in   string
			want time.Duration
		}{
			{"30s", 30 * time.Second},
			{"5m", 5 * time.Minute},
			{"1.5", 1500 * time.Millisecond},
			{"0.25s", 250 * time.Millisecond},
		} {
			got, err := sloParseDuration(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("sloParseDuration(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
			}
		}
		for _, in := range []string{"", "nope", "0", "-1s"} {
			if got, err := sloParseDuration(in); err == nil {
				t.Fatalf("sloParseDuration(%q) = %s, want error", in, got)
			}
		}
	})

	t.Run("config", func(t *testing.T) {
		env := sloValidTestEnv()
		cfg, err := sloConfigFrom(func(key string) string { return env[key] })
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		if cfg.Clients != 4 || cfg.MaxInflight != 4 {
			t.Fatalf("clients/inflight = %d/%d, want 4/4", cfg.Clients, cfg.MaxInflight)
		}
		if cfg.PerWorkerKeys != 250 {
			t.Fatalf("per-worker keys = %d, want 250", cfg.PerWorkerKeys)
		}
		if cfg.PreloadKeys != 0 {
			t.Fatalf("preload keys = %d, want 0", cfg.PreloadKeys)
		}

		env["SLO_MAX_INFLIGHT"] = "7"
		env["SLO_PRELOAD_GB"] = "1"
		env["SLO_VALUE_SIZE"] = "1024"
		cfg, err = sloConfigFrom(func(key string) string { return env[key] })
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		if cfg.MaxInflight != 7 {
			t.Fatalf("max inflight = %d, want 7", cfg.MaxInflight)
		}
		if cfg.PreloadKeys != (int64(sloGiB) / 1024) {
			t.Fatalf("preload keys = %d, want %d", cfg.PreloadKeys, int64(sloGiB)/1024)
		}

		for _, tc := range []struct {
			name string
			key  string
			val  string
		}{
			{"missing-meta", "SLO_META", ""},
			{"missing-nodes", "SLO_NODES", ""},
			{"empty-node", "SLO_NODES", "a:1,,b:1"},
			{"duplicate-node", "SLO_NODES", "a:1,a:1"},
			{"clients-zero", "SLO_CLIENTS", "0"},
			{"duration-zero", "SLO_DURATION", "0s"},
			{"ratio-high", "SLO_READ_RATIO", "1.1"},
			{"keyspace-small", "SLO_KEYSPACE", "3"},
			{"verify-sample-high", "SLO_VERIFY_SAMPLE", "10001"},
			{"preload-negative", "SLO_PRELOAD_GB", "-1"},
			{"p99-negative", "SLO_P99_GATE_MS", "-1"},
		} {
			bad := sloValidTestEnv()
			bad[tc.key] = tc.val
			if _, err := sloConfigFrom(func(key string) string { return bad[key] }); err == nil {
				t.Fatalf("%s: expected validation error", tc.name)
			}
		}
	})

	t.Run("preload-batch", func(t *testing.T) {
		if got := sloPreloadBatchKeys(1024); got != sloPreloadMaxBatchKeys {
			t.Fatalf("batch keys for 1KiB = %d, want %d", got, sloPreloadMaxBatchKeys)
		}
		if got := sloPreloadBatchKeys(1); got != sloPreloadMaxBatchKeys {
			t.Fatalf("batch keys for 1B = %d, want %d", got, sloPreloadMaxBatchKeys)
		}
		if got := sloPreloadBatchKeys(sloMaxValueSize); got != 1 {
			t.Fatalf("batch keys for max value = %d, want 1", got)
		}
	})

	t.Run("gates", func(t *testing.T) {
		good := sloWorkloadResult{Duration: time.Second, Ops: 100, Reads: 100}
		good.All.record(time.Millisecond)
		good.Read.record(time.Millisecond)
		if passed, failures := sloEvaluateGates(sloConfig{}, good, sloVerifyResult{}); !passed {
			t.Fatalf("healthy result failed gates: %v", failures)
		}
		if passed, _ := sloEvaluateGates(sloConfig{}, sloWorkloadResult{}, sloVerifyResult{}); passed {
			t.Fatalf("zero-operation result passed gates")
		}
		withError := good
		withError.Errors = 1
		if passed, _ := sloEvaluateGates(sloConfig{}, withError, sloVerifyResult{}); passed {
			t.Fatalf("error result passed gates")
		}
		slow := good
		slow.All = sloHistogram{}
		slow.All.record(50 * time.Millisecond)
		slow.Read = slow.All
		if passed, _ := sloEvaluateGates(sloConfig{P99GateMS: 10}, slow, sloVerifyResult{}); passed {
			t.Fatalf("slow result passed p99 gate")
		}
		if passed, _ := sloEvaluateGates(sloConfig{}, good, sloVerifyResult{Failed: 1}); passed {
			t.Fatalf("verification failure passed gates")
		}
	})
}

func sloValidTestEnv() map[string]string {
	return map[string]string{
		"SLO_META":          "meta.example:9090",
		"SLO_NODES":         "node1.example:7001,node2.example:7001,node3.example:7001",
		"SLO_CLIENTS":       "4",
		"SLO_DURATION":      "30s",
		"SLO_VALUE_SIZE":    "128",
		"SLO_KEYSPACE":      "1000",
		"SLO_READ_RATIO":    "0.75",
		"SLO_VERIFY_SAMPLE": "8",
	}
}
