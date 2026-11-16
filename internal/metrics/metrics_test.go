package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestCounterGaugeSnapshot(t *testing.T) {
	reg := NewRegistry()
	labels := Labels{"op": "get"}
	c := reg.NewCounter("requests_total", "Requests.", labels)
	labels["op"] = "mutated-after-construction"

	c.Add(2)
	c.Inc()
	c.Add(3)
	if got := c.Value(); got != 6 {
		t.Fatalf("counter = %d, want 6", got)
	}

	g := reg.NewGauge("inflight", "Inflight.", nil)
	g.Set(5)
	g.Add(2)
	g.Dec()
	if got := g.Value(); got != 6 {
		t.Fatalf("gauge = %d, want 6", got)
	}

	snap := reg.Snapshot()
	if len(snap.Metrics) != 2 {
		t.Fatalf("metrics = %d, want 2", len(snap.Metrics))
	}
	counter := findMetric(t, snap, "requests_total")
	if counter.Value != 6 {
		t.Fatalf("snapshot counter = %v, want 6", counter.Value)
	}
	if got := counter.Labels["op"]; got != "get" {
		t.Fatalf("snapshot label = %q, want get", got)
	}
	if counter.Type != CounterType || counter.Help != "Requests." {
		t.Fatalf("counter metadata = %+v", counter)
	}
	gauge := findMetric(t, snap, "inflight")
	if gauge.Value != 6 || gauge.Type != GaugeType {
		t.Fatalf("gauge snapshot = %+v", gauge)
	}
}

func TestRegistryRejectsDuplicateSeries(t *testing.T) {
	t.Run("same series", func(t *testing.T) {
		reg := NewRegistry()
		reg.NewCounter("x_total", "X.", nil)
		assertPanics(t, func() { reg.NewCounter("x_total", "X.", nil) })
	})
	t.Run("same name different labels", func(t *testing.T) {
		reg := NewRegistry()
		reg.NewCounter("x_total", "X.", Labels{"op": "a"})
		reg.NewCounter("x_total", "X.", Labels{"op": "b"}) // valid: distinct series
	})
	t.Run("conflicting type", func(t *testing.T) {
		reg := NewRegistry()
		reg.NewCounter("x", "X.", Labels{"op": "a"})
		assertPanics(t, func() { reg.NewGauge("x", "X.", Labels{"op": "b"}) })
	})
}

func TestConcurrentIncrements(t *testing.T) {
	const (
		goroutines   = 32
		perGoroutine = 1000
	)
	reg := NewRegistry()
	counter := reg.NewCounter("requests_total", "Requests.", nil)
	gauge := reg.NewGauge("inflight", "Inflight.", nil)
	histogram := reg.NewHistogram("latency_seconds", "Latency.", nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				counter.Inc()
				gauge.Inc()
				histogram.Observe(2 * time.Millisecond)
			}
		}()
	}

	// Exercise snapshots while writers are active; go test -race also covers
	// this path. The snapshot values themselves are intentionally best-effort.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = reg.Snapshot()
			}
		}
	}()

	wg.Wait()
	close(stop)
	readers.Wait()

	want := uint64(goroutines * perGoroutine)
	if got := counter.Value(); got != want {
		t.Fatalf("counter = %d, want %d", got, want)
	}
	if got := gauge.Value(); got != int64(want) {
		t.Fatalf("gauge = %d, want %d", got, want)
	}
	snap := histogram.Snapshot()
	if snap.Count != want {
		t.Fatalf("histogram count = %d, want %d", snap.Count, want)
	}
	if snap.Overflow != 0 {
		t.Fatalf("histogram overflow = %d, want 0", snap.Overflow)
	}
	if got := snap.Buckets[len(snap.Buckets)-1]; got != want {
		t.Fatalf("last finite bucket = %d, want %d", got, want)
	}
}

func findMetric(t *testing.T, snap *Snapshot, name string) MetricValue {
	t.Helper()
	for _, m := range snap.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q not found in snapshot", name)
	return MetricValue{}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
