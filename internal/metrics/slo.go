package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// DefaultPrefix is used by NewSLO when SLOConfig.Prefix is empty.
const DefaultPrefix = "db"

// SLOConfig configures the four metrics owned by an SLO.
type SLOConfig struct {
	// Prefix is prepended to the metric names, for example "db" produces
	// db_requests_total. It defaults to DefaultPrefix; a trailing underscore is
	// removed.
	Prefix string
	// Labels are attached to every SLO metric, for example {"node": "n1"}.
	Labels Labels
	// Buckets overrides the latency histogram bounds. nil uses
	// DefaultLatencyBuckets.
	Buckets []time.Duration
}

// SLO is a requests/errors/inflight/latency instrumentation set created by
// NewSLO. It is safe for concurrent use and implements Recorder, Tracker, and
// the per-statement hook shape used by the SQL engine (ObserveStatement).
type SLO struct {
	Requests *Counter
	Errors   *Counter
	Inflight *Gauge
	Latency  *Histogram
}

// NewSLO creates the SLO metrics in reg. If reg is nil a private registry is
// created; callers that also expose the metrics should pass their registry.
func NewSLO(reg *Registry, cfg SLOConfig) *SLO {
	if reg == nil {
		reg = NewRegistry()
	}
	prefix := strings.TrimSuffix(cfg.Prefix, "_")
	if prefix == "" {
		prefix = DefaultPrefix
	}
	labels := cfg.Labels
	return &SLO{
		Requests: reg.NewCounter(prefix+"_requests_total", "Total requests observed.", labels),
		Errors:   reg.NewCounter(prefix+"_errors_total", "Total requests that returned an error.", labels),
		Inflight: reg.NewGauge(prefix+"_inflight", "Requests currently in flight.", labels),
		Latency:  reg.NewHistogram(prefix+"_request_duration_seconds", "Request latency in seconds.", labels, cfg.Buckets),
	}
}

// ObserveRequest records one completed request. A non-nil err increments the
// error counter. The latency is recorded for successes and failures alike.
func (s *SLO) ObserveRequest(latency time.Duration, err error) {
	s.Requests.Inc()
	if err != nil {
		s.Errors.Inc()
	}
	if latency < 0 {
		latency = 0
	}
	s.Latency.Observe(latency)
}

// ObserveStatement adapts SLO to per-statement instrumentation hooks (such as
// sqlengine.Metrics) by folding every statement into the aggregate SLO. The
// kind argument is ignored; callers that need a bounded per-kind breakdown
// should use NewLabeledSLO or SQLMetrics.
func (s *SLO) ObserveStatement(kind string, latency time.Duration, err error) {
	_ = kind
	s.ObserveRequest(latency, err)
}

// Begin marks a request as in flight and starts its timer. The returned
// function must be called when the request finishes; it is safe to call more
// than once, and only the first call updates the SLO.
func (s *SLO) Begin() Done {
	s.Inflight.Inc()
	start := time.Now()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			s.Inflight.Dec()
			s.ObserveRequest(time.Since(start), err)
		})
	}
}

// Report returns an SLO report timestamped with the current UTC time.
func (s *SLO) Report() Report {
	return s.ReportAt(time.Now())
}

// ReportAt returns an SLO report with an explicit timestamp, which is useful
// for deterministic tests and for callers that already have a clock.
func (s *SLO) ReportAt(now time.Time) Report {
	latency := s.Latency.Snapshot()
	requests := s.Requests.Value()
	errs := s.Errors.Value()
	errorRate := 0.0
	if requests > 0 {
		errorRate = float64(errs) / float64(requests)
	}
	return Report{
		Timestamp:    now.UTC(),
		Requests:     requests,
		Errors:       errs,
		ErrorRate:    errorRate,
		Inflight:     s.Inflight.Value(),
		P50MS:        latency.Quantile(0.50) * 1000,
		P95MS:        latency.Quantile(0.95) * 1000,
		P99MS:        latency.Quantile(0.99) * 1000,
		LatencyCount: latency.Count,
		LatencySumMS: latency.Sum * 1000,
	}
}

// Report is the JSON/text shape served by /debug/slo. ErrorRate is a fraction
// in [0,1], while the percentile and sum fields are milliseconds.
type Report struct {
	Timestamp    time.Time `json:"timestamp"`
	Requests     uint64    `json:"requests"`
	Errors       uint64    `json:"errors"`
	ErrorRate    float64   `json:"error_rate"`
	Inflight     int64     `json:"inflight"`
	P50MS        float64   `json:"p50_ms"`
	P95MS        float64   `json:"p95_ms"`
	P99MS        float64   `json:"p99_ms"`
	LatencyCount uint64    `json:"latency_count"`
	LatencySumMS float64   `json:"latency_sum_ms"`
}

// JSON returns the report as compact JSON.
func (r Report) JSON() ([]byte, error) {
	return json.Marshal(r)
}

// WriteJSON writes the report as compact JSON.
func (r Report) WriteJSON(w io.Writer) error {
	return json.NewEncoder(w).Encode(r)
}

// Text renders the report in a small human-readable form.
func (r Report) Text() string {
	var b strings.Builder
	b.WriteString("slo report\n")
	fmt.Fprintf(&b, "timestamp: %s\n", r.Timestamp.Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "requests: %d\n", r.Requests)
	fmt.Fprintf(&b, "errors: %d\n", r.Errors)
	fmt.Fprintf(&b, "error_rate: %.4f (%.2f%%)\n", r.ErrorRate, r.ErrorRate*100)
	fmt.Fprintf(&b, "inflight: %d\n", r.Inflight)
	fmt.Fprintf(&b, "p50: %.3fms\n", r.P50MS)
	fmt.Fprintf(&b, "p95: %.3fms\n", r.P95MS)
	fmt.Fprintf(&b, "p99: %.3fms\n", r.P99MS)
	fmt.Fprintf(&b, "latency_count: %d\n", r.LatencyCount)
	fmt.Fprintf(&b, "latency_sum: %.3fms\n", r.LatencySumMS)
	return b.String()
}

// WriteText writes Text to w.
func (r Report) WriteText(w io.Writer) error {
	_, err := io.WriteString(w, r.Text())
	return err
}

// String implements fmt.Stringer.
func (r Report) String() string { return r.Text() }
