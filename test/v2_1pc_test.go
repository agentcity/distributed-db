package test

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestV2CommitSingle(t *testing.T) {
	ctx := context.Background()
	metaAddr := v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Logger: v2Logger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	addr := v2FreeAddr(t)
	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		ElectionTimeoutMin: 80 * time.Millisecond, ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, CacheSize: 8 << 20, MemTableSize: 4 << 20,
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Raft().Status().Role != "leader" {
		time.Sleep(20 * time.Millisecond)
	}
	client := v2Dial(t, addr)
	metaCC, err := grpc.NewClient(metaAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metaCC.Close() })
	meta := pbv2.NewMetaServiceClient(metaCC)
	ts, err := meta.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.CommitSingle(ctx, &pbv2.CommitSingleRequest{
		StartTs: ts.Start, CommitTs: ts.Start + 1, Primary: []byte("one"),
		Mutations: []*pbv2.Mutation{{Key: []byte("one"), Value: []byte("1pc")}},
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("CommitSingle: %v %+v", err, resp)
	}
	gr, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("one"), Ts: ts.Start + 1})
	if err != nil || !gr.GetFound() || string(gr.GetValue()) != "1pc" {
		t.Fatalf("Get after 1PC: %v %+v", err, gr)
	}
}
