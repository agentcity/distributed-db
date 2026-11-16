package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/pb"
)

// TestNodeEndToEndSingleNode starts a real one-node cluster and exercises the
// HTTP and gRPC adapters against the real Raft core and KV store.
func TestNodeEndToEndSingleNode(t *testing.T) {
	n, err := New(Config{
		ID:       "test-node",
		Shard:    "shard-1",
		RaftAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
		DataDir:  t.TempDir(),
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := n.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := n.Stop(context.Background()); err != nil {
			t.Logf("Stop: %v", err)
		}
	})

	baseURL := "http://" + n.HTTPAddr()
	client := &http.Client{Timeout: 5 * time.Second}

	// Wait for the single node to elect itself.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status httpStatusResponse
		code, err := doJSON(client, http.MethodGet, baseURL+"/v1/status", nil, &status)
		if err == nil && code == http.StatusOK && status.Role == "leader" {
			if status.Shard != "shard-1" {
				t.Fatalf("status shard = %q, want shard-1", status.Shard)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node did not become leader: code=%d err=%v", code, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// PUT raw body.
	code, err := doJSON(client, http.MethodPut, baseURL+"/v1/kv/foo", []byte("bar"), nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("PUT: code=%d err=%v", code, err)
	}

	// GET returns the raw value.
	resp, err := client.Get(baseURL + "/v1/kv/foo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "bar" {
		t.Fatalf("GET: code=%d body=%q", resp.StatusCode, body)
	}

	// DELETE then GET again.
	code, err = doJSON(client, http.MethodDelete, baseURL+"/v1/kv/foo", nil, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("DELETE: code=%d err=%v", code, err)
	}
	resp, err = client.Get(baseURL + "/v1/kv/foo")
	if err != nil {
		t.Fatalf("GET after DELETE: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE: code=%d, want 404", resp.StatusCode)
	}

	// /metrics is exposed from the private registry.
	resp, err = client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, name := range []string{"db_put_total", "db_get_total", "db_delete_total", "db_raft_is_leader"} {
		if !strings.Contains(string(metricsBody), name) {
			t.Errorf("/metrics does not expose %s", name)
		}
	}

	// gRPC NodeService.
	conn, err := grpc.NewClient(n.RaftAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()
	svc := pb.NewNodeServiceClient(conn)

	grpcCtx, grpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer grpcCancel()

	st, err := svc.Status(grpcCtx, &pb.StatusRequest{})
	if err != nil {
		t.Fatalf("gRPC Status: %v", err)
	}
	if st.GetRole() != "leader" || st.GetShard() != "shard-1" || st.GetId() != "test-node" {
		t.Fatalf("gRPC Status = %+v", st)
	}

	put, err := svc.Put(grpcCtx, &pb.PutRequest{Key: "grpc-key", Value: []byte("grpc-value")})
	if err != nil {
		t.Fatalf("gRPC Put: %v", err)
	}
	if !put.GetSuccess() || put.GetIndex() == 0 {
		t.Fatalf("gRPC Put = %+v", put)
	}

	get, err := svc.Get(grpcCtx, &pb.GetRequest{Key: "grpc-key"})
	if err != nil {
		t.Fatalf("gRPC Get: %v", err)
	}
	if !get.GetFound() || !bytes.Equal(get.GetValue(), []byte("grpc-value")) {
		t.Fatalf("gRPC Get = %+v", get)
	}

	del, err := svc.Delete(grpcCtx, &pb.DeleteRequest{Key: "grpc-key"})
	if err != nil {
		t.Fatalf("gRPC Delete: %v", err)
	}
	if !del.GetSuccess() {
		t.Fatalf("gRPC Delete = %+v", del)
	}
}

func doJSON(client *http.Client, method, url string, body []byte, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, payload)
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
