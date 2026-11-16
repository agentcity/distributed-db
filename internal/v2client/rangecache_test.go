package v2client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"
)

func rangeByKey(key []byte) (*pbv2.RangeInfo, error) {
	if bytes.Compare(key, []byte("m")) < 0 {
		return &pbv2.RangeInfo{
			Id: 1, End: []byte("m"), Version: 1,
			Replicas: []string{"kv1"}, Leader: "kv1",
		}, nil
	}
	return &pbv2.RangeInfo{
		Id: 2, Start: []byte("m"), Version: 1,
		Replicas: []string{"kv1"}, Leader: "kv1",
	}, nil
}

func TestRangeCacheHitByBounds(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(rangeByKey)
	c := newTestClient(fake)
	ctx := context.Background()

	r, err := c.rangeFor(ctx, []byte("a"))
	if err != nil {
		t.Fatalf("rangeFor(a): %v", err)
	}
	if r.GetId() != 1 {
		t.Fatalf("rangeFor(a) id = %d, want 1", r.GetId())
	}
	if _, err := c.rangeFor(ctx, []byte("b")); err != nil {
		t.Fatalf("rangeFor(b): %v", err)
	}
	if _, _, calls, _ := fake.stats(); calls != 1 {
		t.Fatalf("meta GetRange calls after two keys in range 1 = %d, want 1", calls)
	}
	r, err = c.rangeFor(ctx, []byte("z"))
	if err != nil {
		t.Fatalf("rangeFor(z): %v", err)
	}
	if r.GetId() != 2 {
		t.Fatalf("rangeFor(z) id = %d, want 2", r.GetId())
	}
	if _, _, calls, _ := fake.stats(); calls != 2 {
		t.Fatalf("meta GetRange calls = %d, want 2", calls)
	}
}

func TestRangeCacheTTL(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(rangeByKey)
	c := newTestClient(fake)
	c.rangeTTL = 10 * time.Millisecond
	ctx := context.Background()

	if _, err := c.rangeFor(ctx, []byte("a")); err != nil {
		t.Fatalf("first rangeFor: %v", err)
	}
	if _, err := c.rangeFor(ctx, []byte("a")); err != nil {
		t.Fatalf("cached rangeFor: %v", err)
	}
	if _, _, calls, _ := fake.stats(); calls != 1 {
		t.Fatalf("meta GetRange calls = %d, want 1", calls)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := c.rangeFor(ctx, []byte("a")); err != nil {
		t.Fatalf("rangeFor after TTL: %v", err)
	}
	if _, _, calls, _ := fake.stats(); calls != 2 {
		t.Fatalf("meta GetRange calls after TTL = %d, want 2", calls)
	}
}

func TestRangeCacheInvalidationAfterSplitAndMerge(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		return &pbv2.RangeInfo{Id: 1, Version: 1, Replicas: []string{"kv1"}, Leader: "kv1"}, nil
	})
	c := newTestClient(fake)
	ctx := context.Background()

	r, err := c.rangeFor(ctx, []byte("a"))
	if err != nil || r.GetId() != 1 || r.GetVersion() != 1 {
		t.Fatalf("initial rangeFor = %+v, %v", r, err)
	}

	// Meta now reports the split ranges, but the client still has the broad
	// v1 descriptor cached until an RPC reports a stale epoch.
	fake.setGetRange(rangeByKey)
	r, err = c.rangeFor(ctx, []byte("z"))
	if err != nil {
		t.Fatalf("rangeFor(z) with cached broad range: %v", err)
	}
	if r.GetId() != 1 || r.GetVersion() != 1 {
		t.Fatalf("cached broad range = id %d version %d, want 1/1", r.GetId(), r.GetVersion())
	}

	c.invalidateRangeForKey(1, []byte("z"))
	if c.cachedRange([]byte("a")) != nil {
		t.Fatal("stale broad range survived invalidation")
	}
	r, err = c.rangeFor(ctx, []byte("z"))
	if err != nil {
		t.Fatalf("rangeFor(z) after invalidation: %v", err)
	}
	if r.GetId() != 2 || r.GetVersion() != 1 {
		t.Fatalf("rangeFor(z) = id %d version %d, want 2/1", r.GetId(), r.GetVersion())
	}
	r, err = c.rangeFor(ctx, []byte("a"))
	if err != nil || r.GetId() != 1 {
		t.Fatalf("rangeFor(a) after split = %+v, %v", r, err)
	}

	// Merge back into range 1 v3. Invalidating the right child must also drop
	// the now-overlapping left child when the merged descriptor is cached.
	fake.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		return &pbv2.RangeInfo{Id: 1, Version: 3, Replicas: []string{"kv1"}, Leader: "kv1"}, nil
	})
	c.invalidateRangeForKey(2, []byte("z"))
	r, err = c.rangeFor(ctx, []byte("z"))
	if err != nil {
		t.Fatalf("rangeFor(z) after merge: %v", err)
	}
	if r.GetId() != 1 || r.GetVersion() != 3 {
		t.Fatalf("merged range = id %d version %d, want 1/3", r.GetId(), r.GetVersion())
	}
	if r := c.cachedRange([]byte("a")); r == nil || r.GetId() != 1 || r.GetVersion() != 3 {
		t.Fatalf("rangeFor(a) after merge = %+v, want merged v3", r)
	}
}

func TestRangeCacheCapEvictsOldest(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(func(key []byte) (*pbv2.RangeInfo, error) {
		n := 0
		_, _ = fmt.Sscanf(string(key), "k%d", &n)
		end := append(append([]byte(nil), key...), '!')
		return &pbv2.RangeInfo{
			Id: uint64(n + 1), Start: append([]byte(nil), key...), End: end, Version: 1,
		}, nil
	})
	c := newTestClient(fake)
	c.rangeCap = 2
	c.rangeTTL = time.Hour
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("k%d", i))
		if _, err := c.rangeFor(ctx, key); err != nil {
			t.Fatalf("rangeFor(%s): %v", key, err)
		}
		if got := len(c.ranges); got > c.rangeCap {
			t.Fatalf("cache holds %d entries, cap %d", got, c.rangeCap)
		}
	}
	if c.cachedRange([]byte("k0")) != nil {
		t.Fatal("oldest cached range was not evicted")
	}
	if r := c.cachedRange([]byte("k9")); r == nil || r.GetId() != 10 {
		t.Fatalf("latest cached range = %+v, want id 10", r)
	}
}

func TestRangeCacheConcurrent(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(func([]byte) (*pbv2.RangeInfo, error) {
		return &pbv2.RangeInfo{Id: 1, Version: 1, Replicas: []string{"kv1"}, Leader: "kv1"}, nil
	})
	c := newTestClient(fake)
	ctx := context.Background()

	const goroutines = 16
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				r, err := c.rangeFor(ctx, []byte("key"))
				if err != nil {
					errs <- err
					return
				}
				if r.GetId() != 1 {
					errs <- fmt.Errorf("range id = %d, want 1", r.GetId())
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, _, calls, _ := fake.stats(); calls > goroutines {
		t.Fatalf("meta GetRange calls = %d, want <= %d", calls, goroutines)
	}
}

func TestMetaFailoverInvalidatesRangeCache(t *testing.T) {
	fake := newFakeMeta("meta1")
	fake.setGetRange(rangeByKey)
	c := newTestClient(fake)
	ctx := context.Background()

	if _, err := c.rangeFor(ctx, []byte("a")); err != nil {
		t.Fatalf("rangeFor: %v", err)
	}
	c.invalidateMetaLeader()
	if r := c.cachedRange([]byte("a")); r != nil {
		t.Fatalf("range cache survived meta failover: %+v", r)
	}
	if _, err := c.rangeFor(ctx, []byte("a")); err != nil {
		t.Fatalf("rangeFor after failover: %v", err)
	}
}
