package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newFreezeTestServer starts a single-process server hosting the requested
// ranges and returns a gRPC client. The caller must not stop it manually.
func newFreezeTestServer(t *testing.T, ranges []RangeConfig) (*Server, pbv2.KVServiceClient) {
	t.Helper()
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		Ranges:             ranges,
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
	for _, rc := range ranges {
		waitRangeLeader(t, srv, rc.ID)
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return srv, pbv2.NewKVServiceClient(cc)
}

// freezePut writes key/value through Prewrite+Commit and fails the test on any
// error. startTS must be unique per test transaction.
func freezePut(t *testing.T, client pbv2.KVServiceClient, key, value string, startTS uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
		StartTs: startTS, CommitTs: startTS + 1, Keys: [][]byte{[]byte(key)},
	})
	if err != nil || !cr.GetSuccess() {
		t.Fatalf("commit %q: %v %+v", key, err, cr)
	}
}

// freezeTryPut attempts Prewrite+Commit and returns the first response that
// failed, without failing the test. A nil error and empty string means the
// transaction succeeded.
func freezeTryPut(t *testing.T, client pbv2.KVServiceClient, key, value string, startTS uint64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: startTS,
		Primary: []byte(key),
		Mutations: []*pbv2.Mutation{{
			Key: []byte(key), Value: []byte(value),
		}},
	})
	if err != nil {
		return err.Error()
	}
	if !pr.GetSuccess() {
		return pr.GetError()
	}
	cr, err := client.Commit(ctx, &pbv2.CommitRequest{
		StartTs: startTS, CommitTs: startTS + 1, Keys: [][]byte{[]byte(key)},
	})
	if err != nil {
		return err.Error()
	}
	if !cr.GetSuccess() {
		return cr.GetError()
	}
	return ""
}

func freezeGet(t *testing.T, client pbv2.KVServiceClient, key, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: ^uint64(0)})
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("get %q returned error %q", key, resp.GetError())
	}
	if !resp.GetFound() || string(resp.GetValue()) != want {
		t.Fatalf("get %q = %q, found=%v; want %q", key, resp.GetValue(), resp.GetFound(), want)
	}
}

func freezeSplitPreparePayload(t *testing.T, key string) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		SplitKey []byte `json:"split_key"`
	}{SplitKey: []byte(key)})
	if err != nil {
		t.Fatalf("encode split prepare: %v", err)
	}
	return payload
}

func freezeMergePreparePayload(t *testing.T, left, right uint64) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		Left  uint64 `json:"left"`
		Right uint64 `json:"right"`
	}{Left: left, Right: right})
	if err != nil {
		t.Fatalf("encode merge prepare: %v", err)
	}
	return payload
}

// TestV2SplitCopyFailureUnfreezesRange injects a copy/import error after the
// split prepare. The range must be writable again, no scratch group may be
// registered, and a retry must complete idempotently.
func TestV2SplitCopyFailureUnfreezesRange(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{{ID: 1}})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "m", "middle", 200)
	freezePut(t, client, "z", "right", 300)

	g := srv.groups[1]
	injected := errors.New("injected split copy failure")
	g.splitCopyHook = func(batch int) error {
		if batch == 0 {
			return injected
		}
		return nil
	}

	if _, err := srv.SplitRange(ctx, 1, []byte("m"), 2); err == nil {
		t.Fatal("split succeeded despite injected copy error")
	} else if !errors.Is(err, injected) {
		t.Fatalf("split error = %v, want %v", err, injected)
	}
	if srv.hostedGroup(2) != nil {
		t.Fatal("scratch range 2 stayed registered after failed split")
	}
	if g.isFrozen() {
		t.Fatal("range 1 stayed frozen after failed split")
	}

	// Both halves still belong to range 1 and must accept writes.
	if msg := freezeTryPut(t, client, "z", "right-after-failure", 400); msg != "" {
		t.Fatalf("write to right half after failed split: %s", msg)
	}
	if msg := freezeTryPut(t, client, "a", "left-after-failure", 410); msg != "" {
		t.Fatalf("write to left half after failed split: %s", msg)
	}
	freezeGet(t, client, "z", "right-after-failure")

	// Retry with the fault cleared: the split must succeed and move the data.
	g.splitCopyHook = nil
	ranges, err := srv.SplitRange(ctx, 1, []byte("m"), 2)
	if err != nil {
		t.Fatalf("split retry: %v", err)
	}
	if len(ranges) != 2 {
		t.Fatalf("ranges after retry = %+v, want 2", ranges)
	}
	waitRangeLeader(t, srv, 2)
	g2 := srv.hostedGroup(2)
	if g2 == nil {
		t.Fatal("range 2 missing after retry")
	}
	freezeGet(t, client, "a", "left-after-failure")
	freezeGet(t, client, "m", "middle")
	freezeGet(t, client, "z", "right-after-failure")

	// Duplicate finish: a second identical call returns success and does not
	// create/copy another group.
	if _, err := srv.SplitRange(ctx, 1, []byte("m"), 2); err != nil {
		t.Fatalf("duplicate split: %v", err)
	}
	if srv.hostedGroup(2) != g2 {
		t.Fatal("duplicate split replaced the existing range 2 group")
	}
	if srv.groups[1].isFrozen() || g2.isFrozen() {
		t.Fatal("duplicate split left a range frozen")
	}
}

// TestV2MergeCopyFailureUnfreezesRanges injects a copy error after the merge
// prepare. Both ranges must stay served and writable, and a retry plus a
// duplicate finish must be idempotent.
func TestV2MergeCopyFailureUnfreezesRanges(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{
		{ID: 1, Start: nil, End: []byte("m")},
		{ID: 2, Start: []byte("m"), End: nil},
	})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "z", "right", 200)

	left, right := srv.groups[1], srv.groups[2]
	injected := errors.New("injected merge copy failure")
	right.mergeCopyHook = func(batch int) error {
		if batch == 0 {
			return injected
		}
		return nil
	}

	if _, err := srv.MergeRanges(ctx, 1, 2); err == nil {
		t.Fatal("merge succeeded despite injected copy error")
	} else if !errors.Is(err, injected) {
		t.Fatalf("merge error = %v, want %v", err, injected)
	}
	if srv.hostedGroup(1) == nil || srv.hostedGroup(2) == nil {
		t.Fatal("merge failure removed a range group")
	}
	if left.isFrozen() || right.isFrozen() {
		t.Fatal("merge failure left a range frozen")
	}

	// Both ranges must accept writes again.
	if msg := freezeTryPut(t, client, "a", "left-after-failure", 300); msg != "" {
		t.Fatalf("write to left range after failed merge: %s", msg)
	}
	if msg := freezeTryPut(t, client, "z", "right-after-failure", 310); msg != "" {
		t.Fatalf("write to right range after failed merge: %s", msg)
	}
	freezeGet(t, client, "a", "left-after-failure")
	freezeGet(t, client, "z", "right-after-failure")

	// Clear the fault and retry: the merge must complete.
	right.mergeCopyHook = nil
	if _, err := srv.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("merge retry: %v", err)
	}
	if srv.hostedGroup(2) != nil {
		t.Fatal("range 2 still registered after merge retry")
	}
	if left.isFrozen() {
		t.Fatal("left range frozen after merge retry")
	}
	freezeGet(t, client, "a", "left-after-failure")
	freezeGet(t, client, "z", "right-after-failure")

	// Duplicate finish after the right group has been removed.
	if _, err := srv.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("duplicate merge: %v", err)
	}
	if left.isFrozen() {
		t.Fatal("duplicate merge left the range frozen")
	}
}

// TestV2SplitWriteQueuedAcrossPrepareRejected models a write proposed before
// the prepare but applied after it. Apply must reject it explicitly instead of
// letting it mutate the old range after the copy.
func TestV2SplitWriteQueuedAcrossPrepareRejected(t *testing.T) {
	sm, _ := newStreamTestSM(t, openStreamTestEngine(t), 1)

	// The write observed generation 0 before the prepare.
	gen, frozen := sm.writeTicket()
	if frozen {
		t.Fatal("fresh state machine is frozen")
	}
	writePayload, err := encodeCommand(applyCommand{
		StartTS: 10, CommitTS: 11, Primary: []byte("z"), Gen: gen,
		Mutations: []mvcc.Mutation{{Key: []byte("z"), Value: []byte("value")}},
	})
	if err != nil {
		t.Fatalf("encode write: %v", err)
	}

	if err := sm.Apply(model.Entry{Type: model.CommandSplitPrepare, Value: freezeSplitPreparePayload(t, "m")}); err != nil {
		t.Fatalf("apply prepare: %v", err)
	}
	err = sm.Apply(model.Entry{Type: model.CommandPrewriteCommit, Value: writePayload})
	if !errors.Is(err, ErrRangeFrozen) {
		t.Fatalf("queued write apply error = %v, want ErrRangeFrozen", err)
	}
	if _, err := sm.store.Get([]byte("z"), 11); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("rejected write mutated the store: %v", err)
	}

	// Clearing the freeze must not let the stale proposal through: its
	// generation predates the barrier.
	sm.clearSplit()
	if err := sm.Apply(model.Entry{Type: model.CommandPrewriteCommit, Value: writePayload}); !errors.Is(err, ErrRangeFrozen) {
		t.Fatalf("stale write after clear error = %v, want ErrRangeFrozen", err)
	}

	// A write proposed after the clear observes the new generation and works.
	newGen, frozen := sm.writeTicket()
	if frozen {
		t.Fatal("range still frozen after clear")
	}
	if newGen == gen {
		t.Fatalf("generation did not advance across freeze: got %d", newGen)
	}
	freshPayload, err := encodeCommand(applyCommand{
		StartTS: 20, CommitTS: 21, Primary: []byte("z"), Gen: newGen,
		Mutations: []mvcc.Mutation{{Key: []byte("z"), Value: []byte("fresh")}},
	})
	if err != nil {
		t.Fatalf("encode fresh write: %v", err)
	}
	if err := sm.Apply(model.Entry{Type: model.CommandPrewriteCommit, Value: freshPayload}); err != nil {
		t.Fatalf("fresh write: %v", err)
	}
	value, err := sm.store.Get([]byte("z"), 21)
	if err != nil || string(value) != "fresh" {
		t.Fatalf("fresh write = %q, %v; want fresh", value, err)
	}
}

// TestV2SplitMergePrepareRetriesDoNotDoubleFreeze checks that repeated
// prepares are idempotent and that conflicting or leftover markers are
// cleared without double-incrementing the barrier generation.
func TestV2SplitMergePrepareRetriesDoNotDoubleFreeze(t *testing.T) {
	sm, _ := newStreamTestSM(t, openStreamTestEngine(t), 1)
	split := func(key string) error {
		return sm.Apply(model.Entry{Type: model.CommandSplitPrepare, Value: freezeSplitPreparePayload(t, key)})
	}
	merge := func(left, right uint64) error {
		return sm.Apply(model.Entry{Type: model.CommandMergePrepare, Value: freezeMergePreparePayload(t, left, right)})
	}

	if err := split("m"); err != nil {
		t.Fatalf("split prepare: %v", err)
	}
	gen, frozen := sm.writeTicket()
	if !frozen || gen != 1 {
		t.Fatalf("after first prepare: gen=%d frozen=%v, want 1/true", gen, frozen)
	}
	if err := split("m"); err != nil {
		t.Fatalf("duplicate split prepare: %v", err)
	}
	if got, _ := sm.writeTicket(); got != gen {
		t.Fatalf("duplicate split bumped generation %d -> %d", gen, got)
	}
	if err := merge(1, 2); !errors.Is(err, ErrRangeFrozen) {
		t.Fatalf("merge prepare during split = %v, want ErrRangeFrozen", err)
	}

	sm.clearSplit()
	sm.clearSplit() // idempotent
	if err := merge(1, 2); err != nil {
		t.Fatalf("merge prepare: %v", err)
	}
	gen, frozen = sm.writeTicket()
	if !frozen {
		t.Fatal("merge prepare did not freeze")
	}
	if err := merge(1, 2); err != nil {
		t.Fatalf("duplicate merge prepare: %v", err)
	}
	if got, _ := sm.writeTicket(); got != gen {
		t.Fatalf("duplicate merge bumped generation %d -> %d", gen, got)
	}
	if err := merge(2, 1); !errors.Is(err, ErrRangeFrozen) {
		t.Fatalf("conflicting merge prepare = %v, want ErrRangeFrozen", err)
	}
	if got, _ := sm.writeTicket(); got != gen {
		t.Fatalf("conflicting merge changed generation %d -> %d", gen, got)
	}

	sm.clearMerge()
	sm.clearMerge() // idempotent
	sm.clearFreeze()
	if err := split("m"); err != nil {
		t.Fatalf("split prepare after clear: %v", err)
	}
	if got, _ := sm.writeTicket(); got != gen+1 {
		t.Fatalf("new barrier generation = %d, want %d", got, gen+1)
	}
}

// TestV2SplitConcurrentWriteDuringFreezeRejected verifies the service-level
// rejection while a freeze is active and that writes recover as soon as it is
// cleared.
func TestV2SplitConcurrentWriteDuringFreezeRejected(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{{ID: 1}})
	g := srv.groups[1]
	if err := g.sm.freezeSplit([]byte("m")); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !g.isFrozen() {
		t.Fatal("range reports writable while frozen")
	}

	const n = 8
	var wg sync.WaitGroup
	msgs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CommitSingle(ctx, &pbv2.CommitSingleRequest{
				StartTs: uint64(1000 + i*10), CommitTs: uint64(1001 + i*10),
				Primary:   []byte(fmt.Sprintf("frozen-%d", i)),
				Mutations: []*pbv2.Mutation{{Key: []byte(fmt.Sprintf("frozen-%d", i)), Value: []byte("v")}},
			})
			if err != nil {
				msgs[i] = err.Error()
				return
			}
			if resp.GetSuccess() {
				msgs[i] = "unexpected success"
				return
			}
			msgs[i] = resp.GetError()
		}(i)
	}
	wg.Wait()
	for i, msg := range msgs {
		if !strings.Contains(msg, "range frozen") {
			t.Fatalf("writer %d response = %q, want range frozen", i, msg)
		}
	}

	// Prewrite is rejected too, not just the 1PC path.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pr, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: 5000, Primary: []byte("prewrite-frozen"),
		Mutations: []*pbv2.Mutation{{Key: []byte("prewrite-frozen"), Value: []byte("v")}},
	})
	if err != nil {
		t.Fatalf("prewrite: %v", err)
	}
	if pr.GetSuccess() || !strings.Contains(pr.GetError(), "range frozen") {
		t.Fatalf("prewrite response = %+v, want range frozen", pr)
	}

	g.sm.clearSplit()
	if msg := freezeTryPut(t, client, "after-freeze", "ok", 6000); msg != "" {
		t.Fatalf("write after clear: %s", msg)
	}
	freezeGet(t, client, "after-freeze", "ok")
}

// TestV2MergeBatchWriteGenerationPerCommand covers the batched 1PC path: a
// stale command is rejected per command while a command from the current
// generation still commits, and the whole entry does not fail.
func TestV2MergeBatchWriteGenerationPerCommand(t *testing.T) {
	sm, _ := newStreamTestSM(t, openStreamTestEngine(t), 1)

	if err := sm.Apply(model.Entry{Type: model.CommandSplitPrepare, Value: freezeSplitPreparePayload(t, "m")}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	sm.clearSplit()
	gen, frozen := sm.writeTicket()
	if frozen || gen == 0 {
		t.Fatalf("after clear: gen=%d frozen=%v, want non-zero/false", gen, frozen)
	}

	batch := txnBatch{ID: 42, Commands: []applyCommand{
		{
			StartTS: 10, CommitTS: 11, Primary: []byte("stale"), Gen: 0,
			Mutations: []mvcc.Mutation{{Key: []byte("stale"), Value: []byte("v")}},
		},
		{
			StartTS: 20, CommitTS: 21, Primary: []byte("fresh"), Gen: gen,
			Mutations: []mvcc.Mutation{{Key: []byte("fresh"), Value: []byte("v")}},
		},
	}}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	if err := sm.Apply(model.Entry{Type: model.CommandTxnBatch, Value: payload}); err != nil {
		t.Fatalf("apply batch: %v", err)
	}
	errs, ok := sm.batchResults.take(batch.ID)
	if !ok {
		t.Fatal("batch results missing")
	}
	if len(errs) != 2 {
		t.Fatalf("batch results = %d, want 2", len(errs))
	}
	if !errors.Is(errs[0], ErrRangeFrozen) {
		t.Fatalf("stale command error = %v, want ErrRangeFrozen", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("fresh command error = %v, want nil", errs[1])
	}
	if _, err := sm.store.Get([]byte("stale"), 11); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("stale command mutated the store: %v", err)
	}
	if value, err := sm.store.Get([]byte("fresh"), 21); err != nil || string(value) != "v" {
		t.Fatalf("fresh command = %q, %v; want v", value, err)
	}
}

// TestV2SplitRPCPrepareCopyFailureUnfreezes is the replicated-prepare variant
// of the split failure test: the leader proposes CommandSplitPrepare, the copy
// fails, and the RPC must clear the freeze before returning.
func TestV2SplitRPCPrepareCopyFailureUnfreezes(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{{ID: 1}})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "z", "right", 200)

	g := srv.groups[1]
	injected := errors.New("injected RPC split copy failure")
	g.splitCopyHook = func(int) error { return injected }

	resp, err := client.SplitRange(ctx, &pbv2.SplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"), NewRangeId: 2,
	})
	if err != nil {
		t.Fatalf("split RPC: %v", err)
	}
	if resp.GetSuccess() || resp.GetError() == "" {
		t.Fatalf("split RPC response = %+v, want failure", resp)
	}
	if srv.hostedGroup(2) != nil {
		t.Fatal("range 2 registered after failed prepare-path split")
	}
	if g.isFrozen() {
		t.Fatal("range stayed frozen after prepare-path split failure")
	}
	if msg := freezeTryPut(t, client, "z", "writable", 300); msg != "" {
		t.Fatalf("write after failed prepare-path split: %s", msg)
	}

	g.splitCopyHook = nil
	resp, err = client.SplitRange(ctx, &pbv2.SplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"), NewRangeId: 2,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("split RPC retry = %+v, %v", resp, err)
	}
	waitRangeLeader(t, srv, 2)
	freezeGet(t, client, "a", "left")
	freezeGet(t, client, "z", "writable")

	g2 := srv.hostedGroup(2)
	resp, err = client.SplitRange(ctx, &pbv2.SplitRangeRequest{
		RangeId: 1, SplitKey: []byte("m"), NewRangeId: 2,
	})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("duplicate split RPC = %+v, %v", resp, err)
	}
	if srv.hostedGroup(2) != g2 {
		t.Fatal("duplicate split RPC replaced the finished group")
	}
}

// TestV2MergeRPCPrepareCopyFailureUnfreezes is the replicated-prepare variant
// of the merge failure test: both ranges freeze, the copy fails, and the RPC
// must clear both freezes.
func TestV2MergeRPCPrepareCopyFailureUnfreezes(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{
		{ID: 1, Start: nil, End: []byte("m")},
		{ID: 2, Start: []byte("m"), End: nil},
	})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "z", "right", 200)

	left, right := srv.groups[1], srv.groups[2]
	injected := errors.New("injected RPC merge copy failure")
	right.mergeCopyHook = func(int) error { return injected }

	resp, err := client.MergeRanges(ctx, &pbv2.MergeRangesRequest{LeftRangeId: 1, RightRangeId: 2})
	if err != nil {
		t.Fatalf("merge RPC: %v", err)
	}
	if resp.GetSuccess() || resp.GetError() == "" {
		t.Fatalf("merge RPC response = %+v, want failure", resp)
	}
	if srv.hostedGroup(1) == nil || srv.hostedGroup(2) == nil {
		t.Fatal("merge RPC failure removed a range")
	}
	if left.isFrozen() || right.isFrozen() {
		t.Fatal("merge prepare-path failure left a range frozen")
	}
	if msg := freezeTryPut(t, client, "a", "left-ok", 300); msg != "" {
		t.Fatalf("left write after failed prepare-path merge: %s", msg)
	}
	if msg := freezeTryPut(t, client, "z", "right-ok", 310); msg != "" {
		t.Fatalf("right write after failed prepare-path merge: %s", msg)
	}

	right.mergeCopyHook = nil
	resp, err = client.MergeRanges(ctx, &pbv2.MergeRangesRequest{LeftRangeId: 1, RightRangeId: 2})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("merge RPC retry = %+v, %v", resp, err)
	}
	if srv.hostedGroup(2) != nil {
		t.Fatal("range 2 still registered after merge RPC retry")
	}
	freezeGet(t, client, "a", "left-ok")
	freezeGet(t, client, "z", "right-ok")

	resp, err = client.MergeRanges(ctx, &pbv2.MergeRangesRequest{LeftRangeId: 1, RightRangeId: 2})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("duplicate merge RPC = %+v, %v", resp, err)
	}
	if left.isFrozen() {
		t.Fatal("duplicate merge RPC left the range frozen")
	}
}
