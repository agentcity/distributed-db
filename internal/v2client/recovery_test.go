package v2client

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
)

// scriptedKV is an in-memory KVServiceClient whose per-RPC behavior is set by
// the test. It records calls so tests can assert on retry and idempotency.
type scriptedKV struct {
	pbv2.KVServiceClient // unimplemented methods panic if used

	addr string

	mu sync.Mutex

	prewriteFn func(*pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error)
	commitFn   func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error)
	rollbackFn func(*pbv2.RollbackRequest) (*pbv2.RollbackResponse, error)
	checkFn    func(*pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error)
	resolveFn  func(*pbv2.ResolveLockRequest) (*pbv2.ResolveLockResponse, error)
	getFn      func(*pbv2.GetRequest) (*pbv2.GetResponse, error)

	prewriteCalls int
	commitCalls   int
	rollbackCalls int
	checkCalls    int
	resolveCalls  int
	getCalls      int

	commits   []commitObservation
	resolves  []*pbv2.ResolveLockRequest
	commitTSs []uint64
}

type commitObservation struct {
	startTS  uint64
	commitTS uint64
	keys     int
}

type nodeCounts struct {
	prewrite, commit, rollback, check, resolve, get int
}

func (n *scriptedKV) counts() nodeCounts {
	n.mu.Lock()
	defer n.mu.Unlock()
	return nodeCounts{n.prewriteCalls, n.commitCalls, n.rollbackCalls, n.checkCalls, n.resolveCalls, n.getCalls}
}

func (n *scriptedKV) commitObservations() []commitObservation {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]commitObservation(nil), n.commits...)
}

func (n *scriptedKV) resolveRequests() []*pbv2.ResolveLockRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*pbv2.ResolveLockRequest(nil), n.resolves...)
}

func (n *scriptedKV) Status(context.Context, *pbv2.StatusRequest, ...grpc.CallOption) (*pbv2.StatusResponse, error) {
	return &pbv2.StatusResponse{Leader: n.addr, Role: "leader"}, nil
}

func (n *scriptedKV) Prewrite(_ context.Context, req *pbv2.PrewriteRequest, _ ...grpc.CallOption) (*pbv2.PrewriteResponse, error) {
	n.mu.Lock()
	n.prewriteCalls++
	fn := n.prewriteFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.PrewriteResponse{Success: true, Leader: n.addr}, nil
}

func (n *scriptedKV) Commit(_ context.Context, req *pbv2.CommitRequest, _ ...grpc.CallOption) (*pbv2.CommitResponse, error) {
	n.mu.Lock()
	n.commitCalls++
	n.commits = append(n.commits, commitObservation{
		startTS: req.GetStartTs(), commitTS: req.GetCommitTs(), keys: len(req.GetKeys()),
	})
	n.commitTSs = append(n.commitTSs, req.GetCommitTs())
	fn := n.commitFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.CommitResponse{Success: true, Leader: n.addr}, nil
}

func (n *scriptedKV) Rollback(_ context.Context, req *pbv2.RollbackRequest, _ ...grpc.CallOption) (*pbv2.RollbackResponse, error) {
	n.mu.Lock()
	n.rollbackCalls++
	fn := n.rollbackFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.RollbackResponse{Success: true, Leader: n.addr}, nil
}

func (n *scriptedKV) CheckTxnStatus(_ context.Context, req *pbv2.CheckTxnStatusRequest, _ ...grpc.CallOption) (*pbv2.CheckTxnStatusResponse, error) {
	n.mu.Lock()
	n.checkCalls++
	fn := n.checkFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.CheckTxnStatusResponse{Leader: n.addr}, nil
}

func (n *scriptedKV) ResolveLock(_ context.Context, req *pbv2.ResolveLockRequest, _ ...grpc.CallOption) (*pbv2.ResolveLockResponse, error) {
	n.mu.Lock()
	n.resolveCalls++
	n.resolves = append(n.resolves, req)
	fn := n.resolveFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.ResolveLockResponse{Success: true, Leader: n.addr}, nil
}

func (n *scriptedKV) Get(_ context.Context, req *pbv2.GetRequest, _ ...grpc.CallOption) (*pbv2.GetResponse, error) {
	n.mu.Lock()
	n.getCalls++
	fn := n.getFn
	n.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &pbv2.GetResponse{Found: false, Leader: n.addr}, nil
}

// newCrossRangeTestClient wires two scripted nodes: range 1 [nil,m) on kv1 and
// range 2 [m,nil) on kv2.
func newCrossRangeTestClient(t *testing.T) (*Client, *scriptedKV, *scriptedKV, *fakeMeta) {
	t.Helper()
	meta := newFakeMeta("meta1")
	meta.setGetRange(func(key []byte) (*pbv2.RangeInfo, error) {
		if bytes.Compare(key, []byte("m")) < 0 {
			return &pbv2.RangeInfo{
				Id: 1, Start: nil, End: []byte("m"), Version: 1,
				Replicas: []string{"kv1"}, Leader: "kv1",
			}, nil
		}
		return &pbv2.RangeInfo{
			Id: 2, Start: []byte("m"), End: nil, Version: 1,
			Replicas: []string{"kv2"}, Leader: "kv2",
		}, nil
	})
	c := newTestClient(meta)
	c.addrs = []string{"kv1", "kv2"}
	n1 := &scriptedKV{addr: "kv1"}
	n2 := &scriptedKV{addr: "kv2"}
	c.clients["kv1"] = n1
	c.clients["kv2"] = n2
	c.leaders[1] = leaderEntry{addr: "kv1", until: time.Now().Add(time.Hour)}
	c.leaders[2] = leaderEntry{addr: "kv2", until: time.Now().Add(time.Hour)}
	return c, n1, n2, meta
}

func beginCrossRangeTxn(t *testing.T, c *Client, opts TxnOptions) *Txn {
	t.Helper()
	txn, err := c.BeginWithOptions(context.Background(), opts)
	if err != nil {
		t.Fatalf("BeginWithOptions: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("z"), []byte("right"))
	return txn
}

// TestV2TwoPCCommitRetriesSecondaryIdempotently covers a transient secondary
// commit failure after the primary commit point: Commit must retry the same
// commitTS instead of rolling back.
func TestV2TwoPCCommitRetriesSecondaryIdempotently(t *testing.T) {
	c, primary, secondary, _ := newCrossRangeTestClient(t)

	var mu sync.Mutex
	failures := 3
	secondary.commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return nil, status.Error(codes.Unavailable, "secondary leader moved")
		}
		return &pbv2.CommitResponse{Success: true, Leader: "kv2"}, nil
	}

	txn := beginCrossRangeTxn(t, c, TxnOptions{CommitRetryTimeout: 3 * time.Second, RetryBackoff: 10 * time.Millisecond})
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatalf("Commit after transient secondary failure: %v", err)
	}

	if got := primary.counts().commit; got != 1 {
		t.Fatalf("primary commit calls = %d, want 1", got)
	}
	if got := secondary.counts().commit; got != 4 {
		t.Fatalf("secondary commit calls = %d, want 4 (3 failures + success)", got)
	}
	if got := primary.counts().rollback + secondary.counts().rollback; got != 0 {
		t.Fatalf("rollback calls = %d after commit point, want 0", got)
	}
	observations := secondary.commitObservations()
	if len(observations) != 4 {
		t.Fatalf("secondary commit observations = %d, want 4", len(observations))
	}
	wantTS := observations[0].commitTS
	for i, ob := range observations {
		if ob.commitTS != wantTS || ob.startTS != observations[0].startTS {
			t.Fatalf("commit %d = start %d commit %d, want stable start %d commit %d",
				i, ob.startTS, ob.commitTS, observations[0].startTS, wantTS)
		}
	}
	if txn.CommitTS() != wantTS {
		t.Fatalf("txn.CommitTS() = %d, want %d", txn.CommitTS(), wantTS)
	}
}

// TestV2TwoPCCommitIncompleteRetriesSamePlan covers exhaustion of the secondary
// retry budget: the transaction reports ErrCommitIncomplete (the primary stays
// committed), refuses Rollback, and a later Commit retries the frozen plan at
// the same commitTS without re-committing the primary.
func TestV2TwoPCCommitIncompleteRetriesSamePlan(t *testing.T) {
	c, primary, secondary, _ := newCrossRangeTestClient(t)

	secondary.commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
		return &pbv2.CommitResponse{Error: "timeout", Leader: "kv2"}, nil
	}

	txn := beginCrossRangeTxn(t, c, TxnOptions{CommitRetryTimeout: 150 * time.Millisecond, RetryBackoff: 10 * time.Millisecond})
	err := txn.Commit(context.Background())
	if !errors.Is(err, ErrCommitIncomplete) {
		t.Fatalf("Commit error = %v, want ErrCommitIncomplete", err)
	}
	var commitErr *CommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("Commit error type = %T, want *CommitError", err)
	}
	if !commitErr.CommitPoint || commitErr.OutcomeUnknown {
		t.Fatalf("CommitError = %+v, want commit point reached and outcome known", commitErr)
	}
	if commitErr.CommitTS == 0 || commitErr.CommitTS != txn.CommitTS() {
		t.Fatalf("CommitError commitTS = %d, txn commitTS = %d", commitErr.CommitTS, txn.CommitTS())
	}
	if got := primary.counts().commit; got != 1 {
		t.Fatalf("primary commit calls = %d, want 1", got)
	}
	if got := primary.counts().rollback + secondary.counts().rollback; got != 0 {
		t.Fatalf("rollback calls = %d, want 0 after commit point", got)
	}
	if err := txn.Rollback(context.Background()); !errors.Is(err, ErrCommitIncomplete) {
		t.Fatalf("Rollback after commit point = %v, want ErrCommitIncomplete", err)
	}

	// The secondary recovers. A retry must reuse the frozen commitTS, skip the
	// already-committed primary and commit only the remaining range.
	firstTS := txn.CommitTS()
	secondary.mu.Lock()
	secondary.commitFn = nil
	secondary.mu.Unlock()
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatalf("Commit retry after secondary recovery: %v", err)
	}
	if got := primary.counts().commit; got != 1 {
		t.Fatalf("primary commit calls after retry = %d, want 1 (primary already committed)", got)
	}
	observations := secondary.commitObservations()
	if len(observations) < 2 {
		t.Fatalf("secondary commit observations = %d, want at least 2", len(observations))
	}
	for i, ob := range observations {
		if ob.commitTS != firstTS {
			t.Fatalf("secondary commit %d commitTS = %d, want %d", i, ob.commitTS, firstTS)
		}
	}
}

// TestV2TwoPCUnknownPrimaryOutcomeRefusesRollback covers a primary commit whose
// outcome cannot be determined: Commit reports ErrCommitUnknown, Rollback is
// refused, and a later Commit replays the same plan successfully.
func TestV2TwoPCUnknownPrimaryOutcomeRefusesRollback(t *testing.T) {
	c, primary, secondary, _ := newCrossRangeTestClient(t)

	primary.commitFn = func(*pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
		return &pbv2.CommitResponse{Error: "timeout", Leader: "kv1"}, nil
	}
	primary.checkFn = func(*pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
		return nil, status.Error(codes.Unavailable, "primary status unavailable")
	}

	txn := beginCrossRangeTxn(t, c, TxnOptions{CommitRetryTimeout: 100 * time.Millisecond, RetryBackoff: 10 * time.Millisecond})
	err := txn.Commit(context.Background())
	if !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("Commit error = %v, want ErrCommitUnknown", err)
	}
	var commitErr *CommitError
	if !errors.As(err, &commitErr) || !commitErr.OutcomeUnknown {
		t.Fatalf("Commit error = %+v, want unknown-outcome CommitError", err)
	}
	if err := txn.Rollback(context.Background()); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("Rollback with unknown outcome = %v, want ErrCommitUnknown", err)
	}
	if got := primary.counts().rollback + secondary.counts().rollback; got != 0 {
		t.Fatalf("rollback calls = %d, want 0", got)
	}

	firstTS := txn.CommitTS()
	primary.mu.Lock()
	primary.commitFn = nil
	primary.checkFn = nil
	primary.mu.Unlock()
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatalf("Commit retry after primary recovery: %v", err)
	}
	if txn.CommitTS() != firstTS {
		t.Fatalf("CommitTs changed across unknown-outcome retry: got %d, want %d", txn.CommitTS(), firstTS)
	}
	if got := secondary.counts().commit; got == 0 {
		t.Fatal("secondary range was never committed")
	}
}

// TestV2TwoPCPrewriteFailureRollsBackEarlierRanges covers a failure before the
// commit point: already-prewritten ranges are rolled back and no range commits.
func TestV2TwoPCPrewriteFailureRollsBackEarlierRanges(t *testing.T) {
	c, primary, secondary, _ := newCrossRangeTestClient(t)

	secondary.prewriteFn = func(*pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
		return &pbv2.PrewriteResponse{Error: "write conflict", Leader: "kv2"}, nil
	}

	txn := beginCrossRangeTxn(t, c, TxnOptions{CommitRetryTimeout: time.Second, RetryBackoff: 10 * time.Millisecond})
	err := txn.Commit(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit error = %v, want ErrConflict", err)
	}
	if got := primary.counts().commit; got != 0 {
		t.Fatalf("primary commit calls = %d, want 0", got)
	}
	if got := secondary.counts().commit; got != 0 {
		t.Fatalf("secondary commit calls = %d, want 0", got)
	}
	if got := primary.counts().rollback; got != 1 {
		t.Fatalf("primary rollback calls = %d, want 1 for the prewritten range", got)
	}
	if got := secondary.counts().rollback; got != 0 {
		t.Fatalf("secondary rollback calls = %d, want 0 (range was never prewritten)", got)
	}
}

// TestV2TwoPCLockWaitIsBoundedWithDiagnostics covers deadlock/long-wait
// behavior: a lock that never clears produces a bounded ErrLockWaitTimeout with
// wait-for diagnostics instead of an endless retry loop.
func TestV2TwoPCLockWaitIsBoundedWithDiagnostics(t *testing.T) {
	c, primary, _, _ := newCrossRangeTestClient(t)

	primary.prewriteFn = func(*pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
		return &pbv2.PrewriteResponse{Error: "locked", Leader: "kv1"}, nil
	}
	primary.checkFn = func(req *pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
		return &pbv2.CheckTxnStatusResponse{
			LockPrimary: []byte("a"), LockTs: 7, TtlMs: 3000, Keys: [][]byte{[]byte("a")},
			Leader: "kv1",
		}, nil
	}

	start := time.Now()
	txn := beginCrossRangeTxn(t, c, TxnOptions{LockWaitTimeout: 120 * time.Millisecond, RetryBackoff: 10 * time.Millisecond})
	err := txn.Commit(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLockWaitTimeout) || !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit error = %v, want ErrLockWaitTimeout/ErrConflict", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("lock wait took %s, want bounded (<=2s)", elapsed)
	}
	var waitErr *LockWaitError
	if !errors.As(err, &waitErr) {
		t.Fatalf("Commit error type = %T, want *LockWaitError", err)
	}
	if len(waitErr.Keys) != 1 {
		t.Fatalf("wait-for entries = %d, want 1", len(waitErr.Keys))
	}
	wf := waitErr.Keys[0]
	if string(wf.Key) != "a" || string(wf.Primary) != "a" || wf.LockStartTS != 7 || wf.Attempts < 2 {
		t.Fatalf("wait-for = %+v, want key/primary a, lock_ts 7, attempts >= 2", wf)
	}
	for _, want := range []string{"wait-for", `key="a"`, "lock_ts=7", "still live"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if got := primary.counts().rollback; got != 0 {
		t.Fatalf("rollback calls = %d, want 0 (nothing was prewritten)", got)
	}
}

// TestV2TwoPCReadResolvesSecondaryLockThroughPrimary covers recovery from a
// secondary lock left after the primary committed: the read path inspects the
// primary lock and resolves the secondary lock as committed, making both values
// visible atomically.
func TestV2TwoPCReadResolvesSecondaryLockThroughPrimary(t *testing.T) {
	c, primary, secondary, meta := newCrossRangeTestClient(t)
	meta.setNext(10)
	txn, err := c.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	var mu sync.Mutex
	gets := 0
	secondary.getFn = func(*pbv2.GetRequest) (*pbv2.GetResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		if gets == 1 {
			return &pbv2.GetResponse{Error: "locked", Leader: "kv2"}, nil
		}
		return &pbv2.GetResponse{Found: true, Value: []byte("right"), Leader: "kv2"}, nil
	}
	secondary.checkFn = func(req *pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
		return &pbv2.CheckTxnStatusResponse{
			LockPrimary: []byte("a"), LockTs: 5, TtlMs: 3000, Keys: [][]byte{[]byte("z")},
			Leader: "kv2",
		}, nil
	}
	primary.checkFn = func(req *pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
		if req.GetStartTs() != 5 || string(req.GetKey()) != "a" {
			t.Errorf("primary status request = %+v, want key a startTS 5", req)
		}
		return &pbv2.CheckTxnStatusResponse{Committed: true, CommitTs: 20, Leader: "kv1"}, nil
	}

	got, err := txn.Get(context.Background(), []byte("z"))
	if err != nil {
		t.Fatalf("Get after lock resolution: %v", err)
	}
	if string(got) != "right" {
		t.Fatalf("Get = %q, want right", got)
	}
	if got := secondary.counts().resolve; got != 1 {
		t.Fatalf("secondary ResolveLock calls = %d, want 1", got)
	}
	resolves := secondary.resolveRequests()
	if len(resolves) != 1 {
		t.Fatalf("ResolveLock requests = %d, want 1", len(resolves))
	}
	req := resolves[0]
	if req.GetStartTs() != 5 || req.GetCommitTs() != 20 {
		t.Fatalf("ResolveLock = start %d commit %d, want 5/20", req.GetStartTs(), req.GetCommitTs())
	}
	if len(req.GetKeys()) != 1 || string(req.GetKeys()[0]) != "z" {
		t.Fatalf("ResolveLock keys = %q, want [z]", req.GetKeys())
	}
	if got := secondary.counts().get; got != 2 {
		t.Fatalf("secondary Get calls = %d, want 2 (locked + resolved)", got)
	}
}
