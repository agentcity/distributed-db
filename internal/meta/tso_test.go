package meta_test

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/meta"
)

func TestMetaAllocTS(t *testing.T) {
	c := meta.New()

	before := uint64(time.Now().UnixMilli())
	start, count := c.AllocTS(8)
	after := uint64(time.Now().UnixMilli())

	if count != 8 {
		t.Fatalf("AllocTS(8) count = %d, want 8", count)
	}
	physical := start >> 18
	if physical+1000 < before || physical > after+1000 {
		t.Fatalf("timestamp physical = %d, want close to [%d, %d]", physical, before, after)
	}
	if start == 0 {
		t.Fatal("first timestamp is zero")
	}

	next, n := c.AllocTS(3)
	if n != 3 {
		t.Fatalf("AllocTS(3) count = %d, want 3", n)
	}
	if next < start+uint64(count) {
		t.Fatalf("next timestamp %d overlaps [%d, %d)", next, start, start+uint64(count))
	}
}

func TestMetaAllocTSNonPositive(t *testing.T) {
	c := meta.New()
	for _, n := range []int{0, -1, -100} {
		start, count := c.AllocTS(n)
		if start != 0 || count != 0 {
			t.Errorf("AllocTS(%d) = (%d, %d), want (0, 0)", n, start, count)
		}
	}
	start, count := c.AllocTS(1)
	if count != 1 || start == 0 {
		t.Fatalf("AllocTS(1) = (%d, %d), want a real reservation", start, count)
	}
}

func TestMetaAllocTSConcurrentMonotonic(t *testing.T) {
	c := meta.New()
	const workers = 8
	const perWorker = 750

	type interval struct {
		start uint64
		count int
	}
	var mu sync.Mutex
	intervals := make([]interval, 0, workers*perWorker)
	errCh := make(chan error, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			var prevEnd uint64
			for i := 0; i < perWorker; i++ {
				n := (i+worker)%5 + 1
				start, count := c.AllocTS(n)
				if count != n {
					errCh <- fmt.Errorf("AllocTS(%d) count = %d", n, count)
					return
				}
				if prevEnd != 0 && start < prevEnd {
					errCh <- fmt.Errorf("timestamp went backwards: start %d, previous end %d", start, prevEnd)
					return
				}
				prevEnd = start + uint64(count)
				mu.Lock()
				intervals = append(intervals, interval{start: start, count: count})
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if len(intervals) != workers*perWorker {
		t.Fatalf("got %d reservations, want %d", len(intervals), workers*perWorker)
	}
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].start < intervals[j].start
	})
	var prevEnd uint64
	for i, in := range intervals {
		if in.count <= 0 {
			t.Fatalf("reservation %d has count %d", i, in.count)
		}
		if i > 0 && in.start < prevEnd {
			t.Fatalf("overlap at reservation %d: start %d < previous end %d", i, in.start, prevEnd)
		}
		prevEnd = in.start + uint64(in.count)
	}
}

func TestMetaAllocTSLogicalRollover(t *testing.T) {
	c := meta.New()

	// Fill the logical field almost completely, then reserve across its end.
	start, count := c.AllocTS(1<<18 - 1)
	if count != 1<<18-1 {
		t.Fatalf("count = %d, want %d", count, 1<<18-1)
	}
	next, n := c.AllocTS(2)
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	if next < start+uint64(count) {
		t.Fatalf("rollover overlap: next %d, previous end %d", next, start+uint64(count))
	}

	// A reservation larger than the logical field must also be safe.
	hugeStart, hugeCount := c.AllocTS(1<<18 + 2)
	hugeNext, hugeNextCount := c.AllocTS(1)
	if hugeNextCount != 1 || hugeNext < hugeStart+uint64(hugeCount) {
		t.Fatalf("large reservation overlap: start %d count %d next %d", hugeStart, hugeCount, hugeNext)
	}
}
