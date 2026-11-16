package v2client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAllocTSBatching(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setNext(1000)
	c := newTestClient(fake)
	c.tsBatchSize = 8
	c.tsBatchTTL = time.Second
	c.tsIdleCheck = time.Hour
	c.tsValidateEvery = 1 << 62

	const calls = 800
	var prev uint64
	seen := make(map[uint64]struct{}, calls)
	for i := 0; i < calls; i++ {
		ts, err := c.AllocTS(context.Background())
		if err != nil {
			t.Fatalf("AllocTS #%d: %v", i, err)
		}
		if ts <= prev {
			t.Fatalf("AllocTS #%d = %d, want > %d", i, ts, prev)
		}
		if _, dup := seen[ts]; dup {
			t.Fatalf("AllocTS returned duplicate %d", ts)
		}
		seen[ts] = struct{}{}
		prev = ts
	}
	allocs, sizes, _, _ := fake.stats()
	wantAllocs := calls / 8
	if allocs != wantAllocs {
		t.Fatalf("meta AllocTS calls = %d, want %d (batch size 8)", allocs, wantAllocs)
	}
	for i, n := range sizes {
		if n != 8 {
			t.Fatalf("AllocTS call %d requested count %d, want 8", i, n)
		}
	}
}

func TestAllocTSConcurrentMonotonic(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setNext(1)
	c := newTestClient(fake)
	c.tsBatchSize = 16
	c.tsBatchTTL = time.Second
	c.tsIdleCheck = time.Hour
	c.tsValidateEvery = 1 << 62

	const goroutines = 16
	const perGoroutine = 100
	results := make(chan []uint64, goroutines)
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := make([]uint64, 0, perGoroutine)
			var prev uint64
			for i := 0; i < perGoroutine; i++ {
				ts, err := c.AllocTS(context.Background())
				if err != nil {
					errs <- fmt.Errorf("AllocTS: %w", err)
					return
				}
				if ts <= prev {
					errs <- fmt.Errorf("goroutine timestamp %d <= %d", ts, prev)
					return
				}
				prev = ts
				out = append(out, ts)
			}
			results <- out
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[uint64]struct{}, goroutines*perGoroutine)
	for out := range results {
		for _, ts := range out {
			if _, dup := seen[ts]; dup {
				t.Fatalf("duplicate timestamp %d across goroutines", ts)
			}
			seen[ts] = struct{}{}
		}
	}
	if want := goroutines * perGoroutine; len(seen) != want {
		t.Fatalf("unique timestamps = %d, want %d", len(seen), want)
	}
	allocs, _, _, _ := fake.stats()
	if max := goroutines*perGoroutine/16 + 1; allocs > max {
		t.Fatalf("meta AllocTS calls = %d, want <= %d", allocs, max)
	}
}

func TestAllocTSRefillsAfterTTL(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setNext(100)
	c := newTestClient(fake)
	c.tsBatchSize = 4
	c.tsBatchTTL = 20 * time.Millisecond
	c.tsIdleCheck = time.Hour
	c.tsValidateEvery = 1 << 62

	ts1, err := c.AllocTS(context.Background())
	if err != nil {
		t.Fatalf("first AllocTS: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	ts2, err := c.AllocTS(context.Background())
	if err != nil {
		t.Fatalf("second AllocTS: %v", err)
	}
	if ts2 <= ts1 {
		t.Fatalf("timestamp after TTL = %d, want > %d", ts2, ts1)
	}
	if ts2 != 104 {
		t.Fatalf("timestamp after TTL = %d, want 104 (new batch)", ts2)
	}
	allocs, _, _, _ := fake.stats()
	if allocs != 2 {
		t.Fatalf("meta AllocTS calls = %d, want 2", allocs)
	}
}

func TestAllocTSMetaFailoverInvalidatesBatch(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setNext(100)
	c := newTestClient(fake)
	c.tsBatchSize = 4
	c.tsBatchTTL = time.Second
	c.tsIdleCheck = 5 * time.Millisecond
	c.tsValidateEvery = 1 << 62

	ts1, err := c.AllocTS(context.Background())
	if err != nil {
		t.Fatalf("first AllocTS: %v", err)
	}
	if ts1 != 100 {
		t.Fatalf("first timestamp = %d, want 100", ts1)
	}

	// Move the meta leader while the client still holds 3 unused timestamps.
	fake.setLeader("meta2")
	fake.setNext(10000)
	time.Sleep(10 * time.Millisecond)

	ts2, err := c.AllocTS(context.Background())
	if err != nil {
		t.Fatalf("AllocTS after failover: %v", err)
	}
	if ts2 < 10000 {
		t.Fatalf("timestamp after failover = %d, want >= 10000 (stale batch reused)", ts2)
	}
	allocs, _, _, _ := fake.stats()
	if allocs != 2 {
		t.Fatalf("meta AllocTS calls = %d, want 2", allocs)
	}
}

func TestAllocTSRecoversFromMetaError(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setNext(7)
	fake.setFailAllocs(1)
	c := newTestClient(fake)
	c.tsBatchSize = 4

	ts, err := c.AllocTS(context.Background())
	if err != nil {
		t.Fatalf("AllocTS after transient failure: %v", err)
	}
	if ts != 7 {
		t.Fatalf("AllocTS = %d, want 7", ts)
	}
	allocs, _, _, _ := fake.stats()
	if allocs != 2 {
		t.Fatalf("meta AllocTS attempts = %d, want 2", allocs)
	}
}

func TestAllocTSPersistentFailure(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setAllocErr(errors.New("meta down"))
	c := newTestClient(fake)
	c.tsBatchSize = 4

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.AllocTS(ctx); err == nil {
		t.Fatal("AllocTS succeeded while meta was down")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("AllocTS took %v to respect context deadline", elapsed)
	}
}
