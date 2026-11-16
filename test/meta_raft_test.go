package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/v2client"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMetaRaftTSOFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	servers := make([]*metaserver.Server, 0, 3)
	for i, addr := range addrs {
		peers := make([]string, 0, 2)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		srv, err := metaserver.New(metaserver.Config{
			Addr: addr, ID: addr, Peers: peers, DataDir: dirs[i], Replicated: true,
			Ranges:             []meta.Range{{ID: 1, Start: nil, End: nil, Replicas: addrs, Version: 1}},
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			Logger:             v2Logger(),
		})
		if err != nil {
			t.Fatalf("meta %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start meta %s: %v", addr, err)
		}
		servers = append(servers, srv)
		t.Cleanup(func() { _ = srv.Stop() })
	}

	leader := waitMetaLeader(t, servers)
	client := metaClient(t, leader.Addr())
	resp, err := client.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 5})
	if err != nil {
		t.Fatalf("AllocTS: %v", err)
	}
	first := resp.GetStart()
	if resp.GetCount() != 5 {
		t.Fatalf("AllocTS count = %d, want 5", resp.GetCount())
	}
	if _, err := client.ListRanges(ctx, &pbv2.ListRangesRequest{}); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}

	// A client started on a follower must discover the meta leader.
	var follower *metaserver.Server
	for _, srv := range servers {
		if srv != leader {
			follower = srv
			break
		}
	}
	if follower == nil {
		t.Fatal("no meta follower")
	}
	vc, err := v2client.New(follower.Addr(), []string{follower.Addr()})
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	redirectedTS, err := vc.AllocTS(ctx)
	if err != nil {
		t.Fatalf("client AllocTS via follower: %v", err)
	}
	if redirectedTS <= first+4 {
		t.Fatalf("redirected TSO = %d, want > %d", redirectedTS, first+4)
	}

	// Kill the meta leader; a new leader must continue strictly above the
	// committed high-water mark.
	if err := leader.Stop(); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	survivors := make([]*metaserver.Server, 0, 2)
	for _, s := range servers {
		if s != leader {
			survivors = append(survivors, s)
		}
	}
	newLeader := waitMetaLeader(t, survivors)
	client2 := metaClient(t, newLeader.Addr())
	resp2, err := client2.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
	if err != nil {
		t.Fatalf("AllocTS after failover: %v", err)
	}
	if resp2.GetStart() <= first+4 {
		t.Fatalf("TSO after failover = %d, want > %d", resp2.GetStart(), first+4)
	}
	if ts, err := vc.AllocTS(ctx); err != nil {
		t.Fatalf("client AllocTS after meta failover: %v (leader=%s follower=%s new=%s)", err, leader.Addr(), follower.Addr(), newLeader.Addr())
	} else if ts <= resp2.GetStart() {
		t.Fatalf("client TSO after failover = %d, want > %d", ts, resp2.GetStart())
	}

	// Restart the old leader on the same data dir/address and ensure it
	// catches up without reusing timestamps.
	restarted, err := metaserver.New(metaserver.Config{
		Addr: leader.Addr(), ID: leader.Addr(),
		Peers: peersFor(addrs, leader.Addr()), DataDir: dirs[addrIndex(addrs, leader.Addr())], Replicated: true,
		Ranges:             []meta.Range{{ID: 1, Start: nil, End: nil, Replicas: addrs, Version: 1}},
		ElectionTimeoutMin: 120 * time.Millisecond,
		ElectionTimeoutMax: 240 * time.Millisecond,
		HeartbeatInterval:  25 * time.Millisecond,
		Logger:             v2Logger(),
	})
	if err != nil {
		t.Fatalf("restart meta: %v", err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("start restarted meta: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Stop() })
	resp3, err := client2.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
	if err != nil {
		t.Fatalf("AllocTS after restart: %v", err)
	}
	if resp3.GetStart() <= resp2.GetStart() {
		t.Fatalf("TSO after restart = %d, want > %d", resp3.GetStart(), resp2.GetStart())
	}
}

func waitMetaLeader(t *testing.T, servers []*metaserver.Server) *metaserver.Server {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range servers {
			node := s.Raft()
			if node != nil && node.Status().Role == raft.Leader {
				return s
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("no meta leader")
	return nil
}

func metaClient(t *testing.T, addr string) pbv2.MetaServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta client: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewMetaServiceClient(cc)
}

func addrIndex(all []string, addr string) int {
	for i, a := range all {
		if a == addr {
			return i
		}
	}
	return 0
}

func peersFor(all []string, self string) []string {
	out := make([]string, 0, len(all)-1)
	for _, p := range all {
		if p != self {
			out = append(out, p)
		}
	}
	return out
}
