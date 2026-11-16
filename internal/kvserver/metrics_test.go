package kvserver

import (
	"context"
	"testing"
	"time"

	"distributed-db/internal/metrics"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestServerMetricsInterceptor(t *testing.T) {
	addr := testFreeAddr(t)
	slo := metrics.NewSLO(metrics.NewRegistry(), metrics.SLOConfig{Prefix: "test"})
	srv, err := New(Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Metrics: slo,
		ElectionTimeoutMin: 40 * time.Millisecond, ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond, CacheSize: 4 << 20, MemTableSize: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := pbv2.NewKVServiceClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = client.Get(ctx, &pbv2.GetRequest{Key: []byte("missing"), Ts: 1})
	report := slo.Report()
	if report.Requests == 0 {
		t.Fatal("no requests recorded")
	}
	if report.LatencyCount == 0 {
		t.Fatal("no latency samples recorded")
	}
}
