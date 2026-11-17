package pitr

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestNoGlobalState runs two archives concurrently and checks that manifests,
// buffers, clocks and retention frontiers are completely per-Writer/Reader.
func TestNoGlobalState(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	clockA := newTestClock()
	clockB := newTestClock()
	clockB.Advance(24 * time.Hour)

	wA := newArchiveWriter(t, dirA, clockA, 1<<20)
	wB := newArchiveWriter(t, dirB, clockB, 1<<20)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := wA.Append(context.Background(), 1, []LogRecord{testRec(1, 1, 100, "a", []byte("A"))}); err != nil {
			errs <- fmt.Errorf("A append: %w", err)
			return
		}
		if err := wA.Seal(context.Background()); err != nil {
			errs <- fmt.Errorf("A seal: %w", err)
			return
		}
		if err := wA.Close(); err != nil {
			errs <- fmt.Errorf("A close: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := wB.Append(context.Background(), 9, []LogRecord{testRec(1, 1, 200, "b", []byte("B"))}); err != nil {
			errs <- fmt.Errorf("B append: %w", err)
			return
		}
		if err := wB.Seal(context.Background()); err != nil {
			errs <- fmt.Errorf("B seal: %w", err)
			return
		}
		if err := wB.Close(); err != nil {
			errs <- fmt.Errorf("B close: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	rA := mustReader(t, dirA)
	rB := mustReader(t, dirB)
	mA, mB := rA.Manifest(), rB.Manifest()
	if len(mA.Segments) != 1 || mA.Segments[0].RangeID != 1 {
		t.Fatalf("A manifest = %+v", mA)
	}
	if len(mB.Segments) != 1 || mB.Segments[0].RangeID != 9 {
		t.Fatalf("B manifest = %+v", mB)
	}
	if mA.Segments[0].SealedAt.Equal(mB.Segments[0].SealedAt) {
		t.Fatalf("independent clocks produced the same seal time")
	}
	gotA := replay(t, rA, 1, Target{})
	gotB := replay(t, rB, 9, Target{})
	if len(gotA) != 1 || len(gotB) != 1 || gotA[0].TSO != 100 || gotB[0].TSO != 200 {
		t.Fatalf("cross-contaminated replay: A=%+v B=%+v", gotA, gotB)
	}
	if !reflect.DeepEqual(gotA[0].Entry.Value, []byte("A")) || !reflect.DeepEqual(gotB[0].Entry.Value, []byte("B")) {
		t.Fatalf("cross-contaminated values: A=%+v B=%+v", gotA[0], gotB[0])
	}
}
