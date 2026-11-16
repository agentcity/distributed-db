package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/coordinator"
	"distributed-db/internal/node"
	pb "distributed-db/internal/pb"

	"github.com/cespare/xxhash/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// freeAddr reserves a loopback port and immediately releases it. The small
// race is acceptable for tests; if a port is taken the node fails to start and
// the test fails loudly.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

type testNode struct {
	n      *node.Node
	addr   string // raft address / id
	client pb.NodeServiceClient
}

func startCluster(t *testing.T, shards map[string][]string) map[string][]*testNode {
	t.Helper()
	out := make(map[string][]*testNode, len(shards))
	ctx := context.Background()
	for shard, addrs := range shards {
		for i, addr := range addrs {
			peers := make([]string, 0, len(addrs)-1)
			for j, p := range addrs {
				if i != j {
					peers = append(peers, p)
				}
			}
			n, err := node.New(node.Config{
				ID:       addr,
				Shard:    shard,
				RaftAddr: addr,
				HTTPAddr: freeAddr(t),
				Peers:    peers,
				DataDir:  t.TempDir(),
				Logger:   testLogger(t),
			})
			if err != nil {
				t.Fatalf("new node %s: %v", addr, err)
			}
			if err := n.Start(ctx); err != nil {
				t.Fatalf("start node %s: %v", addr, err)
			}
			tn := &testNode{n: n, addr: addr}
			tn.client = dial(t, addr)
			t.Cleanup(func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = n.Stop(stopCtx)
			})
			out[shard] = append(out[shard], tn)
		}
	}
	return out
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func dial(t *testing.T, addr string) pb.NodeServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewNodeServiceClient(conn)
}

func status(t *testing.T, c pb.NodeServiceClient) *pb.StatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	resp, err := c.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return nil
	}
	return resp
}

func waitLeader(t *testing.T, nodes []*testNode) *testNode {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, tn := range nodes {
			st := status(t, tn.client)
			if st != nil && strings.EqualFold(st.Role, "leader") {
				return tn
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leader elected in %v", nodes)
	return nil
}

func put(t *testing.T, c pb.NodeServiceClient, key, value string) *pb.PutResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := c.Put(ctx, &pb.PutRequest{Key: key, Value: []byte(value)})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return resp
}

func get(t *testing.T, c pb.NodeServiceClient, key string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := c.Get(ctx, &pb.GetRequest{Key: key})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	if !resp.Found {
		return "", false
	}
	return string(resp.Value), true
}

// getEventually retries a read while the node reports "not leader"; after a
// failover the newly elected leader can briefly step down during a split vote.
func getEventually(t *testing.T, c pb.NodeServiceClient, key string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, err := c.Get(ctx, &pb.GetRequest{Key: key})
		cancel()
		if err == nil && resp.Found {
			return string(resp.Value), true
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Logf("get %s: %v", key, err)
			} else {
				t.Logf("get %s: found=%v error=%q leader=%q", key, resp.Found, resp.Error, resp.Leader)
			}
			return "", false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestRaftClusterReplicationAndFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	addrs := []string{freeAddr(t), freeAddr(t), freeAddr(t)}
	cluster := startCluster(t, map[string][]string{"shard-1": addrs})
	nodes := cluster["shard-1"]

	leader := waitLeader(t, nodes)
	lc := leader.client

	resp := put(t, lc, "hello", "world")
	if !resp.Success {
		t.Fatalf("put failed: %+v", resp)
	}
	if got, ok := getEventually(t, lc, "hello", 3*time.Second); !ok || got != "world" {
		t.Fatalf("get after put = %q, %v", got, ok)
	}

	// Kill the leader and wait for a new one.
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := leader.n.Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}

	var survivors []*testNode
	for _, n := range nodes {
		if n != leader {
			survivors = append(survivors, n)
		}
	}
	newLeader := waitLeader(t, survivors)
	nc := newLeader.client
	if got, ok := getEventually(t, nc, "hello", 5*time.Second); !ok || got != "world" {
		t.Fatalf("committed value lost after failover: %q, %v (leader=%s status=%+v)", got, ok, newLeader.addr, status(t, nc))
	}

	// The new leader must accept writes.
	if resp := put(t, nc, "after", "failover"); !resp.Success {
		t.Fatalf("put on new leader failed: %+v", resp)
	}
}

func TestCoordinatorRoutesTwoShards(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	shardAddrs := map[string][]string{
		"shard-1": {freeAddr(t), freeAddr(t), freeAddr(t)},
		"shard-2": {freeAddr(t), freeAddr(t), freeAddr(t)},
	}
	cluster := startCluster(t, shardAddrs)
	for shard, nodes := range cluster {
		waitLeader(t, nodes)
		t.Logf("%s leader: %s", shard, mustLeader(t, nodes).addr)
	}

	coordHTTP := freeAddr(t)
	coord, err := coordinator.New(coordinator.Config{
		HTTPAddr: coordHTTP,
		GRPCAddr: freeAddr(t),
		Shards:   shardAddrs,
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	ctx := context.Background()
	if err := coord.Start(ctx); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = coord.Stop(stopCtx)
	})

	base := "http://" + coordHTTP
	waitHTTP(t, base+"/health")

	// Pick one key per shard deterministically.
	keys := pickKeysPerShard(t, shardAddrs)
	for shard, key := range keys {
		httpPut(t, base+"/v1/kv/"+key, "value-"+shard, http.StatusOK, http.StatusCreated, http.StatusNoContent)
		body := httpGet(t, base+"/v1/kv/"+key)
		if body != "value-"+shard {
			t.Fatalf("%s: get = %q", key, body)
		}
	}

	// Kill the leader of shard-1 and verify a committed write survives and a
	// new write is routed to the new leader.
	leader := mustLeader(t, cluster["shard-1"])
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := leader.n.Stop(stopCtx); err != nil {
		t.Fatalf("stop shard-1 leader: %v", err)
	}
	var survivors []*testNode
	for _, n := range cluster["shard-1"] {
		if n != leader {
			survivors = append(survivors, n)
		}
	}
	waitLeader(t, survivors)

	key := keys["shard-1"]
	if body := httpGet(t, base+"/v1/kv/"+key); body != "value-shard-1" {
		t.Fatalf("committed value lost after failover: %q", body)
	}
	httpPut(t, base+"/v1/kv/"+key, "after-failover", http.StatusOK, http.StatusCreated, http.StatusNoContent)
	if body := httpGet(t, base+"/v1/kv/"+key); body != "after-failover" {
		t.Fatalf("write after failover: %q", body)
	}
	httpDelete(t, base+"/v1/kv/"+key)
}

func mustLeader(t *testing.T, nodes []*testNode) *testNode {
	t.Helper()
	for _, tn := range nodes {
		st := status(t, tn.client)
		if st != nil && strings.EqualFold(st.Role, "leader") {
			return tn
		}
	}
	t.Fatalf("no leader among %d nodes", len(nodes))
	return nil
}

func pickKeysPerShard(t *testing.T, shards map[string][]string) map[string]string {
	t.Helper()
	names := make([]string, 0, len(shards))
	for name := range shards {
		names = append(names, name)
	}
	sort.Strings(names)
	want := make(map[string]string, len(names))
	for i, name := range names {
		for k := 0; k < 1000; k++ {
			key := fmt.Sprintf("key-%d-%d", i, k)
			if int(xxhash.Sum64String(key)%uint64(len(names))) == i {
				want[name] = key
				break
			}
		}
		if want[name] == "" {
			t.Fatalf("cannot find a key for shard %s", name)
		}
	}
	return want
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("endpoint %s did not become healthy", url)
}

func httpPut(t *testing.T, url, value string, want ...int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte(value)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !statusIn(resp.StatusCode, want) {
		t.Fatalf("PUT %s: status %d body %s", url, resp.StatusCode, body)
	}
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", url, resp.StatusCode, body)
	}
	return string(body)
}

func httpDelete(t *testing.T, url string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	if !statusIn(resp.StatusCode, []int{http.StatusOK, http.StatusNoContent}) {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE %s: status %d body %s", url, resp.StatusCode, body)
	}
}

func statusIn(code int, allowed []int) bool {
	for _, c := range allowed {
		if code == c {
			return true
		}
	}
	return false
}
