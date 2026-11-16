package test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type v2Node struct {
	srv    *kvserver.Server
	addr   string
	client pbv2.KVServiceClient
}

func v2Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func v2FreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func v2Dial(t *testing.T, addr string) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

func v2Status(t *testing.T, c pbv2.KVServiceClient) *pbv2.StatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	resp, err := c.Status(ctx, &pbv2.StatusRequest{})
	if err != nil {
		return nil
	}
	return resp
}

func v2WaitLeader(t *testing.T, nodes []*v2Node) *v2Node {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			st := v2Status(t, n.client)
			if st != nil && strings.EqualFold(st.Role, "leader") {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no v2 leader")
	return nil
}

func TestV2SingleRangeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatalf("meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("meta dial: %v", err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	metaClient := pbv2.NewMetaServiceClient(metaCC)

	addrs := []string{v2FreeAddr(t), v2FreeAddr(t), v2FreeAddr(t)}
	nodes := make([]*v2Node, 0, len(addrs))
	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, p := range addrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		srv, err := kvserver.New(kvserver.Config{
			ID:                 addr,
			RaftAddr:           addr,
			DataDir:            t.TempDir(),
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  30 * time.Millisecond,
			CacheSize:          8 << 20,
			MemTableSize:       4 << 20,
			Logger:             v2Logger(),
		})
		if err != nil {
			t.Fatalf("new node %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start node %s: %v", addr, err)
		}
		n := &v2Node{srv: srv, addr: addr}
		n.client = v2Dial(t, addr)
		nodes = append(nodes, n)
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}

	leader := v2WaitLeader(t, nodes)

	tso := func() (uint64, uint64) {
		t.Helper()
		resp, err := metaClient.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
		if err != nil {
			t.Fatalf("alloc ts: %v", err)
		}
		return resp.Start, resp.Start + 1
	}

	put := func(key, value string) {
		t.Helper()
		startTS, commitTS := tso()
		pr, err := leader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte(key),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte(key),
				Value: []byte(value),
			}},
		})
		if err != nil {
			t.Fatalf("prewrite: %v", err)
		}
		if !pr.GetSuccess() {
			t.Fatalf("prewrite failed: %s (leader %s)", pr.GetError(), pr.GetLeader())
		}
		cr, err := leader.client.Commit(ctx, &pbv2.CommitRequest{
			StartTs:  startTS,
			CommitTs: commitTS,
			Keys:     [][]byte{[]byte(key)},
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if !cr.GetSuccess() {
			t.Fatalf("commit failed: %s", cr.GetError())
		}

		gr, err := leader.client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: commitTS})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !gr.GetFound() || string(gr.GetValue()) != value {
			t.Fatalf("get(%s) = %q, found=%v, err=%q", key, gr.GetValue(), gr.GetFound(), gr.GetError())
		}
	}

	put("alpha", "one")
	put("beta", "two")

	sr, err := leader.client.Scan(ctx, &pbv2.ScanRequest{Start: []byte("a"), End: []byte("z"), Ts: 1 << 62})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sr.GetKvs()) != 2 {
		t.Fatalf("scan returned %d rows: %v", len(sr.GetKvs()), sr.GetKvs())
	}

	// Kill the leader; the committed values must survive on the new leader.
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := leader.srv.Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	survivors := make([]*v2Node, 0, len(nodes)-1)
	for _, n := range nodes {
		if n != leader {
			survivors = append(survivors, n)
		}
	}
	newLeader := v2WaitLeader(t, survivors)

	gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: []byte("alpha"), Ts: 1 << 62})
	if err != nil {
		t.Fatalf("get after failover: %v", err)
	}
	if !gr.GetFound() || string(gr.GetValue()) != "one" {
		t.Fatalf("committed value lost after failover: %q found=%v err=%q", gr.GetValue(), gr.GetFound(), gr.GetError())
	}

	// New transaction must work on the new leader.
	func() {
		startTS, commitTS := tso()
		pr, err := newLeader.client.Prewrite(ctx, &pbv2.PrewriteRequest{
			StartTs: startTS,
			Primary: []byte("gamma"),
			Mutations: []*pbv2.Mutation{{
				Key:   []byte("gamma"),
				Value: []byte("three"),
			}},
		})
		if err != nil || !pr.GetSuccess() {
			t.Fatalf("post-failover prewrite: %v %+v", err, pr)
		}
		cr, err := newLeader.client.Commit(ctx, &pbv2.CommitRequest{
			StartTs: startTS, CommitTs: commitTS, Keys: [][]byte{[]byte("gamma")},
		})
		if err != nil || !cr.GetSuccess() {
			t.Fatalf("post-failover commit: %v %+v", err, cr)
		}
		gr, err := newLeader.client.Get(ctx, &pbv2.GetRequest{Key: []byte("gamma"), Ts: commitTS})
		if err != nil || !gr.GetFound() || string(gr.GetValue()) != "three" {
			t.Fatalf("post-failover get: %v %+v", err, gr)
		}
	}()

	// A real transactional client must discover the new leader and commit.
	vc, err := v2client.New(metaAddr, addrs)
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	txn, err := vc.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("delta"), []byte("four"))
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("client commit: %v", err)
	}
	readTxn, err := vc.Begin(ctx)
	if err != nil {
		t.Fatalf("read txn: %v", err)
	}
	got, err := readTxn.Get(ctx, []byte("delta"))
	if err != nil || string(got) != "four" {
		t.Fatalf("client get delta = %q, %v", got, err)
	}

	if got := newLeader.srv.Raft().Status(); got.Leader == "" {
		t.Fatalf("new leader reports empty leader: %+v", got)
	}
}
