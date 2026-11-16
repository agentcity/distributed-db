package metrics

import (
	"encoding/json"
	"net/http"
	"strings"
)

// NewHTTPHandler returns an http.Handler with the standard metrics endpoints:
//
//	GET /metrics   Prometheus text exposition of reg
//	GET /debug/slo SLO report; JSON by default, text with ?format=text or
//	               an Accept: text/plain request
//	GET /healthz   liveness probe
//
// reg and slo may be nil; /metrics then renders no samples and /debug/slo
// returns 404. The handler only serves GET and never mutates the registry.
func NewHTTPHandler(reg *Registry, slo *SLO) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		switch r.URL.Path {
		case "/metrics":
			writeMetrics(w, reg)
		case "/debug/slo":
			writeSLO(w, r, slo)
		case "/healthz":
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		default:
			writeJSONError(w, http.StatusNotFound, "not found")
		}
	})
}

func writeMetrics(w http.ResponseWriter, reg *Registry) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if reg == nil {
		_, _ = w.Write([]byte("# metrics are not configured\n"))
		return
	}
	if err := reg.WritePrometheus(w); err != nil {
		// The status is already committed by the first write; report the
		// problem in the body so scraping still fails visibly.
		_, _ = w.Write([]byte("# metrics exposition error: " + err.Error() + "\n"))
	}
}

func writeSLO(w http.ResponseWriter, r *http.Request, slo *SLO) {
	if slo == nil {
		writeJSONError(w, http.StatusNotFound, "metrics are not configured")
		return
	}
	report := slo.Report()
	if wantsText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = report.WriteText(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = report.WriteJSON(w)
}

func wantsText(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("format"), "text") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/plain")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
