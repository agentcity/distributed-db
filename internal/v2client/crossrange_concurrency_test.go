package v2client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
)

// newThreeRangeTestClient wires three scripted nodes: range 1 [nil,m) on kv1,
// range 2 [m,t) on kv2 and range 3 [t,nil) on kv3. A transaction over a/n/z
// therefore touches three ranges with the primary on kv1.
func newThreeRangeTestClient(t testing.TB) (*Client, []*scriptedKV, *fakeMeta) {
	t.Helper()
	meta := newFakeMeta("meta1")
	meta.setGetRange(func(key []byte) (*pbv2.RangeInfo, error) {
		switch {
		case bytes.Compare(key, []byte("m")) < 0:
			return &pbv2.RangeInfo{
				Id: 1, Start: nil, End: []byte("m"), Version: 1,
				Replicas: []string{"kv1"}, Leader: "kv1",
			}, nil
		case bytes.Compare(key, []byte("t")) < 0:
			return &pbv2.RangeInfo{
				Id: 2, Start: []byte("m"), End: []byte("t"), Version: 1,
				Replicas: []string{"kv2"}, Leader: "kv2",
			}, nil
		default:
			return &pbv2.RangeInfo{
				Id: 3, Start: []byte("t"), End: nil, Version: 1,
				Replicas: []string{"kv3"}, Leader: "kv3",
			}, nil
		}
	})
	c := newTestClient(meta)
	c.addrs = []string{"kv1", "kv2", "kv3"}
	nodes := []*scriptedKV{
		{addr: "kv1"},
		{addr: "kv2"},
		{addr: "kv3"},
	}
	for i, n := range nodes {
		c.clients[n.addr] = n
		c.leaders[uint64(i+1)] = leaderEntry{addr: n.addr, until: time.Now().Add(time.Hour)}
	}
	return c, nodes, meta
}

func beginThreeRangeTxn(t *testing.T, c *Client, opts TxnOptions) *Txn {
	t.Helper()
	txn, err := c.BeginWithOptions(context.Background(), opts)
	if err != nil {
		t.Fatalf("BeginWithOptions: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("n"), []byte("mid"))
	txn.Set([]byte("z"), []byte("right"))
	return txn
}

// visibleStore is a tiny committed-key registry shared by scripted nodes. It
// lets a test assert that all three ranges ended up visible, not merely that
// their commit RPCs returned success.
type visibleStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newVisibleStore() *visibleStore {
	return &visibleStore{values: make(map[string]string)}
}

func (s *visibleStore) commit(req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
	s.mu.Lock()
	for _, k := range req.GetKeys() {
		s.values[string(k)] = "committed"
	}
	s.mu.Unlock()
	return &pbv2.CommitResponse{Success: true, Leader: "kv"}, nil
}

func (s *visibleStore) get(req *pbv2.GetRequest) (*pbv2.GetResponse, error) {
	s.mu.Lock()
	v, ok := s.values[string(req.GetKey())]
	s.mu.Unlock()
	if !ok {
		return &pbv2.GetResponse{Found: false, Leader: "kv"}, nil
	}
	return &pbv2.GetResponse{Found: true, Value: []byte(v), Leader: "kv"}, nil
}

// TestV2TwoPCCommitSecondariesConcurrently blocks both secondary commits until
// each has started. A serialized implementation can never release the barrier,
// and the measured wall time must stay below the sum of two secondary
// latencies (the primary commit is fast here).
func TestV2TwoPCCommitSecondariesConcurrently(t *testing.T) {
	c, nodes, _ := newThreeRangeTestClient(t)
	const latency = 150 * time.Millisecond

	arrived := make(chan string, 2)
	proceed := make(chan struct{})
	secondaryCommit := func(req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
		arrived <- string(req.GetKeys()[0])
		<-proceed
		time.Sleep(latency)
		return &pbv2.CommitResponse{Success: true, Leader: "kv"}, nil
	}
	nodes[1].commitFn = secondaryCommit
	nodes[2].commitFn = secondaryCommit

	txn := beginThreeRangeTxn(t, c, TxnOptions{
		CommitRetryTimeout: 5 * time.Second,
		RetryBackoff:       10 * time.Millisecond,
	})
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- txn.Commit(context.Background()) }()

	seen := make(map[string]bool, 2)
	for len(seen) < 2 {
		select {
		case key := <-arrived:
			seen[key] = true
		case <-time.After(2 * time.Second):
			close(proceed) // unblock the in-flight commit so the test can fail cleanly
			<-done
			t.Fatalf("secondary commits were serialized: only %v started before one finished", seen)
		}
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatalf("Commit: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("3-range commit wall time: %s (per-secondary latency %s, sequential would be >= %s)",
		elapsed.Round(time.Millisecond), latency, 2*latency)
	if elapsed >= 2*latency {
		t.Fatalf("commit wall time = %s, want < %s (secondary commits should overlap)", elapsed, 2*latency)
	}

	for i, n := range nodes {
		if got := n.counts().commit; got != 1 {
			t.Fatalf("range %d commit calls = %d, want 1", i+1, got)
		}
	}
	obs := append(nodes[0].commitObservations(), nodes[1].commitObservations()...)
	obs = append(obs, nodes[2].commitObservations()...)
	wantTS := obs[0].commitTS
	for i, ob := range obs {
		if ob.commitTS != wantTS || ob.startTS != obs[0].startTS {
			t.Fatalf("commit observation %d = start %d commit %d, want stable start %d commit %d",
				i, ob.startTS, ob.commitTS, obs[0].startTS, wantTS)
		}
	}
	if got := txn.CommitTS(); got != wantTS {
		t.Fatalf("txn.CommitTS() = %d, want %d", got, wantTS)
	}
}

// TestV2TwoPCSecondaryTransientFailureRetriesOnlyFailedRange has one secondary
// fail transiently while the other succeeds immediately. Only the failed range
// is retried; the successful range is committed exactly once, and every range
// is visible at the end.
func TestV2TwoPCSecondaryTransientFailureRetriesOnlyFailedRange(t *testing.T) {
	c, nodes, _ := newThreeRangeTestClient(t)
	store := newVisibleStore()
	for _, n := range nodes {
		n.getFn = store.get
	}
	nodes[0].commitFn = store.commit
	nodes[2].commitFn = store.commit

	var mu sync.Mutex
	failures := 2
	nodes[1].commitFn = func(req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
		mu.Lock()
		remaining := failures
		if remaining > 0 {
			failures--
		}
		mu.Unlock()
		if remaining > 0 {
			return nil, status.Error(codes.Unavailable, "transient secondary failure")
		}
		return store.commit(req)
	}

	txn := beginThreeRangeTxn(t, c, TxnOptions{
		CommitRetryTimeout: 3 * time.Second,
		RetryBackoff:       10 * time.Millisecond,
	})
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatalf("Commit after transient secondary failure: %v", err)
	}
	if got := nodes[0].counts().commit; got != 1 {
		t.Fatalf("primary commit calls = %d, want 1", got)
	}
	if got := nodes[1].counts().commit; got != 3 {
		t.Fatalf("flaky secondary commit calls = %d, want 3 (2 failures + success)", got)
	}
	if got := nodes[2].counts().commit; got != 1 {
		t.Fatalf("healthy secondary commit calls = %d, want 1 (must not be retried)", got)
	}
	if got := nodes[0].counts().rollback + nodes[1].counts().rollback + nodes[2].counts().rollback; got != 0 {
		t.Fatalf("rollback calls = %d after the commit point, want 0", got)
	}

	// All three keys must be visible through the normal read path.
	readTxn, err := c.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin for read: %v", err)
	}
	for _, key := range []string{"a", "n", "z"} {
		got, err := readTxn.Get(context.Background(), []byte(key))
		if err != nil {
			t.Fatalf("Get(%q) after commit: %v", key, err)
		}
		if string(got) != "committed" {
			t.Fatalf("Get(%q) = %q, want committed", key, got)
		}
	}
	if err := readTxn.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback read txn: %v", err)
	}
}

// TestV2TwoPCPrimaryFailurePreventsSecondaryCommits covers a primary range that
// never commits. Whether the outcome is known (rolled back) or unknown, no
// secondary commit may be sent.
func TestV2TwoPCPrimaryFailurePreventsSecondaryCommits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unknown bool
	}{
		{name: "known failure", unknown: false},
		{name: "unknown outcome", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, nodes, _ := newThreeRangeTestClient(t)
			nodes[0].commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
				return nil, status.Error(codes.Unavailable, "primary range down")
			}
			if tc.unknown {
				nodes[0].checkFn = func(*pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
					return nil, status.Error(codes.Unavailable, "primary status unavailable")
				}
			} else {
				nodes[0].checkFn = func(*pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
					return &pbv2.CheckTxnStatusResponse{RolledBack: true, Leader: "kv1"}, nil
				}
			}

			txn := beginThreeRangeTxn(t, c, TxnOptions{
				CommitRetryTimeout: 150 * time.Millisecond,
				RetryBackoff:       10 * time.Millisecond,
			})
			err := txn.Commit(context.Background())
			if err == nil {
				t.Fatal("Commit succeeded while the primary range was down")
			}
			if tc.unknown {
				if !errors.Is(err, ErrCommitUnknown) {
					t.Fatalf("Commit error = %v, want ErrCommitUnknown", err)
				}
				if rerr := txn.Rollback(context.Background()); !errors.Is(rerr, ErrCommitUnknown) {
					t.Fatalf("Rollback with unknown primary outcome = %v, want ErrCommitUnknown", rerr)
				}
			} else if errors.Is(err, ErrCommitUnknown) || errors.Is(err, ErrCommitIncomplete) {
				t.Fatalf("Commit error = %v, want a plain primary failure", err)
			}
			if got := nodes[1].counts().commit + nodes[2].counts().commit; got != 0 {
				t.Fatalf("secondary commit calls = %d after primary failure, want 0", got)
			}
		})
	}
}

// TestV2TwoPCPrewriteFailureRollsBackConcurrentResults fails one secondary
// prewrite while another secondary succeeds. The primary and the successful
// secondary must both be rolled back; the failed range is not rolled back
// because it was never prewritten.
func TestV2TwoPCPrewriteFailureRollsBackConcurrentResults(t *testing.T) {
	c, nodes, _ := newThreeRangeTestClient(t)
	nodes[1].prewriteFn = func(*pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
		return &pbv2.PrewriteResponse{Error: "write conflict", Leader: "kv2"}, nil
	}
	// Delay the healthy secondary so it is still in flight when the failing
	// range reports its conflict.
	nodes[2].prewriteFn = func(*pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
		time.Sleep(20 * time.Millisecond)
		return &pbv2.PrewriteResponse{Success: true, Leader: "kv3"}, nil
	}

	txn := beginThreeRangeTxn(t, c, TxnOptions{
		CommitRetryTimeout: time.Second,
		RetryBackoff:       10 * time.Millisecond,
	})
	err := txn.Commit(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit error = %v, want ErrConflict", err)
	}
	if got := nodes[0].counts().commit + nodes[1].counts().commit + nodes[2].counts().commit; got != 0 {
		t.Fatalf("commit calls = %d after a prewrite failure, want 0", got)
	}
	if got := nodes[0].counts().rollback; got != 1 {
		t.Fatalf("primary rollback calls = %d, want 1", got)
	}
	if got := nodes[1].counts().rollback; got != 0 {
		t.Fatalf("failed secondary rollback calls = %d, want 0 (never prewritten)", got)
	}
	if got := nodes[2].counts().rollback; got != 1 {
		t.Fatalf("healthy secondary rollback calls = %d, want 1 (parallel result rolled back)", got)
	}
}

// TestV2TwoPCParallelTransactionsRace exercises concurrent cross-range commit
// completion under the race detector. Every transaction spans all three ranges
// and commits at the same time.
func TestV2TwoPCParallelTransactionsRace(t *testing.T) {
	c, nodes, _ := newThreeRangeTestClient(t)
	for _, n := range nodes {
		n.commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
			time.Sleep(2 * time.Millisecond)
			return &pbv2.CommitResponse{Success: true, Leader: "kv"}, nil
		}
	}

	const workers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			txn, err := c.Begin(context.Background())
			if err != nil {
				errCh <- fmt.Errorf("worker %d begin: %w", w, err)
				return
			}
			txn.Set([]byte(fmt.Sprintf("a%02d", w)), []byte("left"))
			txn.Set([]byte(fmt.Sprintf("n%02d", w)), []byte("mid"))
			txn.Set([]byte(fmt.Sprintf("z%02d", w)), []byte("right"))
			if err := txn.Commit(context.Background()); err != nil {
				errCh <- fmt.Errorf("worker %d commit: %w", w, err)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// BenchmarkV2TwoPCCommitSecondaries measures a three-range commit where every
// range commit takes the same fixed latency. With concurrent secondary commits
// the expected cost is primary + one secondary latency; the historical
// sequential implementation paid primary + two secondary latencies.
func BenchmarkV2TwoPCCommitSecondaries(b *testing.B) {
	c, nodes, _ := newThreeRangeTestClient(b)
	const latency = 2 * time.Millisecond
	for _, n := range nodes {
		n.commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
			time.Sleep(latency)
			return &pbv2.CommitResponse{Success: true, Leader: "kv"}, nil
		}
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn, err := c.Begin(ctx)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		txn.Set([]byte("a"), []byte("left"))
		txn.Set([]byte("n"), []byte("mid"))
		txn.Set([]byte("z"), []byte("right"))
		if err := txn.Commit(ctx); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}
