package kvserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"distributed-db/internal/metrics"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Storage lifecycle defaults. The zero Config selects these values so that a
// production process which does not tune lifecycle explicitly still compacts
// its Raft logs and collects old MVCC versions.
const (
	// DefaultSnapshotInterval is how often every hosted range asks Raft for a
	// state-machine snapshot and compacts its local log prefix.
	DefaultSnapshotInterval = 10 * time.Minute
	// DefaultGCInterval is how often every hosted range runs an MVCC GC pass.
	DefaultGCInterval = 10 * time.Minute
	// DefaultGCSafePointLag is the conservative fallback retention window used
	// when Config.GCSafePoint is nil. A version is only collectible once it is
	// older than this window (and older than every live lock's startTS).
	DefaultGCSafePointLag = 10 * time.Minute
	// DefaultDiskCheckInterval is how often the node probes free disk space.
	DefaultDiskCheckInterval = time.Second
	// DefaultMinFreeDiskBytes is the free-space floor below which the node
	// rejects writes instead of risking a partially durable state.
	DefaultMinFreeDiskBytes int64 = 256 << 20
)

// ErrDiskFull is returned (wrapped) for every write the node rejects because
// storage is at or near ENOSPC. Reads and snapshots remain available.
var ErrDiskFull = errors.New("kvserver: storage disk is full; writes are temporarily rejected")

// LifecycleStats is a point-in-time copy of the storage lifecycle counters and
// gauges. It is safe to call while the background loop is running.
type LifecycleStats struct {
	// SnapshotRuns and SnapshotErrors count per-range snapshot attempts.
	SnapshotRuns   uint64
	SnapshotErrors uint64
	// LastSnapshot is the most recent successful snapshot attempt (across all
	// ranges); SnapshotAge is the age of that attempt at the time of the call.
	LastSnapshot time.Time
	SnapshotAge  time.Duration
	// LastSnapshotIndex is the highest snapshot index observed.
	LastSnapshotIndex uint64
	// SnapshotAges and SnapshotIndexes break the values down by range ID.
	SnapshotAges    map[uint64]time.Duration
	SnapshotIndexes map[uint64]uint64

	// GCRuns and GCErrors count range-level GC passes, not scheduler ticks.
	GCRuns            uint64
	GCErrors          uint64
	GCDeletedVersions uint64
	GCLocksRetained   uint64
	LastGC            time.Time
	LastGCSafePoint   uint64
	GCSafePoints      map[uint64]uint64

	// DiskFull is true while writes are rejected. DiskFreeBytes is the last
	// successful probe result, and DiskChecks counts probes.
	DiskFull      bool
	DiskFreeBytes int64
	DiskChecks    uint64
}

// lifecycleConfig is the resolved (defaulted) form of the lifecycle fields in
// Config. Keeping it separate from Config makes the defaults explicit and
// testable.
type lifecycleConfig struct {
	snapshotInterval  time.Duration
	gcInterval        time.Duration
	gcSafePointLag    time.Duration
	diskCheckInterval time.Duration
	minFreeDiskBytes  int64
	gcSafePoint       func() uint64
	metrics           *metrics.Registry
}

// resolveLifecycleConfig applies defaults and normalizes disabled loops to 0.
//
// Semantics:
//   - A completely zero lifecycle block (the zero-value Config, as used by the
//     production command) gets the safe defaults. This is what makes a node
//     that never configures maintenance still compact its Raft logs and GC old
//     MVCC versions.
//   - As soon as any lifecycle field is set, the caller is in explicit mode:
//     a 0 interval disables that loop (tests/dev), and a negative interval is
//     also treated as disabled. Loops the caller wants must be positive.
//   - DisableBackgroundLifecycle disables snapshot, GC and periodic disk
//     probes outright. The initial disk probe still runs.
//   - MinFreeDiskBytes 0 keeps the default floor; a negative value disables
//     the proactive floor (write-error ENOSPC detection stays enabled).
//   - GCSafePointLag <= 0 keeps the default retention window; a zero retention
//     window is never used because it would collect versions a just-started
//     read may still need.
func resolveLifecycleConfig(cfg Config) lifecycleConfig {
	lc := lifecycleConfig{
		snapshotInterval:  cfg.SnapshotInterval,
		gcInterval:        cfg.GCInterval,
		gcSafePointLag:    cfg.GCSafePointLag,
		diskCheckInterval: cfg.DiskCheckInterval,
		minFreeDiskBytes:  cfg.MinFreeDiskBytes,
		gcSafePoint:       cfg.GCSafePoint,
		metrics:           cfg.LifecycleMetrics,
	}
	// Only interval knobs opt into explicit mode. Tuning the free-space floor
	// or the retention window must not silently disable the maintenance loops
	// an operator did not mention.
	explicit := cfg.SnapshotInterval != 0 || cfg.GCInterval != 0 ||
		cfg.DiskCheckInterval != 0 || cfg.DisableBackgroundLifecycle

	if cfg.DisableBackgroundLifecycle {
		lc.snapshotInterval = 0
		lc.gcInterval = 0
		lc.diskCheckInterval = 0
	} else if !explicit {
		lc.snapshotInterval = DefaultSnapshotInterval
		lc.gcInterval = DefaultGCInterval
		lc.diskCheckInterval = DefaultDiskCheckInterval
	} else {
		// Explicit mode honors 0=disabled.
		if lc.snapshotInterval < 0 {
			lc.snapshotInterval = 0
		}
		if lc.gcInterval < 0 {
			lc.gcInterval = 0
		}
		if lc.diskCheckInterval < 0 {
			lc.diskCheckInterval = 0
		}
	}
	if lc.gcSafePointLag <= 0 {
		lc.gcSafePointLag = DefaultGCSafePointLag
	}
	switch {
	case lc.minFreeDiskBytes == 0:
		lc.minFreeDiskBytes = DefaultMinFreeDiskBytes
	case lc.minFreeDiskBytes < 0:
		lc.minFreeDiskBytes = 0
	}
	return lc
}

// diskGuardState is shared by the engine wrapper, the periodic prober and the
// lifecycle stats. A single atomic flag makes the write check cheap.
type diskGuardState struct {
	full      atomic.Bool
	freeBytes atomic.Int64
	threshold atomic.Int64
	reason    atomic.Value // string
	probe     func(string) (int64, error)
	path      string
	log       *slog.Logger
}

func newDiskGuardState(path string, minFree int64, log *slog.Logger) *diskGuardState {
	if log == nil {
		log = slog.Default()
	}
	d := &diskGuardState{
		probe: statfsFreeBytes,
		path:  path,
		log:   log,
	}
	d.threshold.Store(minFree)
	return d
}

// statfsFreeBytes reports the bytes available to an unprivileged writer on the
// filesystem holding path. It is a package variable so tests can substitute a
// deterministic probe without touching production storage code.
var statfsFreeBytes = func(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (d *diskGuardState) isFull() bool { return d.full.Load() }

func (d *diskGuardState) free() int64 { return d.freeBytes.Load() }

func (d *diskGuardState) minFree() int64 { return d.threshold.Load() }

func (d *diskGuardState) fullReason() string {
	if v := d.reason.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (d *diskGuardState) markFull(reason string) {
	if !d.full.CompareAndSwap(false, true) {
		return
	}
	d.reason.Store(reason)
	d.log.Error("storage disk full: rejecting writes",
		"path", d.path, "free_bytes", d.free(), "min_free_bytes", d.minFree(), "reason", reason)
}

func (d *diskGuardState) markRecovered(free int64) {
	if !d.full.CompareAndSwap(true, false) {
		return
	}
	d.reason.Store("")
	d.log.Info("storage recovered: accepting writes",
		"path", d.path, "free_bytes", free, "min_free_bytes", d.minFree())
}

// check probes the filesystem and flips the guard. A probe error leaves the
// previous state untouched: a transient statfs failure must not turn a healthy
// node read-only, and a full node must not silently reopen.
func (d *diskGuardState) check() error {
	if d.probe == nil {
		return errors.New("kvserver: no disk free-space probe configured")
	}
	free, err := d.probe(d.path)
	if err != nil {
		return err
	}
	d.freeBytes.Store(free)
	if threshold := d.threshold.Load(); threshold > 0 && free < threshold {
		d.markFull(fmt.Sprintf("free space %d bytes is below the %d-byte floor", free, threshold))
	} else if d.full.Load() {
		d.markRecovered(free)
	}
	return nil
}

// reject returns a clear, matchable error while writes are rejected.
func (d *diskGuardState) reject(op string) error {
	if !d.full.Load() {
		return nil
	}
	reason := d.fullReason()
	if reason == "" {
		reason = "free space is below the configured floor"
	}
	return fmt.Errorf("%w: %s rejected (%s)", ErrDiskFull, op, reason)
}

// noteWriteError observes a storage write error. ENOSPC and quota errors flip
// the guard immediately, even between periodic probes, so subsequent writes
// fail fast instead of repeatedly touching a full filesystem.
func (d *diskGuardState) noteWriteError(op string, err error) error {
	if err == nil || !isNoSpaceError(err) {
		return err
	}
	d.markFull(fmt.Sprintf("%s failed: %v", op, err))
	// Preserve both sentinels: callers can match ErrDiskFull for the API
	// contract and still inspect the underlying ENOSPC/EDQUOT error.
	return fmt.Errorf("%w: %s failed: %w", ErrDiskFull, op, err)
}

func isNoSpaceError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	// Pebble and some filesystems return opaque errors whose text is the only
	// portable signal. Keep this fallback narrow.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left on device") ||
		strings.Contains(msg, "disk quota exceeded")
}

// diskGuardEngine wraps the shared Pebble engine. Every group sees this
// wrapper through its storage.Prefix view, so all MVCC and state-machine
// writes pass through one fail-fast ENOSPC gate without changing storage.
type diskGuardEngine struct {
	inner storage.Engine
	state *diskGuardState
}

var _ storage.Engine = (*diskGuardEngine)(nil)

func newDiskGuardEngine(inner storage.Engine, state *diskGuardState) *diskGuardEngine {
	return &diskGuardEngine{inner: inner, state: state}
}

func (e *diskGuardEngine) Get(key []byte) ([]byte, error) { return e.inner.Get(key) }

func (e *diskGuardEngine) Set(key, value []byte) error {
	if err := e.state.reject("set"); err != nil {
		return err
	}
	return e.state.noteWriteError("set", e.inner.Set(key, value))
}

func (e *diskGuardEngine) Delete(key []byte) error {
	if err := e.state.reject("delete"); err != nil {
		return err
	}
	return e.state.noteWriteError("delete", e.inner.Delete(key))
}

func (e *diskGuardEngine) NewBatch() storage.Batch {
	return &diskGuardBatch{inner: e.inner.NewBatch(), state: e.state}
}

func (e *diskGuardEngine) NewIter(start, end []byte) (storage.Iterator, error) {
	return e.inner.NewIter(start, end)
}

func (e *diskGuardEngine) Snapshot() (storage.Snapshot, error) { return e.inner.Snapshot() }

func (e *diskGuardEngine) Flush() error {
	// Flush is maintenance, not a client write: do not reject it while the
	// guard is tripped (a successful flush can free WAL space), but still
	// record an ENOSPC so the guard latches the failure.
	return e.state.noteWriteError("flush", e.inner.Flush())
}

func (e *diskGuardEngine) Close() error { return e.inner.Close() }

func (e *diskGuardEngine) Stats() storage.Stats { return e.inner.Stats() }

// diskGuardBatch gates Commit, the point where staged mutations become a
// durable write. Set/Delete only stage; a rejected commit leaves the caller's
// in-memory batch untouched, matching the normal "batch must be closed" rule.
type diskGuardBatch struct {
	inner storage.Batch
	state *diskGuardState
}

var _ storage.Batch = (*diskGuardBatch)(nil)

func (b *diskGuardBatch) Set(key, value []byte) error { return b.inner.Set(key, value) }

func (b *diskGuardBatch) Delete(key []byte) error { return b.inner.Delete(key) }

func (b *diskGuardBatch) Commit() error {
	if err := b.state.reject("batch commit"); err != nil {
		return err
	}
	return b.state.noteWriteError("batch commit", b.inner.Commit())
}

func (b *diskGuardBatch) Close() error { return b.inner.Close() }

func (b *diskGuardBatch) Count() int { return b.inner.Count() }

// diskGuardUnaryInterceptor rejects mutating KV RPCs while the disk guard is
// tripped. Without it the service would still return a response with the
// generic "internal error" string (opError does not know ErrDiskFull), which is
// not actionable for clients. Read-only RPCs continue to be served.
func (s *Server) diskGuardUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if s.lifecycle != nil && s.lifecycle.disk.isFull() && isMutatingKVRPC(info.FullMethod) {
		return nil, status.Error(codes.ResourceExhausted,
			"kvserver: storage disk is full; writes are temporarily rejected")
	}
	return handler(ctx, req)
}

func isMutatingKVRPC(fullMethod string) bool {
	switch fullMethod {
	case pbv2.KVService_Prewrite_FullMethodName,
		pbv2.KVService_Commit_FullMethodName,
		pbv2.KVService_CommitSingle_FullMethodName,
		pbv2.KVService_Rollback_FullMethodName,
		pbv2.KVService_ResolveLock_FullMethodName,
		pbv2.KVService_SplitRange_FullMethodName,
		pbv2.KVService_MergeRanges_FullMethodName:
		return true
	default:
		return false
	}
}

// serverLifecycle owns the single background maintenance goroutine of a
// Server. Snapshot and GC passes are serial per server and visit ranges in
// range order, so a large number of ranges cannot produce an unbounded fan-out
// of snapshot/compaction goroutines.
type serverLifecycle struct {
	s    *Server
	cfg  lifecycleConfig
	log  *slog.Logger
	disk *diskGuardState

	snapshotRuns      atomic.Uint64
	snapshotErrors    atomic.Uint64
	lastSnapshotNano  atomic.Int64
	lastSnapshotIndex atomic.Uint64

	gcRuns          atomic.Uint64
	gcErrors        atomic.Uint64
	gcDeleted       atomic.Uint64
	gcLocksRetained atomic.Uint64
	lastGCNano      atomic.Int64
	lastGCSafePoint atomic.Uint64

	diskChecks atomic.Uint64

	mu              sync.Mutex
	snapshotTimes   map[uint64]int64
	snapshotIndexes map[uint64]uint64
	gcSafePoints    map[uint64]uint64

	// Optional Prometheus metrics. They stay nil when no registry is wired.
	mSnapshotRuns    *metrics.Counter
	mSnapshotErrors  *metrics.Counter
	mSnapshotAge     *metrics.Gauge
	mLastSnapshotIdx *metrics.Gauge
	mGCRuns          *metrics.Counter
	mGCErrors        *metrics.Counter
	mGCDeleted       *metrics.Counter
	mGCSafePoint     *metrics.Gauge
	mDiskFull        *metrics.Gauge
	mDiskFreeBytes   *metrics.Gauge
	mDiskChecks      *metrics.Counter
}

func newServerLifecycle(s *Server, cfg lifecycleConfig, disk *diskGuardState) *serverLifecycle {
	l := &serverLifecycle{
		s:               s,
		cfg:             cfg,
		log:             s.log,
		disk:            disk,
		snapshotTimes:   make(map[uint64]int64),
		snapshotIndexes: make(map[uint64]uint64),
		gcSafePoints:    make(map[uint64]uint64),
	}
	l.registerMetrics()
	// Probe once during construction so a node that starts with an almost-full
	// disk rejects writes from the first request, not after one probe interval.
	if err := l.disk.check(); err != nil {
		l.log.Warn("lifecycle: initial disk probe failed", "error", err)
	}
	l.updateDiskMetrics()
	return l
}

func (l *serverLifecycle) registerMetrics() {
	reg := l.cfg.metrics
	if reg == nil {
		return
	}
	labels := metrics.Labels{"node": l.s.cfg.ID}
	l.mSnapshotRuns = reg.NewCounter("kvserver_snapshot_runs_total", "Range state-machine snapshot attempts.", labels)
	l.mSnapshotErrors = reg.NewCounter("kvserver_snapshot_errors_total", "Range snapshot attempts that failed.", labels)
	l.mSnapshotAge = reg.NewGauge("kvserver_snapshot_age_seconds", "Seconds since the last successful snapshot attempt.", labels)
	l.mLastSnapshotIdx = reg.NewGauge("kvserver_last_snapshot_index", "Highest snapshot index persisted by this node.", labels)
	l.mGCRuns = reg.NewCounter("kvserver_gc_runs_total", "MVCC GC passes by range.", labels)
	l.mGCErrors = reg.NewCounter("kvserver_gc_errors_total", "MVCC GC passes that failed.", labels)
	l.mGCDeleted = reg.NewCounter("kvserver_gc_deleted_versions_total", "Data and write versions deleted by MVCC GC.", labels)
	l.mGCSafePoint = reg.NewGauge("kvserver_gc_safe_point", "Last MVCC GC safe point used by this node.", labels)
	l.mDiskFull = reg.NewGauge("kvserver_disk_full", "1 while writes are rejected because storage is full.", labels)
	l.mDiskFreeBytes = reg.NewGauge("kvserver_disk_free_bytes", "Last probed free bytes on the data filesystem.", labels)
	l.mDiskChecks = reg.NewCounter("kvserver_disk_checks_total", "Free-space probes performed.", labels)
}

func (l *serverLifecycle) enabled() bool {
	return l.cfg.snapshotInterval > 0 || l.cfg.gcInterval > 0 || l.cfg.diskCheckInterval > 0
}

func (l *serverLifecycle) tick() time.Duration {
	tick := time.Duration(0)
	for _, d := range []time.Duration{l.cfg.snapshotInterval, l.cfg.gcInterval, l.cfg.diskCheckInterval} {
		if d > 0 && (tick == 0 || d < tick) {
			tick = d
		}
	}
	return tick
}

// start launches the one maintenance goroutine. It returns nil when every loop
// is disabled, so Stop has nothing to wait for.
func (l *serverLifecycle) start(ctx context.Context) <-chan struct{} {
	if !l.enabled() {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.run(ctx)
	}()
	return done
}

func (l *serverLifecycle) run(ctx context.Context) {
	tick := l.tick()
	if tick <= 0 {
		return
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	now := time.Now()
	lastSnapshot, lastGC, lastDisk := now, now, now
	// The initial probe happened in New; probe again at Start in case the node
	// sat idle for a long time between New and Start.
	l.checkDisk(now)

	for {
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
			if l.cfg.diskCheckInterval > 0 && !now.Before(lastDisk.Add(l.cfg.diskCheckInterval)) {
				l.checkDisk(now)
				lastDisk = now
			}
			if l.cfg.snapshotInterval > 0 && !now.Before(lastSnapshot.Add(l.cfg.snapshotInterval)) {
				l.snapshotAll(ctx, now)
				lastSnapshot = now
			}
			if l.cfg.gcInterval > 0 && !now.Before(lastGC.Add(l.cfg.gcInterval)) {
				l.gcAll(ctx, now)
				lastGC = now
			}
		}
	}
}

func (l *serverLifecycle) checkDisk(now time.Time) {
	l.diskChecks.Add(1)
	if l.mDiskChecks != nil {
		l.mDiskChecks.Inc()
	}
	if err := l.disk.check(); err != nil {
		l.log.Warn("lifecycle: disk probe failed", "error", err)
		return
	}
	l.updateDiskMetrics()
	l.updateSnapshotAgeMetric(now)
}

func (l *serverLifecycle) updateDiskMetrics() {
	if l.mDiskFull != nil {
		if l.disk.isFull() {
			l.mDiskFull.Set(1)
		} else {
			l.mDiskFull.Set(0)
		}
	}
	if l.mDiskFreeBytes != nil {
		l.mDiskFreeBytes.Set(l.disk.free())
	}
}

func (l *serverLifecycle) updateSnapshotAgeMetric(now time.Time) {
	if l.mSnapshotAge == nil {
		return
	}
	nano := l.lastSnapshotNano.Load()
	if nano <= 0 {
		l.mSnapshotAge.Set(0)
		return
	}
	age := now.Sub(time.Unix(0, nano))
	if age < 0 {
		age = 0
	}
	l.mSnapshotAge.Set(int64(age.Seconds()))
}

// snapshotAll visits every hosted range in order. It holds Server.mu for the
// duration of one range's maintenance, which serializes lifecycle work with
// split/merge/remove (they hold Server.mu exclusively) but never with client
// reads or writes.
func (l *serverLifecycle) snapshotAll(ctx context.Context, now time.Time) {
	if l.disk.isFull() {
		l.log.Warn("lifecycle: skipping snapshots while storage is full")
		return
	}
	for _, id := range l.s.Groups() {
		if ctx.Err() != nil || l.disk.isFull() {
			return
		}
		start := time.Now()
		meta, err := l.withGroup(id, func(g *rangeGroup) (raft.SnapshotMeta, error) {
			return l.snapshotGroup(g)
		})
		if err != nil {
			if errors.Is(err, errLifecycleSkipped) {
				continue
			}
			l.snapshotErrors.Add(1)
			if l.mSnapshotErrors != nil {
				l.mSnapshotErrors.Inc()
			}
			if isNoSpaceError(err) {
				l.disk.markFull(fmt.Sprintf("snapshot range %d: %v", id, err))
			}
			l.log.Warn("lifecycle: snapshot failed", "range", id, "error", err)
			continue
		}
		finished := time.Now()
		l.snapshotRuns.Add(1)
		if l.mSnapshotRuns != nil {
			l.mSnapshotRuns.Inc()
		}
		if meta.Index > 0 {
			l.lastSnapshotIndex.Store(meta.Index)
			if l.mLastSnapshotIdx != nil {
				l.mLastSnapshotIdx.Set(int64(meta.Index))
			}
		}
		l.lastSnapshotNano.Store(finished.UnixNano())
		l.mu.Lock()
		l.snapshotTimes[id] = finished.UnixNano()
		l.snapshotIndexes[id] = meta.Index
		l.mu.Unlock()
		if l.mSnapshotAge != nil {
			l.mSnapshotAge.Set(0)
		}
		if meta.Index > 0 {
			l.log.Info("lifecycle: range snapshot persisted",
				"range", id, "index", meta.Index, "duration", time.Since(start))
		} else {
			l.log.Debug("lifecycle: range snapshot skipped (no applied state)",
				"range", id, "duration", time.Since(start))
		}
	}
}

// snapshotGroup uses the existing Raft snapshot path: the state machine image
// is persisted first and only then is the log prefix truncated. When the
// streaming capability is unavailable it falls back to Node.Snapshot; it never
// truncates the log directly.
func (l *serverLifecycle) snapshotGroup(g *rangeGroup) (raft.SnapshotMeta, error) {
	if g == nil || g.node == nil {
		return raft.SnapshotMeta{}, errLifecycleSkipped
	}
	meta, err := g.node.SnapshotToStore()
	if err == nil {
		return meta, nil
	}
	if !errors.Is(err, raft.ErrSnapshotStreamUnsupported) {
		return raft.SnapshotMeta{}, err
	}
	snap, err := g.node.Snapshot()
	if err != nil {
		return raft.SnapshotMeta{}, err
	}
	return raft.SnapshotMeta{Index: snap.Index, Term: snap.Term}, nil
}

// gcAll computes one safe point per range and runs the existing MVCC GC. The
// safe point is never allowed to move backwards and is capped by the oldest
// live lock, so an in-flight writer can still resolve/commit.
func (l *serverLifecycle) gcAll(ctx context.Context, now time.Time) {
	if l.disk.isFull() {
		l.log.Warn("lifecycle: skipping MVCC GC while storage is full")
		return
	}
	for _, id := range l.s.Groups() {
		if ctx.Err() != nil || l.disk.isFull() {
			return
		}
		deleted, err := l.withGroupGC(id, func(g *rangeGroup) (uint64, error) {
			return l.gcGroup(g, now)
		})
		if err != nil {
			if errors.Is(err, errLifecycleSkipped) {
				continue
			}
			l.gcErrors.Add(1)
			if l.mGCErrors != nil {
				l.mGCErrors.Inc()
			}
			if isNoSpaceError(err) {
				l.disk.markFull(fmt.Sprintf("gc range %d: %v", id, err))
			}
			l.log.Warn("lifecycle: MVCC GC failed", "range", id, "error", err)
			continue
		}
		if deleted > 0 {
			l.gcDeleted.Add(deleted)
			if l.mGCDeleted != nil {
				l.mGCDeleted.Add(deleted)
			}
		}
	}
}

func (l *serverLifecycle) gcGroup(g *rangeGroup, now time.Time) (uint64, error) {
	if g == nil || g.store == nil {
		return 0, errLifecycleSkipped
	}
	safePoint, source, err := l.safePoint(g, now)
	if err != nil {
		return 0, err
	}
	if safePoint == 0 {
		return 0, errLifecycleSkipped
	}
	stats, err := g.store.GCWithStats(safePoint)
	if err != nil {
		return 0, err
	}
	l.gcRuns.Add(1)
	if l.mGCRuns != nil {
		l.mGCRuns.Inc()
	}
	l.gcLocksRetained.Add(uint64(stats.LocksRetained))
	l.lastGCNano.Store(time.Now().UnixNano())
	l.lastGCSafePoint.Store(safePoint)
	if l.mGCSafePoint != nil {
		l.mGCSafePoint.Set(int64(safePoint))
	}
	l.mu.Lock()
	l.gcSafePoints[g.cfg.ID] = safePoint
	l.mu.Unlock()
	l.log.Info("lifecycle: MVCC GC pass",
		"range", g.cfg.ID,
		"safe_point", safePoint,
		"source", source,
		"deleted_versions", stats.Total(),
		"data_versions", stats.DataVersions,
		"write_records", stats.WriteRecords,
		"rollback_records", stats.RollbackRecords,
		"locks_retained", stats.LocksRetained,
	)
	return uint64(stats.Total()), nil
}

// safePoint returns 0 when no GC pass may run. The returned source is for logs.
//
// Fallback when Config.GCSafePoint is nil:
//
//	candidate = (now - GCSafePointLag) << 18
//
// This mirrors the meta TSO encoding (physical milliseconds << 18) and is
// deliberately conservative: it does not collect anything written or read
// within the retention window. The candidate is then capped by the oldest live
// lock startTS. The known limitation is that the store does not track active
// read-only transaction timestamps; a read-only snapshot older than the
// retention window may lose old versions. Operators must set GCSafePointLag
// above their maximum read-transaction lifetime, or supply Config.GCSafePoint
// from a real TSO/GC coordinator.
func (l *serverLifecycle) safePoint(g *rangeGroup, now time.Time) (uint64, string, error) {
	var (
		candidate uint64
		source    string
	)
	if l.cfg.gcSafePoint != nil {
		candidate = l.cfg.gcSafePoint()
		source = "provider"
		if candidate == 0 {
			// A zero timestamp is the "not established yet" sentinel in MVCC.
			return 0, source, nil
		}
	} else {
		cutoff := now.Add(-l.cfg.gcSafePointLag)
		ms := cutoff.UnixMilli()
		if ms <= 0 {
			return 0, "wall-clock-lag", nil
		}
		if ms > int64(^uint64(0)>>18) {
			return 0, "wall-clock-lag", nil
		}
		candidate = uint64(ms) << 18
		source = "wall-clock-lag"
	}

	oldest, ok, err := g.store.OldestLockStartTS()
	if err != nil {
		return 0, source, err
	}
	if ok && oldest < candidate {
		candidate = oldest
		source += "+oldest-lock"
	}

	l.mu.Lock()
	last := l.gcSafePoints[g.cfg.ID]
	l.mu.Unlock()
	if candidate <= last {
		return 0, source, nil
	}
	return candidate, source, nil
}

var errLifecycleSkipped = errors.New("kvserver: lifecycle work skipped")

// withGroup runs fn on a currently hosted group under Server.mu's read lock.
// The lock prevents split/merge/remove from stopping the group under fn; it
// does not block client reads or writes.
func (l *serverLifecycle) withGroup(id uint64, fn func(*rangeGroup) (raft.SnapshotMeta, error)) (raft.SnapshotMeta, error) {
	l.s.mu.RLock()
	defer l.s.mu.RUnlock()
	if l.s.stopped {
		return raft.SnapshotMeta{}, errLifecycleSkipped
	}
	g, err := l.s.groupForID(id)
	if err != nil {
		return raft.SnapshotMeta{}, errLifecycleSkipped
	}
	return fn(g)
}

// withGroup for the GC path is separate because Go does not allow overloading;
// this wrapper adapts the uint64 return value.
func (l *serverLifecycle) withGroupGC(id uint64, fn func(*rangeGroup) (uint64, error)) (uint64, error) {
	l.s.mu.RLock()
	defer l.s.mu.RUnlock()
	if l.s.stopped {
		return 0, errLifecycleSkipped
	}
	g, err := l.s.groupForID(id)
	if err != nil {
		return 0, errLifecycleSkipped
	}
	return fn(g)
}

func (l *serverLifecycle) stats() LifecycleStats {
	now := time.Now()
	st := LifecycleStats{
		SnapshotRuns:      l.snapshotRuns.Load(),
		SnapshotErrors:    l.snapshotErrors.Load(),
		LastSnapshotIndex: l.lastSnapshotIndex.Load(),
		GCRuns:            l.gcRuns.Load(),
		GCErrors:          l.gcErrors.Load(),
		GCDeletedVersions: l.gcDeleted.Load(),
		GCLocksRetained:   l.gcLocksRetained.Load(),
		LastGCSafePoint:   l.lastGCSafePoint.Load(),
		DiskFull:          l.disk.isFull(),
		DiskFreeBytes:     l.disk.free(),
		DiskChecks:        l.diskChecks.Load(),
	}
	if nano := l.lastSnapshotNano.Load(); nano > 0 {
		st.LastSnapshot = time.Unix(0, nano)
		st.SnapshotAge = now.Sub(st.LastSnapshot)
		if st.SnapshotAge < 0 {
			st.SnapshotAge = 0
		}
	}
	if nano := l.lastGCNano.Load(); nano > 0 {
		st.LastGC = time.Unix(0, nano)
	}
	l.mu.Lock()
	st.SnapshotAges = make(map[uint64]time.Duration, len(l.snapshotTimes))
	for id, nano := range l.snapshotTimes {
		age := now.Sub(time.Unix(0, nano))
		if age < 0 {
			age = 0
		}
		st.SnapshotAges[id] = age
	}
	st.SnapshotIndexes = make(map[uint64]uint64, len(l.snapshotIndexes))
	for id, index := range l.snapshotIndexes {
		st.SnapshotIndexes[id] = index
	}
	st.GCSafePoints = make(map[uint64]uint64, len(l.gcSafePoints))
	for id, sp := range l.gcSafePoints {
		st.GCSafePoints[id] = sp
	}
	l.mu.Unlock()
	return st
}

// LifecycleStats returns a snapshot of the background maintenance counters.
func (s *Server) LifecycleStats() LifecycleStats {
	if s == nil || s.lifecycle == nil {
		return LifecycleStats{}
	}
	return s.lifecycle.stats()
}

// DiskFull reports whether the node is currently rejecting writes.
func (s *Server) DiskFull() bool {
	return s != nil && s.lifecycle != nil && s.lifecycle.disk.isFull()
}
