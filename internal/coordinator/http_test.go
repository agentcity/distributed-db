package coordinator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"distributed-db/internal/pb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthMiddleware(t *testing.T) {
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				return &pb.PutResponse{Success: true, Index: 7, Leader: "n1:7001"}, nil
			},
			get: foundGet("v1"),
		},
	}
	c := newTestCoordinator(t, Config{
		Shards:   map[string][]string{"shard-0": {"n1:7001"}},
		AuthUser: "admin",
		AuthPass: "secret",
	}, clients)

	if rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: status = %d, want 401 (body %s)", rec.Code, rec.Body)
	} else if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("401 response is missing WWW-Authenticate")
	}

	if rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "admin", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d, want 401", rec.Code)
	}
	if rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "wrong", "secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user: status = %d, want 401", rec.Code)
	}
	if rec := doRequest(t, c, http.MethodGet, "/v1/kv/k", nil, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET: status = %d, want 401", rec.Code)
	}

	rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("value"), "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with credentials: status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var put map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	if put["success"] != true || put["index"] != float64(7) {
		t.Fatalf("unexpected PUT body: %v", put)
	}

	rec = doRequest(t, c, http.MethodGet, "/v1/kv/k", nil, "admin", "secret")
	if rec.Code != http.StatusOK || rec.Body.String() != "v1" {
		t.Fatalf("GET with credentials: status = %d, body = %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("GET Content-Type = %q", ct)
	}

	// Auth only guards /v1/kv/*.
	if rec := doRequest(t, c, http.MethodGet, "/v1/status", nil, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated status: status = %d, want 200", rec.Code)
	}
}

func TestAuthDisabled(t *testing.T) {
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				return &pb.PutResponse{Success: true, Index: 1, Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)
	if rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "", ""); rec.Code != http.StatusOK {
		t.Fatalf("PUT without auth configured: status = %d, want 200", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	c := newTestCoordinator(t, Config{
		Shards:    map[string][]string{"s": {"n1:7001"}},
		RateLimit: 1,
		RateBurst: 1,
	}, nil)

	if rec := doRequest(t, c, http.MethodGet, "/health", nil, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}
	rec := doRequest(t, c, http.MethodGet, "/health", nil, "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 response is missing Retry-After")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		client     *fakeNodeClient
		wantStatus int
		wantBody   string
	}{
		{
			name:   "put ok",
			method: http.MethodPut,
			path:   "/v1/kv/k",
			body:   "v",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
					return &pb.PutResponse{Success: true, Index: 3, Leader: "n1:7001"}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `"success":true`,
		},
		{
			name:       "get found",
			method:     http.MethodGet,
			path:       "/v1/kv/k",
			client:     &fakeNodeClient{status: leaderStatus("n1:7001"), get: foundGet("hello")},
			wantStatus: http.StatusOK,
			wantBody:   "hello",
		},
		{
			name:   "get not found",
			method: http.MethodGet,
			path:   "/v1/kv/k",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
					return &pb.GetResponse{Found: false, Leader: "n1:7001"}, nil
				},
			},
			wantStatus: http.StatusNotFound,
			wantBody:   "not found",
		},
		{
			name:   "get upstream error",
			method: http.MethodGet,
			path:   "/v1/kv/k",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
					return &pb.GetResponse{Leader: "n1:7001", Error: "read barrier failed"}, nil
				},
			},
			wantStatus: http.StatusBadGateway,
			wantBody:   "read barrier failed",
		},
		{
			name:   "get rpc error",
			method: http.MethodGet,
			path:   "/v1/kv/k",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
					return nil, status.Error(codes.Unavailable, "connection refused")
				},
			},
			wantStatus: http.StatusBadGateway,
			wantBody:   "connection refused",
		},
		{
			name:   "delete ok",
			method: http.MethodDelete,
			path:   "/v1/kv/k",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				del: func(context.Context, *pb.DeleteRequest) (*pb.DeleteResponse, error) {
					return &pb.DeleteResponse{Success: true, Index: 4, Leader: "n1:7001"}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `"success":true`,
		},
		{
			name:   "delete not found",
			method: http.MethodDelete,
			path:   "/v1/kv/k",
			client: &fakeNodeClient{
				status: leaderStatus("n1:7001"),
				del: func(context.Context, *pb.DeleteRequest) (*pb.DeleteResponse, error) {
					return &pb.DeleteResponse{Success: false, Leader: "n1:7001", Error: "key not found"}, nil
				},
			},
			wantStatus: http.StatusNotFound,
			wantBody:   "not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, fakeClients{"n1:7001": tc.client})
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			rec := doRequest(t, c, tc.method, tc.path, body, "", "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body, tc.wantBody)
			}
		})
	}
}

func TestHealthAndMetrics(t *testing.T) {
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				return &pb.PutResponse{Success: true, Index: 1, Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	if rec := doRequest(t, c, http.MethodGet, "/health", nil, "", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("health: status = %d, body = %q", rec.Code, rec.Body)
	}

	if rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "", ""); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status = %d", rec.Code)
	}
	if _, err := c.ReportLoad(context.Background(), &pb.LoadReportRequest{
		NodeId: "n1:7001", Shard: "s", Load: 0.5, Ops: 12,
	}); err != nil {
		t.Fatalf("ReportLoad: %v", err)
	}

	rec := doRequest(t, c, http.MethodGet, "/metrics", nil, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"db_coordinator_requests_total",
		"db_coordinator_request_duration_seconds",
		"db_coordinator_load",
		`op="put"`,
		`result="ok"`,
		`shard="s"`,
		`node="n1:7001"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body is missing %q:\n%s", want, body)
		}
	}
}
