package pitr

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"distributed-db/internal/backup"
)

type checkpointMeta struct {
	ranges []backup.Range
	err    error
}

func (m checkpointMeta) ListRanges(context.Context) ([]backup.Range, error) {
	return m.ranges, m.err
}

type recordingCheckpointBarrier struct {
	waited uint64
	err    error
}

func (b *recordingCheckpointBarrier) WaitApplied(_ context.Context, ts uint64) error {
	b.waited = ts
	return b.err
}

func (b *recordingCheckpointBarrier) AppliedTS() uint64 { return 0 }

func TestCaptureCheckpointAndPersist(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	dir := t.TempDir()
	w := newArchiveWriter(t, dir, clock, 1<<20)

	meta := checkpointMeta{ranges: []backup.Range{
		baseRange(2, []byte("m"), nil),
		baseRange(1, nil, []byte("m")),
	}}
	barrier := &recordingCheckpointBarrier{}
	cp, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 220, nil },
		Ranges:    meta,
		Barrier:   barrier,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("CaptureCheckpoint: %v", err)
	}
	if cp.TSO != 220 || barrier.waited != 220 {
		t.Fatalf("checkpoint tso %d, barrier waited %d; want 220", cp.TSO, barrier.waited)
	}
	if !cp.CapturedAt.Equal(testTime) {
		t.Fatalf("captured_at = %v, want %v", cp.CapturedAt, testTime)
	}
	if len(cp.Ranges) != 2 || cp.Ranges[0].ID != 1 || cp.Ranges[1].ID != 2 {
		t.Fatalf("checkpoint ranges = %+v, want sorted ids 1,2", cp.Ranges)
	}
	if err := w.SetCheckpoint(*cp); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := mustReader(t, dir)
	got := r.Checkpoint()
	if got == nil {
		t.Fatal("checkpoint did not survive the manifest round trip")
	}
	if got.TSO != 220 || !got.CapturedAt.Equal(testTime) {
		t.Fatalf("reloaded checkpoint = %+v", got)
	}
	if !reflect.DeepEqual(got.Ranges, cp.Ranges) {
		t.Fatalf("reloaded ranges = %+v, want %+v", got.Ranges, cp.Ranges)
	}
	if got.Ranges[0].Replicas != nil {
		t.Fatalf("range metadata clone aliased replicas: %+v", got.Ranges[0])
	}
	if err := r.CheckTargetTSO(220); err != nil {
		t.Fatalf("CheckTargetTSO(220): %v", err)
	}
	if err := r.CheckTargetTSO(221); !errors.Is(err, ErrTargetAboveCheckpoint) {
		t.Fatalf("CheckTargetTSO(221) = %v, want ErrTargetAboveCheckpoint", err)
	}
	if err := r.CheckTargetTSO(0); !errors.Is(err, ErrTargetTSO) {
		t.Fatalf("CheckTargetTSO(0) = %v, want ErrTargetTSO", err)
	}

	// Checkpoint returns a deep copy; mutating it must not change the reader.
	got.TSO = 1
	got.Ranges[0].Start = []byte("mutated")
	again := r.Checkpoint()
	if again.TSO != 220 || !reflect.DeepEqual(again.Ranges, cp.Ranges) {
		t.Fatalf("manifest checkpoint was mutated through its copy: %+v", again)
	}
}

func TestCaptureCheckpointFailures(t *testing.T) {
	ctx := context.Background()
	meta := checkpointMeta{ranges: []backup.Range{baseRange(1, nil, nil)}}
	if _, err := CaptureCheckpoint(ctx, CheckpointSource{Ranges: meta}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("missing high-water error = %v, want ErrCheckpoint", err)
	}
	if _, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 100, nil },
	}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("missing range lister error = %v, want ErrCheckpoint", err)
	}
	if _, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 0, nil },
		Ranges:    meta,
	}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("zero high-water error = %v, want ErrCheckpoint", err)
	}
	if _, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 100, nil },
		Ranges:    checkpointMeta{},
	}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("empty range map error = %v, want ErrCheckpoint", err)
	}
	sentinel := errors.New("barrier timeout")
	if _, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 100, nil },
		Ranges:    meta,
		Barrier:   &recordingCheckpointBarrier{err: sentinel},
	}); !errors.Is(err, sentinel) {
		t.Fatalf("barrier error = %v, want sentinel", err)
	}
}

func TestCheckpointValidationRejectsBadRangeMaps(t *testing.T) {
	now := testTime
	cases := []struct {
		name string
		cp   Checkpoint
	}{
		{name: "zero tso", cp: Checkpoint{CapturedAt: now, Ranges: []backup.Range{{ID: 1}}}},
		{name: "zero captured_at", cp: Checkpoint{TSO: 10, Ranges: []backup.Range{{ID: 1}}}},
		{name: "no ranges", cp: Checkpoint{TSO: 10, CapturedAt: now}},
		{name: "zero range id", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{{ID: 0}}}},
		{name: "duplicate range id", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 1, Start: []byte("m"), End: nil},
		}}},
		{name: "non-adjacent", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("n"), End: nil},
		}}},
		{name: "first not minus infinity", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{
			{ID: 1, Start: []byte("a"), End: nil},
		}}},
		{name: "last not plus infinity", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{
			{ID: 1, Start: nil, End: []byte("m")},
		}}},
		{name: "inverted bounds", cp: Checkpoint{TSO: 10, CapturedAt: now, Ranges: []backup.Range{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: []byte("a")},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cp.Validate(); !errors.Is(err, ErrCheckpoint) {
				t.Fatalf("Validate error = %v, want ErrCheckpoint", err)
			}
		})
	}
}

func TestSetCheckpointRejectsRegression(t *testing.T) {
	dir := t.TempDir()
	w := newArchiveWriter(t, dir, newTestClock(), 1<<20)
	cp := Checkpoint{
		TSO:        200,
		CapturedAt: testTime,
		Ranges:     []backup.Range{baseRange(1, nil, nil)},
	}
	if err := w.SetCheckpoint(cp); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	cp.TSO = 199
	cp.CapturedAt = testTime.Add(time.Second)
	if err := w.SetCheckpoint(cp); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("regressing checkpoint error = %v, want ErrCheckpoint", err)
	}
	if got := w.Manifest().Checkpoint; got == nil || got.TSO != 200 {
		t.Fatalf("manifest checkpoint = %+v, want tso 200", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCheckTargetTSOFallsBackToArchiveBounds(t *testing.T) {
	dir := t.TempDir()
	w := newArchiveWriter(t, dir, newTestClock(), 1<<20)
	mustAppend(t, w, 1, testRec(1, 1, 150, "a", nil))
	mustSeal(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := mustReader(t, dir)
	if r.Checkpoint() != nil {
		t.Fatalf("unexpected checkpoint: %+v", r.Checkpoint())
	}
	if err := r.CheckTargetTSO(150); err != nil {
		t.Fatalf("CheckTargetTSO(150): %v", err)
	}
	if err := r.CheckTargetTSO(151); !errors.Is(err, ErrArchiveIncomplete) {
		t.Fatalf("CheckTargetTSO(151) = %v, want ErrArchiveIncomplete", err)
	}
}

func TestCaptureCheckpointRequiresContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	meta := checkpointMeta{ranges: []backup.Range{baseRange(1, nil, nil)}}
	_, err := CaptureCheckpoint(ctx, CheckpointSource{
		HighWater: func(context.Context) (uint64, error) { return 100, nil },
		Ranges:    meta,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v, want context.Canceled", err)
	}
}
