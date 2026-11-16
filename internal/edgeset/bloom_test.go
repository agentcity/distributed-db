package edgeset

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

func TestBloomFalsePositiveRate(t *testing.T) {
	const (
		n          = 2_000
		target     = 0.01
		queries    = 50_000
		segmentKey = uint16(0xbeef)
	)
	hashes := forcedPrefixHashes(n, segmentKey, 20)
	store, _ := mustBuild(t, hashes, BuildOptions{
		PrefixBytes:       2,
		FalsePositiveRate: target,
	})

	seg, err := store.loadSegment(segmentKey)
	if err != nil {
		t.Fatalf("loadSegment: %v", err)
	}
	if seg.meta.Count != n {
		t.Fatalf("segment count = %d, want %d", seg.meta.Count, n)
	}
	if got := len(seg.bloom.bits); got != (seg.meta.BloomBits+7)/8 {
		t.Fatalf("Bloom byte length = %d, want %d", got, (seg.meta.BloomBits+7)/8)
	}

	for _, h := range hashes {
		if !seg.bloom.maybe(h) {
			t.Fatalf("Bloom filter has a false negative for %s", h)
		}
	}

	rng := rand.New(rand.NewSource(21))
	positives := 0
	for i := 0; i < queries; i++ {
		h := randomHash(rng)
		binary.BigEndian.PutUint16(h[0:2], segmentKey)
		if seg.bloom.maybe(h) {
			positives++
		}
	}
	rate := float64(positives) / float64(queries)
	if rate > 3*target {
		t.Fatalf("Bloom false-positive rate %.4f exceeds 3x target %.4f (%d/%d)", rate, 3*target, positives, queries)
	}
	t.Logf("Bloom false-positive rate = %.4f (%d/%d), target %.2f", rate, positives, queries, target)

	// A Bloom positive is only a hint: Contains must still reject absent hashes
	// through the exact binary search, so the store-level false-positive rate
	// is zero by construction.
	for i := 0; i < 5_000; i++ {
		h := randomHash(rng)
		binary.BigEndian.PutUint16(h[0:2], segmentKey)
		if ok, proof, err := store.Contains(h); err != nil {
			t.Fatalf("Contains: %v", err)
		} else if ok || proof != nil {
			t.Fatalf("Contains returned a false positive for %s", h)
		}
	}
}

func TestBloomNoFalseNegatives(t *testing.T) {
	for _, rate := range []float64{0.001, 0.01, 0.1, 0.5} {
		rate := rate
		t.Run("rate", func(t *testing.T) {
			const n = 500
			filter, err := newBloomFilter(n, rate)
			if err != nil {
				t.Fatalf("newBloomFilter: %v", err)
			}
			if filter.m == 0 || filter.k == 0 {
				t.Fatalf("empty Bloom parameters for n=%d rate=%v", n, rate)
			}
			for _, h := range randomHashes(n, 22) {
				filter.add(h)
			}
			for _, h := range randomHashes(n, 22) {
				if !filter.maybe(h) {
					t.Fatalf("false negative for %s", h)
				}
			}
		})
	}
}

func TestMayContainNoFalseNegatives(t *testing.T) {
	const segmentKey = uint16(0x5151)
	hashes := forcedPrefixHashes(1_000, segmentKey, 23)
	store, _ := mustBuild(t, hashes, BuildOptions{FalsePositiveRate: 0.01})

	for _, h := range hashes {
		maybe, err := store.MayContain(h)
		if err != nil {
			t.Fatalf("MayContain(%s): %v", h, err)
		}
		if !maybe {
			t.Fatalf("MayContain(%s) = false for an inserted hash", h)
		}
	}

	// Reloading exercises the cache path with a fresh Bloom filter.
	reloaded, _ := mustLoad(t, store.Dir(), nil)
	for _, h := range hashes {
		maybe, err := reloaded.MayContain(h)
		if err != nil || !maybe {
			t.Fatalf("reloaded MayContain(%s) = %v, %v", h, maybe, err)
		}
	}

	rng := rand.New(rand.NewSource(24))
	bloomPositives := 0
	for i := 0; i < 5_000; i++ {
		h := randomHash(rng)
		binary.BigEndian.PutUint16(h[0:2], segmentKey)
		if maybe, err := store.MayContain(h); err != nil {
			t.Fatalf("MayContain(absent): %v", err)
		} else if maybe {
			bloomPositives++
		}
		if ok, _, err := store.Contains(h); err != nil {
			t.Fatalf("Contains(absent): %v", err)
		} else if ok {
			t.Fatalf("Contains returned a false positive for %s", h)
		}
	}
	t.Logf("MayContain Bloom positives for absent hashes: %d/5000", bloomPositives)

	// A prefix with no segment is definitely absent without touching a file.
	missing := uint16(0)
	for {
		if _, ok := store.SegmentMeta(missing); !ok {
			break
		}
		missing++
	}
	var absent Hash
	binary.BigEndian.PutUint16(absent[0:2], missing)
	if maybe, err := store.MayContain(absent); err != nil || maybe {
		t.Fatalf("MayContain(missing prefix) = %v, %v; want false, nil", maybe, err)
	}
}
