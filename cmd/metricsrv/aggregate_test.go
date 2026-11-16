package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"distributed-db/internal/metrics"
)

func snapshotJSON(t *testing.T, name string, value uint64) []byte {
	t.Helper()
	reg := metrics.NewRegistry()
	reg.NewCounter(name, "Test counter.", nil).Add(value)
	var buf bytes.Buffer
	if err := reg.Snapshot().WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	return buf.Bytes()
}

func TestAggregatorLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.json")
	if err := os.WriteFile(path, snapshotJSON(t, "peer_requests_total", 7), 0o600); err != nil {
		t.Fatal(err)
	}

	agg := NewAggregator()
	if got := len(agg.Snapshot().Metrics); got != 0 {
		t.Fatalf("empty aggregator metrics = %d, want 0", got)
	}
	if err := agg.LoadFile("", path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	merged := agg.Snapshot()
	metric, ok := findSnapshotMetric(merged, "peer_requests_total")
	if !ok {
		t.Fatalf("peer counter missing from %+v", merged.Metrics)
	}
	if got := metric.Labels["source"]; got != "peer.json" {
		t.Fatalf("source = %q, want basename peer.json", got)
	}
	if metric.Value != 7 {
		t.Fatalf("value = %v, want 7", metric.Value)
	}
}

func TestAggregatorLoadFileErrors(t *testing.T) {
	agg := NewAggregator()
	if err := agg.LoadFile("x", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadFile(missing) error = nil")
	}

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agg.LoadFile("x", path); err == nil {
		t.Fatal("LoadFile(bad JSON) error = nil")
	}
}

func TestAggregatorFetch(t *testing.T) {
	data := snapshotJSON(t, "peer_requests_total", 7)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/snapshot" {
			http.NotFound(w, r)
			return
		}
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") {
			t.Errorf("Accept = %q, want JSON", accept)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	agg := NewAggregator()
	if err := agg.Fetch(context.Background(), upstream.Client(), "peer-1", upstream.URL+"/debug/snapshot"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	metric, ok := findSnapshotMetric(agg.Snapshot(), "peer_requests_total")
	if !ok {
		t.Fatal("fetched counter missing")
	}
	if got := metric.Labels["source"]; got != "peer-1" {
		t.Fatalf("source = %q, want peer-1", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if err := agg.Fetch(context.Background(), bad.Client(), "bad", bad.URL); err == nil {
		t.Fatal("Fetch(503) error = nil")
	}

	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer invalid.Close()
	if err := agg.Fetch(context.Background(), invalid.Client(), "invalid", invalid.URL); err == nil {
		t.Fatal("Fetch(invalid JSON) error = nil")
	}
}

func TestServerIncludesAggregatedMetrics(t *testing.T) {
	local := metrics.NewRegistry()
	slo := metrics.NewSLO(local, metrics.SLOConfig{Prefix: "db"})
	agg := NewAggregator()

	peerSnapData := snapshotJSON(t, "peer_requests_total", 7)
	var peerSnap metrics.Snapshot
	if err := json.Unmarshal(peerSnapData, &peerSnap); err != nil {
		t.Fatalf("unmarshal peer snapshot: %v", err)
	}
	agg.SetSnapshot("peer", &peerSnap)

	ts := httptest.NewServer(NewServer(local, slo, agg).Handler())
	defer ts.Close()

	status, _, body := getBody(t, ts.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, `peer_requests_total{source="peer"} 7`) {
		t.Fatalf("aggregated metric missing:\n%s", body)
	}
	if !strings.Contains(body, "db_requests_total 0") {
		t.Fatalf("local metric missing:\n%s", body)
	}

	status, _, body = getBody(t, ts.URL+"/debug/snapshot")
	if status != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", status)
	}
	var snap metrics.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	metric, ok := findSnapshotMetric(&snap, "peer_requests_total")
	if !ok {
		t.Fatalf("peer metric missing from snapshot:\n%s", body)
	}
	if metric.Labels["source"] != "peer" {
		t.Fatalf("snapshot source = %q, want peer", metric.Labels["source"])
	}
}
