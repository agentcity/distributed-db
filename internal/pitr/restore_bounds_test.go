package pitr

import (
	"context"
	"errors"
	"testing"

	"distributed-db/internal/backup"
)

// buildCheckpointedArchive returns an archive with two ranges whose records
// reach TSO 190 and a checkpoint at TSO 150 covering both ranges.
func buildCheckpointedArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1,
		testRec(1, 1, 120, "a", []byte("a1")),
		testRec(2, 1, 180, "a", []byte("a2")))
	mustAppend(t, w, 2,
		testRec(1, 1, 130, "b", []byte("b1")),
		testRec(2, 1, 190, "b", []byte("b2")))
	mustSeal(t, w)
	if err := w.SetCheckpoint(Checkpoint{
		TSO:        150,
		CapturedAt: testTime,
		Ranges: []backup.Range{
			baseRange(1, nil, []byte("m")),
			baseRange(2, []byte("m"), nil),
		},
	}); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir
}

func countedSink() (RestoreSink, *int) {
	applied := new(int)
	return RestoreSinkFunc(func(context.Context, PlannedRecord) error {
		*applied = *applied + 1
		return nil
	}), applied
}

func TestRestorePlannerCheckpointBounds(t *testing.T) {
	dir := buildCheckpointedArchive(t)
	planner, err := NewPlanner(mustReader(t, dir))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	base := testBase(100,
		baseRange(1, nil, []byte("m")),
		baseRange(2, []byte("m"), nil),
	)
	ctx := context.Background()

	// A target within the checkpoint high-water is a safe restore plan.
	plan, err := planner.BuildPlan(ctx, PlanRequest{Base: base, TargetTSO: 140})
	if err != nil {
		t.Fatalf("BuildPlan target 140: %v", err)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("unexpected warnings for target 140: %v", plan.Warnings)
	}
	if len(plan.Records) != 2 {
		t.Fatalf("target 140 records = %+v, want tso 120 and 130", plan.Records)
	}

	// Above the checkpoint: BuildPlan warns (it is side-effect free) and
	// RestoreToTS fails closed before the sink sees a record.
	plan, err = planner.BuildPlan(ctx, PlanRequest{Base: base, TargetTSO: 160})
	if err != nil {
		t.Fatalf("BuildPlan target 160: %v", err)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("target above checkpoint did not produce a coverage warning")
	}
	sink, applied := countedSink()
	if _, err := planner.RestoreToTS(ctx, PlanRequest{Base: base, TargetTSO: 160}, sink); !errors.Is(err, ErrArchiveIncomplete) {
		t.Fatalf("RestoreToTS target 160 = %v, want ErrArchiveIncomplete", err)
	}
	if *applied != 0 {
		t.Fatalf("sink saw %d records after a rejected target", *applied)
	}

	// The explicit opt-out executes the same plan.
	sink, applied = countedSink()
	result, err := planner.RestoreToTS(ctx, PlanRequest{
		Base: base, TargetTSO: 160, AllowUnprovenTarget: true,
	}, sink)
	if err != nil {
		t.Fatalf("RestoreToTS opt-out: %v", err)
	}
	if result.Applied != 2 || *applied != 2 {
		t.Fatalf("opt-out applied %d/%d, want 2", result.Applied, *applied)
	}

	// A barrier frontier below the target is an additional fail-closed bound
	// even when the checkpoint itself would cover the target.
	sink, applied = countedSink()
	if _, err := planner.RestoreToTS(ctx, PlanRequest{
		Base: base, TargetTSO: 140, BarrierFrontier: 125,
	}, sink); !errors.Is(err, ErrArchiveIncomplete) {
		t.Fatalf("barrier-bound RestoreToTS = %v, want ErrArchiveIncomplete", err)
	}
	if *applied != 0 {
		t.Fatalf("sink saw %d records with an exceeded barrier frontier", *applied)
	}
	sink, applied = countedSink()
	if _, err := planner.RestoreToTS(ctx, PlanRequest{
		Base: base, TargetTSO: 140, BarrierFrontier: 125, AllowUnprovenTarget: true,
	}, sink); err != nil {
		t.Fatalf("barrier-bound opt-out: %v", err)
	}
	if *applied != 2 {
		t.Fatalf("barrier-bound opt-out applied %d records, want 2", *applied)
	}
}

func TestRestorePlannerCheckpointRangeCoverage(t *testing.T) {
	dir := buildCheckpointedArchive(t)
	planner, err := NewPlanner(mustReader(t, dir))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	// Range 3 is in the base but absent from the checkpoint range map. Without
	// split/merge lineage the planner cannot prove its coverage, so it fails
	// closed even for a target at the base.
	base := testBase(100,
		baseRange(1, nil, []byte("m")),
		baseRange(2, []byte("m"), []byte("t")),
		baseRange(3, []byte("t"), nil),
	)
	if _, err := planner.BuildPlan(context.Background(), PlanRequest{Base: base, TargetTSO: 100}); !errors.Is(err, ErrRangeMismatch) {
		t.Fatalf("BuildPlan with uncovered range = %v, want ErrRangeMismatch", err)
	}
}

func TestRestorePlannerBaseTargetStillAllowedBelowCheckpoint(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 150, "a", nil))
	mustSeal(t, w)
	if err := w.SetCheckpoint(Checkpoint{
		TSO:        120,
		CapturedAt: testTime,
		Ranges:     []backup.Range{baseRange(1, nil, nil)},
	}); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	planner, _ := NewPlanner(mustReader(t, dir))
	base := testBase(130, baseRange(1, nil, nil))
	// Target == base needs no archived records; a checkpoint below the base
	// (which can happen when an older checkpoint is retained) must not block it.
	sink, applied := countedSink()
	result, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 130}, sink)
	if err != nil {
		t.Fatalf("RestoreToTS at base: %v", err)
	}
	if result.Applied != 0 || *applied != 0 {
		t.Fatalf("base-only restore applied %d/%d records", result.Applied, *applied)
	}
	if _, err := planner.RestoreToTS(context.Background(), PlanRequest{Base: base, TargetTSO: 129}, sink); !errors.Is(err, ErrTargetBeforeBase) {
		t.Fatalf("target before base error = %v, want ErrTargetBeforeBase", err)
	}
}
