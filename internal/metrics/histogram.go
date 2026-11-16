package metrics

import (
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// defaultLatencyBuckets covers sub-millisecond RPCs through multi-second
// snapshot transfers. Snapshot converts the bounds to seconds for exposition
// and quantile calculations.
var defaultLatencyBuckets = []time.Duration{
	100 * time.Microsecond,
	250 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// DefaultLatencyBuckets returns a copy of the default histogram bucket bounds.
func DefaultLatencyBuckets() []time.Duration {
	out := make([]time.Duration, len(defaultLatencyBuckets))
	copy(out, defaultLatencyBuckets)
	return out
}

// Histogram is a fixed-bucket latency histogram. Observations are stored as
// monotonic bucket counts, so recording is lock-free and bounded regardless of
// traffic volume. Percentiles are estimated from the buckets with linear
// interpolation inside the containing bucket, the same approximation used by
// Prometheus histogram_quantile.
type Histogram struct {
	desc metricDesc

	bounds   []time.Duration
	counts   []atomic.Uint64 // one per finite bound; non-cumulative
	overflow atomic.Uint64   // observations above the last finite bound
	count    atomic.Uint64
	sum      atomic.Int64 // total observed nanoseconds
}

func newHistogram(desc metricDesc, buckets []time.Duration) *Histogram {
	bounds := normalizeBuckets(buckets)
	return &Histogram{
		desc:   desc,
		bounds: bounds,
		counts: make([]atomic.Uint64, len(bounds)),
	}
}

func normalizeBuckets(in []time.Duration) []time.Duration {
	if len(in) == 0 {
		return DefaultLatencyBuckets()
	}
	sorted := make([]time.Duration, 0, len(in))
	for _, b := range in {
		if b >= 0 {
			sorted = append(sorted, b)
		}
	}
	if len(sorted) == 0 {
		return DefaultLatencyBuckets()
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make([]time.Duration, 0, len(sorted))
	for i, b := range sorted {
		if i == 0 || b != sorted[i-1] {
			out = append(out, b)
		}
	}
	return out
}

// Observe records one latency. Negative durations are clamped to zero.
//
// Bucket counters are updated before the total count and sum, so a snapshot
// taken concurrently with writers can lag the count slightly but cannot
// corrupt the histogram. A quiescent histogram is exact.
func (h *Histogram) Observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	i := sort.Search(len(h.bounds), func(i int) bool { return d <= h.bounds[i] })
	if i == len(h.bounds) {
		h.overflow.Add(1)
	} else {
		h.counts[i].Add(1)
	}
	h.sum.Add(int64(d))
	h.count.Add(1)
}

// Count returns the number of recorded observations.
func (h *Histogram) Count() uint64 { return h.count.Load() }

// Snapshot returns a copy of the histogram's bucket counts and aggregates.
func (h *Histogram) Snapshot() *HistogramSnapshot {
	s := &HistogramSnapshot{
		Bounds:  make([]float64, len(h.bounds)),
		Buckets: make([]uint64, len(h.bounds)),
	}
	var cumulative uint64
	for i := range h.bounds {
		cumulative += h.counts[i].Load()
		s.Buckets[i] = cumulative
		s.Bounds[i] = h.bounds[i].Seconds()
	}
	s.Overflow = h.overflow.Load()
	s.Sum = float64(h.sum.Load()) / float64(time.Second)
	s.Count = h.count.Load()
	return s
}

func (h *Histogram) metricSnapshot() MetricValue {
	return MetricValue{
		Name:      h.desc.name,
		Help:      h.desc.help,
		Type:      h.desc.typ,
		Labels:    h.desc.labels.clone(),
		Histogram: h.Snapshot(),
	}
}

// HistogramSnapshot is a point-in-time copy of a Histogram. Bounds are finite
// upper bounds in seconds; Buckets holds cumulative counts for those bounds;
// Overflow counts observations above the last finite bound. The implicit
// +Inf bucket count is Count.
type HistogramSnapshot struct {
	Bounds   []float64 `json:"bounds_seconds"`
	Buckets  []uint64  `json:"buckets"`
	Overflow uint64    `json:"overflow"`
	Count    uint64    `json:"count"`
	Sum      float64   `json:"sum_seconds"`
}

// Quantile estimates the q-quantile (0..1) in seconds. It returns 0 for an
// empty histogram, clamps q to [0,1], interpolates within the containing
// bucket, and returns the last finite bound for observations in the +Inf
// bucket.
func (s *HistogramSnapshot) Quantile(q float64) float64 {
	if s == nil || s.Count == 0 || len(s.Bounds) == 0 || len(s.Bounds) != len(s.Buckets) || math.IsNaN(q) || q <= 0 {
		return 0
	}
	if q > 1 {
		q = 1
	}
	rank := q * float64(s.Count)
	idx := sort.Search(len(s.Buckets), func(i int) bool {
		return float64(s.Buckets[i]) >= rank
	})
	if idx == len(s.Buckets) {
		return s.Bounds[len(s.Bounds)-1]
	}

	upper := s.Bounds[idx]
	var lower float64
	var before uint64
	if idx > 0 {
		lower = s.Bounds[idx-1]
		before = s.Buckets[idx-1]
	}
	inBucket := s.Buckets[idx] - before
	if inBucket == 0 {
		return upper
	}
	fraction := (rank - float64(before)) / float64(inBucket)
	return lower + (upper-lower)*fraction
}

// Mean returns the mean observation in seconds, or 0 when no observations
// have been recorded.
func (s *HistogramSnapshot) Mean() float64 {
	if s == nil || s.Count == 0 {
		return 0
	}
	return s.Sum / float64(s.Count)
}

// P50 returns the estimated median in seconds.
func (s *HistogramSnapshot) P50() float64 { return s.Quantile(0.50) }

// P95 returns the estimated 95th percentile in seconds.
func (s *HistogramSnapshot) P95() float64 { return s.Quantile(0.95) }

// P99 returns the estimated 99th percentile in seconds.
func (s *HistogramSnapshot) P99() float64 { return s.Quantile(0.99) }
