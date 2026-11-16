package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/pb"
	"distributed-db/internal/raft"
)

type testClusterNode struct {
	node    *Node
	raftURL string
	httpURL string
	dir     string
	peers   []string
}

// freePorts reserves n distinct TCP ports. All listeners are opened before any
// is closed so the kernel cannot hand out the same port twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			t.Fatalf("reserve port: %v", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}

func waitForLeaderNode(t *testing.T, members []*testClusterNode) *testClusterNode {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		for _, m := range members {
			resp, err := client.Get(m.httpURL + "/v1/status")
			if err != nil {
				continue
			}
			var st httpStatusResponse
			err = json.NewDecoder(resp.Body).Decode(&st)
			_ = resp.Body.Close()
			if err == nil && st.Role == string(raft.Leader) && st.Leader != "" {
				return m
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return nil
}

func TestThreeNodeClusterFailover(t *testing.T) {
	// Reserve all addresses first: peers must be known before any node starts.
	ports := freePorts(t, 6)
	addrs := make([]string, 3)
	httpAddrs := make([]string, 3)
	for i := 0; i < 3; i++ {
		addrs[i] = fmt.Sprintf("127.0.0.1:%d", ports[2*i])
		httpAddrs[i] = fmt.Sprintf("127.0.0.1:%d", ports[2*i+1])
	}

	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	members := make([]*testClusterNode, 3)
	for i := 0; i < 3; i++ {
		peers := make([]string, 0, 2)
		for j := 0; j < 3; j++ {
			if i != j {
				peers = append(peers, addrs[j])
			}
		}
		n, err := New(Config{
			ID:       addrs[i],
			Shard:    "shard-1",
			RaftAddr: addrs[i],
			HTTPAddr: httpAddrs[i],
			Peers:    peers,
			DataDir:  dirs[i],
			Logger:   testLogger(),
		})
		if err != nil {
			t.Fatalf("New node %d: %v", i, err)
		}
		if err := n.Start(context.Background()); err != nil {
			t.Fatalf("Start node %d: %v", i, err)
		}
		members[i] = &testClusterNode{
			node:    n,
			raftURL: addrs[i],
			httpURL: "http://" + httpAddrs[i],
			dir:     dirs[i],
			peers:   peers,
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, m := range members {
			_ = m.node.Stop(ctx)
		}
	})

	leader := waitForLeaderNode(t, members)

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPut, leader.httpURL+"/v1/kv/alpha", bytes.NewReader([]byte("one")))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT to leader: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT to leader: code=%d body=%s", resp.StatusCode, body)
	}

	// A follower must answer 503 with a leader hint and the JSON contract.
	var follower *testClusterNode
	for _, m := range members {
		if m != leader {
			follower = m
			break
		}
	}
	resp, err = client.Get(follower.httpURL + "/v1/kv/alpha")
	if err != nil {
		t.Fatalf("GET from follower: %v", err)
	}
	followerBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET from follower: code=%d body=%s, want 503", resp.StatusCode, followerBody)
	}
	if got := resp.Header.Get("X-Leader"); got == "" {
		t.Fatalf("GET from follower: missing X-Leader header")
	}
	var nl notLeaderResponse
	if err := json.Unmarshal(followerBody, &nl); err != nil || nl.Error != "not leader" || nl.Leader == "" {
		t.Fatalf("GET from follower body=%s err=%v", followerBody, err)
	}

	// The gRPC adapter must also answer not-leader as a normal response with a
	// leader hint, not as a gRPC status error.
	conn, err := grpc.NewClient(follower.raftURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc client for follower: %v", err)
	}
	defer func() { _ = conn.Close() }()
	svc := pb.NewNodeServiceClient(conn)
	grpcCtx, grpcCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer grpcCancel()

	getResp, err := svc.Get(grpcCtx, &pb.GetRequest{Key: "alpha"})
	if err != nil {
		t.Fatalf("gRPC Get on follower returned a transport error: %v", err)
	}
	if getResp.GetError() != "not leader" || getResp.GetLeader() == "" || getResp.GetFound() {
		t.Fatalf("gRPC Get on follower = %+v", getResp)
	}
	putResp, err := svc.Put(grpcCtx, &pb.PutRequest{Key: "beta", Value: []byte("two")})
	if err != nil {
		t.Fatalf("gRPC Put on follower returned a transport error: %v", err)
	}
	if putResp.GetSuccess() || putResp.GetError() != "not leader" || putResp.GetLeader() == "" {
		t.Fatalf("gRPC Put on follower = %+v", putResp)
	}

	// Kill the leader; a new leader must be elected and keep the value.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := leader.node.Stop(ctx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	cancel()

	var remaining []*testClusterNode
	for _, m := range members {
		if m != leader {
			remaining = append(remaining, m)
		}
	}
	newLeader := waitForLeaderNode(t, remaining)

	// The new leader may still be committing its no-op entry when it first
	// reports the leader role, so poll briefly for the committed value.
	valueDeadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = client.Get(newLeader.httpURL + "/v1/kv/alpha")
		if err != nil {
			t.Fatalf("GET from new leader: %v", err)
		}
		value, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(value), "one") {
			break
		}
		if time.Now().After(valueDeadline) {
			t.Fatalf("GET from new leader: code=%d body=%q", resp.StatusCode, value)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Restart the old leader on the same address/data dir and let it catch up.
	restarted, err := New(Config{
		ID:       leader.raftURL,
		Shard:    "shard-1",
		RaftAddr: leader.raftURL,
		HTTPAddr: strings.TrimPrefix(leader.httpURL, "http://"),
		Peers:    leader.peers,
		DataDir:  leader.dir,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = restarted.Stop(ctx)
	})

	deadline := time.Now().Add(8 * time.Second)
	for {
		st := restarted.Status()
		if st.LastApplied >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted node did not catch up: %+v", st)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
