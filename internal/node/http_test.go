package node

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuthMiddleware(t *testing.T) {
	n := &Node{
		cfg: Config{AuthUser: "admin", AuthPass: "s3cret"},
		log: testLogger(),
	}
	handler := n.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name   string
		path   string
		user   string
		pass   string
		set    bool
		status int
	}{
		{name: "no credentials", path: "/v1/kv/a", status: http.StatusUnauthorized},
		{name: "wrong password", path: "/v1/kv/a", user: "admin", pass: "nope", set: true, status: http.StatusUnauthorized},
		{name: "wrong user", path: "/v1/kv/a", user: "root", pass: "s3cret", set: true, status: http.StatusUnauthorized},
		{name: "valid", path: "/v1/kv/a", user: "admin", pass: "s3cret", set: true, status: http.StatusNoContent},
		{name: "health is public", path: "/health", status: http.StatusNoContent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.set {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if rec.Code == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
					t.Fatalf("WWW-Authenticate = %q, want Basic challenge", got)
				}
				if strings.Contains(rec.Body.String(), "s3cret") || strings.Contains(rec.Body.String(), "admin") {
					t.Fatalf("response leaks credentials: %q", rec.Body.String())
				}
			}
		})
	}
}

func TestAuthMiddlewareWithHash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	n := &Node{
		cfg: Config{AuthUser: "admin", AuthHash: string(hash)},
		log: testLogger(),
	}
	handler := n.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/kv/a", nil)
	req.SetBasicAuth("admin", "hunter2")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid hash credentials: status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/kv/a", nil)
	req.SetBasicAuth("admin", "wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid password: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	n := &Node{
		cfg:     Config{RateLimit: 1, RateBurst: 1},
		log:     testLogger(),
		limiter: rate.NewLimiter(rate.Limit(1), 1),
	}
	handler := n.rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 response is missing Retry-After")
	}
}

func TestPublicHTTPEndpoints(t *testing.T) {
	n := &Node{
		cfg: Config{ID: "node-1", Shard: "shard-1", RaftAddr: ":0", DataDir: t.TempDir()},
		log: testLogger(),
	}
	n.metrics = newMetrics(n)
	handler := n.buildHTTPHandler()

	// /metrics exposes every registered metric family with a private registry,
	// even before the first request has been observed.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"db_put_total",
		"db_get_total",
		"db_delete_total",
		"db_errors_total",
		"db_http_request_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics does not expose %s", name)
		}
	}

	// /health is always 200.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}

	// Unsupported methods are rejected by the ServeMux patterns.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/kv/a", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/kv/a status = %d, want 405", rec.Code)
	}
}
