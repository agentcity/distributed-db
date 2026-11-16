package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func newTestSLO(t *testing.T) *SLO {
	t.Helper()
	reg := NewRegistry()
	return NewSLO(reg, SLOConfig{
		Prefix:  "db",
		Buckets: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
	})
}

func TestSLOReport(t *testing.T) {
	slo := newTestSLO(t)
	slo.ObserveRequest(10*time.Millisecond, nil)
	slo.ObserveRequest(20*time.Millisecond, errors.New("boom"))
	slo.ObserveRequest(30*time.Millisecond, nil)

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	report := slo.ReportAt(now)

	if !report.Timestamp.Equal(now) {
		t.Fatalf("timestamp = %v, want %v", report.Timestamp, now)
	}
	if report.Requests != 3 || report.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", report.Requests, report.Errors)
	}
	if math.Abs(report.ErrorRate-1.0/3.0) > 1e-12 {
		t.Fatalf("error rate = %v, want 1/3", report.ErrorRate)
	}
	if report.Inflight != 0 {
		t.Fatalf("inflight = %d, want 0", report.Inflight)
	}
	if math.Abs(report.P50MS-15) > 1e-9 {
		t.Errorf("p50 = %v, want 15", report.P50MS)
	}
	if math.Abs(report.P95MS-28.5) > 1e-9 {
		t.Errorf("p95 = %v, want 28.5", report.P95MS)
	}
	if math.Abs(report.P99MS-29.7) > 1e-9 {
		t.Errorf("p99 = %v, want 29.7", report.P99MS)
	}
	if report.LatencyCount != 3 {
		t.Errorf("latency count = %d, want 3", report.LatencyCount)
	}
	if math.Abs(report.LatencySumMS-60) > 1e-9 {
		t.Errorf("latency sum = %v, want 60", report.LatencySumMS)
	}
}

func TestSLOJSON(t *testing.T) {
	slo := newTestSLO(t)
	slo.ObserveRequest(10*time.Millisecond, nil)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	report := slo.ReportAt(now)

	data, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Requests != 1 || decoded.Errors != 0 || decoded.Inflight != 0 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !strings.Contains(string(data), `"p50_ms"`) {
		t.Fatalf("JSON missing p50_ms: %s", data)
	}
	if decoded.Timestamp.Sub(now) != 0 {
		t.Fatalf("decoded timestamp = %v, want %v", decoded.Timestamp, now)
	}

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("WriteJSON wrote nothing")
	}
}

func TestSLOText(t *testing.T) {
	slo := newTestSLO(t)
	slo.ObserveRequest(10*time.Millisecond, nil)
	slo.ObserveRequest(20*time.Millisecond, errors.New("boom"))
	slo.ObserveRequest(30*time.Millisecond, nil)
	text := slo.ReportAt(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)).Text()

	for _, want := range []string{
		"slo report",
		"requests: 3",
		"errors: 1",
		"error_rate: 0.3333 (33.33%)",
		"inflight: 0",
		"p50: 15.000ms",
		"p95: 28.500ms",
		"p99: 29.700ms",
		"latency_count: 3",
		"latency_sum: 60.000ms",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

func TestSLOBeginAndDoubleDone(t *testing.T) {
	slo := newTestSLO(t)
	doneA := slo.Begin()
	doneB := slo.Begin()

	if got := slo.ReportAt(time.Now()).Inflight; got != 2 {
		t.Fatalf("inflight = %d, want 2", got)
	}
	doneA(nil)
	doneA(errors.New("ignored duplicate")) // must not double count
	doneB(errors.New("failed"))

	report := slo.ReportAt(time.Now())
	if report.Inflight != 0 {
		t.Fatalf("inflight after done = %d, want 0", report.Inflight)
	}
	if report.Requests != 2 || report.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 2/1", report.Requests, report.Errors)
	}
	if report.LatencyCount != 2 {
		t.Fatalf("latency count = %d, want 2", report.LatencyCount)
	}
}
