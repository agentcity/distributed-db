package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPrometheusExposition(t *testing.T) {
	reg := NewRegistry()

	counter := reg.NewCounter("rpc_requests_total", "RPC requests", Labels{
		"method": `say "hi"`,
		"path":   `a\b`,
	})
	counter.Add(2)

	gauge := reg.NewGauge("inflight", "Inflight requests", nil)
	gauge.Set(1)

	histogram := reg.NewHistogram("rpc_latency_seconds", "RPC latency", Labels{"op": "get"}, []time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
	})
	histogram.Observe(500 * time.Microsecond)
	histogram.Observe(2 * time.Millisecond)
	histogram.Observe(10 * time.Millisecond)

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}

	want := `# HELP inflight Inflight requests
# TYPE inflight gauge
inflight 1
# HELP rpc_latency_seconds RPC latency
# TYPE rpc_latency_seconds histogram
rpc_latency_seconds_bucket{le="0.001",op="get"} 1
rpc_latency_seconds_bucket{le="0.005",op="get"} 2
rpc_latency_seconds_bucket{le="+Inf",op="get"} 3
rpc_latency_seconds_sum{op="get"} 0.0125
rpc_latency_seconds_count{op="get"} 3
# HELP rpc_requests_total RPC requests
# TYPE rpc_requests_total counter
rpc_requests_total{method="say \"hi\"",path="a\\b"} 2
`
	if got := buf.String(); got != want {
		t.Fatalf("exposition mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrometheusEscaping(t *testing.T) {
	snap := &Snapshot{Metrics: []MetricValue{{
		Name:   "escaped_total",
		Help:   "line one\nline two \\ end",
		Type:   CounterType,
		Labels: Labels{"v": "a\nb\\c\"d"},
		Value:  1,
	}}}
	var buf bytes.Buffer
	if err := snap.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	want := `# HELP escaped_total line one\nline two \\ end
# TYPE escaped_total counter
escaped_total{v="a\nb\\c\"d"} 1
`
	if got := buf.String(); got != want {
		t.Fatalf("escaping mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrometheusMultipleSeriesOneFamily(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("ops_total", "Operations.", Labels{"op": "get"}).Inc()
	reg.NewCounter("ops_total", "Operations.", Labels{"op": "put"}).Add(2)

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	got := buf.String()
	if n := strings.Count(got, "# HELP ops_total Operations."); n != 1 {
		t.Fatalf("HELP count = %d, want 1:\n%s", n, got)
	}
	if n := strings.Count(got, "# TYPE ops_total counter"); n != 1 {
		t.Fatalf("TYPE count = %d, want 1:\n%s", n, got)
	}
	for _, want := range []string{`ops_total{op="get"} 1`, `ops_total{op="put"} 2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestPrometheusEmptyAndConflicts(t *testing.T) {
	empty := &Snapshot{}
	var buf bytes.Buffer
	if err := empty.WritePrometheus(&buf); err != nil {
		t.Fatalf("empty WritePrometheus: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty exposition = %q, want empty", buf.String())
	}

	bad := &Snapshot{Metrics: []MetricValue{
		{Name: "x", Type: CounterType, Value: 1},
		{Name: "x", Type: GaugeType, Value: 2},
	}}
	if err := bad.WritePrometheus(&buf); err == nil || !strings.Contains(err.Error(), "conflicting types") {
		t.Fatalf("conflicting types error = %v", err)
	}
}
