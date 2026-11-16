package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/metrics"
)

func newTestServer(t *testing.T) (*metrics.Registry, *metrics.SLO, *httptest.Server) {
	t.Helper()
	reg := metrics.NewRegistry()
	slo := metrics.NewSLO(reg, metrics.SLOConfig{Prefix: "db"})
	ts := httptest.NewServer(NewServer(reg, slo, nil).Handler())
	t.Cleanup(ts.Close)
	return reg, slo, ts
}

func getBody(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func TestHealthz(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, header, body := getBody(t, ts.URL+"/healthz")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q, want JSON", header.Get("Content-Type"))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("health = %v, want status ok", got)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, slo, ts := newTestServer(t)
	slo.ObserveRequest(2*time.Millisecond, nil)
	slo.ObserveRequest(4*time.Millisecond, errors.New("boom"))

	status, header, body := getBody(t, ts.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	for _, want := range []string{
		"# HELP db_requests_total Total requests observed.",
		"# TYPE db_requests_total counter",
		"db_requests_total 2",
		"# TYPE db_errors_total counter",
		"db_errors_total 1",
		"# TYPE db_request_duration_seconds histogram",
		"db_request_duration_seconds_count 2",
		`db_request_duration_seconds_bucket{le="+Inf"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestSLOEndpointJSON(t *testing.T) {
	_, slo, ts := newTestServer(t)
	slo.ObserveRequest(2*time.Millisecond, nil)
	slo.ObserveRequest(4*time.Millisecond, errors.New("boom"))

	status, header, body := getBody(t, ts.URL+"/debug/slo")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q, want JSON", header.Get("Content-Type"))
	}
	var report metrics.Report
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, body)
	}
	if report.Requests != 2 || report.Errors != 1 || report.Inflight != 0 {
		t.Fatalf("report = %+v, want 2 requests, 1 error, 0 inflight", report)
	}
	if report.P50MS <= 0 || report.P95MS <= 0 || report.P99MS <= 0 {
		t.Fatalf("percentiles not populated: %+v", report)
	}
	if report.Timestamp.IsZero() {
		t.Fatal("timestamp is zero")
	}
}

func TestSLOEndpointText(t *testing.T) {
	_, slo, ts := newTestServer(t)
	slo.ObserveRequest(2*time.Millisecond, nil)

	status, header, body := getBody(t, ts.URL+"/debug/slo?format=text")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "text/plain") {
		t.Fatalf("content type = %q, want text/plain", header.Get("Content-Type"))
	}
	for _, want := range []string{"slo report", "requests: 1", "p50:"} {
		if !strings.Contains(body, want) {
			t.Errorf("text report missing %q:\n%s", want, body)
		}
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/slo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with Accept: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Accept content type = %q, want text/plain", ct)
	}

	status, _, body = getBody(t, ts.URL+"/debug/slo?format=yaml")
	if status != http.StatusBadRequest {
		t.Fatalf("invalid format status = %d, want 400", status)
	}
	if !strings.Contains(body, "unsupported format") {
		t.Fatalf("invalid format body = %q", body)
	}
}

func TestSnapshotEndpoint(t *testing.T) {
	_, slo, ts := newTestServer(t)
	slo.ObserveRequest(2*time.Millisecond, nil)

	status, header, body := getBody(t, ts.URL+"/debug/snapshot")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q, want JSON", header.Get("Content-Type"))
	}
	var snap metrics.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	metric, ok := findSnapshotMetric(&snap, "db_requests_total")
	if !ok {
		t.Fatalf("db_requests_total not in snapshot: %s", body)
	}
	if metric.Value != 1 {
		t.Fatalf("requests = %v, want 1", metric.Value)
	}
	if len(snap.Metrics) < 4 {
		t.Fatalf("snapshot metrics = %d, want at least the four SLO metrics", len(snap.Metrics))
	}
}

func TestRoutingAndMethods(t *testing.T) {
	_, _, ts := newTestServer(t)

	status, _, _ := getBody(t, ts.URL+"/nope")
	if status != http.StatusNotFound {
		t.Fatalf("missing route status = %d, want 404", status)
	}

	resp, err := http.Post(ts.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}

	head, err := http.Head(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("HEAD /healthz: %v", err)
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", head.StatusCode)
	}
}

func findSnapshotMetric(snap *metrics.Snapshot, name string) (metrics.MetricValue, bool) {
	for _, m := range snap.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return metrics.MetricValue{}, false
}
