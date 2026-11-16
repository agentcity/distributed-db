package main

import (
	"math"
	"sort"
	"time"
)

// LatencySummary is a snapshot of a LatencyCollector.
type LatencySummary struct {
	Count int
	Min   time.Duration
	Mean  time.Duration
	Max   time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	P999  time.Duration
}

// LatencyCollector gathers per-operation latencies. Percentiles use the
// nearest-rank definition: for p in (0, 100], the result is the
// ceil(p/100 * n)-th smallest sample. p <= 0 yields the minimum, p >= 100 the
// maximum, and an empty collector yields zero.
//
// The collector is deliberately not safe for concurrent use: each worker owns
// one collector and the results are merged once at the end, which avoids a
// mutex on the measured path.
type LatencyCollector struct {
	samples []time.Duration
	sorted  bool
}

// NewLatencyCollector returns a collector with room for capacity samples.
func NewLatencyCollector(capacity int) *LatencyCollector {
	if capacity < 0 {
		capacity = 0
	}
	return &LatencyCollector{samples: make([]time.Duration, 0, capacity)}
}

// Add records one latency sample.
func (c *LatencyCollector) Add(d time.Duration) {
	c.samples = append(c.samples, d)
	c.sorted = false
}

// Len reports the number of recorded samples.
func (c *LatencyCollector) Len() int {
	return len(c.samples)
}

// Reset drops all samples.
func (c *LatencyCollector) Reset() {
	c.samples = c.samples[:0]
	c.sorted = true
}

// Merge appends the samples of others. It is safe to merge an empty or nil
// collector; merging a collector into itself is ignored.
func (c *LatencyCollector) Merge(others ...*LatencyCollector) {
	for _, other := range others {
		if other == nil || other == c || len(other.samples) == 0 {
			continue
		}
		c.samples = append(c.samples, other.samples...)
		c.sorted = false
	}
}

// Percentile returns the nearest-rank percentile for p in [0, 100].
// Non-finite or out-of-range values are clamped to the [0, 100] endpoints.
func (c *LatencyCollector) Percentile(p float64) time.Duration {
	if len(c.samples) == 0 {
		return 0
	}
	c.ensureSorted()
	return percentileOfSorted(c.samples, p)
}

// Summary computes count, min, mean, max and the p50/p95/p99/p999
// percentiles. The mean is truncated to a whole nanosecond.
func (c *LatencyCollector) Summary() LatencySummary {
	n := len(c.samples)
	if n == 0 {
		return LatencySummary{}
	}
	c.ensureSorted()

	var sum time.Duration
	for _, d := range c.samples {
		sum += d
	}

	return LatencySummary{
		Count: n,
		Min:   c.samples[0],
		Mean:  sum / time.Duration(n),
		Max:   c.samples[n-1],
		P50:   percentileOfSorted(c.samples, 50),
		P95:   percentileOfSorted(c.samples, 95),
		P99:   percentileOfSorted(c.samples, 99),
		P999:  percentileOfSorted(c.samples, 99.9),
	}
}

func (c *LatencyCollector) ensureSorted() {
	if c.sorted {
		return
	}
	sort.Slice(c.samples, func(i, j int) bool { return c.samples[i] < c.samples[j] })
	c.sorted = true
}

// percentileOfSorted implements the nearest-rank percentile on a sorted
// slice. The caller must pass an ascending slice.
func percentileOfSorted(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if math.IsNaN(p) || p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}

	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
