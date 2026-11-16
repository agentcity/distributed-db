package coordinator

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"distributed-db/internal/pb"
)

// newTestCoordinator builds a coordinator without starting listeners. Node
// clients are replaced by fakes so no real node is needed.
func newTestCoordinator(t *testing.T, cfg Config, clients fakeClients) *Coordinator {
	t.Helper()
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:0"
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = "127.0.0.1:0"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if clients != nil {
		c.newNodeClient = clients.newNodeClientFactory()
	}
	t.Cleanup(func() {
		_ = c.Stop(context.Background())
	})
	return c
}

// doRequest runs one request against the coordinator's HTTP handler.
func doRequest(t *testing.T, c *Coordinator, method, target string, body io.Reader, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	c.httpHandler.ServeHTTP(rec, req)
	return rec
}

// leaderStatus returns a fake Status implementation for a leader node.
func leaderStatus(addr string) func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
		return &pb.StatusResponse{Id: addr, Role: "leader", Leader: addr}, nil
	}
}

func foundGet(value string) func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
	return func(_ context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
		return &pb.GetResponse{Found: true, Value: []byte(value), Leader: "self"}, nil
	}
}
