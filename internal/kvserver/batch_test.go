package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newBatchTestServer starts one single-node range and returns a gRPC client
// for it. The caller must not stop the server manually.
func newBatchTestServer(t *testing.T) (*Server, pbv2.KVServiceClient, *rangeGroup) {
	t.Helper()
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("stop server: %v", err)
		}
	})
	waitRangeLeader(t, srv, 1)

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return srv, pbv2.NewKVServiceClient(cc), srv.groups[1]
}

func commitSingleMutation(key, value string, startTS, commitTS uint64) *pbv2.CommitSingleRequest {
	return &pbv2.CommitSingleRequest{
		StartTs: startTS, CommitTs: commitTS,
		Primary:   []byte(key),
		Mutations: []*pbv2.Mutation{{Key: []byte(key), Value: []byte(value)}},
	}
}

// TestCommitSingleBatchesConcurrentRequests checks the core promise: many
// independent CommitSingle RPCs are applied by one Raft entry (one proposal,
// one batch) and every caller gets its own success.
func TestCommitSingleBatchesConcurrentRequests(t *testing.T) {
	_, client, g := newBatchTestServer(t)
	// A wide window makes the coalescing deterministic even under -race.
	g.batcher.setWindow(50 * time.Millisecond)
	before := g.node.Status().LastApplied
	baseStats := g.batcher.stats()

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	resps := make([]*pbv2.CommitSingleResponse, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("batch-key-%02d", i)
			startTS := uint64(1000 + i*10)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CommitSingle(ctx, commitSingleMutation(key, "value", startTS, startTS+1))
			errs[i], resps[i] = err, resp
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil || !resps[i].GetSuccess() {
			t.Fatalf("CommitSingle %d: err=%v resp=%+v", i, errs[i], resps[i])
		}
	}
	stats := g.batcher.stats()
	if got := stats.Batches - baseStats.Batches; got != 1 {
		t.Fatalf("batch proposals = %d, want 1 (stats=%+v)", got, stats)
	}
	if got := stats.Commands - baseStats.Commands; got != n {
		t.Fatalf("batched commands = %d, want %d", got, n)
	}
	if got := stats.Singles - baseStats.Singles; got != 0 {
		t.Fatalf("single fallback proposals = %d, want 0", got)
	}
	after := g.node.Status().LastApplied
	if got := after - before; got != 1 {
		t.Fatalf("Raft applied %d entries for the batch, want 1", got)
	}

	// Every transaction is visible and left no lock behind.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("batch-key-%02d", i)
		commitTS := uint64(1000+i*10) + 1
		value, err := g.store.Get([]byte(key), commitTS)
		if err != nil || string(value) != "value" {
			t.Fatalf("read %q: value=%q err=%v", key, value, err)
		}
	}
	locks, err := g.store.ExpiredLockKeys(time.Now().Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("scan locks: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("locks left after successful batch: %q", locks)
	}
}

// TestCommitSingleBatchOneConflict verifies per-command result plumbing: a
// write conflict in one command fails only that transaction, while the other
// command in the same Raft entry commits and becomes visible.
func TestCommitSingleBatchOneConflict(t *testing.T) {
	_, client, g := newBatchTestServer(t)
	g.batcher.setWindow(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Establish a committed version newer than the conflicting txn's startTS.
	seed := commitSingleMutation("conflict-key", "winner", 100, 101)
	if resp, err := client.CommitSingle(ctx, seed); err != nil || !resp.GetSuccess() {
		t.Fatalf("seed commit: err=%v resp=%+v", err, resp)
	}

	baseStats := g.batcher.stats()
	var wg sync.WaitGroup
	start := make(chan struct{})
	var badResp, goodResp *pbv2.CommitSingleResponse
	var badErr, goodErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		badResp, badErr = client.CommitSingle(ctx, commitSingleMutation("conflict-key", "loser", 50, 51))
	}()
	go func() {
		defer wg.Done()
		<-start
		goodResp, goodErr = client.CommitSingle(ctx, commitSingleMutation("good-key", "ok", 200, 201))
	}()
	close(start)
	wg.Wait()

	if badErr != nil {
		t.Fatalf("conflicting request: rpc error %v", badErr)
	}
	if badResp.GetSuccess() || badResp.GetError() != "write conflict" {
		t.Fatalf("conflicting request = %+v, want write conflict", badResp)
	}
	if goodErr != nil || !goodResp.GetSuccess() {
		t.Fatalf("good request: err=%v resp=%+v", goodErr, goodResp)
	}
	stats := g.batcher.stats()
	if got := stats.Batches - baseStats.Batches; got != 1 {
		t.Fatalf("batch proposals = %d, want 1 (stats=%+v)", got, stats)
	}
	if got := stats.Commands - baseStats.Commands; got != 2 {
		t.Fatalf("batched commands = %d, want 2", got)
	}

	if value, err := g.store.Get([]byte("good-key"), 201); err != nil || string(value) != "ok" {
		t.Fatalf("good key not visible: value=%q err=%v", value, err)
	}
	value, err := g.store.Get([]byte("conflict-key"), 201)
	if err != nil || string(value) != "winner" {
		t.Fatalf("conflict key = %q err=%v, want the older committed value", value, err)
	}
	locks, err := g.store.ExpiredLockKeys(time.Now().Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("scan locks: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("locks left after mixed batch: %q", locks)
	}
}

// TestCommitSingleBatchProposalFailure checks that a failed CommandTxnBatch
// proposal completes every waiter with the proposal error.
func TestCommitSingleBatchProposalFailure(t *testing.T) {
	_, _, g := newBatchTestServer(t)
	g.batcher.setWindow(50 * time.Millisecond)
	injected := errors.New("injected proposal failure")
	g.batcher.setProposeHook(func(context.Context, model.Entry) (uint64, error) {
		return 0, injected
	})
	defer g.batcher.setProposeHook(nil)
	baseStats := g.batcher.stats()

	const n = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			startTS := uint64(2000 + i*10)
			errs[i] = g.batcher.submit(context.Background(), applyCommand{
				StartTS: startTS, CommitTS: startTS + 1,
				Primary:   []byte(fmt.Sprintf("fail-key-%d", i)),
				Mutations: []mvcc.Mutation{{Key: []byte(fmt.Sprintf("fail-key-%d", i)), Value: []byte("v")}},
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, injected) {
			t.Fatalf("waiter %d error = %v, want %v", i, err, injected)
		}
	}
	stats := g.batcher.stats()
	if got := stats.Batches - baseStats.Batches; got != 1 {
		t.Fatalf("batch proposals = %d, want 1", got)
	}
	if got := stats.Commands - baseStats.Commands; got != n {
		t.Fatalf("batched commands = %d, want %d", got, n)
	}
}

// TestCommitSingleBatchOneWaiterCanceled checks that one caller's context
// cancellation neither aborts the shared proposal nor changes another
// caller's result.
func TestCommitSingleBatchOneWaiterCanceled(t *testing.T) {
	_, _, g := newBatchTestServer(t)
	g.batcher.setWindow(50 * time.Millisecond)

	entered := make(chan model.Entry, 1)
	release := make(chan struct{})
	realPropose := g.node.Propose
	g.batcher.setProposeHook(func(ctx context.Context, entry model.Entry) (uint64, error) {
		entered <- entry
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return realPropose(ctx, entry)
	})
	defer g.batcher.setProposeHook(nil)

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	var wg sync.WaitGroup
	var err1, err2 error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		err1 = g.batcher.submit(cancelCtx, applyCommand{
			StartTS: 3000, CommitTS: 3001, Primary: []byte("cancel-key"),
			Mutations: []mvcc.Mutation{{Key: []byte("cancel-key"), Value: []byte("canceled")}},
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		err2 = g.batcher.submit(ctx2, applyCommand{
			StartTS: 3010, CommitTS: 3011, Primary: []byte("other-key"),
			Mutations: []mvcc.Mutation{{Key: []byte("other-key"), Value: []byte("survivor")}},
		})
	}()
	close(start)

	// Both commands are already inside the batch when it enters Propose.
	entry := <-entered
	if entry.Type != model.CommandTxnBatch {
		t.Fatalf("proposal type = %q, want %q", entry.Type, model.CommandTxnBatch)
	}
	var batch txnBatch
	if err := json.Unmarshal(entry.Value, &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(batch.Commands) != 2 {
		t.Fatalf("batch has %d commands, want 2", len(batch.Commands))
	}

	// Cancel one waiter while the shared proposal is in flight.
	cancel()
	close(release)
	wg.Wait()

	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err1)
	}
	if err2 != nil {
		t.Fatalf("surviving waiter error = %v, want nil", err2)
	}
	if value, err := g.store.Get([]byte("other-key"), 3011); err != nil || string(value) != "survivor" {
		t.Fatalf("surviving txn not visible: value=%q err=%v", value, err)
	}
}

// TestCommitBatcherStopCompletesInflight checks that stopping a group stops
// the batcher goroutine, completes an in-flight waiter, and rejects new work.
func TestCommitBatcherStopCompletesInflight(t *testing.T) {
	srv, _, g := newBatchTestServer(t)
	g.batcher.setWindow(5 * time.Millisecond)
	entered := make(chan struct{}, 1)
	g.batcher.setProposeHook(func(ctx context.Context, _ model.Entry) (uint64, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	})

	submitDone := make(chan error, 1)
	go func() {
		submitDone <- g.batcher.submit(context.Background(), applyCommand{
			StartTS: 4000, CommitTS: 4001, Primary: []byte("inflight-key"),
			Mutations: []mvcc.Mutation{{Key: []byte("inflight-key"), Value: []byte("v")}},
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("batch proposal did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("stop server: %v", err)
	}
	select {
	case err := <-submitDone:
		if err == nil {
			t.Fatal("in-flight waiter succeeded, want stop/cancel error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight waiter did not complete after stop")
	}
	select {
	case <-g.batcher.doneCh:
	default:
		t.Fatal("batcher goroutine still running after Stop")
	}
	if err := g.batcher.submit(context.Background(), applyCommand{
		StartTS: 4010, CommitTS: 4011,
		Mutations: []mvcc.Mutation{{Key: []byte("after-stop"), Value: []byte("v")}},
	}); !errors.Is(err, raft.ErrStopped) {
		t.Fatalf("submit after stop = %v, want %v", err, raft.ErrStopped)
	}
}

// TestCommitSingleSequentialFallback checks that a lone request still works
// and uses the cheaper single-command path.
func TestCommitSingleSequentialFallback(t *testing.T) {
	_, client, g := newBatchTestServer(t)
	baseStats := g.batcher.stats()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.CommitSingle(ctx, commitSingleMutation("single-key", "one", 5000, 5001))
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("sequential CommitSingle: err=%v resp=%+v", err, resp)
	}
	stats := g.batcher.stats()
	if got := stats.Singles - baseStats.Singles; got != 1 {
		t.Fatalf("single fallback proposals = %d, want 1 (stats=%+v)", got, stats)
	}
	if got := stats.Batches - baseStats.Batches; got != 0 {
		t.Fatalf("batch proposals = %d, want 0", got)
	}
	if value, err := g.store.Get([]byte("single-key"), 5001); err != nil || string(value) != "one" {
		t.Fatalf("single key not visible: value=%q err=%v", value, err)
	}
}

// TestBatchResultStoreBounded exercises the result map's consume-once and
// bounded-retention behavior without a Raft node.
func TestBatchResultStoreBounded(t *testing.T) {
	var store batchResultStore
	store.put(7, []error{nil, errors.New("conflict")})
	errs, ok := store.take(7)
	if !ok || len(errs) != 2 || errs[0] != nil || errs[1] == nil {
		t.Fatalf("take = %v, %v", errs, ok)
	}
	if _, ok := store.take(7); ok {
		t.Fatal("take consumed the result twice")
	}
	for i := 0; i < maxBatchResults+100; i++ {
		store.put(uint64(i), nil)
	}
	store.mu.Lock()
	got := len(store.results)
	store.mu.Unlock()
	if got > maxBatchResults {
		t.Fatalf("retained %d results, want <= %d", got, maxBatchResults)
	}
}
