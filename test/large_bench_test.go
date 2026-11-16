package test

// Large-data benchmark harness (goal G6).
//
// This file is deliberately self-contained: it starts its own three-node
// in-process v2 cluster instead of using newSpeedCluster because the speed
// helper hides the per-node data directories (needed for on-disk byte
// accounting) and hard-codes its storage sizes. Nothing in this file modifies
// or depends on the speed harness beyond the shared package test helpers
// v2Logger().
//
// Everything is gated: unless LARGE_BENCH=1, every entry point skips
// immediately with t.Skip/b.Skip. The harness never runs as part of the normal
// test suite.
//
// Environment:
//
//	LARGE_BENCH=1             required gate
//	LARGE_BENCH_GB            target dataset size in GiB (default 100)
//	LARGE_BENCH_VALUE         value payload bytes per key (default 256)
//	LARGE_BENCH_CLIENTS       parallel client goroutines (default 10)
//	LARGE_BENCH_DURATION      measured time per phase, Go duration (default 30s)
//
// Dataset sizing: LARGE_BENCH_GB counts value payload only (key bytes, Raft
// log, Pebble/WAL and replication overhead are extra). The number of keys is
// target_bytes/value_bytes, so the logical dataset is always <= the target.
// Preload streams bounded batches (at most ~4 MiB or 4096 keys per
// transaction, whichever binds first, with a bounded number of transactions
// in flight) and never holds the dataset in RAM: keys are generated from their
// ordinal, and one value buffer is reused (Txn.Set copies it).
//
// Reported numbers:
//   - sequential and parallel single-key write/read transactions;
//   - p50/p95/p99 latency from a bounded, fixed-array HDR-like histogram
//     (512 exponential sub-buckets, no external dependency; reported
//     quantiles are conservative bucket upper bounds);
//   - throughput, error counts, first error;
//   - approximate on-disk bytes from walking every node data directory
//     (this triple-counts replicated bytes: three nodes x the range);
//   - target vs actual logical dataset bytes, plus preload throughput;
//   - one machine-readable JSON line prefixed with LARGE_BENCH_JSON.
//
// Smoke run (about 50 MiB, finishes in seconds):
//
//	LARGE_BENCH=1 LARGE_BENCH_GB=0.05 \
//	  go test -run TestLargeBenchHarnessSmoke -v ./test
//
// Benchmark entry point (one measured pass; -benchtime=1x avoids a second
// calibration pass, and the pass itself is far longer than the default
// -benchtime so the framework normally will not repeat it):
//
//	LARGE_BENCH=1 LARGE_BENCH_GB=10 LARGE_BENCH_DURATION=15s \
//	  go test -run '^$' -bench '^BenchmarkLargeData$' -benchtime=1x \
//	  -timeout 2h ./test
//
// Full test entry point:
//
//	LARGE_BENCH=1 LARGE_BENCH_GB=10 LARGE_BENCH_DURATION=15s \
//	  go test -run '^TestLargeBenchHarness$' -v -timeout 2h ./test
//
// How to run the 1 TB case (do NOT run this on a laptop):
//
//	LARGE_BENCH=1 LARGE_BENCH_GB=1024 LARGE_BENCH_VALUE=256 \
//	LARGE_BENCH_CLIENTS=16 LARGE_BENCH_DURATION=5m \
//	  go test -run '^TestLargeBenchHarness$' -v -timeout 72h ./test
//
// The 1 TB case needs roughly 3 TiB free under TMPDIR (tb.TempDir() is used
// for all three nodes and replication factor is 3), many hours of wall clock,
// and a raised go test -timeout. It is a single-process capacity/stability
// probe on loopback, not a substitute for a multi-host 1 TB benchmark.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

const (
	lbEnvEnable   = "LARGE_BENCH"
	lbEnvGB       = "LARGE_BENCH_GB"
	lbEnvValue    = "LARGE_BENCH_VALUE"
	lbEnvClients  = "LARGE_BENCH_CLIENTS"
	lbEnvDuration = "LARGE_BENCH_DURATION"

	lbGiB = int64(1) << 30

	lbDefaultGB       = 100.0
	lbDefaultValue    = 256
	lbDefaultClients  = 10
	lbDefaultDuration = 30 * time.Second

	// The smoke test is capped so an inherited production-sized environment
	// cannot turn it into a multi-hour test.
	lbSmokeGB          = 0.05
	lbSmokeDuration    = time.Second
	lbSmokeMaxDuration = 2 * time.Second

	lbMaxValueBytes = 16 << 20
	lbMaxClients    = 512

	// Preload a bounded amount per transaction so neither the transaction
	// nor the Raft entry grows with the dataset size.
	lbPreloadBatchBytes = 4 << 20
	lbPreloadMaxKeys    = 4096
	lbPreloadTxns       = 4

	lbCacheSize    = 64 << 20
	lbMemTableSize = 32 << 20

	// Bounded HDR-like histogram: values below 16ns use linear buckets;
	// larger values use 8 sub-buckets per power of two.
	lbHistBuckets = 512
	lbHistLinear  = 16

	// Lower bound on measured operations per phase. Duration normally
	// dominates; this guarantees enough samples for a meaningful p99 even
	// when LARGE_BENCH_DURATION is tiny. A slow cluster can therefore extend
	// a phase beyond LARGE_BENCH_DURATION until this many ops have run.
	lbMinPhaseOps = 100
)

const (
	lbDataPrefix     = "large-bench/data/"
	lbSeqWritePrefix = "large-bench/seq-write/"
	lbParWritePrefix = "large-bench/par-write/"
)

// lbConfig is the fully validated harness configuration.
type lbConfig struct {
	Entrypoint  string
	TargetGB    float64
	TargetBytes int64
	ValueBytes  int
	Clients     int
	Duration    time.Duration
}

// lbRequireEnabled skips immediately unless LARGE_BENCH=1.
func lbRequireEnabled(tb testing.TB) {
	tb.Helper()
	if os.Getenv(lbEnvEnable) != "1" {
		tb.Skip("large-data benchmark disabled: set LARGE_BENCH=1 to enable")
	}
}

// lbConfigFromEnv reads and validates the environment. Invalid values are
// fatal rather than silently ignored so a long run cannot use the wrong size.
func lbConfigFromEnv(tb testing.TB, smoke bool, entrypoint string) lbConfig {
	tb.Helper()
	lbRequireEnabled(tb)

	gb := lbDefaultGB
	dur := lbDefaultDuration
	if smoke {
		gb = lbSmokeGB
		dur = lbSmokeDuration
	}

	if raw := strings.TrimSpace(os.Getenv(lbEnvGB)); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			tb.Fatalf("%s=%q: want a positive number of GiB", lbEnvGB, raw)
		}
		gb = v
	}
	if smoke && gb > lbSmokeGB {
		tb.Logf("large-bench smoke: clamping %s=%.6g GiB to %.2f GiB", lbEnvGB, gb, lbSmokeGB)
		gb = lbSmokeGB
	}

	valueBytes := lbDefaultValue
	if raw := strings.TrimSpace(os.Getenv(lbEnvValue)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > lbMaxValueBytes {
			tb.Fatalf("%s=%q: want an integer in [1,%d]", lbEnvValue, raw, lbMaxValueBytes)
		}
		valueBytes = n
	}

	clients := lbDefaultClients
	if raw := strings.TrimSpace(os.Getenv(lbEnvClients)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > lbMaxClients {
			tb.Fatalf("%s=%q: want an integer in [1,%d]", lbEnvClients, raw, lbMaxClients)
		}
		clients = n
	}

	if raw := strings.TrimSpace(os.Getenv(lbEnvDuration)); raw != "" {
		d, err := lbParseDuration(raw)
		if err != nil || d <= 0 {
			tb.Fatalf("%s=%q: want a positive Go duration (example 30s) or seconds", lbEnvDuration, raw)
		}
		dur = d
	}
	if smoke && dur > lbSmokeMaxDuration {
		tb.Logf("large-bench smoke: clamping %s=%s to %s", lbEnvDuration, dur, lbSmokeMaxDuration)
		dur = lbSmokeMaxDuration
	}

	targetBytes := int64(gb * float64(lbGiB))
	if targetBytes < int64(valueBytes) {
		targetBytes = int64(valueBytes)
	}

	return lbConfig{
		Entrypoint:  entrypoint,
		TargetGB:    gb,
		TargetBytes: targetBytes,
		ValueBytes:  valueBytes,
		Clients:     clients,
		Duration:    dur,
	}
}

// lbParseDuration accepts a Go duration ("30s", "5m") or a bare number of
// seconds ("30", "0.5").
func lbParseDuration(raw string) (time.Duration, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0, fmt.Errorf("not a duration or number of seconds")
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// lbCluster is a dedicated three-node v2 cluster whose data directories are
// retained (until test cleanup) so on-disk usage can be measured.
type lbCluster struct {
	metaAddr string
	dataDirs []string
	client   *v2client.Client
}

func lbFreeAddr(tb testing.TB) string {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("large-bench: reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func lbNewCluster(tb testing.TB) *lbCluster {
	tb.Helper()
	ctx := context.Background()

	metaAddr := lbFreeAddr(tb)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		tb.Fatalf("large-bench: meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		tb.Fatalf("large-bench: start meta: %v", err)
	}
	tb.Cleanup(func() { _ = ms.Stop() })

	addrs := []string{lbFreeAddr(tb), lbFreeAddr(tb), lbFreeAddr(tb)}
	servers := make([]*kvserver.Server, 0, len(addrs))
	dirs := make([]string, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		dir := tb.TempDir()
		srv, err := kvserver.New(kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            dir,
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          lbCacheSize,
			MemTableSize:       lbMemTableSize,
			Logger:             v2Logger(),
		})
		if err != nil {
			tb.Fatalf("large-bench: new node %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			tb.Fatalf("large-bench: start node %s: %v", addr, err)
		}
		servers = append(servers, srv)
		dirs = append(dirs, dir)
		tb.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}

	leader := lbWaitLeader(tb, servers)
	client, err := v2client.New(metaAddr, addrs)
	if err != nil {
		tb.Fatalf("large-bench: client: %v", err)
	}
	tb.Cleanup(func() { _ = client.Close() })

	tb.Logf("large-bench: cluster ready meta=%s leader=%s nodes=%d data-dirs=%d",
		metaAddr, leader.Raft().Status().ID, len(servers), len(dirs))
	return &lbCluster{metaAddr: metaAddr, dataDirs: dirs, client: client}
}

func lbWaitLeader(tb testing.TB, servers []*kvserver.Server) *kvserver.Server {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, srv := range servers {
			if srv.Raft().Status().Role == "leader" {
				return srv
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	tb.Fatalf("large-bench: no leader among %d KV nodes", len(servers))
	return nil
}

// lbKeyCount derives the key count from the value-payload target.
func lbKeyCount(targetBytes int64, valueBytes int) int64 {
	n := targetBytes / int64(valueBytes)
	if n < 1 {
		n = 1
	}
	return n
}

// lbPreloadKeys picks a transaction size that keeps a single preload
// transaction near lbPreloadBatchBytes without exceeding lbPreloadMaxKeys.
func lbPreloadKeys(valueBytes int) int {
	n := lbPreloadBatchBytes / valueBytes
	if n < 1 {
		n = 1
	}
	if n > lbPreloadMaxKeys {
		n = lbPreloadMaxKeys
	}
	return n
}

// lbAppendKey appends the decimal ordinal to prefix, reusing dst. Callers must
// not retain the result across the next call; Txn.Set and Txn.Get copy as
// needed, but the read benchmark also consumes it immediately.
func lbAppendKey(dst []byte, prefix string, id uint64) []byte {
	dst = dst[:0]
	dst = append(dst, prefix...)
	return strconv.AppendUint(dst, id, 10)
}

// lbMakeValue builds the single reusable value payload. Its first and last
// bytes encode the length so a truncated/corrupt read is visible.
func lbMakeValue(n int) []byte {
	v := make([]byte, n)
	for i := range v {
		v[i] = byte('a' + i%26)
	}
	v[0] = byte(n & 0xff)
	v[n-1] = byte((n >> 8) & 0xff)
	return v
}

// lbPreloadStats summarizes the dataset load.
type lbPreloadStats struct {
	Keys     int64
	Txns     int64
	Duration time.Duration
}

// lbPreload writes keyCount single-key rows in bounded batched transactions.
// It uses CommitBatch to keep lbPreloadTxns transactions in flight, which
// mainly hides TSO/commit round-trips during setup. The dataset itself is
// never materialized in memory.
func lbPreload(tb testing.TB, c *v2client.Client, cfg lbConfig, keyCount int64, value []byte) lbPreloadStats {
	tb.Helper()
	ctx := context.Background()
	perTxn := lbPreloadKeys(cfg.ValueBytes)
	started := time.Now()
	lastLog := started

	var (
		next   int64
		txns   int64
		rounds int64
	)
	keyBuf := make([]byte, 0, 64)
	for next < keyCount {
		round := make([]*v2client.Txn, 0, lbPreloadTxns)
		for len(round) < lbPreloadTxns && next < keyCount {
			txn, err := c.Begin(ctx)
			if err != nil {
				tb.Fatalf("large-bench: preload begin at key %d: %v", next, err)
			}
			n := int64(perTxn)
			if remaining := keyCount - next; n > remaining {
				n = remaining
			}
			for i := int64(0); i < n; i++ {
				key := lbAppendKey(keyBuf, lbDataPrefix, uint64(next+i))
				txn.Set(key, value)
			}
			next += n
			round = append(round, txn)
		}
		for i, err := range c.CommitBatch(ctx, round) {
			if err != nil {
				tb.Fatalf("large-bench: preload commit transaction %d of round %d: %v", i, rounds, err)
			}
		}
		txns += int64(len(round))
		rounds++

		if now := time.Now(); next == keyCount || now.Sub(lastLog) >= 2*time.Second {
			elapsed := now.Sub(started)
			doneBytes := next * int64(cfg.ValueBytes)
			rate := 0.0
			if elapsed > 0 {
				rate = float64(doneBytes) / elapsed.Seconds() / (1 << 20)
			}
			tb.Logf("large-bench preload: %d/%d keys (%.1f%%) txns=%d elapsed=%s %.1f MiB/s",
				next, keyCount, 100*float64(next)/float64(keyCount), txns, elapsed.Round(time.Millisecond), rate)
			lastLog = now
		}
	}

	elapsed := time.Since(started)
	return lbPreloadStats{Keys: next, Txns: txns, Duration: elapsed}
}

// lbDiskUsage walks every node data directory and sums regular file sizes.
// Errors are counted rather than fatal: files can disappear during Pebble
// compaction, and the value is explicitly approximate.
func lbDiskUsage(dirs []string) (total int64, errs int64) {
	for _, root := range dirs {
		err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				errs++
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				errs++
				return nil
			}
			total += info.Size()
			return nil
		})
		if err != nil {
			errs++
		}
	}
	return total, errs
}

// lbHistogram is a bounded HDR-like latency histogram. Each Record is O(1)
// and the whole structure is 512 uint64 counters, so it stays cheap even for
// millions of operations. Each worker uses a private histogram; lbMeasure
// merges them after the phase, avoiding atomics on the hot path.
type lbHistogram struct {
	counts [lbHistBuckets]uint64
	total  uint64
	min    time.Duration
	max    time.Duration
	sum    time.Duration
}

func lbHistIndex(v uint64) int {
	if v < lbHistLinear {
		return int(v)
	}
	e := uint(bits.Len64(v) - 1) // >= 4 here
	frac := (v >> (e - 3)) & 0x7
	return lbHistLinear + int(e-4)*8 + int(frac)
}

// lbHistUpper returns the exclusive upper bound of a bucket in nanoseconds.
// Quantiles report this conservative upper bound, clamped to the observed
// maximum and to math.MaxInt64.
func lbHistUpper(idx int) time.Duration {
	if idx < lbHistLinear {
		return time.Duration(idx + 1)
	}
	if idx >= lbHistBuckets {
		idx = lbHistBuckets - 1
	}
	p := idx - lbHistLinear
	e := uint(p/8) + 4
	frac := p % 8
	shift := e - 3
	upper := (uint64(1) << e) + (uint64(frac)+1)<<shift
	if upper > math.MaxInt64 {
		upper = math.MaxInt64
	}
	return time.Duration(upper)
}

func (h *lbHistogram) record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	idx := lbHistIndex(uint64(d))
	if idx >= lbHistBuckets {
		idx = lbHistBuckets - 1
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

func (h *lbHistogram) merge(other *lbHistogram) {
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

func (h *lbHistogram) quantile(q float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	switch {
	case q <= 0:
		return h.min
	case q >= 1:
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
			d := lbHistUpper(i)
			if d > h.max {
				d = h.max
			}
			return d
		}
	}
	return h.max
}

func (h *lbHistogram) mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return h.sum / time.Duration(h.total)
}

// lbPhaseResult is one measured workload phase. Latencies are milliseconds so
// the JSON stays readable for both microsecond and second-scale latencies.
type lbPhaseResult struct {
	Name       string  `json:"name"`
	Workers    int     `json:"workers"`
	Ops        int64   `json:"ops"`
	Errors     int64   `json:"errors"`
	ErrorRate  float64 `json:"error_rate"`
	DurationMS float64 `json:"duration_ms"`
	Throughput float64 `json:"throughput_ops_per_sec"`
	P50MS      float64 `json:"p50_ms"`
	P95MS      float64 `json:"p95_ms"`
	P99MS      float64 `json:"p99_ms"`
	MeanMS     float64 `json:"mean_ms"`
	MinMS      float64 `json:"min_ms"`
	MaxMS      float64 `json:"max_ms"`
	FirstError string  `json:"first_error,omitempty"`
}

// lbSummary is the machine-readable harness outcome.
type lbSummary struct {
	Harness          string          `json:"harness"`
	Entrypoint       string          `json:"entrypoint"`
	Timestamp        string          `json:"timestamp"`
	TargetGB         float64         `json:"target_gb"`
	TargetBytes      int64           `json:"target_bytes"`
	ValueBytes       int             `json:"value_bytes"`
	KeyCount         int64           `json:"key_count"`
	LogicalBytes     int64           `json:"logical_bytes"`
	LogicalGB        float64         `json:"logical_gb"`
	TargetRatio      float64         `json:"target_ratio"`
	PreloadTxns      int64           `json:"preload_txns"`
	PreloadSeconds   float64         `json:"preload_seconds"`
	PreloadMiBPerSec float64         `json:"preload_mib_per_sec"`
	PreloadDiskBytes int64           `json:"preload_disk_bytes"`
	FinalDiskBytes   int64           `json:"final_disk_bytes"`
	DiskWalkErrors   int64           `json:"disk_walk_errors"`
	DurationSec      float64         `json:"phase_duration_sec"`
	Clients          int             `json:"clients"`
	Nodes            int             `json:"nodes"`
	Phases           []lbPhaseResult `json:"phases"`
	TotalOps         int64           `json:"total_ops"`
	TotalErrors      int64           `json:"total_errors"`
	VerifyErrors     int64           `json:"verify_errors"`
}

// lbPhase describes one measured workload.
type lbPhase struct {
	Name    string
	Workers int
	Write   bool
	Prefix  string
}

// lbWorkerStats is one goroutine's result within a phase.
type lbWorkerStats struct {
	hist     lbHistogram
	ops      int64
	errors   int64
	firstErr error
}

// lbRun executes the whole harness: cluster, preload, verification, four
// measured phases, disk accounting, and logging. The caller owns the skip gate.
func lbRun(tb testing.TB, cfg lbConfig) *lbSummary {
	tb.Helper()

	c := lbNewCluster(tb)
	keyCount := lbKeyCount(cfg.TargetBytes, cfg.ValueBytes)
	logicalBytes := keyCount * int64(cfg.ValueBytes)
	value := lbMakeValue(cfg.ValueBytes)

	tb.Logf("large-bench: entrypoint=%s target=%.4g GiB (%d B) value=%d B keys=%d clients=%d phase-duration=%s",
		cfg.Entrypoint, cfg.TargetGB, cfg.TargetBytes, cfg.ValueBytes, keyCount, cfg.Clients, cfg.Duration)

	pre := lbPreload(tb, c.client, cfg, keyCount, value)
	if pre.Keys != keyCount {
		tb.Fatalf("large-bench: preloaded %d keys, want %d", pre.Keys, keyCount)
	}
	preDisk, preDiskErrs := lbDiskUsage(c.dataDirs)
	preRate := 0.0
	if pre.Duration > 0 {
		preRate = float64(logicalBytes) / pre.Duration.Seconds() / (1 << 20)
	}

	verifyErrs, verifyFirst := lbVerifySample(tb, c.client, keyCount, value)
	if verifyErrs > 0 {
		tb.Logf("large-bench: sample verification failures=%d first=%q", verifyErrs, verifyFirst)
	} else {
		tb.Logf("large-bench: sample verification passed")
	}

	phases := []lbPhase{
		{Name: "seq-write", Workers: 1, Write: true, Prefix: lbSeqWritePrefix},
		{Name: "par-write", Workers: cfg.Clients, Write: true, Prefix: lbParWritePrefix},
		{Name: "seq-read", Workers: 1, Write: false},
		{Name: "par-read", Workers: cfg.Clients, Write: false},
	}
	results := make([]lbPhaseResult, 0, len(phases))
	for _, p := range phases {
		results = append(results, lbMeasure(tb, c.client, cfg, p, keyCount, value))
	}

	finalDisk, finalDiskErrs := lbDiskUsage(c.dataDirs)
	summary := &lbSummary{
		Harness:          "large-bench-v1",
		Entrypoint:       cfg.Entrypoint,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		TargetGB:         cfg.TargetGB,
		TargetBytes:      cfg.TargetBytes,
		ValueBytes:       cfg.ValueBytes,
		KeyCount:         keyCount,
		LogicalBytes:     logicalBytes,
		LogicalGB:        float64(logicalBytes) / float64(lbGiB),
		TargetRatio:      float64(logicalBytes) / float64(cfg.TargetBytes),
		PreloadTxns:      pre.Txns,
		PreloadSeconds:   pre.Duration.Seconds(),
		PreloadMiBPerSec: preRate,
		PreloadDiskBytes: preDisk,
		FinalDiskBytes:   finalDisk,
		DiskWalkErrors:   preDiskErrs + finalDiskErrs,
		DurationSec:      cfg.Duration.Seconds(),
		Clients:          cfg.Clients,
		Nodes:            len(c.dataDirs),
		Phases:           results,
		VerifyErrors:     verifyErrs,
	}
	for _, r := range results {
		summary.TotalOps += r.Ops
		summary.TotalErrors += r.Errors
	}
	lbLogSummary(tb, summary)
	return summary
}

// lbVerifySample reads a handful of spread-out preloaded keys and checks the
// payload. It warms the read path before the measured phases and catches gross
// preload/read corruption early.
func lbVerifySample(tb testing.TB, c *v2client.Client, keyCount int64, value []byte) (int64, string) {
	tb.Helper()
	ctx := context.Background()
	n := int64(8)
	if keyCount < n {
		n = keyCount
	}
	var errs int64
	var first string
	keyBuf := make([]byte, 0, 64)
	for i := int64(0); i < n; i++ {
		id := uint64(i * keyCount / n)
		txn, err := c.Begin(ctx)
		if err != nil {
			errs++
			if first == "" {
				first = fmt.Sprintf("begin: %v", err)
			}
			continue
		}
		got, err := txn.Get(ctx, lbAppendKey(keyBuf, lbDataPrefix, id))
		if err != nil {
			errs++
			if first == "" {
				first = fmt.Sprintf("get %d: %v", id, err)
			}
			continue
		}
		if !bytes.Equal(got, value) {
			errs++
			if first == "" {
				first = fmt.Sprintf("get %d: value mismatch (%d bytes)", id, len(got))
			}
		}
	}
	return errs, first
}

// lbMeasure runs one phase, merges per-worker histograms, and logs a row.
func lbMeasure(tb testing.TB, c *v2client.Client, cfg lbConfig, p lbPhase, keyCount int64, value []byte) lbPhaseResult {
	tb.Helper()
	if p.Workers < 1 {
		p.Workers = 1
	}

	stats := make([]lbWorkerStats, p.Workers)
	started := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < p.Workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			stats[w] = lbRunWorker(c, cfg, p, w, keyCount, value)
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(started)

	var hist lbHistogram
	var ops, errs int64
	var firstErr error
	for i := range stats {
		hist.merge(&stats[i].hist)
		ops += stats[i].ops
		errs += stats[i].errors
		if firstErr == nil && stats[i].firstErr != nil {
			firstErr = stats[i].firstErr
		}
	}

	p50, p95, p99 := hist.quantile(0.50), hist.quantile(0.95), hist.quantile(0.99)
	throughput := 0.0
	if elapsed > 0 {
		throughput = float64(ops) / elapsed.Seconds()
	}
	res := lbPhaseResult{
		Name:       p.Name,
		Workers:    p.Workers,
		Ops:        ops,
		Errors:     errs,
		ErrorRate:  0,
		DurationMS: float64(elapsed) / float64(time.Millisecond),
		Throughput: throughput,
		P50MS:      float64(p50) / float64(time.Millisecond),
		P95MS:      float64(p95) / float64(time.Millisecond),
		P99MS:      float64(p99) / float64(time.Millisecond),
		MeanMS:     float64(hist.mean()) / float64(time.Millisecond),
		MinMS:      float64(hist.min) / float64(time.Millisecond),
		MaxMS:      float64(hist.max) / float64(time.Millisecond),
	}
	if ops > 0 {
		res.ErrorRate = float64(errs) / float64(ops)
	}
	if firstErr != nil {
		res.FirstError = firstErr.Error()
	}
	tb.Logf("large-bench phase %-9s ops=%d errors=%d elapsed=%s throughput=%.1f ops/s p50=%s p95=%s p99=%s",
		p.Name, ops, errs, elapsed.Round(time.Millisecond), throughput,
		lbFormatDuration(p50), lbFormatDuration(p95), lbFormatDuration(p99))
	return res
}

// lbRunWorker executes single-key transactions until the phase deadline and
// the minimum sample count are both satisfied. Errors are counted, not fatal:
// a benchmark run should report a degraded cluster rather than abort on the
// first transient error.
func lbRunWorker(c *v2client.Client, cfg lbConfig, p lbPhase, worker int, keyCount int64, value []byte) lbWorkerStats {
	ctx := context.Background()
	deadline := time.Now().Add(cfg.Duration)
	minOps := lbMinOpsForWorkers(p.Workers)
	keyBuf := make([]byte, 0, 64)

	var st lbWorkerStats
	for n := int64(0); ; n++ {
		if n >= minOps && time.Now().After(deadline) {
			break
		}
		started := time.Now()
		var err error

		if p.Write {
			// worker<<40 gives each parallel writer a disjoint key range.
			key := lbAppendKey(keyBuf, p.Prefix, (uint64(worker)<<40)|uint64(n))
			txn, beginErr := c.Begin(ctx)
			if beginErr != nil {
				err = fmt.Errorf("begin: %w", beginErr)
			} else {
				txn.Set(key, value)
				if commitErr := txn.Commit(ctx); commitErr != nil {
					err = fmt.Errorf("commit: %w", commitErr)
				}
			}
		} else {
			id := (uint64(n)*uint64(p.Workers) + uint64(worker)) % uint64(keyCount)
			key := lbAppendKey(keyBuf, lbDataPrefix, id)
			txn, beginErr := c.Begin(ctx)
			if beginErr != nil {
				err = fmt.Errorf("begin: %w", beginErr)
			} else {
				got, getErr := txn.Get(ctx, key)
				switch {
				case getErr != nil:
					err = fmt.Errorf("get: %w", getErr)
				case !bytes.Equal(got, value):
					err = fmt.Errorf("get: value mismatch for key %d (%d bytes)", id, len(got))
				}
			}
		}

		st.hist.record(time.Since(started))
		st.ops++
		if err != nil {
			st.errors++
			if st.firstErr == nil {
				st.firstErr = err
			}
		}
	}
	return st
}

func lbMinOpsForWorkers(workers int) int64 {
	if workers < 1 {
		workers = 1
	}
	n := int64(lbMinPhaseOps) / int64(workers)
	if n < 8 {
		n = 8
	}
	return n
}

func lbLogSummary(tb testing.TB, s *lbSummary) {
	tb.Helper()
	payload, err := json.Marshal(s)
	if err != nil {
		tb.Fatalf("large-bench: marshal summary: %v", err)
	}

	tb.Logf("large-bench dataset: target=%s actual=%s (%.2f%%) value=%dB keys=%d",
		lbFormatBytes(s.TargetBytes), lbFormatBytes(s.LogicalBytes), 100*s.TargetRatio, s.ValueBytes, s.KeyCount)
	tb.Logf("large-bench preload: txns=%d elapsed=%s rate=%.1f MiB/s disk=%s (3 nodes, replication factor 3)",
		s.PreloadTxns, lbFormatSeconds(s.PreloadSeconds), s.PreloadMiBPerSec, lbFormatBytes(s.PreloadDiskBytes))
	tb.Logf("large-bench on-disk: after-preload=%s final=%s walk-errors=%d",
		lbFormatBytes(s.PreloadDiskBytes), lbFormatBytes(s.FinalDiskBytes), s.DiskWalkErrors)
	tb.Logf("%-10s %10s %10s %10s %10s %12s %8s", "phase", "ops", "p50", "p95", "p99", "ops/sec", "errors")
	for _, p := range s.Phases {
		tb.Logf("%-10s %10d %10s %10s %10s %12.1f %8d",
			p.Name, p.Ops,
			lbFormatDuration(time.Duration(p.P50MS*float64(time.Millisecond))),
			lbFormatDuration(time.Duration(p.P95MS*float64(time.Millisecond))),
			lbFormatDuration(time.Duration(p.P99MS*float64(time.Millisecond))),
			p.Throughput, p.Errors)
	}
	tb.Logf("large-bench totals: ops=%d errors=%d verify-errors=%d nodes=%d clients=%d phase-duration=%s",
		s.TotalOps, s.TotalErrors, s.VerifyErrors, s.Nodes, s.Clients, lbFormatSeconds(s.DurationSec))
	tb.Logf("LARGE_BENCH_JSON %s", payload)
}

func lbFormatBytes(n int64) string {
	if n < 0 {
		return "-" + lbFormatBytes(-n)
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func lbFormatDuration(d time.Duration) string {
	if d < time.Microsecond {
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fus", float64(d)/float64(time.Microsecond))
	}
	if d < time.Second {
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func lbFormatSeconds(sec float64) string {
	return (time.Duration(sec * float64(time.Second))).Round(time.Millisecond).String()
}

// TestLargeBenchHarness is the full test entry point. It is gated by
// LARGE_BENCH=1 and runs for clients x duration across four phases after
// preloading the target dataset.
func TestLargeBenchHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("large-data benchmark skipped in -short")
	}
	cfg := lbConfigFromEnv(t, false, "test")
	lbRun(t, cfg)
}

// BenchmarkLargeData is the benchmark entry point. All real measurement is
// internal (throughput is ops/wall-clock per phase), so the framework's ns/op
// is suppressed. Use -benchtime=1x: the harness intentionally ignores b.N
// because one full pass already exceeds the default benchtime.
func BenchmarkLargeData(b *testing.B) {
	if testing.Short() {
		b.Skip("large-data benchmark skipped in -short")
	}
	cfg := lbConfigFromEnv(b, false, "benchmark")
	b.ResetTimer()
	sum := lbRun(b, cfg)
	b.StopTimer()

	b.ReportMetric(0, "ns/op")
	b.ReportMetric(float64(sum.TotalOps), "ops")
	b.ReportMetric(float64(sum.TotalErrors), "errors")
	b.ReportMetric(float64(sum.LogicalBytes)/float64(lbGiB), "dataset-GiB")
	b.ReportMetric(float64(sum.FinalDiskBytes)/float64(lbGiB), "disk-GiB")
	for _, p := range sum.Phases {
		b.ReportMetric(p.P50MS, "p50-ms/"+p.Name)
		b.ReportMetric(p.P95MS, "p95-ms/"+p.Name)
		b.ReportMetric(p.P99MS, "p99-ms/"+p.Name)
	}
}

// TestLargeBenchHarnessSmoke is the fast, always-gated smoke test. It proves
// the harness can build a cluster, preload a small dataset, measure every
// phase, and produce non-zero p99 latencies without errors. It is skipped
// under -short and when LARGE_BENCH is unset.
func TestLargeBenchHarnessSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("large-data benchmark smoke skipped in -short")
	}
	cfg := lbConfigFromEnv(t, true, "smoke")
	sum := lbRun(t, cfg)

	if sum.TotalErrors != 0 {
		t.Fatalf("large-bench smoke: %d operation errors across phases (first: %s)",
			sum.TotalErrors, firstPhaseError(sum.Phases))
	}
	if sum.VerifyErrors != 0 {
		t.Fatalf("large-bench smoke: %d sample verification errors", sum.VerifyErrors)
	}
	if sum.KeyCount <= 0 {
		t.Fatalf("large-bench smoke: no keys preloaded")
	}
	for _, p := range sum.Phases {
		if p.Ops == 0 {
			t.Fatalf("large-bench smoke: phase %s measured no operations", p.Name)
		}
		if p.P99MS <= 0 {
			t.Fatalf("large-bench smoke: phase %s has non-positive p99 (%v ms)", p.Name, p.P99MS)
		}
	}
}

func firstPhaseError(phases []lbPhaseResult) string {
	for _, p := range phases {
		if p.FirstError != "" {
			return p.Name + ": " + p.FirstError
		}
	}
	return "unknown"
}
