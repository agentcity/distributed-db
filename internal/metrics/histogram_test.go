package metrics

import (
	"math"
	"testing"
	"time"
)

func TestHistogramQuantiles(t *testing.T) {
	reg := NewRegistry()
	buckets := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		buckets = append(buckets, time.Duration(i)*10*time.Millisecond)
	}
	h := reg.NewHistogram("latency_seconds", "Latency.", nil, buckets)
	for i := 1; i <= 100; i++ {
		h.Observe(time.Duration(i) * 10 * time.Millisecond)
	}

	snap := h.Snapshot()
	if snap.Count != 100 {
		t.Fatalf("count = %d, want 100", snap.Count)
	}
	if snap.Overflow != 0 {
		t.Fatalf("overflow = %d, want 0", snap.Overflow)
	}
	if got, want := snap.Sum, 50.5; math.Abs(got-want) > 1e-12 {
		t.Fatalf("sum = %v, want %v", got, want)
	}

	cases := []struct {
		q    float64
		want float64 // milliseconds
	}{
		{0.50, 500},
		{0.95, 950},
		{0.99, 990},
		{1.00, 1000},
	}
	for _, tc := range cases {
		got := snap.Quantile(tc.q) * 1000
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Quantile(%v) = %vms, want %vms", tc.q, got, tc.want)
		}
	}
	if got := snap.Quantile(0); got != 0 {
		t.Errorf("Quantile(0) = %v, want 0", got)
	}
	if got, want := snap.P50()*1000, 500.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("P50 = %vms, want %vms", got, want)
	}
	if got, want := snap.P95()*1000, 950.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("P95 = %vms, want %vms", got, want)
	}
	if got, want := snap.P99()*1000, 990.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("P99 = %vms, want %vms", got, want)
	}
	if got, want := snap.Mean()*1000, 505.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("Mean = %vms, want %vms", got, want)
	}
}

func TestHistogramInterpolation(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("latency_seconds", "", nil, []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
	})
	// Spread values evenly through (10ms, 20ms]; the median should land near
	// the bucket midpoint rather than at either edge.
	for i := 0; i < 100; i++ {
		h.Observe(10*time.Millisecond + time.Duration(i+1)*100*time.Microsecond)
	}
	snap := h.Snapshot()
	got := snap.Quantile(0.50) * 1000
	if math.Abs(got-15) > 0.11 {
		t.Fatalf("Quantile(0.50) = %vms, want about 15ms", got)
	}
	if got := snap.Quantile(1.50); math.Abs(got-0.020) > 1e-12 {
		t.Fatalf("Quantile(1.5) = %v, want last finite bound 0.020", got)
	}
}

func TestHistogramOverflowAndEmpty(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("latency_seconds", "", nil, []time.Duration{
		time.Millisecond,
		2 * time.Millisecond,
	})
	h.Observe(time.Millisecond)
	h.Observe(2 * time.Millisecond)
	h.Observe(3 * time.Millisecond)
	h.Observe(100 * time.Millisecond)

	snap := h.Snapshot()
	if snap.Count != 4 {
		t.Fatalf("count = %d, want 4", snap.Count)
	}
	if snap.Overflow != 2 {
		t.Fatalf("overflow = %d, want 2", snap.Overflow)
	}
	wantBuckets := []uint64{1, 2}
	for i, want := range wantBuckets {
		if snap.Buckets[i] != want {
			t.Fatalf("bucket[%d] = %d, want %d", i, snap.Buckets[i], want)
		}
	}
	if got := snap.Quantile(0.50); got != 0.002 {
		t.Errorf("Quantile(0.50) = %v, want 0.002", got)
	}
	// The +Inf bucket has no finite upper bound; quantiles inside it clamp to
	// the last finite bound.
	if got := snap.Quantile(0.75); got != 0.002 {
		t.Errorf("Quantile(0.75) = %v, want 0.002", got)
	}

	empty := (&HistogramSnapshot{}).Quantile(0.95)
	if empty != 0 {
		t.Fatalf("empty Quantile = %v, want 0", empty)
	}
	if got := (&HistogramSnapshot{}).Mean(); got != 0 {
		t.Fatalf("empty Mean = %v, want 0", got)
	}
}

func TestHistogramBucketNormalization(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("latency_seconds", "", nil, []time.Duration{
		5 * time.Millisecond,
		-time.Millisecond, // ignored
		time.Millisecond,
		5 * time.Millisecond, // duplicate removed
	})
	h.Observe(time.Millisecond)
	h.Observe(5 * time.Millisecond)
	snap := h.Snapshot()
	want := []float64{0.001, 0.005}
	if len(snap.Bounds) != len(want) {
		t.Fatalf("bounds = %v, want %v", snap.Bounds, want)
	}
	for i := range want {
		if snap.Bounds[i] != want[i] {
			t.Fatalf("bounds = %v, want %v", snap.Bounds, want)
		}
	}
	if snap.Buckets[0] != 1 || snap.Buckets[1] != 2 {
		t.Fatalf("buckets = %v, want [1 2]", snap.Buckets)
	}
}
