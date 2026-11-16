package kvserver

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newStatsServer starts one single-node range and returns a KV client for it.
func newStatsServer(t *testing.T, start, end []byte) (*Server, string, pbv2.KVServiceClient) {
	t.Helper()
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:       addr,
		RaftAddr: addr,
		DataDir:  t.TempDir(),
		Ranges: []RangeConfig{{
			ID: 1, Start: cloneBytes(start), End: cloneBytes(end),
		}},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	waitRangeLeader(t, srv, 1)
	return srv, addr, pbv2.NewKVServiceClient(cc)
}

func waitRangeLeader(t *testing.T, srv *Server, rangeID uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		node, err := srv.GroupForID(rangeID)
		if err == nil && node.Status().Role == raft.Leader {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("range %d did not elect a leader", rangeID)
}

func putStatsKeys(t *testing.T, client pbv2.KVServiceClient, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		value := strings.Repeat("v", 32)
		startTS := uint64(1000 + i*10)
		commitTS := startTS + 1
		pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte(key),
			Mutations: []*pbv2.Mutation{{
				Key: []byte(key), Value: []byte(value),
			}},
		})
		if err != nil || !pr.GetSuccess() {
			t.Fatalf("prewrite %q: %v %+v", key, err, pr)
		}
		cr, err := client.Commit(ctx, &pbv2.CommitRequest{
			StartTs: startTS, CommitTs: commitTS, Keys: [][]byte{[]byte(key)},
		})
		if err != nil || !cr.GetSuccess() {
			t.Fatalf("commit %q: %v %+v", key, err, cr)
		}
	}
}

func TestV2RangeHostRangeStats(t *testing.T) {
	_, addr, client := newStatsServer(t, []byte("a"), []byte("z"))
	ctx := context.Background()

	// An empty range has no safe split key.
	empty, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1, Epoch: 1})
	if err != nil {
		t.Fatalf("RangeStats(empty): %v", err)
	}
	if !empty.GetSuccess() || empty.GetLeader() != addr {
		t.Fatalf("empty stats = %+v, want success with leader %q", empty, addr)
	}
	if empty.GetKeyCount() != 0 || len(empty.GetSplitKey()) != 0 {
		t.Fatalf("empty stats = %+v, want 0 keys and empty split key", empty)
	}
	if !bytes.Equal(empty.GetStart(), []byte("a")) || !bytes.Equal(empty.GetEnd(), []byte("z")) {
		t.Fatalf("empty stats bounds = [%q, %q), want [a, z)", empty.GetStart(), empty.GetEnd())
	}

	const n = 10
	putStatsKeys(t, client, n)

	stats, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1, Epoch: 1})
	if err != nil {
		t.Fatalf("RangeStats: %v", err)
	}
	if !stats.GetSuccess() || stats.GetError() != "" {
		t.Fatalf("RangeStats = %+v, want success", stats)
	}
	if stats.GetLeader() != addr {
		t.Fatalf("stats leader = %q, want %q", stats.GetLeader(), addr)
	}
	if stats.GetSizeBytes() == 0 {
		t.Fatal("stats size_bytes = 0, want a positive size estimate")
	}
	if stats.GetKeyCount() != n {
		t.Fatalf("stats key_count = %d, want %d", stats.GetKeyCount(), n)
	}
	if !bytes.Equal(stats.GetStart(), []byte("a")) || !bytes.Equal(stats.GetEnd(), []byte("z")) {
		t.Fatalf("stats bounds = [%q, %q), want [a, z)", stats.GetStart(), stats.GetEnd())
	}
	splitKey := stats.GetSplitKey()
	if len(splitKey) == 0 {
		t.Fatal("stats split_key is empty for a range with 10 keys")
	}
	if bytes.Compare(splitKey, []byte("a")) <= 0 || bytes.Compare(splitKey, []byte("z")) >= 0 {
		t.Fatalf("stats split_key = %q, want strictly inside [a, z)", splitKey)
	}
	if want := "key-05"; string(splitKey) != want {
		t.Fatalf("stats split_key = %q, want %q (median of 10 keys)", splitKey, want)
	}

	// A stale epoch is rejected but still carries the leader hint.
	stale, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1, Epoch: 99})
	if err != nil {
		t.Fatalf("RangeStats(stale epoch): %v", err)
	}
	if stale.GetSuccess() || !strings.Contains(stale.GetError(), "stale epoch") {
		t.Fatalf("stale epoch stats = %+v, want stale-epoch failure", stale)
	}
	if stale.GetLeader() != addr {
		t.Fatalf("stale epoch leader = %q, want %q", stale.GetLeader(), addr)
	}

	// An unknown range is reported, not silently mapped to the default range.
	missing, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 42})
	if err != nil {
		t.Fatalf("RangeStats(unknown): %v", err)
	}
	if missing.GetSuccess() || !strings.Contains(missing.GetError(), "not hosted") {
		t.Fatalf("unknown range stats = %+v, want not-hosted failure", missing)
	}
	if missing.GetLeader() != addr {
		t.Fatalf("unknown range leader = %q, want %q", missing.GetLeader(), addr)
	}
}

func TestV2RangeHostRangeStatsFewKeys(t *testing.T) {
	_, _, client := newStatsServer(t, nil, nil)
	ctx := context.Background()

	empty, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1})
	if err != nil {
		t.Fatalf("RangeStats(empty): %v", err)
	}
	if !empty.GetSuccess() || len(empty.GetSplitKey()) != 0 {
		t.Fatalf("empty stats = %+v, want success with no split key", empty)
	}

	// One key cannot form two non-empty ranges, so no split key is offered.
	putStatsKeys(t, client, 1)
	one, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1})
	if err != nil {
		t.Fatalf("RangeStats(one key): %v", err)
	}
	if !one.GetSuccess() || one.GetKeyCount() != 1 {
		t.Fatalf("one-key stats = %+v, want success with 1 key", one)
	}
	if len(one.GetSplitKey()) != 0 {
		t.Fatalf("one-key split_key = %q, want empty", one.GetSplitKey())
	}
}

func TestV2RangeHostRangeStatsFollowerHintsLeader(t *testing.T) {
	addr1 := testFreeAddr(t)
	addr2 := testFreeAddr(t)
	newNode := func(addr, peer string) *Server {
		t.Helper()
		srv, err := New(Config{
			ID:       addr,
			RaftAddr: addr,
			DataDir:  t.TempDir(),
			Ranges: []RangeConfig{{
				ID: 1, Peers: []string{peer},
			}},
			ElectionTimeoutMin: 80 * time.Millisecond,
			ElectionTimeoutMax: 160 * time.Millisecond,
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Stop(ctx)
		})
		return srv
	}
	srv1 := newNode(addr1, addr2)
	srv2 := newNode(addr2, addr1)

	clients := map[string]pbv2.KVServiceClient{}
	for _, addr := range []string{addr1, addr2} {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		clients[addr] = pbv2.NewKVServiceClient(cc)
	}

	servers := map[string]*Server{addr1: srv1, addr2: srv2}
	var leaderAddr, followerAddr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, addr := range []string{addr1, addr2} {
			node, err := servers[addr].GroupForID(1)
			if err != nil || node.Status().Role != raft.Leader {
				continue
			}
			other := addr1
			if addr == addr1 {
				other = addr2
			}
			otherNode, err := servers[other].GroupForID(1)
			if err == nil && otherNode.Leader() == addr {
				leaderAddr, followerAddr = addr, other
			}
		}
		if followerAddr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if followerAddr == "" {
		t.Fatal("no stable two-node leader/follower pair")
	}

	resp, err := clients[followerAddr].RangeStats(context.Background(),
		&pbv2.RangeStatsRequest{RangeId: 1, Epoch: 1})
	if err != nil {
		t.Fatalf("RangeStats on follower: %v", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetError(), "not leader") {
		t.Fatalf("follower stats = %+v, want not-leader failure", resp)
	}
	if resp.GetLeader() != leaderAddr {
		t.Fatalf("follower leader hint = %q, want %q", resp.GetLeader(), leaderAddr)
	}
}

func TestV2RangeHostRangeStatsSplitKeyPolicy(t *testing.T) {
	start, end := []byte("a"), []byte("z")
	keys := [][]byte{[]byte("a"), []byte("b")}
	// The first key equals the inclusive start, so the split point moves to
	// the second key; splitting at "a" would leave an empty left range.
	if got := medianSplitKey(keys, start, end); string(got) != "b" {
		t.Fatalf("medianSplitKey(%q, %q) = %q, want b", keys, start, end)
	}
	if got := medianSplitKey([][]byte{[]byte("a")}, start, end); got != nil {
		t.Fatalf("medianSplitKey with one key = %q, want nil", got)
	}
	if got := medianSplitKey(nil, start, end); got != nil {
		t.Fatalf("medianSplitKey with no keys = %q, want nil", got)
	}
	// Every returned key must be strictly inside the range.
	got := medianSplitKey([][]byte{[]byte("a"), []byte("b"), []byte("c")}, start, end)
	if bytes.Compare(got, start) <= 0 || bytes.Compare(got, end) >= 0 {
		t.Fatalf("medianSplitKey = %q, want strictly inside [a, z)", got)
	}
}
