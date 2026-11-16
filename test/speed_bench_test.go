package test

// Permanent speed-benchmark harness for the v2 cluster.
//
// Every benchmark starts one in-process meta service plus a three-node KV Raft
// group over loopback, using throwaway data directories. Setup (cluster start,
// leader election, seeding) happens before the benchmark timer is reset, so
// the reported numbers cover only the measured client operations.
//
// Run:
//
//	go test -run '^$' -bench . -benchtime=30x ./test
//
// Timings are reported, never asserted on: CI can compare the output over time
// and use TestV2SpeedSmoke as a cheap non-benchmark regression signal.

import (
	"bytes"
	"context"
	"net"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

const (
	speedCacheSize    = 64 << 20
	speedMemTableSize = 32 << 20
	speedSeedBatch    = 64
	speedGetKeys      = 256
	speedScanRows     = 512
)

// speedValue is a small, fixed value so key formatting stays cheap and update
// paths are comparable across runs.
var speedValue = []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

// speedCluster is one meta service plus a three-node KV group.
type speedCluster struct {
	metaAddr string
	leader   *v2Node
	client   *v2client.Client
}

func speedFreeAddr(tb testing.TB) string {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("speed bench: reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// speedWaitLeader polls each KV node's local Raft role. Waiting locally avoids
// an extra gRPC probe path and matches how the existing integration tests
// verify leadership.
func speedWaitLeader(tb testing.TB, nodes []*v2Node) *v2Node {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.srv.Raft().Status().Role == "leader" {
				return n
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	tb.Fatalf("speed bench: no leader among %d KV nodes", len(nodes))
	return nil
}

// newSpeedCluster starts the meta service and three KV nodes, waits for a
// leader, and registers teardown with tb.Cleanup. All listeners are on
// 127.0.0.1 and all storage is under tb.TempDir().
func newSpeedCluster(tb testing.TB) *speedCluster {
	tb.Helper()
	ctx := context.Background()

	metaAddr := speedFreeAddr(tb)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		tb.Fatalf("speed bench: meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		tb.Fatalf("speed bench: start meta: %v", err)
	}
	tb.Cleanup(func() { _ = ms.Stop() })

	addrs := []string{speedFreeAddr(tb), speedFreeAddr(tb), speedFreeAddr(tb)}
	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		cfg := kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            tb.TempDir(),
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          speedCacheSize,
			MemTableSize:       speedMemTableSize,
			Logger:             v2Logger(),
			// Benchmarks measure the explicit fast profile; the production
			// default is sync and is intentionally slower.
			DurabilityMode: kvserver.DurabilityFast,
		}
		srv, err := kvserver.New(cfg)
		if err != nil {
			tb.Fatalf("speed bench: new node %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			tb.Fatalf("speed bench: start node %s: %v", addr, err)
		}
		nodes = append(nodes, &v2Node{srv: srv, addr: addr, cfg: cfg})
		tb.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}

	leader := speedWaitLeader(tb, nodes)
	client, err := v2client.New(metaAddr, addrs)
	if err != nil {
		tb.Fatalf("speed bench: client: %v", err)
	}
	tb.Cleanup(func() { _ = client.Close() })
	return &speedCluster{metaAddr: metaAddr, leader: leader, client: client}
}

// speedWarmup performs one throwaway transaction so the first measured
// operation does not absorb client leader discovery and gRPC dialing.
func speedWarmup(tb testing.TB, c *v2client.Client) {
	tb.Helper()
	ctx := context.Background()
	txn, err := c.Begin(ctx)
	if err != nil {
		tb.Fatalf("speed bench: warmup begin: %v", err)
	}
	txn.Set([]byte("bench-warmup/key"), speedValue)
	if err := txn.Commit(ctx); err != nil {
		tb.Fatalf("speed bench: warmup commit: %v", err)
	}
}

// speedAppendKey appends decimal id to prefix into dst, reusing dst's backing
// array. Callers that retain the key must copy it (Txn.Set already copies).
func speedAppendKey(dst []byte, prefix string, id uint64) []byte {
	dst = dst[:0]
	dst = append(dst, prefix...)
	return strconv.AppendUint(dst, id, 10)
}

// speedSeed writes n unique keys under prefix in batched transactions. It runs
// outside the measured region and returns the individual keys.
func speedSeed(tb testing.TB, c *v2client.Client, prefix string, n int) [][]byte {
	tb.Helper()
	ctx := context.Background()
	keys := make([][]byte, n)
	buf := make([]byte, 0, 64)
	for base := 0; base < n; base += speedSeedBatch {
		end := base + speedSeedBatch
		if end > n {
			end = n
		}
		txn, err := c.Begin(ctx)
		if err != nil {
			tb.Fatalf("speed bench: seed begin: %v", err)
		}
		for i := base; i < end; i++ {
			key := speedAppendKey(buf, prefix, uint64(i))
			keys[i] = append([]byte(nil), key...)
			txn.Set(key, speedValue)
		}
		if err := txn.Commit(ctx); err != nil {
			tb.Fatalf("speed bench: seed commit [%d,%d): %v", base, end, err)
		}
	}
	return keys
}

// BenchmarkV2Txn3Node measures sequential Begin/Set/Commit transactions with
// one key each.
func BenchmarkV2Txn3Node(b *testing.B) {
	c := newSpeedCluster(b)
	speedWarmup(b, c.client)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	buf := make([]byte, 0, 64)
	for i := 0; i < b.N; i++ {
		txn, err := c.client.Begin(ctx)
		if err != nil {
			b.Fatalf("speed bench: begin: %v", err)
		}
		txn.Set(speedAppendKey(buf, "bench-txn/", uint64(i)), speedValue)
		if err := txn.Commit(ctx); err != nil {
			b.Fatalf("speed bench: commit: %v", err)
		}
	}
}

// BenchmarkV2Txn3NodeParallel measures Begin/Set/Commit transactions from
// several goroutines. Keys come from one atomic counter, so they remain unique
// without coordinating on the hot path.
func BenchmarkV2Txn3NodeParallel(b *testing.B) {
	c := newSpeedCluster(b)
	speedWarmup(b, c.client)
	ctx := context.Background()
	var seq atomic.Uint64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 0, 64)
		for pb.Next() {
			txn, err := c.client.Begin(ctx)
			if err != nil {
				b.Errorf("speed bench: parallel begin: %v", err)
				return
			}
			txn.Set(speedAppendKey(buf, "bench-par/", seq.Add(1)), speedValue)
			if err := txn.Commit(ctx); err != nil {
				b.Errorf("speed bench: parallel commit: %v", err)
				return
			}
		}
	})
}

// BenchmarkV2Get3Node seeds a key space, then performs a fresh Begin/Get per
// operation. Begin reserves a new TSO, so this measures the read path at a
// current snapshot rather than a cached timestamp.
func BenchmarkV2Get3Node(b *testing.B) {
	c := newSpeedCluster(b)
	keys := speedSeed(b, c.client, "bench-get/", speedGetKeys)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn, err := c.client.Begin(ctx)
		if err != nil {
			b.Fatalf("speed bench: get begin: %v", err)
		}
		got, err := txn.Get(ctx, keys[i%len(keys)])
		if err != nil {
			b.Fatalf("speed bench: get: %v", err)
		}
		if !bytes.Equal(got, speedValue) {
			b.Fatalf("speed bench: get returned %d bytes, want %d", len(got), len(speedValue))
		}
	}
}

// BenchmarkV2Scan3Node seeds speedScanRows keys, then scans the whole bounded
// key range at a fresh snapshot per operation.
func BenchmarkV2Scan3Node(b *testing.B) {
	c := newSpeedCluster(b)
	speedSeed(b, c.client, "bench-scan/", speedScanRows)
	start, end := []byte("bench-scan/"), []byte("bench-scan0")
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn, err := c.client.Begin(ctx)
		if err != nil {
			b.Fatalf("speed bench: scan begin: %v", err)
		}
		rows, err := txn.Scan(ctx, start, end, 0)
		if err != nil {
			b.Fatalf("speed bench: scan: %v", err)
		}
		if len(rows) != speedScanRows {
			b.Fatalf("speed bench: scan returned %d rows, want %d", len(rows), speedScanRows)
		}
	}
	b.ReportMetric(float64(speedScanRows), "rows/op")
}

// TestV2SpeedSmoke is the cheap, always-on regression signal. It writes and
// reads a handful of keys and logs throughput and latency; it never asserts on
// timing so a loaded CI machine cannot make it flaky.
func TestV2SpeedSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("speed smoke test skipped in -short")
	}
	c := newSpeedCluster(t)
	ctx := context.Background()
	speedWarmup(t, c.client)
	t.Logf("v2 smoke cluster: meta=%s leader=%s", c.metaAddr, c.leader.addr)

	const txns = 24
	keys := make([][]byte, txns)
	latencies := make([]time.Duration, 0, txns)
	buf := make([]byte, 0, 64)

	started := time.Now()
	for i := 0; i < txns; i++ {
		opStart := time.Now()
		txn, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("smoke: begin: %v", err)
		}
		key := append([]byte(nil), speedAppendKey(buf, "smoke-txn/", uint64(i))...)
		keys[i] = key
		txn.Set(key, speedValue)
		if err := txn.Commit(ctx); err != nil {
			t.Fatalf("smoke: commit: %v", err)
		}
		latencies = append(latencies, time.Since(opStart))
	}
	elapsed := time.Since(started)
	minLat, avgLat, p95Lat, maxLat := speedLatencyStats(latencies)
	t.Logf("v2 smoke txns: n=%d elapsed=%s throughput=%.1f ops/sec latency: min=%s avg=%s p95=%s max=%s",
		txns, elapsed, float64(txns)/elapsed.Seconds(), minLat, avgLat, p95Lat, maxLat)

	latencies = latencies[:0]
	started = time.Now()
	for i, key := range keys {
		opStart := time.Now()
		txn, err := c.client.Begin(ctx)
		if err != nil {
			t.Fatalf("smoke: get begin: %v", err)
		}
		got, err := txn.Get(ctx, key)
		if err != nil {
			t.Fatalf("smoke: get %d: %v", i, err)
		}
		if !bytes.Equal(got, speedValue) {
			t.Fatalf("smoke: get %d returned %q", i, got)
		}
		latencies = append(latencies, time.Since(opStart))
	}
	elapsed = time.Since(started)
	minLat, avgLat, p95Lat, maxLat = speedLatencyStats(latencies)
	t.Logf("v2 smoke gets: n=%d elapsed=%s throughput=%.1f ops/sec latency: min=%s avg=%s p95=%s max=%s",
		txns, elapsed, float64(txns)/elapsed.Seconds(), minLat, avgLat, p95Lat, maxLat)

	opStart := time.Now()
	txn, err := c.client.Begin(ctx)
	if err != nil {
		t.Fatalf("smoke: scan begin: %v", err)
	}
	rows, err := txn.Scan(ctx, []byte("smoke-txn/"), []byte("smoke-txn0"), 0)
	if err != nil {
		t.Fatalf("smoke: scan: %v", err)
	}
	if len(rows) != txns {
		t.Fatalf("smoke: scan returned %d rows, want %d", len(rows), txns)
	}
	t.Logf("v2 smoke scan: n=%d elapsed=%s", len(rows), time.Since(opStart))
}

// speedLatencyStats returns min/avg/p95/max without requiring any assertion.
func speedLatencyStats(samples []time.Duration) (min, avg, p95, max time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	min = sorted[0]
	max = sorted[len(sorted)-1]
	avg = total / time.Duration(len(sorted))
	idx := (95*len(sorted) - 1) / 100
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	p95 = sorted[idx]
	return min, avg, p95, max
}
