package test

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

// secondaryCommitProbe delays secondary Commit RPCs and holds each one until all
// expected secondaries are in flight. A serialized implementation therefore
// fails the test instead of merely looking slow.
type secondaryCommitProbe struct {
	delay time.Duration

	mu   sync.Mutex
	seen map[string]bool
	want map[string]bool
	all  chan struct{}
	once sync.Once
}

func newSecondaryCommitProbe(delay time.Duration, keys ...string) *secondaryCommitProbe {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	return &secondaryCommitProbe{
		delay: delay,
		seen:  make(map[string]bool),
		want:  want,
		all:   make(chan struct{}),
	}
}

func (p *secondaryCommitProbe) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if method == pbv2.KVService_Commit_FullMethodName {
		if cr, ok := req.(*pbv2.CommitRequest); ok {
			key := ""
			for _, k := range cr.GetKeys() {
				if p.want[string(k)] {
					key = string(k)
					break
				}
			}
			if key != "" {
				p.mu.Lock()
				p.seen[key] = true
				if len(p.seen) >= len(p.want) {
					p.once.Do(func() { close(p.all) })
				}
				p.mu.Unlock()
				select {
				case <-p.all:
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}
				time.Sleep(p.delay)
			}
		}
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

func (p *secondaryCommitProbe) overlapped() bool {
	select {
	case <-p.all:
		return true
	default:
		return false
	}
}

// TestV2CrossRange2PCConcurrentSecondaryCommits runs a real three-range
// transaction where secondary commits are held until both are in flight. It
// verifies that the secondary commit phase overlaps (wall time close to one
// secondary latency, not their sum) and that every range becomes visible.
func TestV2CrossRange2PCConcurrentSecondaryCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr, addr := v2FreeAddr(t), v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Leader: addr, Version: 1},
			{ID: 2, Start: []byte("m"), End: []byte("t"), Replicas: []string{addr}, Leader: addr, Version: 1},
			{ID: 3, Start: []byte("t"), End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
		},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	srv, err := kvserver.New(kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
		Ranges: []kvserver.RangeConfig{
			{ID: 1, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: []byte("t")},
			{ID: 3, Start: []byte("t")},
		},
		ElectionTimeoutMin: 80 * time.Millisecond,
		ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          32 << 20,
		MemTableSize:       16 << 20,
		Logger:             v2Logger(),
	})
	if err != nil {
		t.Fatalf("kvserver: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start kvserver: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(sctx)
	})
	for _, rangeID := range []uint64{1, 2, 3} {
		if !waitGroupLeader(t, srv, rangeID) {
			t.Fatalf("range %d did not elect a leader", rangeID)
		}
	}

	const latency = 200 * time.Millisecond
	probe := newSecondaryCommitProbe(latency, "n", "z")
	client, err := v2client.NewWithDialOptions(metaAddr, []string{addr}, grpc.WithChainUnaryInterceptor(probe.intercept))
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{
		CommitRetryTimeout: 10 * time.Second,
		RetryBackoff:       10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("n"), []byte("mid"))
	txn.Set([]byte("z"), []byte("right"))

	start := time.Now()
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("cross-range commit: %v", err)
	}
	elapsed := time.Since(start)
	if !probe.overlapped() {
		t.Fatal("secondary commits did not overlap: a serialized implementation cannot pass")
	}
	t.Logf("3-range commit wall time: %s (per-secondary latency %s, sequential would be >= %s)",
		elapsed.Round(time.Millisecond), latency, 2*latency)
	if elapsed >= 2*latency {
		t.Fatalf("commit wall time = %s, want < %s (secondary commits should overlap)", elapsed, 2*latency)
	}

	for _, tc := range []struct{ key, want string }{
		{"a", "left"}, {"n", "mid"}, {"z", "right"},
	} {
		readTxn, err := client.Begin(ctx)
		if err != nil {
			t.Fatalf("begin read %q: %v", tc.key, err)
		}
		got, gerr := readTxn.Get(ctx, []byte(tc.key))
		_ = readTxn.Rollback(ctx)
		if gerr != nil || string(got) != tc.want {
			t.Fatalf("get %s = %q, %v; want %q", tc.key, got, gerr, tc.want)
		}
	}
}
