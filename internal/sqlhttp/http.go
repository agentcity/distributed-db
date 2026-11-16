// Package sqlhttp exposes the P0 SQL engine over HTTP.
package sqlhttp

import (
	"encoding/json"
	"net/http"

	"distributed-db/internal/sqlengine"
)

// Handler serves POST /v1/sql and GET /health.
type Handler struct {
	engine *sqlengine.Engine
}

// New returns an http.Handler backed by engine.
func New(engine *sqlengine.Engine) *Handler { return &Handler{engine: engine} }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
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
