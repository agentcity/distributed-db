package metrics

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPHandlerEndpoints(t *testing.T) {
	reg := NewRegistry()
	slo := NewSLO(reg, SLOConfig{Prefix: "sql"})
	slo.ObserveRequest(5*time.Millisecond, nil)
	slo.ObserveRequest(7*time.Millisecond, errors.New("boom"))
	h := NewHTTPHandler(reg, slo)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"# TYPE sql_requests_total counter",
		"sql_requests_total 2",
		"sql_errors_total 1",
		"sql_request_duration_seconds_count 2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q:\n%s", want, body)
		}
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/slo", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/debug/slo status = %d, want 200", rr.Code)
	}
	var report Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode /debug/slo: %v", err)
	}
	if report.Requests != 2 || report.Errors != 1 {
		t.Fatalf("report requests/errors = %d/%d, want 2/1", report.Requests, report.Errors)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/slo?format=text", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "requests: 2") {
		t.Fatalf("/debug/slo?format=text = %d %q", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/debug/slo", nil)
	req.Header.Set("Accept", "text/plain")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "error_rate:") {
		t.Fatalf("/debug/slo Accept text/plain = %q", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok"`) {
		t.Fatalf("/healthz = %d %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /metrics = %d Allow=%q", rr.Code, rr.Header().Get("Allow"))
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", rr.Code)
	}
}

func TestHTTPHandlerWithoutMetrics(t *testing.T) {
	h := NewHTTPHandler(nil, nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "not configured") {
		t.Fatalf("unconfigured /metrics = %d %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/slo", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unconfigured /debug/slo = %d, want 404", rr.Code)
	}
}
