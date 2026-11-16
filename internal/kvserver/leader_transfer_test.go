package kvserver

import (
	"context"
	"strings"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startLeaderTestCluster starts count single-range KV servers that are peers of
// each other and returns their gRPC clients.
func startLeaderTestCluster(t *testing.T, count int) ([]string, []pbv2.KVServiceClient) {
	t.Helper()
	addrs := make([]string, count)
	for i := range addrs {
		addrs[i] = testFreeAddr(t)
	}
	clients := make([]pbv2.KVServiceClient, 0, count)
	for i, addr := range addrs {
		peers := make([]string, 0, count-1)
		for j, peer := range addrs {
			if i != j {
				peers = append(peers, peer)
			}
		}
		srv, err := New(Config{
			ID:       addr,
			RaftAddr: addr,
			DataDir:  t.TempDir(),
			Ranges: []RangeConfig{{
				ID:    1,
				Start: nil,
				End:   nil,
				Peers: peers,
				Epoch: 1,
			}},
			ElectionTimeoutMin: 100 * time.Millisecond,
			ElectionTimeoutMax: 200 * time.Millisecond,
			HeartbeatInterval:  20 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             testLogger(),
		})
		if err != nil {
			t.Fatalf("new %s: %v", addr, err)
		}
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start %s: %v", addr, err)
		}
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		clients = append(clients, pbv2.NewKVServiceClient(cc))
	}
	return addrs, clients
}

func waitTransferLeader(t *testing.T, clients []pbv2.KVServiceClient) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for i, c := range clients {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			st, err := c.Status(ctx, &pbv2.StatusRequest{RangeId: 1})
			cancel()
			if err == nil && strings.EqualFold(st.GetRole(), "leader") {
				return i
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("range 1 did not elect a leader")
	return -1
}

func TestTransferLeaderRPCAndEpoch(t *testing.T) {
	addrs, clients := startLeaderTestCluster(t, 3)
	leader := waitTransferLeader(t, clients)
	target := (leader + 1) % len(addrs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// An old range epoch is rejected without moving leadership.
	resp, err := clients[leader].TransferLeader(ctx, &pbv2.TransferLeaderRequest{
		RangeId: 1,
		Target:  addrs[target],
		Epoch:   999,
	})
	if err != nil {
		t.Fatalf("TransferLeader(stale epoch): %v", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetError(), "stale epoch") {
		t.Fatalf("stale-epoch response = %+v, want stale epoch failure", resp)
	}
	if got := waitTransferLeader(t, clients); got != leader {
		t.Fatalf("leader changed to %s after a rejected epoch check", addrs[got])
	}

	resp, err = clients[leader].TransferLeader(ctx, &pbv2.TransferLeaderRequest{
		RangeId: 1,
		Target:  addrs[target],
		Epoch:   1,
	})
	if err != nil {
		t.Fatalf("TransferLeader: %v", err)
	}
	if !resp.GetSuccess() || resp.GetLeader() != addrs[target] {
		t.Fatalf("TransferLeader = %+v, want success with leader %s", resp, addrs[target])
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := waitTransferLeader(t, clients)
		if got == target {
			// The old leader must no longer claim leadership.
			st, err := clients[leader].Status(ctx, &pbv2.StatusRequest{RangeId: 1})
			if err == nil && !strings.EqualFold(st.GetRole(), "leader") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not become range leader after transfer", addrs[target])
}
