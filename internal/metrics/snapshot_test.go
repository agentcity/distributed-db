package metrics

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

func TestSnapshotJSONRoundTrip(t *testing.T) {
	reg := NewRegistry()
	counter := reg.NewCounter("requests_total", "Requests.", Labels{"op": "get"})
	counter.Add(7)
	gauge := reg.NewGauge("inflight", "Inflight.", nil)
	gauge.Set(3)
	histogram := reg.NewHistogram("latency_seconds", "Latency.", Labels{"op": "get"}, []time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
	})
	histogram.Observe(500 * time.Microsecond)
	histogram.Observe(2 * time.Millisecond)

	snap := reg.Snapshot()
	var buf bytes.Buffer
	if err := snap.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	loaded, err := ReadSnapshot(&buf)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if !reflect.DeepEqual(snap, loaded) {
		t.Fatalf("round trip mismatch\ngot:  %#v\nwant: %#v", loaded, snap)
	}
}

func TestSnapshotMergeAndLabel(t *testing.T) {
	a := &Snapshot{Metrics: []MetricValue{
		{Name: "a_total", Type: CounterType, Value: 1, Labels: Labels{"k": "v"}},
	}}
	b := &Snapshot{Metrics: []MetricValue{
		{Name: "b_total", Type: CounterType, Value: 2},
	}}

	a.Merge(b)
	if len(a.Metrics) != 2 {
		t.Fatalf("merged metrics = %d, want 2", len(a.Metrics))
	}
	if len(a.Metrics[0].Labels) != 1 {
		t.Fatalf("original labels mutated: %v", a.Metrics[0].Labels)
	}

	tagged := a.WithLabel("source", "peer-1")
	for _, m := range tagged.Metrics {
		if got := m.Labels["source"]; got != "peer-1" {
			t.Fatalf("tagged source = %q, want peer-1", got)
		}
	}
	for _, m := range a.Metrics {
		if _, ok := m.Labels["source"]; ok {
			t.Fatalf("WithLabel mutated receiver: %v", m.Labels)
		}
	}

	// nil snapshots are ignored by Merge.
	tagged.Merge(nil)
	if len(tagged.Metrics) != 2 {
		t.Fatalf("merge nil changed metrics: %d", len(tagged.Metrics))
	}
}

func TestSnapshotSortAndClone(t *testing.T) {
	snap := &Snapshot{Metrics: []MetricValue{
		{Name: "b", Type: CounterType},
		{Name: "a", Type: CounterType, Labels: Labels{"z": "1"}},
		{Name: "a", Type: CounterType, Labels: Labels{"a": "1"}},
	}}
	snap.Sort()
	got := []string{
		snap.Metrics[0].Name,
		snap.Metrics[1].Name,
		snap.Metrics[2].Name,
	}
	if !reflect.DeepEqual(got, []string{"a", "a", "b"}) {
		t.Fatalf("sort names = %v", got)
	}
	if snap.Metrics[0].Labels["a"] != "1" || snap.Metrics[1].Labels["z"] != "1" {
		t.Fatalf("label sort order wrong: %v", snap.Metrics)
	}

	clone := snap.Clone()
	clone.Metrics[0].Labels["a"] = "changed"
	if snap.Metrics[0].Labels["a"] != "1" {
		t.Fatal("Clone shared label map")
	}
}
