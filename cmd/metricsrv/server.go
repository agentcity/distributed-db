package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"distributed-db/internal/metrics"
)

// Server serves the metrics, SLO, health, and snapshot endpoints.
type Server struct {
	registry   *metrics.Registry
	slo        *metrics.SLO
	aggregator *Aggregator
}

// NewServer creates a Server. A nil registry or SLO gets a zero-value local
// one; a nil aggregator disables federation.
func NewServer(reg *metrics.Registry, slo *metrics.SLO, agg *Aggregator) *Server {
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	if slo == nil {
		slo = metrics.NewSLO(reg, metrics.SLOConfig{})
	}
	return &Server{registry: reg, slo: slo, aggregator: agg}
}

// Handler returns the HTTP routes:
//
//	GET /metrics          Prometheus text exposition (local + aggregated)
//	GET /debug/slo        SLO report as JSON, or text with ?format=text
//	GET /healthz          liveness probe
//	GET /debug/snapshot   JSON snapshot used for optional HTTP federation
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/debug/slo", s.handleSLO)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/debug/snapshot", s.handleSnapshot)
	return mux
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	snap := s.registry.Snapshot()
	if s.aggregator != nil {
		snap.Merge(s.aggregator.Snapshot())
	}

	var buf bytes.Buffer
	if err := snap.WritePrometheus(&buf); err != nil {
		http.Error(w, "metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleSLO(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" && strings.Contains(r.Header.Get("Accept"), "text/plain") {
		format = "text"
	}

	report := s.slo.Report()
	var buf bytes.Buffer
	switch format {
	case "", "json":
		if err := report.WriteJSON(&buf); err != nil {
			http.Error(w, "slo: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
	case "text":
		if err := report.WriteText(&buf); err != nil {
			http.Error(w, "slo: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		http.Error(w, "unsupported format: use json or text", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	snap := s.registry.Snapshot()
	if s.aggregator != nil {
		snap.Merge(s.aggregator.Snapshot())
	}

	var buf bytes.Buffer
	if err := snap.WriteJSON(&buf); err != nil {
		http.Error(w, "snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
