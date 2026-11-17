package pitr

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestRestorePlanOrderingAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1,
		testRec(1, 1, 120, "a", []byte("a1")),
		testRec(2, 1, 160, "a", []byte("a2")))
	mustAppend(t, w, 2,
		testRec(1, 1, 110, "b", []byte("b1")),
		testRec(2, 1, 150, "b", []byte("b2")),
		testRec(3, 1, 200, "b", []byte("b3")))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	base := testBase(100,
		baseRange(2, []byte("m"), nil),
		baseRange(1, nil, []byte("m")),
	)
	planner, err := NewPlanner(mustReader(t, dir))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	plan, err := planner.Plan(context.Background(), base, 170)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := plan.NeededRanges, []uint64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("needed ranges = %v, want %v", got, want)
	}
	if len(plan.Segments) != 2 {
		t.Fatalf("plan segments = %d, want 2", len(plan.Segments))
	}
	if len(plan.Records) != 4 {
		t.Fatalf("plan records = %d, want 4: %+v", len(plan.Records), plan.Records)
	}
	want := []struct {
		rangeID uint64
		index   uint64
		tso     uint64
	}{
		{1, 1, 120},
		{1, 2, 160},
		{2, 1, 110},
		{2, 2, 150},
	}
	for i, w := range want {
		got := plan.Records[i]
		if got.Seq != uint64(i+1) || got.RangeID != w.rangeID || got.Entry.Index != w.index || got.TSO != w.tso {
			t.Fatalf("record %d = %+v, want range %d index %d tso %d", i, got, w.rangeID, w.index, w.tso)
		}
	}
	if plan.Hash == "" || len(plan.Hash) != 64 {
		t.Fatalf("plan hash = %q", plan.Hash)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", plan.Warnings)
	}

	// Replanning identical immutable inputs must produce an identical plan,
	// including order and hash.
	plan2, err := planner.BuildPlan(context.Background(), PlanRequest{Base: base, TargetTSO: 170})
	if err != nil {
		t.Fatalf("BuildPlan again: %v", err)
	}
	if !reflect.DeepEqual(plan, plan2) {
		t.Fatalf("replanned output differs:\n%+v\n%+v", plan, plan2)
	}

	// Applying the plan twice must not change the resulting state when the
	// sink upserts by (range,index) at the recorded TSO.
	type key struct {
		rangeID uint64
		index   uint64
	}
	state := make(map[key]uint64)
	apply := func() error {
		return plan.Apply(context.Background(), func(rec PlannedRecord) error {
			state[key{rec.RangeID, rec.Entry.Index}] = rec.TSO
			return nil
		})
	}
	if err := apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	first := make(map[key]uint64, len(state))
	for k, v := range state {
		first[k] = v
	}
	if err := apply(); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if !reflect.DeepEqual(first, state) || len(state) != 4 {
		t.Fatalf("plan apply is not idempotent: first=%v second=%v", first, state)
	}

	// A base-only target selects no replay records.
	atBase, err := planner.Plan(context.Background(), base, 100)
	if err != nil {
		t.Fatalf("base-target Plan: %v", err)
	}
	if len(atBase.Records) != 0 {
		t.Fatalf("base-target records = %+v", atBase.Records)
	}
	if _, err := planner.Plan(context.Background(), base, 99); !errors.Is(err, ErrTargetBeforeBase) {
		t.Fatalf("target-before-base error = %v", err)
	}
	subset, err := planner.BuildPlan(context.Background(), PlanRequest{Base: base, TargetTSO: 170, Ranges: []uint64{2}})
	if err != nil {
		t.Fatalf("subset Plan: %v", err)
	}
	if len(subset.Records) != 2 || subset.Records[0].RangeID != 2 || subset.Records[1].RangeID != 2 {
		t.Fatalf("subset records = %+v", subset.Records)
	}
	if _, err := planner.BuildPlan(context.Background(), PlanRequest{Base: base, TargetTSO: 170, Ranges: []uint64{99}}); !errors.Is(err, ErrRangeMismatch) {
		t.Fatalf("unknown range error = %v, want ErrRangeMismatch", err)
	}
}

func TestRestorePlanRejectsTrimAboveBase(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 120, "a", []byte("a1")))
	mustSeal(t, w)
	if _, err := w.GC(context.Background(), RetentionPolicy{MaxBytes: 1}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	planner, err := NewPlanner(r)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	base := testBase(100, baseRange(1, nil, nil))
	if _, err := planner.Plan(context.Background(), base, 150); !errors.Is(err, ErrGap) {
		t.Fatalf("plan error = %v, want ErrGap", err)
	}
	// A later base at or above the trim TSO makes the deleted prefix
	// irrelevant; the plan can proceed with no replay records.
	base2 := testBase(130, baseRange(1, nil, nil))
	plan, err := planner.Plan(context.Background(), base2, 150)
	if err != nil {
		t.Fatalf("plan with later base: %v", err)
	}
	if len(plan.Records) != 0 {
		t.Fatalf("records = %+v", plan.Records)
	}
	if len(plan.Warnings) == 0 {
		t.Fatalf("expected an archive coverage warning")
	}
}

func TestRestorePlanRejectsMissingOrCorruptSegments(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1, testRec(1, 1, 110, "a", nil))
		mustSeal(t, w)
		mustAppend(t, w, 1, testRec(2, 1, 130, "a", nil))
		mustSeal(t, w)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r := mustReader(t, dir)
		segs := r.ListSegments()
		if err := os.Remove(segmentFile(t, dir, segs[1])); err != nil {
			t.Fatalf("remove segment: %v", err)
		}
		planner, _ := NewPlanner(r)
		_, err := planner.Plan(context.Background(), testBase(100, baseRange(1, nil, nil)), 140)
		if !errors.Is(err, ErrGap) || !errors.Is(err, ErrMissing) {
			t.Fatalf("plan error = %v, want ErrGap/ErrMissing", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		clock := newTestClock()
		w := newArchiveWriter(t, dir, clock, 1<<20)
		mustAppend(t, w, 1, testRec(1, 1, 110, "a", nil))
		mustSeal(t, w)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r := mustReader(t, dir)
		info := r.ListSegments()[0]
		p := segmentFile(t, dir, info)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		data[len(data)-1] ^= 0xff
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		planner, _ := NewPlanner(r)
		_, err = planner.Plan(context.Background(), testBase(100, baseRange(1, nil, nil)), 140)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("plan error = %v, want ErrCorrupt", err)
		}
	})
}

func TestRestorePlanBaseValidation(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock()
	w := newArchiveWriter(t, dir, clock, 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 120, "a", nil))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	planner, _ := NewPlanner(mustReader(t, dir))
	if _, err := planner.Plan(context.Background(), nil, 150); !errors.Is(err, ErrInvalidBase) {
		t.Fatalf("nil base error = %v", err)
	}
	if _, err := planner.Plan(context.Background(), testBase(0, baseRange(1, nil, nil)), 150); !errors.Is(err, ErrInvalidBase) {
		t.Fatalf("zero base tso error = %v", err)
	}
	bad := testBase(100, baseRange(1, nil, []byte("m")), baseRange(2, []byte("x"), nil))
	if _, err := planner.Plan(context.Background(), bad, 150); !errors.Is(err, ErrInvalidBase) {
		t.Fatalf("non-adjacent base error = %v", err)
	}
	unknown := testBase(100, baseRange(2, []byte("m"), nil), baseRange(1, nil, []byte("m")))
	plan, err := planner.Plan(context.Background(), unknown, 150)
	if err != nil {
		t.Fatalf("unsorted base Plan: %v", err)
	}
	if len(plan.Records) != 1 || plan.Records[0].RangeID != 1 {
		t.Fatalf("unsorted base records = %+v", plan.Records)
	}
}
