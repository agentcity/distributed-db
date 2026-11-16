package coordinator

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"distributed-db/internal/pb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLeaderDiscoveryIsCached(t *testing.T) {
	var statusCalls, getCalls int
	clients := fakeClients{
		"n1:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				statusCalls++
				return &pb.StatusResponse{Id: "n1:7001", Role: "leader", Leader: "n1:7001"}, nil
			},
			get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
				getCalls++
				return &pb.GetResponse{Found: true, Value: []byte("v"), Leader: "n1:7001"}, nil
			},
		},
		// n2 is a follower that also knows the leader; it must not be probed
		// once n1 answered.
		"n2:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				return &pb.StatusResponse{Id: "n2:7001", Role: "follower", Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{
		Shards: map[string][]string{"s": {"n1:7001", "n2:7001"}},
	}, clients)

	for i := 0; i < 3; i++ {
		if rec := doRequest(t, c, http.MethodGet, "/v1/kv/k", nil, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("GET %d: status = %d", i, rec.Code)
		}
	}
	if statusCalls != 1 {
		t.Fatalf("Status calls = %d, want 1 (cached)", statusCalls)
	}
	if getCalls != 3 {
		t.Fatalf("Get calls = %d, want 3", getCalls)
	}
	if got := c.cachedLeaders()["s"]; got != "n1:7001" {
		t.Fatalf("cached leader = %q, want n1:7001", got)
	}
}

func TestLeaderCacheInvalidatedOnNotLeader(t *testing.T) {
	var n1StatusCalls, n1PutCalls, n2PutCalls int
	switched := false

	clients := fakeClients{
		"n1:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				n1StatusCalls++
				if switched {
					return &pb.StatusResponse{Id: "n1:7001", Role: "follower", Leader: "n2:7001"}, nil
				}
				return &pb.StatusResponse{Id: "n1:7001", Role: "leader", Leader: "n1:7001"}, nil
			},
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				n1PutCalls++
				switched = true
				return &pb.PutResponse{Success: false, Leader: "n2:7001", Error: "not leader: current leader is n2:7001"}, nil
			},
		},
		"n2:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				return &pb.StatusResponse{Id: "n2:7001", Role: "leader", Leader: "n2:7001"}, nil
			},
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				n2PutCalls++
				return &pb.PutResponse{Success: true, Index: 9, Leader: "n2:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{
		Shards: map[string][]string{"s": {"n1:7001", "n2:7001"}},
	}, clients)

	rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Leader") != "" {
		t.Fatalf("successful PUT must not carry X-Leader")
	}
	if n1PutCalls != 1 || n2PutCalls != 1 {
		t.Fatalf("put calls = (%d on n1, %d on n2), want (1, 1)", n1PutCalls, n2PutCalls)
	}
	if n1StatusCalls != 2 {
		t.Fatalf("n1 Status calls = %d, want 2 (initial + refresh)", n1StatusCalls)
	}
	if got := c.cachedLeaders()["s"]; got != "n2:7001" {
		t.Fatalf("cached leader = %q, want n2:7001", got)
	}
}

func TestNotLeaderAfterRetriesIs503WithXLeader(t *testing.T) {
	var putCalls int
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				putCalls++
				return &pb.PutResponse{Success: false, Leader: "n9:7001", Error: "not leader"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	rec := doRequest(t, c, http.MethodPut, "/v1/kv/k", strings.NewReader("v"), "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Leader"); got != "n9:7001" {
		t.Fatalf("X-Leader = %q, want n9:7001", got)
	}
	if putCalls != maxLeaderAttempts {
		t.Fatalf("Put attempts = %d, want %d", putCalls, maxLeaderAttempts)
	}
	if !strings.Contains(rec.Body.String(), "not leader") {
		t.Fatalf("body = %q, want a not-leader error", rec.Body)
	}
}

func TestRPCErrorInvalidatesCacheAndRetries(t *testing.T) {
	var getCalls, statusCalls int
	clients := fakeClients{
		"n1:7001": {
			status: func(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
				statusCalls++
				return &pb.StatusResponse{Id: "n1:7001", Role: "leader", Leader: "n1:7001"}, nil
			},
			get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
				getCalls++
				if getCalls == 1 {
					return nil, status.Error(codes.Unavailable, "transport closing")
				}
				return &pb.GetResponse{Found: true, Value: []byte("after-retry"), Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	rec := doRequest(t, c, http.MethodGet, "/v1/kv/k", nil, "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "after-retry" {
		t.Fatalf("GET: status = %d, body = %q", rec.Code, rec.Body)
	}
	if getCalls != 2 {
		t.Fatalf("Get calls = %d, want 2", getCalls)
	}
	if statusCalls != 2 {
		t.Fatalf("Status calls = %d, want 2 (re-discovery after invalidation)", statusCalls)
	}
}

func TestRPCErrorAfterRetriesIs502(t *testing.T) {
	var getCalls int
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
				getCalls++
				return nil, status.Error(codes.Unavailable, "node down")
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	rec := doRequest(t, c, http.MethodGet, "/v1/kv/k", nil, "", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body)
	}
	if getCalls != maxLeaderAttempts {
		t.Fatalf("Get attempts = %d, want %d", getCalls, maxLeaderAttempts)
	}
}

func TestGRPCNotLeaderIsNormalResponse(t *testing.T) {
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			put: func(context.Context, *pb.PutRequest) (*pb.PutResponse, error) {
				return &pb.PutResponse{Success: false, Leader: "n9:7001", Error: "not leader"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)

	resp, err := c.Put(context.Background(), &pb.PutRequest{Key: "k", Value: []byte("v")})
	if err != nil {
		t.Fatalf("Put returned gRPC error: %v", err)
	}
	if resp.GetSuccess() || resp.GetLeader() != "n9:7001" || resp.GetError() != "not leader" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
