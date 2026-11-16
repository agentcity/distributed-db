// Package sqlhttp exposes the P0 SQL engine over HTTP.
package sqlhttp

import (
	"encoding/json"
	"net/http"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
)

// Config configures a Handler. Engine is required. Metrics, when non-nil, is
// exposed as Prometheus text at /metrics; SLO, when non-nil, backs /debug/slo.
type Config struct {
	Engine  *sqlengine.Engine
	Metrics *metrics.Registry
	SLO     *metrics.SLO
}

// Handler serves POST /v1/sql, GET /health, GET /healthz, GET /metrics, and
// GET /debug/slo.
type Handler struct {
	engine         *sqlengine.Engine
	metricsHandler http.Handler
}

// New returns an http.Handler backed by engine without metrics endpoints
// configured. /metrics still responds, but with no samples.
func New(engine *sqlengine.Engine) *Handler {
	return NewWithConfig(Config{Engine: engine})
}

// NewWithConfig returns an http.Handler backed by cfg.Engine that also
// exposes the configured metrics registry and SLO.
func NewWithConfig(cfg Config) *Handler {
	return &Handler{
		engine:         cfg.Engine,
		metricsHandler: metrics.NewHTTPHandler(cfg.Metrics, cfg.SLO),
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/metrics",
		r.Method == http.MethodGet && r.URL.Path == "/debug/slo",
		r.Method == http.MethodGet && r.URL.Path == "/healthz":
		h.metricsHandler.ServeHTTP(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sql":
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if req.SQL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sql is required"})
			return
		}
		res, err := h.engine.Execute(r.Context(), req.SQL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
