package pitr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"distributed-db/internal/backup"
	"distributed-db/internal/model"
)

const (
	// PlanFormatName identifies a serialized restore plan.
	PlanFormatName = "distributed-db-pitr-restore-plan"
	// PlanFormatVersion is the restore plan schema version.
	PlanFormatVersion = 1
)

// Planner builds restore plans from a base backup manifest and an archive.
type Planner struct {
	reader *Reader
}

// NewPlanner returns a planner for an already opened archive.
func NewPlanner(r *Reader) (*Planner, error) {
	if r == nil {
		return nil, errors.New("pitr: nil archive reader")
	}
	return &Planner{reader: r}, nil
}

// PlanRequest describes one restore plan.
type PlanRequest struct {
	// Base is the logical base backup manifest. Only SnapshotTS and Ranges
	// are used; chunk data is restored by the existing backup package before
	// the plan is replayed.
	Base *backup.Manifest
	// TargetTSO is the requested restore point. It must be >= Base.SnapshotTS.
	TargetTSO uint64
	// Ranges optionally restricts the plan to a subset of base range ids. An
	// empty slice means every range in the base manifest.
	Ranges []uint64
	// AllowUnprovenTarget lets RestoreToTS execute a target above the newest
	// TSO observed in the archive. BuildPlan treats that as a warning because
	// archive completeness cannot be proven; RestoreToTS rejects it by default
	// with ErrArchiveIncomplete. It has no effect on BuildPlan.
	AllowUnprovenTarget bool
}

// PlannedRecord is one archived log record selected for replay, in total plan
// order. Seq is 1-based.
type PlannedRecord struct {
	Seq     uint64      `json:"seq"`
	RangeID uint64      `json:"range_id"`
	TSO     uint64      `json:"tso"`
	Entry   model.Entry `json:"entry"`
}

// Plan is a deterministic, side-effect-free restore plan. Building the same
// inputs twice produces the same records, order, segments and Hash. Replaying
// the same plan against the same base is therefore idempotent, provided the
// restore executor upserts records by their recorded commit TSO rather than
// allocating new timestamps. This package does not implement that executor or
// command semantics.
type Plan struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	ClusterID     string          `json:"cluster_id"`
	BaseTSO       uint64          `json:"base_tso"`
	TargetTSO     uint64          `json:"target_tso"`
	NeededRanges  []uint64        `json:"needed_ranges"`
	Records       []PlannedRecord `json:"records"`
	Segments      []SegmentInfo   `json:"segments"`
	Warnings      []string        `json:"warnings,omitempty"`
	Hash          string          `json:"hash"`
}

// Plan is a convenience wrapper around BuildPlan using every base range.
func (p *Planner) Plan(ctx context.Context, base *backup.Manifest, targetTSO uint64) (*Plan, error) {
	return p.BuildPlan(ctx, PlanRequest{Base: base, TargetTSO: targetTSO})
}

// BuildPlan validates the requested range coverage and produces ordered
// records. It rejects a target before the base, an unknown needed range, a
// trimmed prefix that may contain records after the base, and any missing,
// corrupt or non-contiguous required segment discovered while reading.
func (p *Planner) BuildPlan(ctx context.Context, req PlanRequest) (*Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.reader == nil {
		return nil, errors.New("pitr: nil planner")
	}
	if req.Base == nil {
		return nil, fmt.Errorf("%w: missing base manifest", ErrInvalidBase)
	}
	if req.Base.SnapshotTS == 0 {
		return nil, fmt.Errorf("%w: base snapshot_ts is zero", ErrInvalidBase)
	}
	if req.TargetTSO < req.Base.SnapshotTS {
		return nil, fmt.Errorf("%w: target %d, base %d", ErrTargetBeforeBase, req.TargetTSO, req.Base.SnapshotTS)
	}
	baseRanges, err := normalizeBaseRanges(req.Base.Ranges)
	if err != nil {
		return nil, err
	}
	needed, err := neededRanges(baseRanges, req.Ranges)
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		Format:        PlanFormatName,
		FormatVersion: PlanFormatVersion,
		ClusterID:     p.reader.manifest.ClusterID,
		BaseTSO:       req.Base.SnapshotTS,
		TargetTSO:     req.TargetTSO,
		NeededRanges:  needed,
	}

	if req.TargetTSO == req.Base.SnapshotTS {
		if err := finishPlan(plan); err != nil {
			return nil, err
		}
		return plan, nil
	}

	for _, r := range baseRanges {
		if !containsRangeID(needed, r.ID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t, ok := p.reader.manifest.TrimmedFor(r.ID); ok && t.MaxTSO > req.Base.SnapshotTS {
			return nil, fmt.Errorf("%w: needed range %d was trimmed through index %d with max tso %d above base %d",
				ErrGap, r.ID, t.ThroughIndex, t.MaxTSO, req.Base.SnapshotTS)
		}
		segs := p.reader.manifest.SegmentsForRange(r.ID)
		if len(segs) == 0 {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("range %d has no retained archive segments; assuming no changes after base", r.ID))
			continue
		}
		plan.Segments = append(plan.Segments, segs...)
		lastIndex := uint64(0)
		err := p.reader.ReplayRange(ctx, r.ID, Target{}, func(rec LogRecord) error {
			// Validate index continuity over every retained record, before TSO
			// filtering. TSO order is not assumed to follow Raft index order,
			// so selecting only records in the (base,target] window can
			// legitimately skip an index whose TSO is above the target; that
			// must not be reported as an archive gap.
			if lastIndex != 0 && rec.Entry.Index != lastIndex+1 {
				return fmt.Errorf("%w: range %d index %d, want %d", ErrGap, r.ID, rec.Entry.Index, lastIndex+1)
			}
			lastIndex = rec.Entry.Index
			if rec.TSO <= req.Base.SnapshotTS {
				// The base snapshot already contains every commit at or
				// below its TSO. Zero-TSO metadata entries are likewise not
				// part of the logical replay in this MVP.
				return nil
			}
			if rec.TSO > req.TargetTSO {
				// A later commit at a lower Raft index is not part of this
				// restore point, but the indices around it are still checked.
				return nil
			}
			plan.Records = append(plan.Records, PlannedRecord{
				Seq:     uint64(len(plan.Records) + 1),
				RangeID: r.ID,
				TSO:     rec.TSO,
				Entry:   rec.Entry,
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	baseIDs := make(map[uint64]struct{}, len(baseRanges))
	for _, r := range baseRanges {
		baseIDs[r.ID] = struct{}{}
	}
	var extra []uint64
	for _, id := range p.reader.manifest.SegmentRanges() {
		if _, ok := baseIDs[id]; !ok {
			extra = append(extra, id)
		}
	}
	for _, id := range p.reader.manifest.Trimmed {
		if _, ok := baseIDs[id.RangeID]; !ok && !containsRangeID(extra, id.RangeID) {
			extra = append(extra, id.RangeID)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	for _, id := range extra {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("archive range %d is not in the base range map; skipped (split/merge lineage is not implemented)", id))
	}
	if latest := archiveMaxTSO(p.reader.manifest); latest == 0 || req.TargetTSO > latest {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("target tso %d exceeds newest observed archived tso %d; archive completeness is not proven", req.TargetTSO, latest))
	}

	if err := finishPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply visits planned records in order. Calling Apply twice against a target
// that upserts by (range,key,commitTS) yields the same state; this package does
// not provide that target.
func (p *Plan) Apply(ctx context.Context, fn func(PlannedRecord) error) error {
	if p == nil {
		return errors.New("pitr: nil plan")
	}
	if fn == nil {
		return errors.New("pitr: nil plan apply function")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, rec := range p.Records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSink applies one planned record to an isolated restore target. It is
// the only place a restore executes command semantics. An implementation must
// write at rec.TSO (never allocate a new timestamp) and upsert by
// (range,key,commitTS) so replaying the same plan is idempotent. A lock
// resolution or cross-range transaction layer, if needed, belongs in the sink.
type RestoreSink interface {
	Apply(ctx context.Context, rec PlannedRecord) error
}

// RestoreSinkFunc adapts a function to RestoreSink.
type RestoreSinkFunc func(ctx context.Context, rec PlannedRecord) error

// Apply implements RestoreSink.
func (f RestoreSinkFunc) Apply(ctx context.Context, rec PlannedRecord) error {
	if f == nil {
		return errors.New("pitr: nil RestoreSinkFunc")
	}
	return f(ctx, rec)
}

// RestoreResult reports one RestoreToTS execution. Applied counts the sink calls
// that returned nil; on a sink error it describes the confirmed prefix.
type RestoreResult struct {
	Plan      *Plan
	BaseTSO   uint64
	TargetTSO uint64
	Applied   int
}

// RestoreToTS builds a deterministic plan from a base backup manifest and this
// archive, then feeds its records to sink in plan order. It is the restore-to-
// TSO MVP entry point: the caller restores the base backup first, supplies a
// sink that writes into a dedicated/fenced target, and only then makes that
// target visible. RestoreToTS does not provision the target, delete keys that
// are absent from the base, resolve cross-range locks, or perform an atomic
// cluster cutover.
//
// A target above the newest TSO observed for any needed range is rejected
// with ErrArchiveIncomplete before the sink sees any record unless
// PlanRequest.AllowUnprovenTarget is set. Gaps, missing segments and corrupt
// segments found while building the plan are returned before any sink call.
// A sink error stops the replay and is returned with the confirmed Applied
// count; there is no rollback.
func (p *Planner) RestoreToTS(ctx context.Context, req PlanRequest, sink RestoreSink) (*RestoreResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		return nil, errors.New("pitr: RestoreToTS requires a sink")
	}
	plan, err := p.BuildPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.TargetTSO > plan.BaseTSO && !req.AllowUnprovenTarget {
		latest := coverageTSO(p.reader.manifest, plan.NeededRanges)
		if latest == 0 || req.TargetTSO > latest {
			return nil, fmt.Errorf("%w: target %d, newest observed archived tso %d across needed ranges",
				ErrArchiveIncomplete, req.TargetTSO, latest)
		}
	}
	result := &RestoreResult{Plan: plan, BaseTSO: plan.BaseTSO, TargetTSO: plan.TargetTSO}
	if err := plan.Apply(ctx, func(rec PlannedRecord) error {
		if err := sink.Apply(ctx, rec); err != nil {
			return fmt.Errorf("pitr: sink rejected planned record %d (range %d index %d): %w",
				rec.Seq, rec.RangeID, rec.Entry.Index, err)
		}
		result.Applied++
		return nil
	}); err != nil {
		return result, err
	}
	return result, nil
}

func finishPlan(plan *Plan) error {
	if plan.Records == nil {
		plan.Records = []PlannedRecord{}
	}
	if plan.Segments == nil {
		plan.Segments = []SegmentInfo{}
	}
	if plan.NeededRanges == nil {
		plan.NeededRanges = []uint64{}
	}
	type digestPlan struct {
		Format        string          `json:"format"`
		FormatVersion int             `json:"format_version"`
		ClusterID     string          `json:"cluster_id"`
		BaseTSO       uint64          `json:"base_tso"`
		TargetTSO     uint64          `json:"target_tso"`
		NeededRanges  []uint64        `json:"needed_ranges"`
		Records       []PlannedRecord `json:"records"`
		Segments      []SegmentInfo   `json:"segments"`
	}
	data, err := json.Marshal(digestPlan{
		Format:        plan.Format,
		FormatVersion: plan.FormatVersion,
		ClusterID:     plan.ClusterID,
		BaseTSO:       plan.BaseTSO,
		TargetTSO:     plan.TargetTSO,
		NeededRanges:  plan.NeededRanges,
		Records:       plan.Records,
		Segments:      plan.Segments,
	})
	if err != nil {
		return fmt.Errorf("pitr: encode restore plan: %w", err)
	}
	sum := sha256.Sum256(data)
	plan.Hash = hex.EncodeToString(sum[:])
	return nil
}

func archiveMaxTSO(m *Manifest) uint64 {
	var max uint64
	for _, s := range m.Segments {
		if s.MaxTSO > max {
			max = s.MaxTSO
		}
	}
	for _, t := range m.Trimmed {
		if t.MaxTSO > max {
			max = t.MaxTSO
		}
	}
	return max
}

// coverageTSO returns the highest TSO through which every needed range has an
// observed archive record (or trim), taking the minimum across ranges. It is a
// conservative heuristic, not a global safe point: an archiver can lag behind
// a commit whose TSO is below the last archived record, and this package has no
// T_safe. A needed range with no retained segments or trim yields zero, so a
// target above the base fails closed instead of assuming "no changes".
func coverageTSO(m *Manifest, needed []uint64) uint64 {
	var latest uint64
	first := true
	for _, id := range needed {
		var max uint64
		for _, s := range m.Segments {
			if s.RangeID == id && s.MaxTSO > max {
				max = s.MaxTSO
			}
		}
		if t, ok := m.TrimmedFor(id); ok && t.MaxTSO > max {
			max = t.MaxTSO
		}
		if first {
			latest = max
			first = false
			continue
		}
		if max < latest {
			latest = max
		}
	}
	return latest
}

func normalizeBaseRanges(in []backup.Range) ([]backup.Range, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: base manifest has no ranges", ErrInvalidBase)
	}
	out := make([]backup.Range, len(in))
	for i, r := range in {
		r.Start = normalizeBound(r.Start)
		r.End = normalizeBound(r.End)
		out[i] = r
	}
	sort.Slice(out, func(i, j int) bool { return compareKeys(out[i].Start, out[j].Start) < 0 })
	seen := make(map[uint64]struct{}, len(out))
	for i, r := range out {
		if r.ID == 0 {
			return nil, fmt.Errorf("%w: base range %d has zero id", ErrInvalidBase, i)
		}
		if _, ok := seen[r.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate base range id %d", ErrInvalidBase, r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Start != nil && r.End != nil && compareKeys(r.Start, r.End) >= 0 {
			return nil, fmt.Errorf("%w: base range %d has inverted bounds", ErrInvalidBase, r.ID)
		}
		if i == 0 {
			if r.Start != nil {
				return nil, fmt.Errorf("%w: first base range does not start at -inf", ErrInvalidBase)
			}
			continue
		}
		prev := out[i-1]
		if prev.End == nil {
			return nil, fmt.Errorf("%w: base range %d has no end bound", ErrInvalidBase, prev.ID)
		}
		if compareKeys(r.Start, prev.End) != 0 {
			return nil, fmt.Errorf("%w: base range %d is not adjacent to range %d", ErrInvalidBase, r.ID, prev.ID)
		}
	}
	last := out[len(out)-1]
	if last.End != nil {
		return nil, fmt.Errorf("%w: last base range does not end at +inf", ErrInvalidBase)
	}
	for i := 0; i < len(out)-1; i++ {
		if out[i].End == nil {
			return nil, fmt.Errorf("%w: base range %d has no end bound", ErrInvalidBase, out[i].ID)
		}
	}
	return out, nil
}

func normalizeBound(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func neededRanges(base []backup.Range, requested []uint64) ([]uint64, error) {
	if len(requested) == 0 {
		out := make([]uint64, len(base))
		for i, r := range base {
			out[i] = r.ID
		}
		return out, nil
	}
	want := make(map[uint64]struct{}, len(requested))
	for _, id := range requested {
		if id == 0 {
			return nil, fmt.Errorf("%w: requested range id is zero", ErrRangeMismatch)
		}
		want[id] = struct{}{}
	}
	known := make(map[uint64]struct{}, len(base))
	for _, r := range base {
		known[r.ID] = struct{}{}
	}
	for id := range want {
		if _, ok := known[id]; !ok {
			return nil, fmt.Errorf("%w: requested range %d is not in the base manifest", ErrRangeMismatch, id)
		}
	}
	out := make([]uint64, 0, len(want))
	for _, r := range base {
		if _, ok := want[r.ID]; ok {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

func containsRangeID(ids []uint64, id uint64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
