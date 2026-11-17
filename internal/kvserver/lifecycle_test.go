package kvserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// lifecycleBaseConfig builds a quiet single-node, single-range server. The
// caller decides which lifecycle loops to enable; by default they are disabled
// so a test only exercises what it asks for.
func lifecycleBaseConfig(t *testing.T) Config {
	t.Helper()
	addr := testFreeAddr(t)
	return Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		ElectionTimeoutMin: 40 * time.Millisecond,
		ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
		// Negative durations disable a loop explicitly (see
		// resolveLifecycleConfig). Individual tests enable only what they need.
		SnapshotInterval:  -1,
		GCInterval:        -1,
		DiskCheckInterval: -1,
	}
}

func newLifecycleTestServer(t *testing.T, mutate func(*Config)) *Server {
	t.Helper()
	cfg := lifecycleBaseConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new lifecycle server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("start lifecycle server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("stop lifecycle server: %v", err)
		}
	})
	waitRangeLeader(t, srv, 1)
	return srv
}

func dialLifecycleKV(t *testing.T, srv *Server) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

func waitForLifecycle(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func putLifecycleVersion(t *testing.T, g *rangeGroup, key, value string, startTS, commitTS uint64) {
	t.Helper()
	mutation := mvcc.Mutation{Key: []byte(key), Value: []byte(value)}
	if err := g.store.Prewrite([]mvcc.Mutation{mutation}, []byte(key), startTS); err != nil {
		t.Fatalf("prewrite %q at %d: %v", key, startTS, err)
	}
	if err := g.store.Commit([][]byte{[]byte(key)}, startTS, commitTS); err != nil {
		t.Fatalf("commit %q at %d: %v", key, commitTS, err)
	}
}

func readLifecycleValue(t *testing.T, g *rangeGroup, key string, ts uint64) string {
	t.Helper()
	got, err := g.store.Get([]byte(key), ts)
	if err != nil {
		t.Fatalf("get %q at %d: %v", key, ts, err)
	}
	return string(got)
}

// TestLifecycleDefaults documents and locks in the production-safe defaults.
func TestLifecycleDefaults(t *testing.T) {
	lc := resolveLifecycleConfig(Config{})
	if lc.snapshotInterval != DefaultSnapshotInterval {
		t.Fatalf("snapshot default = %v, want %v", lc.snapshotInterval, DefaultSnapshotInterval)
	}
	if lc.gcInterval != DefaultGCInterval {
		t.Fatalf("gc default = %v, want %v", lc.gcInterval, DefaultGCInterval)
	}
	if lc.gcSafePointLag != DefaultGCSafePointLag {
		t.Fatalf("gc lag default = %v, want %v", lc.gcSafePointLag, DefaultGCSafePointLag)
	}
	if lc.diskCheckInterval != DefaultDiskCheckInterval {
		t.Fatalf("disk interval default = %v, want %v", lc.diskCheckInterval, DefaultDiskCheckInterval)
	}
	if lc.minFreeDiskBytes != DefaultMinFreeDiskBytes {
		t.Fatalf("min free default = %d, want %d", lc.minFreeDiskBytes, DefaultMinFreeDiskBytes)
	}

	disabled := resolveLifecycleConfig(Config{DisableBackgroundLifecycle: true})
	if disabled.snapshotInterval != 0 || disabled.gcInterval != 0 || disabled.diskCheckInterval != 0 {
		t.Fatalf("disable flag left loops on: %+v", disabled)
	}
	negative := resolveLifecycleConfig(Config{SnapshotInterval: -1, GCInterval: -1, DiskCheckInterval: -1})
	if negative.snapshotInterval != 0 || negative.gcInterval != 0 || negative.diskCheckInterval != 0 {
		t.Fatalf("negative intervals did not disable loops: %+v", negative)
	}
	// Once any lifecycle field is set, zero means "disabled" as requested for
	// tests/dev; the unset loops must not inherit the production defaults.
	partial := resolveLifecycleConfig(Config{SnapshotInterval: 10 * time.Millisecond})
	if partial.snapshotInterval != 10*time.Millisecond {
		t.Fatalf("partial snapshot interval = %v", partial.snapshotInterval)
	}
	if partial.gcInterval != 0 || partial.diskCheckInterval != 0 {
		t.Fatalf("partial config silently enabled unset loops: %+v", partial)
	}
	if partial.minFreeDiskBytes != DefaultMinFreeDiskBytes {
		t.Fatalf("partial config lost the disk floor default: %d", partial.minFreeDiskBytes)
	}
	// Tuning the floor or retention must not disable the loops.
	tuned := resolveLifecycleConfig(Config{MinFreeDiskBytes: 1 << 30, GCSafePointLag: time.Hour})
	if tuned.snapshotInterval != DefaultSnapshotInterval || tuned.gcInterval != DefaultGCInterval {
		t.Fatalf("tuning floor/retention disabled maintenance: %+v", tuned)
	}
}

// TestLifecycleSnapshotPersistsAndStops verifies the existing Raft snapshot
// path is driven by the loop: a snapshot file appears, the WAL records the safe
// prefix truncation, stats advance, and Stop joins the lifecycle goroutine.
func TestLifecycleSnapshotPersistsAndStops(t *testing.T) {
	var dir string
	srv := newLifecycleTestServer(t, func(c *Config) {
		c.SnapshotInterval = 20 * time.Millisecond
		c.GCInterval = -1
		c.DiskCheckInterval = -1
		dir = c.DataDir
	})
	client := dialLifecycleKV(t, srv)
	putStatsKV(t, client, "snapshot-key", "value", 1000)

	applied := srv.groups[1].node.Status().LastApplied
	if applied == 0 {
		t.Fatal("no log entry applied; snapshot test needs committed state")
	}

	metaPath := filepath.Join(dir, "raft", "range-1", "snapshot", "meta.json")
	var (
		snapshotIndex uint64
		snapshotFile  string
	)
	waitForLifecycle(t, "persisted snapshot meta", func() bool {
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			return false
		}
		var meta struct {
			Index    uint64 `json:"index"`
			DataFile string `json:"data_file"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Index == 0 || meta.DataFile == "" {
			return false
		}
		if meta.Index < applied {
			return false
		}
		snapshotIndex, snapshotFile = meta.Index, meta.DataFile
		return true
	})
	if _, err := os.Stat(filepath.Join(dir, "raft", "range-1", "snapshot", snapshotFile)); err != nil {
		t.Fatalf("snapshot data file %q: %v", snapshotFile, err)
	}
	if snapshotIndex < applied {
		t.Fatalf("snapshot index %d < applied %d", snapshotIndex, applied)
	}

	// The FileStore WAL is append-only; a safe truncation is a durable
	// truncate_prefix record written only after the snapshot was persisted.
	walPath := filepath.Join(dir, "raft", "range-1", "wal.log")
	waitForLifecycle(t, "truncate_prefix WAL record", func() bool {
		raw, err := os.ReadFile(walPath)
		return err == nil && bytes.Contains(raw, []byte("truncate_prefix"))
	})

	stats := srv.LifecycleStats()
	if stats.SnapshotRuns == 0 {
		t.Fatal("LifecycleStats.SnapshotRuns = 0 after a snapshot")
	}
	waitForLifecycle(t, "lifecycle stats to catch up with the snapshot", func() bool {
		cur := srv.LifecycleStats()
		return cur.LastSnapshotIndex >= applied && cur.SnapshotIndexes[1] >= applied
	})
	stats = srv.LifecycleStats()
	if stats.LastSnapshotIndex < applied {
		t.Fatalf("LastSnapshotIndex = %d, want >= %d", stats.LastSnapshotIndex, applied)
	}
	if got := stats.SnapshotIndexes[1]; got < applied {
		t.Fatalf("SnapshotIndexes[1] = %d, want >= %d", got, applied)
	}
	if stats.LastSnapshot.IsZero() {
		t.Fatal("LastSnapshot is zero after a snapshot")
	}

	done := srv.lifecycleDone
	if done == nil {
		t.Fatal("lifecycle goroutine was not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lifecycle goroutine did not stop with Server.Stop")
	}
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestLifecycleSnapshotsEveryGroup checks the server-level loop visits each
// hosted range rather than only the default one.
func TestLifecycleSnapshotsEveryGroup(t *testing.T) {
	var dir string
	srv := newLifecycleTestServer(t, func(c *Config) {
		c.Ranges = []RangeConfig{
			{ID: 1, End: []byte("m")},
			{ID: 2, Start: []byte("m")},
		}
		c.SnapshotInterval = 20 * time.Millisecond
		c.GCInterval = -1
		c.DiskCheckInterval = -1
		dir = c.DataDir
	})
	waitRangeLeader(t, srv, 1)
	waitRangeLeader(t, srv, 2)
	client := dialLifecycleKV(t, srv)
	putStatsKV(t, client, "alpha", "one", 1000)
	putStatsKV(t, client, "zulu", "two", 2000)

	for _, id := range []int{1, 2} {
		meta := filepath.Join(dir, "raft", fmt.Sprintf("range-%d", id), "snapshot", "meta.json")
		waitForLifecycle(t, fmt.Sprintf("snapshot meta for range %d", id), func() bool {
			raw, err := os.ReadFile(meta)
			if err != nil {
				return false
			}
			var m struct {
				Index uint64 `json:"index"`
			}
			return json.Unmarshal(raw, &m) == nil && m.Index > 0
		})
	}
}

// TestLifecycleGCRemovesOldVersionsPreservesReadable exercises the provider
// safe point: superseded versions below it are collected, while the newest base
// version and every version at/above the safe point remain readable.
func TestLifecycleGCRemovesOldVersionsPreservesReadable(t *testing.T) {
	// Use TSO-shaped timestamps near the wall clock: the background lock
	// resolver interprets startTS>>18 as milliseconds, so arbitrary small
	// values would look expired (the first test deliberately leaves no locks,
	// but the second one needs a live lock).
	base := uint64(time.Now().UnixMilli()) << 18
	var safePoint atomic.Uint64
	srv := newLifecycleTestServer(t, func(c *Config) {
		c.SnapshotInterval = -1
		c.GCInterval = 10 * time.Millisecond
		c.DiskCheckInterval = -1
		c.GCSafePoint = func() uint64 { return safePoint.Load() }
	})
	g := srv.groups[1]
	putLifecycleVersion(t, g, "a", "v1", base+10, base+20)
	putLifecycleVersion(t, g, "a", "v2", base+30, base+40)
	putLifecycleVersion(t, g, "a", "v3", base+50, base+60)

	safePoint.Store(base + 45)
	waitForLifecycle(t, "GC pass at safe point base+45", func() bool {
		st := srv.LifecycleStats()
		return st.GCRuns > 0 && st.LastGCSafePoint == base+45 && st.GCDeletedVersions >= 2
	})

	if got := readLifecycleValue(t, g, "a", base+45); got != "v2" {
		t.Fatalf("get a@base+45 = %q, want v2 (base version at/below safe point)", got)
	}
	if got := readLifecycleValue(t, g, "a", base+60); got != "v3" {
		t.Fatalf("get a@base+60 = %q, want v3", got)
	}
	if got := readLifecycleValue(t, g, "a", base+100); got != "v3" {
		t.Fatalf("get a@base+100 = %q, want v3", got)
	}
	// The old version is no longer visible at an older snapshot; this is the
	// expected consequence of GC below the safe point, not a lost latest read.
	if _, err := g.store.Get([]byte("a"), base+30); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("get a@base+30 error = %v, want ErrKeyNotFound after collecting version base+20", err)
	}
}

// TestLifecycleGCCappedByOldestLock proves a live lock prevents the safe point
// from passing its startTS, so GC cannot collect versions that transaction may
// still need. After the lock is resolved the safe point advances.
func TestLifecycleGCCappedByOldestLock(t *testing.T) {
	base := uint64(time.Now().UnixMilli()) << 18
	var safePoint atomic.Uint64
	srv := newLifecycleTestServer(t, func(c *Config) {
		c.SnapshotInterval = -1
		c.GCInterval = 10 * time.Millisecond
		c.DiskCheckInterval = -1
		c.GCSafePoint = func() uint64 { return safePoint.Load() }
	})
	g := srv.groups[1]
	putLifecycleVersion(t, g, "a", "v1", base+10, base+20)
	putLifecycleVersion(t, g, "a", "v2", base+30, base+40)
	putLifecycleVersion(t, g, "a", "v3", base+50, base+60)

	// base+25 has the same physical millisecond as now, so the lock is not
	// expired and the resolver leaves it alone during this short test.
	if err := g.store.Prewrite([]mvcc.Mutation{{Key: []byte("b"), Value: []byte("bv")}}, []byte("b"), base+25); err != nil {
		t.Fatalf("prewrite b: %v", err)
	}
	safePoint.Store(base + 45)
	waitForLifecycle(t, "GC capped at oldest lock", func() bool {
		st := srv.LifecycleStats()
		return st.GCRuns > 0 && st.GCSafePoints[1] == base+25
	})
	st := srv.LifecycleStats()
	if st.LastGCSafePoint != base+25 {
		t.Fatalf("LastGCSafePoint = %d, want %d (oldest lock)", st.LastGCSafePoint, base+25)
	}
	if st.GCDeletedVersions != 0 {
		t.Fatalf("GC deleted %d versions despite a lock at base+25", st.GCDeletedVersions)
	}
	if _, err := g.store.Get([]byte("b"), base+100); !errors.Is(err, mvcc.ErrLocked) {
		t.Fatalf("get b = %v, want ErrLocked before resolution", err)
	}

	if err := g.store.Rollback([][]byte{[]byte("b")}, base+25); err != nil {
		t.Fatalf("rollback b: %v", err)
	}
	waitForLifecycle(t, "GC advances after lock resolution", func() bool {
		cur := srv.LifecycleStats()
		return cur.LastGCSafePoint == base+45 && cur.GCDeletedVersions >= 3
	})
	if got := readLifecycleValue(t, g, "a", base+45); got != "v2" {
		t.Fatalf("get a@base+45 = %q, want v2", got)
	}
	if got := readLifecycleValue(t, g, "a", base+100); got != "v3" {
		t.Fatalf("get a@base+100 = %q, want v3", got)
	}
}

// TestDiskGuardRejectsWritesAndRecovers checks the fail-fast ENOSPC gate: a
// high floor flips the guard during New, writes return ErrDiskFull, and once
// the guard observes free space again the same engine accepts writes.
func TestDiskGuardRejectsWritesAndRecovers(t *testing.T) {
	cfg := lifecycleBaseConfig(t)
	cfg.MinFreeDiskBytes = 1 << 62 // force the proactive guard to trip
	cfg.DisableBackgroundLifecycle = true

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if !srv.DiskFull() {
		t.Fatal("DiskFull = false after a floor the filesystem cannot meet")
	}
	if err := srv.engine.Set([]byte("k"), []byte("v")); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("engine.Set error = %v, want ErrDiskFull", err)
	}
	if err := srv.groups[1].store.Prewrite(
		[]mvcc.Mutation{{Key: []byte("k"), Value: []byte("v")}}, []byte("k"), 10); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("mvcc.Prewrite error = %v, want ErrDiskFull", err)
	}
	if st := srv.LifecycleStats(); !st.DiskFull {
		t.Fatalf("LifecycleStats.DiskFull = false: %+v", st)
	}

	// Simulate the operator freeing space / raising the floor. The engine
	// must resume accepting writes without a restart.
	srv.lifecycle.disk.threshold.Store(0)
	srv.lifecycle.checkDisk(time.Now())
	if srv.DiskFull() {
		t.Fatal("DiskFull stayed true after free space recovered")
	}
	if err := srv.engine.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("engine.Set after recovery: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRangeLeader(t, srv, 1)
	if err := srv.groups[1].store.Prewrite(
		[]mvcc.Mutation{{Key: []byte("w"), Value: []byte("v")}}, []byte("w"), 20); err != nil {
		t.Fatalf("prewrite after recovery: %v", err)
	}
}

// TestDiskGuardReturnsClearRPCError checks the client-visible contract: a
// mutating RPC fails with ResourceExhausted and an actionable message while a
// read RPC still succeeds.
func TestDiskGuardReturnsClearRPCError(t *testing.T) {
	cfg := lifecycleBaseConfig(t)
	cfg.MinFreeDiskBytes = 1 << 62
	cfg.DisableBackgroundLifecycle = true
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	waitRangeLeader(t, srv, 1)
	client := dialLifecycleKV(t, srv)

	_, err = client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: 1,
		Primary: []byte("k"),
		Mutations: []*pbv2.Mutation{{
			Key: []byte("k"), Value: []byte("v"),
		}},
	})
	if err == nil {
		t.Fatal("Prewrite succeeded on a full disk")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("Prewrite status = %v (%v), want ResourceExhausted", got, err)
	}
	if !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("Prewrite error %q does not say the disk is full", err)
	}

	resp, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("k"), Ts: 1})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("read while full: resp=%+v err=%v", resp, err)
	}
	if resp.GetFound() {
		t.Fatal("Get reported a value while all writes were rejected")
	}
}

// TestDiskGuardLatchesStorageError checks that an ENOSPC returned by storage
// (rather than the periodic probe) latches the guard and is still matchable.
func TestDiskGuardLatchesStorageError(t *testing.T) {
	state := newDiskGuardState(t.TempDir(), 0, testLogger())
	err := state.noteWriteError("batch commit", fmt.Errorf("pebble: %w", syscall.ENOSPC))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("error = %v, want ErrDiskFull", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("error = %v, want underlying ENOSPC preserved", err)
	}
	if !state.isFull() {
		t.Fatal("ENOSPC did not latch the disk guard")
	}
	if err := state.check(); err != nil {
		t.Fatalf("check: %v", err)
	}
	if state.isFull() {
		t.Fatal("guard did not clear after a successful probe with a zero floor")
	}
}

// TestLifecycleMetricsRegistered checks the optional Prometheus surface is
// wired without requiring a full snapshot/GC cycle.
func TestLifecycleMetricsRegistered(t *testing.T) {
	reg := metrics.NewRegistry()
	_ = newLifecycleTestServer(t, func(c *Config) {
		c.LifecycleMetrics = reg
		c.SnapshotInterval = -1
		c.GCInterval = -1
		c.DiskCheckInterval = -1
	})
	names := make(map[string]bool)
	for _, m := range reg.Snapshot().Metrics {
		names[m.Name] = true
	}
	for _, name := range []string{
		"kvserver_snapshot_runs_total",
		"kvserver_snapshot_errors_total",
		"kvserver_snapshot_age_seconds",
		"kvserver_last_snapshot_index",
		"kvserver_gc_runs_total",
		"kvserver_gc_errors_total",
		"kvserver_gc_deleted_versions_total",
		"kvserver_gc_safe_point",
		"kvserver_disk_full",
		"kvserver_disk_free_bytes",
		"kvserver_disk_checks_total",
	} {
		if !names[name] {
			t.Fatalf("metric %q not registered", name)
		}
	}
}

// TestLifecycleDisabledStartsNoGoroutine verifies the tests/dev escape hatch.
func TestLifecycleDisabledStartsNoGoroutine(t *testing.T) {
	srv := newLifecycleTestServer(t, func(c *Config) {
		c.DisableBackgroundLifecycle = true
	})
	if srv.lifecycle.enabled() {
		t.Fatal("lifecycle reports enabled after DisableBackgroundLifecycle")
	}
	if srv.lifecycleDone != nil {
		t.Fatal("lifecycle goroutine started despite disable flag")
	}
}
