package kvserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/storage"
)

// writeCrashRangeMap writes the local range map exactly as a successful
// split/merge would have left it.
func writeCrashRangeMap(t *testing.T, dir string, ranges []RangeConfig) {
	t.Helper()
	data, err := json.Marshal(ranges)
	if err != nil {
		t.Fatalf("marshal range map: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ranges.json"), data, 0o644); err != nil {
		t.Fatalf("write range map: %v", err)
	}
}

func requireNoIntent(t *testing.T, dir string, rangeID uint64) {
	t.Helper()
	if _, err := os.Stat(adminIntentPath(dir, rangeID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin intent for range %d still exists (err=%v)", rangeID, err)
	}
}

// TestV2AdminIntentCrashAfterCopyBeforeMapPublish models a crash after the
// child copy is durable and the parent right half is cut, but before the local
// range map is published. The durable intent must drive the abort path:
// restore the right half from the child copy, discard the scratch child,
// unfreeze, clear the intent, and allow a clean split retry.
func TestV2AdminIntentCrashAfterCopyBeforeMapPublish(t *testing.T) {
	for _, phase := range []string{adminIntentPhaseCopyDone, adminIntentPhaseMapPublishPending} {
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
			if err := parentStore.DeleteRange([]byte("m"), nil); err != nil {
				t.Fatalf("cut parent right half: %v", err)
			}
			writeCrashAdminRecord(t, parentEngine, adminRecord{
				Kind: adminKindSplit, Phase: adminPhaseCut,
				SplitKey: []byte("m"), NewRangeID: 2, Gen: 1,
			})
			if err := writeAdminIntent(dir, adminIntent{
				Op: adminKindSplit, Phase: phase,
				RangeID: 1, RightID: 2,
				SplitKey: []byte("m"), RightStart: []byte("m"),
				Epoch: 1, NewEpoch: 2, Generation: 1,
			}); err != nil {
				t.Fatalf("write admin intent: %v", err)
			}
			if err := engine.Close(); err != nil {
				t.Fatalf("close engine: %v", err)
			}

			restarted := startCrashTestServer(t, cfg)
			waitRangeLeader(t, restarted, 1)
			if groups := restarted.Groups(); len(groups) != 1 || groups[0] != 1 {
				t.Fatalf("groups after recovery = %v, want [1]", groups)
			}
			if restarted.hostedGroup(2) != nil {
				t.Fatal("unpublished child range was left hosted")
			}
			if restarted.hostedGroup(1).isFrozen() {
				t.Fatal("range stayed frozen after intent abort recovery")
			}
			if pending := restarted.PendingIntents(); len(pending) != 0 {
				t.Fatalf("pending intents after abort recovery = %+v", pending)
			}
			requireNoIntent(t, dir, 1)
			client = dialCrashTestClient(t, cfg.RaftAddr)
			freezeGet(t, client, "a", "left")
			freezeGet(t, client, "m", "middle")
			freezeGet(t, client, "z", "right")
			if msg := freezeTryPut(t, client, "z", "after-recovery", 400); msg != "" {
				t.Fatalf("write after abort recovery: %s", msg)
			}
			stopCrashTestServer(t, restarted)

			// The scratch child prefix must not survive the abort.
			engine = openCrashTestEngine(t, dir)
			if left := prefixKeyCount(t, engine, 2); left != 0 {
				t.Fatalf("child prefix has %d stranded keys after intent recovery", left)
			}
			if err := engine.Close(); err != nil {
				t.Fatalf("close engine: %v", err)
			}

			// A clean retry must still work.
			final := startCrashTestServer(t, cfg)
			waitRangeLeader(t, final, 1)
			ranges, err := final.SplitRange(ctx, 1, []byte("m"), 2)
			if err != nil {
				t.Fatalf("split retry after intent recovery: %v", err)
			}
			if len(ranges) != 2 {
				t.Fatalf("ranges after retry = %+v, want 2", ranges)
			}
			waitRangeLeader(t, final, 2)
			if final.hostedGroup(1).isFrozen() || final.hostedGroup(2).isFrozen() {
				t.Fatal("retried split left a range frozen")
			}
			if pending := final.PendingIntents(); len(pending) != 0 {
				t.Fatalf("pending intents after clean retry = %+v", pending)
			}
		})
	}
}

// TestV2AdminIntentCrashAfterMapPublishFinalizes models a crash after the local
// range map was published but before the intent was cleared. Recovery must
// finalize instead of aborting: unfreeze, drop the split copy marker, and clean
// the absorbed right prefix for a merge.
func TestV2AdminIntentCrashAfterMapPublishFinalizes(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		dir := t.TempDir()
		cfg := crashTestConfig(t, dir, []RangeConfig{{ID: 1}})

		srv := startCrashTestServer(t, cfg)
		waitRangeLeader(t, srv, 1)
		client := dialCrashTestClient(t, cfg.RaftAddr)
		freezePut(t, client, "a", "left", 100)
		freezePut(t, client, "m", "middle", 200)
		freezePut(t, client, "z", "right", 300)
		stopCrashTestServer(t, srv)

		// Crash image: right half copied and cut, local map already split, but
		// the intent is still pending and the child copy marker remains.
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
		if err := parentStore.DeleteRange([]byte("m"), nil); err != nil {
			t.Fatalf("cut parent right half: %v", err)
		}
		writeCrashAdminRecord(t, parentEngine, adminRecord{
			Kind: adminKindSplit, Phase: adminPhaseCut,
			SplitKey: []byte("m"), NewRangeID: 2, Gen: 1,
		})
		if err := engine.Close(); err != nil {
			t.Fatalf("close engine: %v", err)
		}
		writeCrashRangeMap(t, dir, []RangeConfig{
			{ID: 1, End: []byte("m"), Epoch: 2},
			{ID: 2, Start: []byte("m"), Epoch: 2},
		})
		if err := writeAdminIntent(dir, adminIntent{
			Op: adminKindSplit, Phase: adminIntentPhaseMapPublishPending,
			RangeID: 1, RightID: 2,
			SplitKey: []byte("m"), RightStart: []byte("m"),
			Epoch: 1, NewEpoch: 2, Generation: 1,
		}); err != nil {
			t.Fatalf("write admin intent: %v", err)
		}

		restarted := startCrashTestServer(t, cfg)
		waitRangeLeader(t, restarted, 1)
		waitRangeLeader(t, restarted, 2)
		if got := restarted.Groups(); len(got) != 2 {
			t.Fatalf("groups after finalize = %v, want 2", got)
		}
		if restarted.hostedGroup(1).isFrozen() || restarted.hostedGroup(2).isFrozen() {
			t.Fatal("finalized split left a range frozen")
		}
		if pending := restarted.PendingIntents(); len(pending) != 0 {
			t.Fatalf("pending intents after finalize = %+v", pending)
		}
		requireNoIntent(t, dir, 1)
		client = dialCrashTestClient(t, cfg.RaftAddr)
		freezeGet(t, client, "a", "left")
		freezeGet(t, client, "m", "middle")
		freezeGet(t, client, "z", "right")
		if msg := freezeTryPut(t, client, "zz", "right-new", 400); msg != "" {
			t.Fatalf("write to finalized child: %s", msg)
		}
		stopCrashTestServer(t, restarted)

		// Repeated recovery must be a no-op for data and freezes.
		repeated := startCrashTestServer(t, cfg)
		waitRangeLeader(t, repeated, 1)
		waitRangeLeader(t, repeated, 2)
		client = dialCrashTestClient(t, cfg.RaftAddr)
		freezeGet(t, client, "zz", "right-new")
		if repeated.hostedGroup(1).isFrozen() || repeated.hostedGroup(2).isFrozen() {
			t.Fatal("repeated recovery left a range frozen")
		}
		stopCrashTestServer(t, repeated)

		engine = openCrashTestEngine(t, dir)
		if copied, err := childCopyComplete(engine, 2); err != nil || copied {
			t.Fatalf("child copy marker after finalize = %v, %v; want false, nil", copied, err)
		}
		if err := engine.Close(); err != nil {
			t.Fatalf("close engine: %v", err)
		}
	})

	t.Run("merge", func(t *testing.T) {
		dir := t.TempDir()
		cfg := crashTestConfig(t, dir, []RangeConfig{
			{ID: 1, End: []byte("m")},
			{ID: 2, Start: []byte("m")},
		})

		srv := startCrashTestServer(t, cfg)
		waitRangeLeader(t, srv, 1)
		waitRangeLeader(t, srv, 2)
		client := dialCrashTestClient(t, cfg.RaftAddr)
		freezePut(t, client, "a", "left", 100)
		freezePut(t, client, "z", "right", 200)
		stopCrashTestServer(t, srv)

		// Crash image: right copied into left, local map already merged, right
		// prefix still present, intent still pending. RightEnd is nil because
		// range 2 is unbounded.
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
		writeCrashAdminRecord(t, leftEngine, adminRecord{
			Kind: adminKindMerge, Phase: adminPhaseCut,
			Left: 1, Right: 2, RightStart: []byte("m"), Gen: 1,
		})
		if err := engine.Close(); err != nil {
			t.Fatalf("close engine: %v", err)
		}
		writeCrashRangeMap(t, dir, []RangeConfig{{ID: 1, Epoch: 2}})
		if err := writeAdminIntent(dir, adminIntent{
			Op: adminKindMerge, Phase: adminIntentPhaseMapPublishPending,
			RangeID: 1, RightID: 2,
			RightStart: []byte("m"),
			Epoch:      1, NewEpoch: 2, Generation: 1,
		}); err != nil {
			t.Fatalf("write admin intent: %v", err)
		}

		restarted := startCrashTestServer(t, cfg)
		waitRangeLeader(t, restarted, 1)
		if restarted.hostedGroup(2) != nil {
			t.Fatal("merged-away range still hosted after finalize")
		}
		if restarted.hostedGroup(1).isFrozen() {
			t.Fatal("finalized merge left the left range frozen")
		}
		if pending := restarted.PendingIntents(); len(pending) != 0 {
			t.Fatalf("pending intents after merge finalize = %+v", pending)
		}
		requireNoIntent(t, dir, 1)
		client = dialCrashTestClient(t, cfg.RaftAddr)
		freezeGet(t, client, "a", "left")
		freezeGet(t, client, "z", "right")
		if msg := freezeTryPut(t, client, "zz", "merged-new", 300); msg != "" {
			t.Fatalf("write to finalized merge: %s", msg)
		}
		stopCrashTestServer(t, restarted)

		repeated := startCrashTestServer(t, cfg)
		waitRangeLeader(t, repeated, 1)
		client = dialCrashTestClient(t, cfg.RaftAddr)
		freezeGet(t, client, "z", "right")
		freezeGet(t, client, "zz", "merged-new")
		if repeated.hostedGroup(1).isFrozen() {
			t.Fatal("repeated merge recovery left the range frozen")
		}
		stopCrashTestServer(t, repeated)

		engine = openCrashTestEngine(t, dir)
		if left := prefixKeyCount(t, engine, 2); left != 0 {
			t.Fatalf("right prefix has %d stranded keys after merge finalize", left)
		}
		if err := engine.Close(); err != nil {
			t.Fatalf("close engine: %v", err)
		}
	})
}

// TestV2AdminIntentMergeAbortBeforeMapPublish is the merge analogue of the
// split abort test: the copy is durable but ranges.json still has both ranges.
// Recovery must remove the copied right half from the left range, clear both
// freezes, clear the intent, and allow a clean merge retry.
func TestV2AdminIntentMergeAbortBeforeMapPublish(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := crashTestConfig(t, dir, []RangeConfig{
		{ID: 1, End: []byte("m")},
		{ID: 2, Start: []byte("m")},
	})

	srv := startCrashTestServer(t, cfg)
	waitRangeLeader(t, srv, 1)
	waitRangeLeader(t, srv, 2)
	client := dialCrashTestClient(t, cfg.RaftAddr)
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "z", "right", 200)
	stopCrashTestServer(t, srv)

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
		Left: 1, Right: 2, RightStart: []byte("m"), RightEnd: nil, Gen: 1,
	}
	writeCrashAdminRecord(t, leftEngine, rec)
	writeCrashAdminRecord(t, rightEngine, rec)
	if err := writeAdminIntent(dir, adminIntent{
		Op: adminKindMerge, Phase: adminIntentPhaseCopyDone,
		RangeID: 1, RightID: 2,
		End: []byte("m"), RightStart: []byte("m"),
		Epoch: 1, NewEpoch: 2, Generation: 1,
	}); err != nil {
		t.Fatalf("write admin intent: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}

	restarted := startCrashTestServer(t, cfg)
	waitRangeLeader(t, restarted, 1)
	waitRangeLeader(t, restarted, 2)
	left, right := restarted.hostedGroup(1), restarted.hostedGroup(2)
	if left == nil || right == nil {
		t.Fatal("merge intent abort removed a range")
	}
	if left.isFrozen() || right.isFrozen() {
		t.Fatal("merge intent abort left a range frozen")
	}
	if pending := restarted.PendingIntents(); len(pending) != 0 {
		t.Fatalf("pending intents after merge abort = %+v", pending)
	}
	requireNoIntent(t, dir, 1)
	if _, err := left.store.Get([]byte("z"), ^uint64(0)); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("left range still holds copied right data: %v", err)
	}
	if value, err := right.store.Get([]byte("z"), ^uint64(0)); err != nil || string(value) != "right" {
		t.Fatalf("right range data = %q, %v; want right", value, err)
	}

	if _, err := restarted.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("merge retry after intent abort: %v", err)
	}
	if restarted.hostedGroup(2) != nil {
		t.Fatal("right range still hosted after merge retry")
	}
	left = restarted.hostedGroup(1)
	if left == nil {
		t.Fatal("left range missing after merge retry")
	}
	if left.isFrozen() {
		t.Fatal("left range frozen after merge retry")
	}
	if value, err := left.store.Get([]byte("z"), ^uint64(0)); err != nil || string(value) != "right" {
		t.Fatalf("merged left data = %q, %v; want right", value, err)
	}
}

// TestV2AdminIntentCorruptFailsClosed verifies that a malformed or unknown
// intent aborts group construction instead of guessing whether to finalize or
// abort a split/merge.
func TestV2AdminIntentCorruptFailsClosed(t *testing.T) {
	cases := map[string]string{
		"malformed":     `{"op":"split"`,
		"unknown_op":    `{"version":1,"op":"swap","phase":"prepared","range_id":1,"right_id":2,"split_key":"bQ=="}`,
		"unknown_phase": `{"version":1,"op":"split","phase":"halfway","range_id":1,"right_id":2,"split_key":"bQ=="}`,
		"future_version": `{"version":99,"op":"split","phase":"prepared","range_id":1,"right_id":2,` +
			`"split_key":"bQ=="}`,
		"missing_split_key": `{"version":1,"op":"split","phase":"copy_done","range_id":1,"right_id":2}`,
		"wrong_range_id":    `{"version":1,"op":"merge","phase":"prepared","range_id":2,"right_id":3}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := crashTestConfig(t, dir, []RangeConfig{{ID: 1}})
			if err := os.WriteFile(adminIntentPath(dir, 1), []byte(content), 0o644); err != nil {
				t.Fatalf("write corrupt intent: %v", err)
			}
			srv, err := New(cfg)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Stop(ctx)
				t.Fatal("New accepted a corrupt admin intent")
			}
			if !strings.Contains(err.Error(), "admin intent") {
				t.Fatalf("New error = %v, want an admin intent error", err)
			}
		})
	}
}

// TestV2AdminIntentVisibleDuringLocalSplit checks the status surface: a split
// publishes its intent before copying and clears it after the local map update.
func TestV2AdminIntentVisibleDuringLocalSplit(t *testing.T) {
	srv, client := newFreezeTestServer(t, []RangeConfig{{ID: 1}})
	ctx := context.Background()
	freezePut(t, client, "a", "left", 100)
	freezePut(t, client, "m", "middle", 200)
	freezePut(t, client, "z", "right", 300)

	g := srv.groups[1]
	entered := make(chan struct{})
	release := make(chan struct{})
	g.splitCopyHook = func(batch int) error {
		if batch == 0 {
			close(entered)
			<-release
		}
		return nil
	}
	defer func() { g.splitCopyHook = nil }()

	done := make(chan error, 1)
	go func() {
		_, err := srv.SplitRange(ctx, 1, []byte("m"), 2)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("split never reached the copy")
	}

	pending := srv.PendingIntents()
	if len(pending) != 1 {
		t.Fatalf("pending intents during split = %+v, want one", pending)
	}
	in := pending[0]
	if in.Op != adminKindSplit || in.RangeID != 1 || in.RightID != 2 {
		t.Fatalf("pending intent = %+v", in)
	}
	if in.Phase != adminIntentPhasePrepared && in.Phase != adminIntentPhaseCopyDone {
		t.Fatalf("pending intent phase = %q", in.Phase)
	}
	if string(in.SplitKey) != "m" || in.Epoch == 0 || in.Generation == 0 {
		t.Fatalf("pending intent bounds/epoch/generation = %+v", in)
	}
	if got := srv.PendingIntentCount(); got != 1 {
		t.Fatalf("PendingIntentCount = %d, want 1", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("split: %v", err)
	}
	if pending := srv.PendingIntents(); len(pending) != 0 {
		t.Fatalf("pending intents after split = %+v", pending)
	}
	requireNoIntent(t, srv.cfg.DataDir, 1)
	if srv.hostedGroup(1).isFrozen() || srv.hostedGroup(2).isFrozen() {
		t.Fatal("split left a range frozen")
	}
}
