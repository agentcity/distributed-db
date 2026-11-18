package pitr

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"distributed-db/internal/backup"
)

var (
	// ErrCheckpoint is returned when a meta/TSO checkpoint is missing required
	// fields or its range metadata is not a valid contiguous range map.
	ErrCheckpoint = errors.New("pitr: invalid checkpoint")
	// ErrTargetTSO is returned when a restore target timestamp is zero or
	// otherwise not a usable TSO.
	ErrTargetTSO = errors.New("pitr: invalid target tso")
	// ErrTargetAboveCheckpoint is returned when a target TSO is newer than the
	// high-water mark recorded in the archive checkpoint.
	ErrTargetAboveCheckpoint = errors.New("pitr: target tso exceeds checkpoint high-water")
)

// CheckpointSource is the read-only input to CaptureCheckpoint.
type CheckpointSource struct {
	// HighWater returns the meta/TSO high-water mark to publish. It is
	// required. Capturing an allocation frontier is not the same as proving
	// that every earlier commit is archived; Barrier and the archive coverage
	// checks narrow that gap, but this MVP still has no global T_safe.
	HighWater func(ctx context.Context) (uint64, error)
	// Ranges lists the meta range map captured alongside the TSO. It is
	// required and is normally a *backup.MetaClient.
	Ranges backup.RangeLister
	// Barrier, when non-nil, is waited on at HighWater before the checkpoint
	// is written. A wait error aborts the capture; the checkpoint is not
	// published with an unproven barrier. Nil leaves the checkpoint without an
	// applied-commit guarantee. backup.BackupBarrier has exactly this method;
	// *mvcc.Store, backup.StoreBarrier and backup.LeaseBarrier implement it.
	Barrier backup.BackupBarrier
	// Now supplies CapturedAt. Nil selects time.Now.
	Now func() time.Time
}

// Checkpoint is the meta/TSO record persisted in an archive manifest. TSO is
// the captured high-water mark; Ranges is the range map observed at capture.
// It is an additive manifest field: archives without a checkpoint remain
// readable, but a planner then has no declarative safe high-water.
type Checkpoint struct {
	TSO        uint64         `json:"tso"`
	CapturedAt time.Time      `json:"captured_at"`
	Ranges     []backup.Range `json:"ranges"`
}

// Validate checks the checkpoint's own invariants and that Ranges is a sorted,
// gap-free, full key-space range map.
func (c *Checkpoint) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil checkpoint", ErrCheckpoint)
	}
	if c.TSO == 0 {
		return fmt.Errorf("%w: tso is zero", ErrCheckpoint)
	}
	if c.CapturedAt.IsZero() || c.CapturedAt.UnixNano() <= 0 {
		return fmt.Errorf("%w: missing capture time", ErrCheckpoint)
	}
	return validateCheckpointRanges(c.Ranges)
}

// Clone returns a deep copy safe to mutate independently.
func (c *Checkpoint) Clone() *Checkpoint {
	if c == nil {
		return nil
	}
	out := *c
	out.Ranges = cloneCheckpointRanges(c.Ranges)
	return &out
}

// HasRange reports whether the checkpoint range map contains id.
func (c *Checkpoint) HasRange(id uint64) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Ranges {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Range returns the checkpoint metadata for id.
func (c *Checkpoint) Range(id uint64) (backup.Range, bool) {
	if c == nil {
		return backup.Range{}, false
	}
	for _, r := range c.Ranges {
		if r.ID == id {
			return cloneBackupRange(r), true
		}
	}
	return backup.Range{}, false
}

// CaptureCheckpoint reads the high-water TSO and range metadata, optionally
// waits on the applied-commit barrier at that TSO and returns a validated
// checkpoint. The caller persists it with Writer.SetCheckpoint (or
// Writer.CaptureCheckpoint). Capture alone does not prove archive coverage.
func CaptureCheckpoint(ctx context.Context, src CheckpointSource) (*Checkpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src.HighWater == nil {
		return nil, fmt.Errorf("%w: no high-water provider", ErrCheckpoint)
	}
	if src.Ranges == nil {
		return nil, fmt.Errorf("%w: no range lister", ErrCheckpoint)
	}

	tso, err := src.HighWater(ctx)
	if err != nil {
		return nil, fmt.Errorf("pitr: read meta high-water tso: %w", err)
	}
	if tso == 0 {
		return nil, fmt.Errorf("%w: meta high-water tso is zero", ErrCheckpoint)
	}
	ranges, err := src.Ranges.ListRanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("pitr: read range metadata for checkpoint: %w", err)
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("%w: range metadata is empty", ErrCheckpoint)
	}
	ranges = cloneCheckpointRanges(ranges)
	sort.SliceStable(ranges, func(i, j int) bool {
		return compareKeys(ranges[i].Start, ranges[j].Start) < 0
	})

	if src.Barrier != nil {
		if err := src.Barrier.WaitApplied(ctx, tso); err != nil {
			return nil, fmt.Errorf("pitr: checkpoint barrier wait at tso %d: %w", tso, err)
		}
	}

	now := src.Now
	if now == nil {
		now = time.Now
	}
	captured := now().UTC()
	if captured.IsZero() {
		return nil, fmt.Errorf("%w: clock returned zero time", ErrCheckpoint)
	}
	cp := &Checkpoint{TSO: tso, CapturedAt: captured, Ranges: ranges}
	if err := cp.Validate(); err != nil {
		return nil, err
	}
	return cp, nil
}

// SetCheckpoint persists cp in the manifest. A checkpoint TSO may not move
// backwards; range metadata may be replaced because the map can legitimately
// change between captures. The manifest rewrite is atomic and fsynced before
// SetCheckpoint returns.
func (w *Writer) SetCheckpoint(cp Checkpoint) error {
	if w == nil {
		return fmt.Errorf("%w: nil writer", ErrCheckpoint)
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if prev := w.manifest.Checkpoint; prev != nil && cp.TSO < prev.TSO {
		return fmt.Errorf("%w: checkpoint tso %d precedes existing %d", ErrCheckpoint, cp.TSO, prev.TSO)
	}
	next := w.manifest.Clone()
	next.Checkpoint = cp.Clone()
	touchManifest(next, w.now().UTC())
	if err := next.Validate(); err != nil {
		return err
	}
	if err := writeManifestDir(w.dir, next); err != nil {
		return err
	}
	w.manifest = next
	return nil
}

// CaptureCheckpoint captures a checkpoint from src and persists it.
func (w *Writer) CaptureCheckpoint(ctx context.Context, src CheckpointSource) (*Checkpoint, error) {
	cp, err := CaptureCheckpoint(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := w.SetCheckpoint(*cp); err != nil {
		return nil, err
	}
	return cp, nil
}

// Checkpoint returns a deep copy of the manifest checkpoint, or nil.
func (r *Reader) Checkpoint() *Checkpoint {
	if r == nil || r.manifest == nil {
		return nil
	}
	return r.manifest.Checkpoint.Clone()
}

// CheckTargetTSO validates targetTSO against the checkpoint high-water mark.
// When the archive has a checkpoint, that advertised bound is authoritative
// and the archive's observed TSO bounds are not used as a second ceiling (a
// high-water allocation can legitimately be newer than the last commit).
// Without a checkpoint it falls back to the newest TSO observed in segments
// or a GC trim, which is a conservative heuristic rather than a safe point.
//
// A caller that deliberately wants an unproven target must not treat this
// method as the only gate; PlanRequest.AllowUnprovenTarget is the explicit
// restore-time opt-out.
func (r *Reader) CheckTargetTSO(targetTSO uint64) error {
	if r == nil || r.manifest == nil {
		return fmt.Errorf("%w: nil archive reader", ErrTargetTSO)
	}
	if targetTSO == 0 {
		return fmt.Errorf("%w: target tso is zero", ErrTargetTSO)
	}
	if cp := r.manifest.Checkpoint; cp != nil {
		if targetTSO > cp.TSO {
			return fmt.Errorf("%w: target %d, checkpoint %d", ErrTargetAboveCheckpoint, targetTSO, cp.TSO)
		}
		return nil
	}
	if latest := archiveMaxTSO(r.manifest); latest == 0 || targetTSO > latest {
		return fmt.Errorf("%w: target %d, newest observed archived tso %d",
			ErrArchiveIncomplete, targetTSO, latest)
	}
	return nil
}

// advertisedHighWater returns the smallest declarative upper bound available
// for a restore target: the observed archive maximum, the manifest checkpoint
// and an externally supplied barrier frontier. Zero means no bound is known.
func advertisedHighWater(m *Manifest, barrierFrontier uint64) uint64 {
	var high uint64
	set := func(v uint64) {
		if v == 0 {
			return
		}
		if high == 0 || v < high {
			high = v
		}
	}
	set(archiveMaxTSO(m))
	if m != nil && m.Checkpoint != nil {
		set(m.Checkpoint.TSO)
	}
	set(barrierFrontier)
	return high
}

// restoreHighWater combines the advertised high-water with per-needed-range
// archive coverage. It is a conservative completeness bound, not a global
// T_safe: a range with no retained segment or trim contributes zero, so a
// target above the base fails closed instead of assuming "no changes".
func restoreHighWater(m *Manifest, needed []uint64, barrierFrontier uint64) uint64 {
	high := advertisedHighWater(m, barrierFrontier)
	coverage := coverageTSO(m, needed)
	switch {
	case high == 0:
		return coverage
	case coverage == 0:
		return 0
	case coverage < high:
		return coverage
	default:
		return high
	}
}

func validateCheckpointRanges(ranges []backup.Range) error {
	if len(ranges) == 0 {
		return fmt.Errorf("%w: range metadata is empty", ErrCheckpoint)
	}
	seen := make(map[uint64]struct{}, len(ranges))
	for i, r := range ranges {
		if r.ID == 0 {
			return fmt.Errorf("%w: range %d has zero id", ErrCheckpoint, i)
		}
		if _, ok := seen[r.ID]; ok {
			return fmt.Errorf("%w: duplicate range id %d", ErrCheckpoint, r.ID)
		}
		seen[r.ID] = struct{}{}
		if i == 0 {
			if r.Start != nil {
				return fmt.Errorf("%w: first range does not start at -inf", ErrCheckpoint)
			}
		} else {
			prev := ranges[i-1]
			if prev.End == nil {
				return fmt.Errorf("%w: range %d has no end bound", ErrCheckpoint, prev.ID)
			}
			if r.Start == nil {
				return fmt.Errorf("%w: range %d has no start bound", ErrCheckpoint, r.ID)
			}
			if compareKeys(r.Start, prev.End) != 0 {
				return fmt.Errorf("%w: range %d is not adjacent to range %d", ErrCheckpoint, r.ID, prev.ID)
			}
		}
		if r.Start != nil && r.End != nil && compareKeys(r.Start, r.End) >= 0 {
			return fmt.Errorf("%w: range %d has inverted bounds", ErrCheckpoint, r.ID)
		}
	}
	last := ranges[len(ranges)-1]
	if last.End != nil {
		return fmt.Errorf("%w: last range does not end at +inf", ErrCheckpoint)
	}
	for i := 0; i < len(ranges)-1; i++ {
		if ranges[i].End == nil {
			return fmt.Errorf("%w: range %d has no end bound", ErrCheckpoint, ranges[i].ID)
		}
	}
	return nil
}

func cloneCheckpointRanges(in []backup.Range) []backup.Range {
	if in == nil {
		return nil
	}
	out := make([]backup.Range, len(in))
	for i, r := range in {
		out[i] = cloneBackupRange(r)
	}
	return out
}

func cloneBackupRange(r backup.Range) backup.Range {
	out := r
	out.Start = cloneCheckpointBytes(r.Start)
	out.End = cloneCheckpointBytes(r.End)
	if r.Replicas != nil {
		out.Replicas = make([]string, len(r.Replicas))
		copy(out.Replicas, r.Replicas)
	}
	return out
}

func cloneCheckpointBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
