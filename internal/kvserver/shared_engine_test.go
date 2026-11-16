package kvserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestV2RangeHostSplitMergeSharedEngine verifies the shared-engine layout: two
// ranges live in one Pebble DB with disjoint prefixes, their data is isolated,
// and both ranges (and their data) survive a restart.
func TestV2RangeHostSplitMergeSharedEngine(t *testing.T) {
	ctx := context.Background()
	addr := testFreeAddr(t)
	dir := t.TempDir()
	cfg := Config{
		ID:       addr,
		RaftAddr: addr,
		DataDir:  dir,
		Ranges: []RangeConfig{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: nil},
		},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if srv.engine == nil {
		t.Fatal("Server did not open a shared engine")
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	stop := func(s *Server) error {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.Stop(sctx)
	}
	t.Cleanup(func() {
		// The restarted server is registered first below; this only stops the
		// original if the test fails before the restart.
		_ = stop(srv)
	})

	// The shared layout must not create per-range Pebble databases.
	for _, id := range []uint64{1, 2} {
		old := filepath.Join(dir, "db", fmt.Sprintf("range-%d", id))
		if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("per-range DB %s exists (err=%v), want none", old, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sharedEngineDir(dir), "CURRENT")); err != nil {
		t.Fatalf("shared engine CURRENT: %v", err)
	}
	if srv.groups[1].engine == srv.groups[2].engine {
		t.Fatal("ranges share the same wrapper Engine value, want distinct prefix wrappers")
	}
	if srv.groups[1].ownsEngine || srv.groups[2].ownsEngine {
		t.Fatal("range groups claim ownership of the shared engine")
	}

	waitRangeLeader(t, srv, 1)
	waitRangeLeader(t, srv, 2)

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := pbv2.NewKVServiceClient(cc)
	putStatsKV(t, client, "alpha", "one", 100)
	putStatsKV(t, client, "zulu", "two", 200)
	if err := cc.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	// Each range sees only its own prefix even though the same engine backs
	// both, and Stats are range-local.
	if _, err := srv.groups[1].store.Get([]byte("alpha"), 1<<62); err != nil {
		t.Fatalf("range 1 get alpha: %v", err)
	}
	if _, err := srv.groups[2].store.Get([]byte("zulu"), 1<<62); err != nil {
		t.Fatalf("range 2 get zulu: %v", err)
	}
	if _, err := srv.groups[1].store.Get([]byte("zulu"), 1<<62); err == nil {
		t.Fatal("range 1 can see range 2 key zulu")
	}
	if _, err := srv.groups[2].store.Get([]byte("alpha"), 1<<62); err == nil {
		t.Fatal("range 2 can see range 1 key alpha")
	}
	st1 := srv.groups[1].engine.Stats()
	st2 := srv.groups[2].engine.Stats()
	if st1.DiskSize <= 0 || st2.DiskSize <= 0 {
		t.Fatalf("range stats = %+v / %+v, want both positive", st1, st2)
	}
	if st1.MemTableSize != 0 || st2.MemTableSize != 0 {
		t.Fatalf("range MemTableSize = %d/%d, want 0", st1.MemTableSize, st2.MemTableSize)
	}

	// Raw keys in the shared engine are physically separated by range prefix.
	raw, err := srv.engine.NewIter(rangePrefix(1), rangePrefix(2))
	if err != nil {
		t.Fatalf("raw iterator: %v", err)
	}
	if !raw.Valid() {
		t.Fatal("no raw keys under range 1 prefix")
	}
	for ; raw.Valid(); raw.Next() {
		if !bytes.HasPrefix(raw.Key(), rangePrefix(1)) {
			t.Fatalf("raw key %x outside range 1 prefix", raw.Key())
		}
	}
	if err := raw.Error(); err != nil {
		t.Fatalf("raw iterator: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw iterator close: %v", err)
	}

	// A clean stop closes the shared engine and all groups.
	shared := srv.engine
	if err := stop(srv); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if srv.engine != nil {
		t.Fatal("Server still references the shared engine after Stop")
	}
	if _, err := shared.Get([]byte("anything")); err == nil {
		t.Fatal("shared engine is still usable after Stop")
	}

	// Restart with the original static config. The shared DB path and both
	// per-range Raft logs are reused.
	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("restart new: %v", err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("restart start: %v", err)
	}
	t.Cleanup(func() { _ = stop(restarted) })
	if got := restarted.Groups(); !equalIDs(got, []uint64{1, 2}) {
		t.Fatalf("restored groups = %v, want [1 2]", got)
	}
	waitRangeLeader(t, restarted, 1)
	waitRangeLeader(t, restarted, 2)

	cc2, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	t.Cleanup(func() { _ = cc2.Close() })
	client2 := pbv2.NewKVServiceClient(cc2)
	get := func(key, want string) {
		t.Helper()
		resp, err := client2.Get(ctx, &pbv2.GetRequest{Key: []byte(key), Ts: 1 << 62})
		if err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
		if !resp.GetFound() || string(resp.GetValue()) != want {
			t.Fatalf("get %q = %q, found=%v, err=%q; want %q",
				key, resp.GetValue(), resp.GetFound(), resp.GetError(), want)
		}
	}
	get("alpha", "one")
	get("zulu", "two")
}
