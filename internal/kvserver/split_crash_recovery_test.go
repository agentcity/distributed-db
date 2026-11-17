package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	"distributed-db/internal/storage"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startCrashTestServer starts a server whose DataDir and range set are owned
// by the test (so it can be stopped, mutated on disk and restarted).
func startCrashTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv
}

func crashTestConfig(t *testing.T, dir string, ranges []RangeConfig) Config {
	t.Helper()
	addr := testFreeAddr(t)
	return Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            dir,
		Ranges:             ranges,
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	}
}

func stopCrashTestServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("stop server: %v", err)
	}
}

func dialCrashTestClient(t *testing.T, addr string) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

func openCrashTestEngine(t *testing.T, dir string) storage.Engine {
	t.Helper()
	engine, err := storage.Open(sharedEngineDir(dir), storage.Options{SyncWrites: true})
	if err != nil {
		t.Fatalf("open shared engine: %v", err)
	}
	return engine
}

func writeCrashAdminRecord(t *testing.T, engine storage.Engine, rec adminRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal admin record: %v", err)
	}
	if err := engine.Set(rangeAdminKey, data); err != nil {
		t.Fatalf("write admin record: %v", err)
	}
}

func prefixKeyCount(t *testing.T, engine storage.Engine, id uint64) int {
	t.Helper()
	it, err := storage.Prefix(engine, rangePrefix(id)).NewIter(nil, nil)
	if err != nil {
		t.Fatalf("iterate prefix %d: %v", id, err)
	}
	defer it.Close()
	count := 0
	for it.Valid() {
		count++
		it.Next()
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterate prefix %d: %v", id, err)
	}
	return count
}

// isFrozenLocked is a test helper: writeTicket already takes the read lock.
func (m *stateMachine) isFrozenLocked() bool {
	_, frozen := m.writeTicket()
	return frozen
}

// TestV2SplitCrashBetweenCopyAndFinishRecovers models a process death after the
// child copy is durable and the parent right half was cut, but before the range
// map was published. Restart must abort the split: restore the right half,
// discard the child prefix, leave the range writable, and allow a clean retry.
func TestV2SplitCrashBetweenCopyAndFinishRecovers(t *testing.T) {
	for _, phase := range []string{adminPhasePrepared, adminPhaseCut} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			cfg := crashTestConfig(t, dir, []RangeConfig{{ID: 1}})

			srv := startCrashTestServer(t, cfg)
			waitRangeLeader(t, srv, 1)
			client := dialCrashTestClient(t, cfg.RaftAddr)
			freezePut(t, client, "a", "left", 100)
			freezePut(t, client, "m", "middle", 200)
			freezePut(t, client, "z", "right", 300)
			stopCrashTestServer(t, srv)

			// Reconstruct the on-disk crash image: the child has a complete
			// copy (plus the durable marker) and the parent has cut the right
			// half; the parent's range end and ranges.json were never updated.
			engine := openCrashTestEngine(t, dir)
			parentEngine := storage.Prefix(engine, rangePrefix(1))
			childEngine := storage.Prefix(engine, rangePrefix(2))
			parentStore := mvcc.New(parentEngine)
			childStore := mvcc.New(childEngine)
			if err := parentStore.StreamRange([]byte("m"), nil, 1024, func(records []mvcc.Record) error {
				return childStore.ImportRecords(records)
			}); err != nil {
				t.Fatalf("seed child copy: %v", err)
			}
			if err := childEngine.Set(rangeSplitCopyKey, splitCopyMarker); err != nil {
				t.Fatalf("mark child copy: %v", err)
			}
			if phase == adminPhaseCut {
				if err := parentStore.DeleteRange([]byte("m"), nil); err != nil {
					t.Fatalf("cut parent right half: %v", err)
				}
			}
			writeCrashAdminRecord(t, parentEngine, adminRecord{
				Kind:       adminKindSplit,
				Phase:      phase,
				SplitKey:   []byte("m"),
				NewRangeID: 2,
				Gen:        1,
			})
			if err := engine.Close(); err != nil {
				t.Fatalf("close engine: %v", err)
			}

			// Restart: recovery must abort before serving.
			restarted := startCrashTestServer(t, cfg)
			waitRangeLeader(t, restarted, 1)
			if groups := restarted.Groups(); len(groups) != 1 || groups[0] != 1 {
				t.Fatalf("groups after recovery = %v, want [1]", groups)
			}
			if restarted.hostedGroup(2) != nil {
				t.Fatal("unpublished child range was left hosted")
			}
			g := restarted.hostedGroup(1)
			if g.isFrozen() {
				t.Fatal("range stayed frozen after abort recovery")
			}
			client = dialCrashTestClient(t, cfg.RaftAddr)
			freezeGet(t, client, "a", "left")
			freezeGet(t, client, "m", "middle")
			freezeGet(t, client, "z", "right")
			if msg := freezeTryPut(t, client, "z", "after-recovery", 400); msg != "" {
				t.Fatalf("write after abort recovery: %s", msg)
			}

			// The child prefix must not be left behind as stranded data.
			stopCrashTestServer(t, restarted)
			engine = openCrashTestEngine(t, dir)
			if left := prefixKeyCount(t, engine, 2); left != 0 {
				t.Fatalf("child prefix has %d stranded keys after recovery", left)
			}
			if err := engine.Close(); err != nil {
				t.Fatalf("close engine: %v", err)
			}

			// The same operation must now retry cleanly.
			final := startCrashTestServer(t, cfg)
			waitRangeLeader(t, final, 1)
			ranges, err := final.SplitRange(ctx, 1, []byte("m"), 2)
			if err != nil {
				t.Fatalf("split retry after recovery: %v", err)
			}
			if len(ranges) != 2 {
				t.Fatalf("ranges after retry = %+v, want 2", ranges)
			}
			waitRangeLeader(t, final, 2)
			client = dialCrashTestClient(t, cfg.RaftAddr)
			freezeGet(t, client, "a", "left")
			freezeGet(t, client, "m", "middle")
			freezeGet(t, client, "z", "after-recovery")
			if final.hostedGroup(1).isFrozen() || final.hostedGroup(2).isFrozen() {
				t.Fatal("retried split left a range frozen")
			}
		})
	}
}

// TestV2MergeCrashBeforeMapUpdateRecovers models a process death after the
// right range was stopped and its data copied into the left range, but before
// the wider left range map was published. Restart must abort the merge: remove
// the copied records from the left prefix, keep the right range serving its
// data, and allow a clean retry.
func TestV2MergeCrashBeforeMapUpdateRecovers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := crashTestConfig(t, dir, []RangeConfig{
		{ID: 1, Start: nil, End: []byte("m")},
		{ID: 2, Start: []byte("m"), End: nil},
	})

	srv := startCrashTestServer(t, cfg)
	waitRangeLeader(t, srv, 1)
	waitRangeLeader(t, srv, 2)
	client := dialCrashTestClient(t, cfg.RaftAddr)
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "z", "right", 200)
	stopCrashTestServer(t, srv)

	// Reconstruct the crash image: right copied into left, both ranges marked
	// merge-prepared, but the left end still stops at "m".
	engine := openCrashTestEngine(t, dir)
	leftEngine := storage.Prefix(engine, rangePrefix(1))
	rightEngine := storage.Prefix(engine, rangePrefix(2))
	leftStore := mvcc.New(leftEngine)
	rightStore := mvcc.New(rightEngine)
	if err := rightStore.StreamRange(nil, nil, 1024, func(records []mvcc.Record) error {
		return leftStore.ImportRecords(records)
	}); err != nil {
		t.Fatalf("seed left copy: %v", err)
	}
	rec := adminRecord{
		Kind: adminKindMerge, Phase: adminPhaseCut,
		Left: 1, Right: 2, RightStart: []byte("m"), Gen: 1,
	}
	writeCrashAdminRecord(t, leftEngine, rec)
	writeCrashAdminRecord(t, rightEngine, rec)
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}

	restarted := startCrashTestServer(t, cfg)
	waitRangeLeader(t, restarted, 1)
	waitRangeLeader(t, restarted, 2)
	left, right := restarted.hostedGroup(1), restarted.hostedGroup(2)
	if left == nil || right == nil {
		t.Fatal("merge abort recovery removed a range")
	}
	if left.isFrozen() || right.isFrozen() {
		t.Fatal("merge abort recovery left a range frozen")
	}
	if _, err := left.store.Get([]byte("z"), ^uint64(0)); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("left range still holds copied right data after aborted merge: %v", err)
	}
	if value, err := right.store.Get([]byte("z"), ^uint64(0)); err != nil || string(value) != "right" {
		t.Fatalf("right range data = %q, %v; want right", value, err)
	}
	client = dialCrashTestClient(t, cfg.RaftAddr)
	if msg := freezeTryPut(t, client, "z", "right-after-recovery", 300); msg != "" {
		t.Fatalf("write to right range after recovery: %s", msg)
	}

	if _, err := restarted.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("merge retry after recovery: %v", err)
	}
	if restarted.hostedGroup(2) != nil {
		t.Fatal("right range still hosted after merge retry")
	}
	left = restarted.hostedGroup(1)
	if left.isFrozen() {
		t.Fatal("left range frozen after merge retry")
	}
	if value, err := left.store.Get([]byte("z"), ^uint64(0)); err != nil || string(value) != "right-after-recovery" {
		t.Fatalf("merged left data = %q, %v; want right-after-recovery", value, err)
	}
}

// TestV2SplitFinishClearReplayIdempotent applies duplicate, repeated and
// out-of-order finish/clear entries. They must be idempotent, must not move the
// freeze generation, and must leave the range writable.
func TestV2SplitFinishClearReplayIdempotent(t *testing.T) {
	sm, _ := newStreamTestSM(t, openStreamTestEngine(t), 1)
	apply := func(index uint64, op adminOp) error {
		payload, err := encodeAdminOp(op)
		if err != nil {
			t.Fatalf("encode admin op: %v", err)
		}
		return sm.Apply(model.Entry{Type: model.CommandSplitPrepare, Index: index, Value: payload})
	}
	prepared := adminOp{SplitKey: []byte("m"), NewRangeID: 2}
	finished := adminOp{Op: adminOpFinish, SplitKey: []byte("m"), NewRangeID: 2}
	cleared := adminOp{Op: adminOpClear, SplitKey: []byte("m"), NewRangeID: 2}

	if err := apply(10, prepared); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !sm.isFrozenLocked() || sm.freezeGen != 1 {
		t.Fatalf("after prepare: frozen=%v gen=%d, want true/1", sm.isFrozenLocked(), sm.freezeGen)
	}
	if err := apply(11, finished); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if sm.isFrozenLocked() {
		t.Fatal("finish left the range frozen")
	}
	if sm.freezeGen != 1 {
		t.Fatalf("finish changed generation to %d", sm.freezeGen)
	}
	// Duplicate finish at the same index and at a higher index.
	if err := apply(11, finished); err != nil {
		t.Fatalf("duplicate finish: %v", err)
	}
	if err := apply(12, finished); err != nil {
		t.Fatalf("repeated finish: %v", err)
	}
	// Out-of-order older finish: must not regress the admin index or gen.
	if err := apply(9, finished); err != nil {
		t.Fatalf("out-of-order finish: %v", err)
	}
	if sm.freezeGen != 1 || sm.adminIndex != 12 {
		t.Fatalf("after duplicate finishes: gen=%d index=%d, want 1/12", sm.freezeGen, sm.adminIndex)
	}
	// Clear and its duplicates.
	if err := apply(20, cleared); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if sm.isFrozenLocked() || sm.freezeGen != 1 {
		t.Fatalf("after clear: frozen=%v gen=%d, want false/1", sm.isFrozenLocked(), sm.freezeGen)
	}
	if err := apply(20, cleared); err != nil {
		t.Fatalf("duplicate clear: %v", err)
	}
	if err := apply(15, cleared); err != nil {
		t.Fatalf("out-of-order clear: %v", err)
	}
	if sm.freezeGen != 1 || sm.adminIndex != 20 {
		t.Fatalf("after duplicate clears: gen=%d index=%d, want 1/20", sm.freezeGen, sm.adminIndex)
	}
	// A new prepare after the abort is a new barrier and bumps once.
	if err := apply(30, prepared); err != nil {
		t.Fatalf("new prepare: %v", err)
	}
	if !sm.isFrozenLocked() || sm.freezeGen != 2 {
		t.Fatalf("new prepare: frozen=%v gen=%d, want true/2", sm.isFrozenLocked(), sm.freezeGen)
	}
	if err := apply(30, prepared); err != nil {
		t.Fatalf("duplicate new prepare: %v", err)
	}
	if sm.freezeGen != 2 {
		t.Fatalf("duplicate new prepare bumped generation to %d", sm.freezeGen)
	}
}

// TestV2SplitGenerationSurvivesRestart replays a log on a fresh state machine
// built over the same durable prefix. Writes that committed before the barrier
// must still apply, writes proposed before the barrier but appended after it
// must be rejected, and the post-barrier generation must be visible to new
// write tickets.
func TestV2SplitGenerationSurvivesRestart(t *testing.T) {
	shared := openStreamTestEngine(t)
	sm, engine := newStreamTestSM(t, shared, 1)
	apply := func(m *stateMachine, index uint64, typ model.CommandType, payload []byte) error {
		return m.Apply(model.Entry{Type: typ, Index: index, Value: payload})
	}
	prepPayload, err := encodeAdminOp(adminOp{SplitKey: []byte("m"), NewRangeID: 2})
	if err != nil {
		t.Fatal(err)
	}
	finishPayload, err := encodeAdminOp(adminOp{Op: adminOpFinish, SplitKey: []byte("m"), NewRangeID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(sm, 10, model.CommandSplitPrepare, prepPayload); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := apply(sm, 11, model.CommandSplitPrepare, finishPayload); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// Simulated restart: a new state machine over the same durable engine.
	restarted := &stateMachine{store: mvcc.New(engine), engine: engine}
	if err := restarted.loadAdminState(); err != nil {
		t.Fatalf("load admin state: %v", err)
	}
	writePayload := func(key string, gen uint64) []byte {
		payload, encErr := encodeCommand(applyCommand{
			StartTS: 100, CommitTS: 101, Primary: []byte(key), Gen: gen,
			Mutations: []mvcc.Mutation{{Key: []byte(key), Value: []byte("v")}},
		})
		if encErr != nil {
			t.Fatalf("encode write: %v", encErr)
		}
		return payload
	}
	// A write that committed before the barrier (index 5, generation 0) is
	// replayed before the prepare and must still be accepted.
	if err := apply(restarted, 5, model.CommandPrewriteCommit, writePayload("before", 0)); err != nil {
		t.Fatalf("pre-barrier write after restart: %v", err)
	}
	if value, err := restarted.store.Get([]byte("before"), 101); err != nil || string(value) != "v" {
		t.Fatalf("pre-barrier write = %q, %v; want v", value, err)
	}
	// Replay the prepare and finish, then a stale generation-0 write.
	if err := apply(restarted, 10, model.CommandSplitPrepare, prepPayload); err != nil {
		t.Fatalf("replayed prepare: %v", err)
	}
	if err := apply(restarted, 11, model.CommandSplitPrepare, finishPayload); err != nil {
		t.Fatalf("replayed finish: %v", err)
	}
	if err := apply(restarted, 12, model.CommandPrewriteCommit, writePayload("stale", 0)); !errors.Is(err, ErrRangeFrozen) {
		t.Fatalf("stale write after restart = %v, want ErrRangeFrozen", err)
	}
	if _, err := restarted.store.Get([]byte("stale"), 101); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatal("stale write mutated the store after restart")
	}
	gen, frozen := restarted.writeTicket()
	if frozen || gen != 1 {
		t.Fatalf("post-restart ticket: gen=%d frozen=%v, want 1/false", gen, frozen)
	}
	if err := apply(restarted, 13, model.CommandPrewriteCommit, writePayload("fresh", gen)); err != nil {
		t.Fatalf("fresh write after restart: %v", err)
	}
}

// TestV2SplitConcurrentAdminSerialized starts a second split while the first is
// stalled inside its local copy. The admin mutex must serialise them: the first
// completes, the second re-checks the now-split range and is rejected without
// freezing anything or corrupting data.
func TestV2SplitConcurrentAdminSerialized(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{{ID: 1}})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "m", "middle", 200)
	freezePut(t, client, "z", "right", 300)

	g := srv.groups[1]
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	g.splitCopyHook = func(batch int) error {
		if batch == 0 {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	defer func() { g.splitCopyHook = nil }()

	firstDone := make(chan error, 1)
	go func() {
		_, err := srv.SplitRange(ctx, 1, []byte("m"), 2)
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first split never reached the copy")
	}

	secondDone := make(chan error, 1)
	go func() {
		// "z" is in the right half, so after the first split completes it is
		// no longer inside range 1 and the second call must be rejected.
		_, err := srv.SplitRange(ctx, 1, []byte("z"), 3)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second admin op completed before the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first split: %v", err)
	}
	if err := <-secondDone; err == nil {
		t.Fatal("concurrent split of the same range succeeded")
	} else if !strings.Contains(err.Error(), "not inside") {
		t.Fatalf("concurrent split error = %v, want not-inside rejection", err)
	}
	if srv.hostedGroup(2) == nil {
		t.Fatal("first split did not publish range 2")
	}
	waitRangeLeader(t, srv, 2)
	if srv.hostedGroup(3) != nil {
		t.Fatal("rejected split published range 3")
	}
	if srv.groups[1].isFrozen() || srv.hostedGroup(2).isFrozen() {
		t.Fatal("serialised split left a range frozen")
	}
	freezeGet(t, client, "a", "left")
	freezeGet(t, client, "m", "middle")
	freezeGet(t, client, "z", "right")
}
