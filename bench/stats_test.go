package main

import (
	"math"
	"testing"
	"time"
)

func TestLatencyCollectorEmpty(t *testing.T) {
	c := NewLatencyCollector(8)
	if c.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", c.Len())
	}
	for _, p := range []float64{0, 1, 50, 95, 99, 99.9, 100, -10, 110, math.NaN()} {
		if got := c.Percentile(p); got != 0 {
			t.Errorf("Percentile(%v) = %v, want 0 for empty collector", p, got)
		}
	}
	if got := c.Summary(); got != (LatencySummary{}) {
		t.Errorf("Summary() = %+v, want zero value for empty collector", got)
	}
}

func TestLatencyPercentileNearestRank(t *testing.T) {
	c := NewLatencyCollector(100)
	for i := 1; i <= 100; i++ {
		c.Add(time.Duration(i))
	}

	tests := []struct {
		p    float64
		want time.Duration
	}{
		{-5, 1},
		{0, 1},
		{0.5, 1},
		{1, 1},
		{49.9, 50},
		{50, 50},
		{50.1, 51},
		{95, 95},
		{99, 99},
		{99.9, 100},
		{100, 100},
		{101, 100},
		{math.Inf(1), 100},
		{math.Inf(-1), 1},
	}
	for _, tt := range tests {
		if got := c.Percentile(tt.p); got != tt.want {
			t.Errorf("Percentile(%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestLatencySummaryUnsorted(t *testing.T) {
	c := NewLatencyCollector(0)
	for _, d := range []time.Duration{30, 10, 40, 50, 20} {
		c.Add(d)
	}

	got := c.Summary()
	want := LatencySummary{
		Count: 5,
		Min:   10,
		Mean:  30,
		Max:   50,
		P50:   30,
		P95:   50,
		P99:   50,
		P999:  50,
	}
	if got != want {
		t.Fatalf("Summary() = %+v, want %+v", got, want)
	}
}

func TestLatencySingleSample(t *testing.T) {
	c := NewLatencyCollector(1)
	c.Add(7 * time.Nanosecond)

	got := c.Summary()
	if got.Count != 1 || got.Min != 7 || got.Mean != 7 || got.Max != 7 {
		t.Fatalf("Summary() = %+v, want all fields = 7ns with count 1", got)
	}
	for _, p := range []float64{0, 50, 95, 99, 99.9, 100} {
		if got := c.Percentile(p); got != 7*time.Nanosecond {
			t.Errorf("Percentile(%v) = %v, want 7ns", p, got)
		}
	}
}

func TestLatencyTwoSamples(t *testing.T) {
	c := NewLatencyCollector(2)
	c.Add(10)
	c.Add(20)

	if got := c.Percentile(50); got != 10 {
		t.Errorf("p50 = %v, want 10", got)
	}
	if got := c.Percentile(95); got != 20 {
		t.Errorf("p95 = %v, want 20", got)
	}
	if got := c.Percentile(99.9); got != 20 {
		t.Errorf("p999 = %v, want 20", got)
	}
}

func TestLatencyMeanTruncates(t *testing.T) {
	c := NewLatencyCollector(2)
	c.Add(1)
	c.Add(2)
	if got := c.Summary().Mean; got != 1 {
		t.Errorf("Mean = %v, want 1ns (truncated)", got)
	}
}

func TestLatencyAddAfterPercentile(t *testing.T) {
	c := NewLatencyCollector(4)
	c.Add(3)
	c.Add(1)
	c.Add(2)

	if got := c.Percentile(50); got != 2 {
		t.Fatalf("p50 before add = %v, want 2", got)
	}
	c.Add(10)
	if got := c.Percentile(50); got != 2 {
		t.Errorf("p50 after add = %v, want 2", got)
	}
	if got := c.Percentile(100); got != 10 {
		t.Errorf("max after add = %v, want 10", got)
	}
}

func TestLatencyMerge(t *testing.T) {
	a := NewLatencyCollector(2)
	a.Add(1)
	a.Add(2)
	if got := a.Percentile(100); got != 2 {
		t.Fatalf("a max = %v, want 2", got)
	}

	b := NewLatencyCollector(2)
	b.Add(3)
	b.Add(4)

	a.Merge(b, nil, a) // nil and self merges are ignored
	if a.Len() != 4 {
		t.Fatalf("Len after merge = %d, want 4", a.Len())
	}
	if got := a.Percentile(100); got != 4 {
		t.Errorf("max after merge = %v, want 4", got)
	}
	if got := a.Percentile(50); got != 2 {
		t.Errorf("p50 after merge = %v, want 2", got)
	}
}

func TestLatencyReset(t *testing.T) {
	c := NewLatencyCollector(2)
	c.Add(5)
	c.Reset()
	if c.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", c.Len())
	}
	c.Add(9)
	if got := c.Percentile(50); got != 9 {
		t.Errorf("p50 after Reset+Add = %v, want 9", got)
	}
}

func TestLatencyPercentilesMonotonic(t *testing.T) {
	c := NewLatencyCollector(1000)
	for i := 0; i < 1000; i++ {
		c.Add(time.Duration((i * 7919) % 1000))
	}
	s := c.Summary()
	if !(s.Min <= s.P50 && s.P50 <= s.P95 && s.P95 <= s.P99 && s.P99 <= s.P999 && s.P999 <= s.Max) {
		t.Fatalf("percentiles not monotonic: %+v", s)
	}
	if s.Mean < s.Min || s.Mean > s.Max {
		t.Fatalf("mean outside [min,max]: %+v", s)
	}
}
