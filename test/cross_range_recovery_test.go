package test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

// cr2pcEnv is a two-node cluster with one single-replica range per node:
// node "primary" owns [nil,"m") and node "secondary" owns ["m",nil). Keeping
// the ranges on separate servers lets a test stop exactly the secondary range's
// leader between the primary commit and the secondary commit.
type cr2pcEnv struct {
	t *testing.T

	metaAddr string
	meta     *metaserver.Server

	primaryAddr   string
	secondaryAddr string
	primary       *kvserver.Server
	secondary     *kvserver.Server
	secondaryDir  string
	secondaryCfg  kvserver.Config
}

func startCR2PCEnv(t *testing.T) *cr2pcEnv {
	t.Helper()
	ctx := context.Background()
	e := &cr2pcEnv{
		t:             t,
		metaAddr:      v2FreeAddr(t),
		primaryAddr:   v2FreeAddr(t),
		secondaryAddr: v2FreeAddr(t),
		secondaryDir:  t.TempDir(),
	}
	ms, err := metaserver.New(metaserver.Config{
		Addr: e.metaAddr,
		Ranges: []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{e.primaryAddr}, Leader: e.primaryAddr, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{e.secondaryAddr}, Leader: e.secondaryAddr, Version: 1},
		},
		Logger: v2Logger(),
	})
	if err != nil {
		t.Fatalf("meta server: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	e.meta = ms
	t.Cleanup(func() { _ = ms.Stop() })

	e.primary = e.startNode(e.primaryAddr, kvserver.RangeConfig{ID: 1, End: []byte("m")}, t.TempDir())
	e.secondaryCfg = e.nodeConfig(e.secondaryAddr, kvserver.RangeConfig{ID: 2, Start: []byte("m")}, e.secondaryDir)
	e.secondary = e.startServer(e.secondaryCfg)
	t.Cleanup(func() {
		e.stopSecondary(3 * time.Second)
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = e.primary.Stop(sctx)
	})
	return e
}

func (e *cr2pcEnv) nodeConfig(addr string, rc kvserver.RangeConfig, dir string) kvserver.Config {
	return kvserver.Config{
		ID: addr, RaftAddr: addr, DataDir: dir,
		Ranges:             []kvserver.RangeConfig{rc},
		ElectionTimeoutMin: 60 * time.Millisecond,
		ElectionTimeoutMax: 120 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             v2Logger(),
	}
}

func (e *cr2pcEnv) startNode(addr string, rc kvserver.RangeConfig, dir string) *kvserver.Server {
	e.t.Helper()
	return e.startServer(e.nodeConfig(addr, rc, dir))
}

func (e *cr2pcEnv) startServer(cfg kvserver.Config) *kvserver.Server {
	e.t.Helper()
	srv, err := kvserver.New(cfg)
	if err != nil {
		e.t.Fatalf("new kvserver %s: %v", cfg.ID, err)
	}
	if err := srv.Start(context.Background()); err != nil {
		e.t.Fatalf("start kvserver %s: %v", cfg.ID, err)
	}
	if !waitGroupLeader(e.t, srv, cfg.Ranges[0].ID) {
		e.t.Fatalf("range %d did not elect a leader on %s", cfg.Ranges[0].ID, cfg.ID)
	}
	return srv
}

func (e *cr2pcEnv) stopSecondary(timeout time.Duration) {
	if e.secondary == nil {
		return
	}
	srv := e.secondary
	e.secondary = nil
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = srv.Stop(ctx)
}

// restartSecondary starts a new server process on the same address and data
// directory. The persisted Raft log replays the secondary prewrite, so the
// transaction can be completed without re-prewriting.
func (e *cr2pcEnv) restartSecondary() {
	e.t.Helper()
	e.secondary = e.startServer(e.secondaryCfg)
}

func (e *cr2pcEnv) client(interceptors ...grpc.UnaryClientInterceptor) *v2client.Client {
	e.t.Helper()
	opts := make([]grpc.DialOption, 0, 1)
	if len(interceptors) > 0 {
		opts = append(opts, grpc.WithChainUnaryInterceptor(interceptors...))
	}
	c, err := v2client.NewWithDialOptions(e.metaAddr, []string{e.primaryAddr, e.secondaryAddr}, opts...)
	if err != nil {
		e.t.Fatalf("v2 client: %v", err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

// commitPauseInjector blocks the first Commit RPC that touches key on the
// client side. The test can therefore stop the secondary leader exactly
// between the primary commit and the secondary commit.
type commitPauseInjector struct {
	once    sync.Once
	paused  chan struct{}
	release chan struct{}
	key     string
}

func newCommitPauseInjector(key string) *commitPauseInjector {
	return &commitPauseInjector{
		paused:  make(chan struct{}),
		release: make(chan struct{}),
		key:     key,
	}
}

func (p *commitPauseInjector) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if method == pbv2.KVService_Commit_FullMethodName {
		if cr, ok := req.(*pbv2.CommitRequest); ok && commitHasKey(cr.GetKeys(), p.key) {
			p.once.Do(func() {
				close(p.paused)
				select {
				case <-p.release:
				case <-ctx.Done():
				}
			})
		}
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// prewriteFailInjector answers a specific range's Prewrite with a conflict
// response without contacting that range.
type prewriteFailInjector struct {
	key string
}

func (p *prewriteFailInjector) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if method == pbv2.KVService_Prewrite_FullMethodName {
		if pr, ok := req.(*pbv2.PrewriteRequest); ok && prewriteHasKey(pr, p.key) {
			if resp, ok := reply.(*pbv2.PrewriteResponse); ok {
				resp.Error = "write conflict"
				return nil
			}
		}
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// lostCommitResponseInjector lets the first Commit RPC for key reach the server
// and then reports a transport error, simulating a lost success response after
// the range applied the commit.
type lostCommitResponseInjector struct {
	once sync.Once
	key  string
}

func (p *lostCommitResponseInjector) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	lose := false
	if method == pbv2.KVService_Commit_FullMethodName {
		if cr, ok := req.(*pbv2.CommitRequest); ok && commitHasKey(cr.GetKeys(), p.key) {
			p.once.Do(func() { lose = true })
		}
	}
	if !lose {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	if err := invoker(ctx, method, req, reply, cc, opts...); err != nil {
		return err
	}
	return grpcstatus.Error(codes.Unavailable, "injected: commit response lost")
}

func commitHasKey(keys [][]byte, key string) bool {
	for _, k := range keys {
		if string(k) == key {
			return true
		}
	}
	return false
}

func prewriteHasKey(req *pbv2.PrewriteRequest, key string) bool {
	for _, m := range req.GetMutations() {
		if string(m.GetKey()) == key {
			return true
		}
	}
	return false
}

func waitCR2PC(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// readCR2PCInvariant reads both ranges at one fresh snapshot and enforces the
// cross-range atomicity invariant: both values are visible or neither is. A
// lock is retried (the client resolves it through the primary); observing one
// key while the other is definitively absent is an immediate test failure.
func readCR2PCInvariant(t *testing.T, client *v2client.Client, ctx context.Context, timeout time.Duration) (left, right []byte, both bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{
			LockWaitTimeout:    2 * time.Second,
			CommitRetryTimeout: time.Second,
		})
		if err != nil {
			last = "begin: " + err.Error()
		} else {
			l, lerr := txn.Get(ctx, []byte("a"))
			r, rerr := txn.Get(ctx, []byte("z"))
			_ = txn.Rollback(ctx)
			switch {
			case lerr == nil && rerr == nil:
				return l, r, true
			case errors.Is(lerr, v2client.ErrNotFound) && errors.Is(rerr, v2client.ErrNotFound):
				return nil, nil, false
			case lerr == nil && errors.Is(rerr, v2client.ErrNotFound),
				errors.Is(lerr, v2client.ErrNotFound) && rerr == nil:
				t.Fatalf("partial cross-range visibility: a=(%q, %v), z=(%q, %v)", l, lerr, r, rerr)
			}
			last = fmt.Sprintf("a=(%q, %v), z=(%q, %v)", l, lerr, r, rerr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("cross-range invariant did not settle after %s: %s", timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertCR2PCValues requires atomically visible values on both ranges.
func assertCR2PCValues(t *testing.T, client *v2client.Client, ctx context.Context, wantLeft, wantRight string, timeout time.Duration) {
	t.Helper()
	left, right, both := readCR2PCInvariant(t, client, ctx, timeout)
	if !both {
		t.Fatalf("both keys absent, want a=%q z=%q", wantLeft, wantRight)
	}
	if string(left) != wantLeft || string(right) != wantRight {
		t.Fatalf("visible values = a=%q z=%q, want a=%q z=%q", left, right, wantLeft, wantRight)
	}
}

// assertCR2PCAbsent requires that neither range is visible.
func assertCR2PCAbsent(t *testing.T, client *v2client.Client, ctx context.Context) {
	t.Helper()
	left, right, both := readCR2PCInvariant(t, client, ctx, 5*time.Second)
	if both {
		t.Fatalf("both keys visible after rollback: a=%q z=%q", left, right)
	}
}

// TestV2CrossRange2PCRecoveryAfterPrimaryCommit injects a crash of the
// secondary range leader after the primary range committed and before the
// secondary commit, restarts it, and verifies the client retries the secondary
// commit idempotently until both ranges are visible.
func TestV2CrossRange2PCRecoveryAfterPrimaryCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	env := startCR2PCEnv(t)
	pause := newCommitPauseInjector("z")
	client := env.client(pause.intercept)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{
		CommitRetryTimeout: 12 * time.Second,
		RetryBackoff:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("z"), []byte("right"))

	done := make(chan error, 1)
	go func() { done <- txn.Commit(ctx) }()

	// The primary commit has completed: the client is now blocked in the
	// secondary Commit RPC. Kill the secondary leader at this exact point.
	waitCR2PC(t, pause.paused, 10*time.Second, "secondary commit to pause")
	env.stopSecondary(5 * time.Second)
	close(pause.release)
	env.restartSecondary()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Commit after secondary restart: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Commit did not finish after the secondary leader restarted")
	}
	assertCR2PCValues(t, client, ctx, "left", "right", 10*time.Second)
}

// TestV2CrossRange2PCCommitIncompleteResolver exhausts the secondary retry
// budget while the secondary leader is down, verifies the primary stays
// committed and Rollback is refused, then restarts the secondary and checks
// that both the read-path resolver and an idempotent Commit retry make the
// transaction visible atomically.
func TestV2CrossRange2PCCommitIncompleteResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	env := startCR2PCEnv(t)
	pause := newCommitPauseInjector("z")
	client := env.client(pause.intercept)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{
		CommitRetryTimeout: 300 * time.Millisecond,
		RetryBackoff:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("z"), []byte("right"))

	done := make(chan error, 1)
	go func() { done <- txn.Commit(ctx) }()
	waitCR2PC(t, pause.paused, 10*time.Second, "secondary commit to pause")
	env.stopSecondary(5 * time.Second)
	close(pause.release)

	var commitErr error
	select {
	case commitErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Commit did not report an error while the secondary was down")
	}
	if !errors.Is(commitErr, v2client.ErrCommitIncomplete) {
		t.Fatalf("Commit error = %v, want ErrCommitIncomplete", commitErr)
	}
	commitTS := txn.CommitTS()
	if commitTS == 0 {
		t.Fatal("txn.CommitTS() = 0 after reaching the commit point")
	}
	if err := txn.Rollback(ctx); !errors.Is(err, v2client.ErrCommitIncomplete) {
		t.Fatalf("Rollback after commit point = %v, want ErrCommitIncomplete", err)
	}

	// Restart well within the 3s lock TTL so the secondary lock is still
	// resolvable through the committed primary.
	env.restartSecondary()

	// The read path resolves the stale secondary lock by inspecting the
	// primary lock, so a reader sees both values (not one) atomically.
	assertCR2PCValues(t, client, ctx, "left", "right", 10*time.Second)

	// A retried Commit reuses the frozen commitTS and finishes idempotently.
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("idempotent Commit retry after recovery: %v", err)
	}
	if got := txn.CommitTS(); got != commitTS {
		t.Fatalf("CommitTs changed across retry: got %d, want %d", got, commitTS)
	}
	assertCR2PCValues(t, client, ctx, "left", "right", 5*time.Second)
}

// TestV2CrossRange2PCConcurrentConflict runs concurrent cross-range
// transactions over the same keys: exactly one commits, the other aborts with a
// conflict after bounded waiting, and the final state contains both keys from
// the winner (never a partial mix).
func TestV2CrossRange2PCConcurrentConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	env := startCR2PCEnv(t)
	client := env.client()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := v2client.TxnOptions{
		LockWaitTimeout:    2 * time.Second,
		CommitRetryTimeout: 5 * time.Second,
		RetryBackoff:       10 * time.Millisecond,
	}
	for round := 0; round < 3; round++ {
		leftVal := fmt.Sprintf("left-%d", round)
		rightVal := fmt.Sprintf("right-%d", round)
		t1, err := client.BeginWithOptions(ctx, opts)
		if err != nil {
			t.Fatalf("round %d: begin t1: %v", round, err)
		}
		t1.Set([]byte("a"), []byte(leftVal))
		t1.Set([]byte("z"), []byte(leftVal))
		t2, err := client.BeginWithOptions(ctx, opts)
		if err != nil {
			t.Fatalf("round %d: begin t2: %v", round, err)
		}
		t2.Set([]byte("a"), []byte(rightVal))
		t2.Set([]byte("z"), []byte(rightVal))

		start := make(chan struct{})
		type outcome struct {
			which int
			err   error
		}
		results := make(chan outcome, 2)
		go func() { <-start; results <- outcome{which: 1, err: t1.Commit(ctx)} }()
		go func() { <-start; results <- outcome{which: 2, err: t2.Commit(ctx)} }()
		close(start)
		first, second := <-results, <-results
		err1, err2 := first.err, second.err
		if first.which == 2 {
			err1, err2 = second.err, first.err
		}

		if (err1 == nil) == (err2 == nil) {
			t.Fatalf("round %d: commit results err1=%v err2=%v, want exactly one winner", round, err1, err2)
		}
		var loserErr error
		winnerVal := leftVal
		if err1 != nil {
			loserErr = err1
			winnerVal = rightVal
		} else {
			loserErr = err2
		}
		if !errors.Is(loserErr, v2client.ErrConflict) && !errors.Is(loserErr, v2client.ErrLockWaitTimeout) {
			t.Fatalf("round %d: loser error = %v, want conflicting abort", round, loserErr)
		}
		assertCR2PCValues(t, client, ctx, winnerVal, winnerVal, 10*time.Second)
	}
}

// TestV2CrossRange2PCResolverLeavesForeignSecondaryLock puts both ranges on one
// node. A secondary key's lock has a primary in the other range; its TTL is
// expired and the primary range has committed. The background resolver must not
// locally roll the secondary lock back (which would make the transaction
// partially visible). A client read must instead resolve it through the primary
// range and see both values.
func TestV2CrossRange2PCResolverLeavesForeignSecondaryLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	metaAddr, addr := v2FreeAddr(t), v2FreeAddr(t)
	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{addr}, Leader: addr, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{addr}, Leader: addr, Version: 1},
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
			{ID: 2, Start: []byte("m")},
		},
		ElectionTimeoutMin: 80 * time.Millisecond,
		ElectionTimeoutMax: 160 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
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
	if !waitGroupLeader(t, srv, 1) || !waitGroupLeader(t, srv, 2) {
		t.Fatal("ranges did not elect leaders")
	}
	raw := v2Dial(t, addr)

	// The lock is already past its 3s TTL: startTS physical time is 10s ago.
	oldTS := uint64(time.Now().Add(-10*time.Second).UnixMilli()) << 18
	pr, err := raw.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: oldTS, Primary: []byte("a"),
		Mutations: []*pbv2.Mutation{{Key: []byte("z"), Value: []byte("right")}},
	})
	if err != nil || !pr.GetSuccess() {
		t.Fatalf("prewrite secondary: %v %+v", err, pr)
	}
	// Commit the primary range in one Raft entry; its key is in range 1 while
	// the secondary lock is in range 2.
	cr, err := raw.CommitSingle(ctx, &pbv2.CommitSingleRequest{
		StartTs: oldTS, CommitTs: oldTS + 1, Primary: []byte("a"),
		Mutations: []*pbv2.Mutation{{Key: []byte("a"), Value: []byte("left")}},
	})
	if err != nil || !cr.GetSuccess() {
		t.Fatalf("commit primary: %v %+v", err, cr)
	}

	// Give the background resolver at least one tick. It must skip the foreign
	// secondary lock instead of TTL-rolling it back.
	time.Sleep(1500 * time.Millisecond)
	if got := srv.ResolvedLocks(); got != 0 {
		t.Fatalf("background resolver resolved %d locks, want 0 (foreign secondary lock must be skipped)", got)
	}

	// A raw read must report the still-present lock, not a locally resolved
	// absence. The high-level client below is what resolves it through the
	// primary range.
	nowTS := uint64(time.Now().UnixMilli()) << 18
	gr, err := raw.Get(ctx, &pbv2.GetRequest{Key: []byte("z"), Ts: nowTS})
	if err != nil {
		t.Fatalf("raw get secondary: %v", err)
	}
	if gr.GetError() != "locked" {
		t.Fatalf("raw get secondary = %+v, want still locked (resolver must not roll it back locally)", gr)
	}

	client, err := v2client.New(metaAddr, []string{addr})
	if err != nil {
		t.Fatalf("v2 client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	assertCR2PCValues(t, client, ctx, "left", "right", 5*time.Second)
}

// TestV2CrossRange2PCCommitResponseLoss applies the commit on a range and then
// drops the response. The client retries the same commitTS; the server's
// idempotent replay must make the transaction succeed exactly once.
func TestV2CrossRange2PCCommitResponseLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "primary", key: "a"},
		{name: "secondary", key: "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startCR2PCEnv(t)
			inject := &lostCommitResponseInjector{key: tc.key}
			client := env.client(inject.intercept)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{
				CommitRetryTimeout: 8 * time.Second,
				RetryBackoff:       20 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			txn.Set([]byte("a"), []byte("left"))
			txn.Set([]byte("z"), []byte("right"))
			if err := txn.Commit(ctx); err != nil {
				t.Fatalf("Commit after lost %s commit response: %v", tc.name, err)
			}
			assertCR2PCValues(t, client, ctx, "left", "right", 10*time.Second)
		})
	}
}

// TestV2CrossRange2PCPrewriteRollback fails the secondary prewrite after the
// primary range was prewritten and verifies the client rolls the primary range
// back: neither key becomes visible and no range commits.
func TestV2CrossRange2PCPrewriteRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	env := startCR2PCEnv(t)
	fail := &prewriteFailInjector{key: "z"}
	client := env.client(fail.intercept)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	txn, err := client.BeginWithOptions(ctx, v2client.TxnOptions{RetryBackoff: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txn.Set([]byte("a"), []byte("left"))
	txn.Set([]byte("z"), []byte("right"))
	err = txn.Commit(ctx)
	if !errors.Is(err, v2client.ErrConflict) {
		t.Fatalf("Commit error = %v, want ErrConflict", err)
	}
	assertCR2PCAbsent(t, client, ctx)

	st, serr := client.CheckTxnStatus(ctx, []byte("a"), txn.StartTS())
	if serr != nil {
		t.Fatalf("CheckTxnStatus(a): %v", serr)
	}
	if !st.GetRolledBack() || st.GetCommitted() {
		t.Fatalf("primary key status = %+v, want rolled back", st)
	}
}
