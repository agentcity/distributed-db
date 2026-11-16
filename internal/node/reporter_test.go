package node

import (
	"testing"
	"time"
)

func TestOpsPerSecond(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		prevOps  uint64
		prevTime time.Time
		ops      uint64
		now      time.Time
		want     float64
	}{
		{
			name: "first sample",
			ops:  100, now: base, want: 0,
		},
		{
			name:    "100 ops in 5s",
			prevOps: 50, prevTime: base,
			ops: 150, now: base.Add(5 * time.Second),
			want: 20,
		},
		{
			name:    "no new ops",
			prevOps: 150, prevTime: base,
			ops: 150, now: base.Add(5 * time.Second),
			want: 0,
		},
		{
			name:    "zero interval",
			prevOps: 10, prevTime: base,
			ops: 20, now: base,
			want: 0,
		},
		{
			name:    "counter reset",
			prevOps: 100, prevTime: base,
			ops: 5, now: base.Add(time.Second),
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := opsPerSecond(tc.prevOps, tc.prevTime, tc.ops, tc.now)
			if got != tc.want {
				t.Fatalf("opsPerSecond() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadReporterObserve(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &loadReporter{lastOps: 100, lastReport: base}

	if got := r.observe(200, base.Add(5*time.Second)); got != 20 {
		t.Fatalf("first observe = %v ops/sec, want 20", got)
	}
	// The sample was recorded: the same counter over the next interval is idle.
	if got := r.observe(200, base.Add(10*time.Second)); got != 0 {
		t.Fatalf("second observe = %v ops/sec, want 0", got)
	}
	// Another 100 ops over 5 seconds is 20 ops/sec again.
	if got := r.observe(300, base.Add(15*time.Second)); got != 20 {
		t.Fatalf("third observe = %v ops/sec, want 20", got)
	}
}
