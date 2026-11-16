package coordinator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/pb"
)

func TestNewValidation(t *testing.T) {
	validShards := map[string][]string{"s": {"n1:7001"}}
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing HTTPAddr", Config{GRPCAddr: "127.0.0.1:0", Shards: validShards}},
		{"missing GRPCAddr", Config{HTTPAddr: "127.0.0.1:0", Shards: validShards}},
		{"no shards", Config{HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0"}},
		{"shard without members", Config{HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0", Shards: map[string][]string{"s": {}}}},
		{"blank node address", Config{HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0", Shards: map[string][]string{"s": {" "}}}},
		{"negative rate", Config{HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0", Shards: validShards, RateLimit: -1}},
		{"cert without key", Config{HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0", Shards: validShards, TLSCert: "cert.pem"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("New(%+v) succeeded, want error", tc.name)
			}
		})
	}
}

func TestStartStopIdempotent(t *testing.T) {
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, nil)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded, want error")
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop succeeded, want error")
	}
}

func TestStopWithCanceledContextDoesNotHang(t *testing.T) {
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- c.Stop(ctx) }()
	select {
	case <-done:
		// A canceled shutdown context may produce an error, but it must not
		// block.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return with a canceled context")
	}

	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if !stopped {
		t.Fatal("coordinator is not marked stopped")
	}

	done = make(chan error, 1)
	go func() { done <- c.Stop(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Stop did not return")
	}
}

func TestConcurrentHTTPRequests(t *testing.T) {
	var statusCalls, getCalls atomic.Int32
	clients := fakeClients{
		"n1:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				statusCalls.Add(1)
				return &pb.StatusResponse{Id: "n1:7001", Role: "leader", Leader: "n1:7001"}, nil
			},
			get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
				getCalls.Add(1)
				return &pb.GetResponse{Found: true, Value: []byte("v"), Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/kv/k", nil)
			rec := httptest.NewRecorder()
			c.httpHandler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != "v" {
				errs <- rec.Body.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatalf("concurrent GET failed: %s", msg)
	}
	if getCalls.Load() != workers {
		t.Fatalf("Get calls = %d, want %d", getCalls.Load(), workers)
	}
	if statusCalls.Load() > workers {
		t.Fatalf("Status calls = %d, expected coalescing/caching", statusCalls.Load())
	}
}
