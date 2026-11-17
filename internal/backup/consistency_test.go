package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/storage"
	"distributed-db/internal/v2client"
)

// newLocalMVCC opens a real Pebble-backed MVCC store for backup tests that
// exercise the in-process lock-aware scanner.
func newLocalMVCC(t *testing.T) *mvcc.Store {
	t.Helper()
	engine, err := storage.Open(t.TempDir(), storage.Options{
		CacheSize:    8 << 20,
		MemTableSize: 1 << 20,
		Compression:  "none",
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("engine.Close: %v", err)
		}
	})
	return mvcc.New(engine)
}

func storePut(t *testing.T, store *mvcc.Store, key, value string, startTS, commitTS uint64) {
	t.Helper()
	if err := store.Prewrite([]mvcc.Mutation{{Key: []byte(key), Value: []byte(value)}}, []byte(key), startTS); err != nil {
		t.Fatalf("prewrite %q: %v", key, err)
	}
	if err := store.Commit([][]byte{[]byte(key)}, startTS, commitTS); err != nil {
		t.Fatalf("commit %q: %v", key, err)
	}
}

func backupValues(t *testing.T, dir string, m *Manifest) map[string]string {
	t.Helper()
	byID := make(map[uint64]Range, len(m.Ranges))
	for _, r := range m.Ranges {
		byID[r.ID] = r
	}
	out := make(map[string]string, m.TotalKeys)
	for _, c := range m.Chunks {
		ch, err := verifiedChunk(dir, c, byID[c.RangeID])
		if err != nil {
			t.Fatalf("verifiedChunk %d: %v", c.Index, err)
		}
		for _, kv := range ch.rows {
			out[string(kv.Key)] = string(kv.Value)
		}
	}
	return out
}

type flakyScanner struct {
	inner     SnapshotScanner
	failCalls int
	calls     int
}

func (s *flakyScanner) ScanForBackup(ctx context.Context, start, end []byte, ts uint64, limit int) ([]v2client.KV, error) {
	s.calls++
	if s.calls <= s.failCalls {
		return nil, &mvcc.SnapshotLockError{Key: []byte("flaky"), StartTS: 1, SnapshotTS: ts}
	}
	return s.inner.ScanForBackup(ctx, start, end, ts, limit)
}

func rawKVClient(t *testing.T, addr string) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial kv node: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

func rawPrewrite(t *testing.T, client pbv2.KVServiceClient, key, value string, startTS uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Prewrite(ctx, &pbv2.PrewriteRequest{
		StartTs: startTS,
		Primary: []byte(key),
		Mutations: []*pbv2.Mutation{{
			Key: []byte(key), Value: []byte(value),
		}},
	})
	if err != nil {
		t.Fatalf("raw prewrite: %v", err)
	}
	if resp.GetError() != "" || !resp.GetSuccess() {
		t.Fatalf("raw prewrite refused: success=%v error=%q", resp.GetSuccess(), resp.GetError())
	}
}

func rawRollback(t *testing.T, client pbv2.KVServiceClient, key string, startTS uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Rollback(ctx, &pbv2.RollbackRequest{
		StartTs: startTS, Keys: [][]byte{[]byte(key)},
	})
	if err != nil {
		t.Errorf("raw rollback: %v", err)
		return
	}
	if resp.GetError() != "" {
		t.Errorf("raw rollback refused: %q", resp.GetError())
	}
}

func TestBackupStoreScannerAbortsOnLockBelowTSO(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "committed", 10, 20)

	// A lock at startTS 15 is at or below every real TSO the cluster will
	// issue, so the lock-aware scan must abort instead of freezing the old
	// committed value.
	if err := store.Prewrite([]mvcc.Mutation{{Key: []byte("beta"), Value: []byte("pending")}}, []byte("beta"), 15); err != nil {
		t.Fatalf("prewrite lock: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "locked")
	_, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner:           StoreScanner{Store: store},
		LockRetryAttempts: 2, LockRetryBackoff: time.Millisecond,
	})
	if !errors.Is(err, ErrSnapshotLocked) {
		t.Fatalf("Backup error = %v, want ErrSnapshotLocked", err)
	}
	if !errors.Is(err, mvcc.ErrLocked) {
		t.Fatalf("Backup error = %v, want errors.Is(_, mvcc.ErrLocked)", err)
	}
	var lockErr *SnapshotLockedError
	if !errors.As(err, &lockErr) {
		t.Fatalf("Backup error = %v, want *SnapshotLockedError", err)
	}
	if string(lockErr.Key) != "beta" || lockErr.LockTS != 15 || lockErr.Attempts != 2 {
		t.Fatalf("lock error = %+v, want key beta lock_ts 15 attempts 2", lockErr)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("aborted backup published %s (stat error %v)", dir, statErr)
	}
}

func TestBackupRetriesLockErrorThenSucceeds(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	store := newLocalMVCC(t)
	storePut(t, store, "alpha", "one", 10, 20)
	storePut(t, store, "beta", "two", 30, 40)

	scanner := &flakyScanner{inner: StoreScanner{Store: store}, failCalls: 1}
	parent := t.TempDir()
	dir := filepath.Join(parent, "retry")
	m, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		Scanner: scanner, LockRetryAttempts: 3, LockRetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Backup after retry: %v", err)
	}
	if scanner.calls < 2 {
		t.Fatalf("scanner calls = %d, want at least 2 (one lock failure + retry)", scanner.calls)
	}
	if m.TotalKeys != 2 {
		t.Fatalf("manifest total_keys = %d, want 2", m.TotalKeys)
	}
	values := backupValues(t, dir, m)
	if values["alpha"] != "one" || values["beta"] != "two" {
		t.Fatalf("backup values = %v", values)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			t.Fatalf("retry left staging directory %q behind", entry.Name())
		}
	}
}

func TestBackupDefaultPathAbortsOnLockOnCommittedKey(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	putKV(t, cluster.client, "alpha", "old")

	startTS, err := cluster.client.AllocTS(ctx)
	if err != nil {
		t.Fatalf("AllocTS: %v", err)
	}
	raw := rawKVClient(t, cluster.nodeAddr)
	rawPrewrite(t, raw, "alpha", "new", startTS)
	defer rawRollback(t, raw, "alpha", startTS)

	dir := filepath.Join(t.TempDir(), "default-locked")
	_, err = Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		LockRetryAttempts: 2, LockRetryBackoff: time.Millisecond,
	})
	if !errors.Is(err, ErrSnapshotLocked) {
		t.Fatalf("Backup error = %v, want ErrSnapshotLocked", err)
	}
	var lockErr *SnapshotLockedError
	if !errors.As(err, &lockErr) {
		t.Fatalf("Backup error = %v, want *SnapshotLockedError", err)
	}
	if string(lockErr.Key) != "alpha" || lockErr.LockTS != startTS {
		t.Fatalf("lock error = %+v, want key alpha lock_ts %d", lockErr, startTS)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("aborted backup published %s (stat error %v)", dir, statErr)
	}
}

// TestBackupConcurrentCommitEitherIncludedOrFails races a commit against a
// backup. A successful backup must contain the acknowledged value; the only
// other allowed outcome is a clear ErrSnapshotLocked abort.
func TestBackupConcurrentCommitEitherIncludedOrFails(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)

	const iterations = 20
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iter-%02d", i), func(t *testing.T) {
			store := newLocalMVCC(t)
			if err := store.Prewrite([]mvcc.Mutation{{Key: []byte("race"), Value: []byte("v")}}, []byte("race"), 10); err != nil {
				t.Fatalf("prewrite: %v", err)
			}
			commitErr := make(chan error, 1)
			go func() {
				commitErr <- store.Commit([][]byte{[]byte("race")}, 10, 50)
			}()

			dir := filepath.Join(t.TempDir(), "race")
			m, err := Backup(ctx, BackupConfig{
				Client: cluster.client, Meta: cluster.meta, OutDir: dir,
				Scanner:           StoreScanner{Store: store},
				LockRetryAttempts: 2, LockRetryBackoff: time.Millisecond,
			})
			if cerr := <-commitErr; cerr != nil {
				t.Fatalf("commit: %v", cerr)
			}
			if err != nil {
				if !errors.Is(err, ErrSnapshotLocked) {
					t.Fatalf("Backup error = %v, want ErrSnapshotLocked", err)
				}
				if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed backup published %s", dir)
				}
				return
			}
			values := backupValues(t, dir, m)
			if values["race"] != "v" {
				t.Fatalf("acknowledged commit is missing from successful backup: %v", values)
			}
		})
	}
}

func TestBackupRequireLockAwareScanRejectsFallback(t *testing.T) {
	ctx := context.Background()
	cluster := startTestCluster(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "strict")
	_, err := Backup(ctx, BackupConfig{
		Client: cluster.client, Meta: cluster.meta, OutDir: dir,
		RequireLockAwareScan: true,
	})
	if err == nil {
		t.Fatal("Backup accepted a non-lock-aware scan with RequireLockAwareScan")
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected backup created %s", dir)
	}
}

func TestRestoreRejectsCorruptSecondChunkWithoutPartialWrite(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	putKV(t, src.client, "beta", "two")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{
		Client: src.client, Meta: src.meta, OutDir: dir, ChunkKeys: 1,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(m.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(m.Chunks))
	}
	path, err := ChunkPath(dir, m.Chunks[1])
	if err != nil {
		t.Fatalf("chunk path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write corrupt chunk: %v", err)
	}

	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore error = %v, want ErrCorrupt", err)
	}
	// The first chunk was valid. Restore must still have written nothing,
	// because every chunk is verified before the first write.
	for _, key := range []string{"alpha", "beta"} {
		if _, err := getKV(t, dst.client, key); !errors.Is(err, v2client.ErrNotFound) {
			t.Fatalf("restore of corrupt backup wrote %q: %v", key, err)
		}
	}
}

func TestRestoreVerifyHookRunsBeforeWrites(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dst := startTestCluster(t, nil, nil)
	hookErr := errors.New("operator rejected this restore")
	called := false
	_, err := Restore(ctx, RestoreConfig{
		Client: dst.client, Meta: dst.meta, InDir: dir,
		VerifyHook: func(_ context.Context, report *VerifyReport) error {
			called = true
			if report.Keys != 1 || report.Chunks != 1 || report.SnapshotTS == 0 {
				return fmt.Errorf("unexpected report: %+v", report)
			}
			return hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("Restore error = %v, want hook error", err)
	}
	if !called {
		t.Fatal("VerifyHook was not called")
	}
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("hook-rejected restore wrote alpha: %v", err)
	}
}

func TestRestoreRequireEmptyRejectsTarget(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dst := startTestCluster(t, nil, nil)
	putKV(t, dst.client, "existing", "data")
	if _, err := Restore(ctx, RestoreConfig{
		Client: dst.client, Meta: dst.meta, InDir: dir, RequireEmpty: true,
	}); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("Restore error = %v, want ErrTargetNotEmpty", err)
	}
	requireKV(t, dst.client, "existing", "data")
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("rejected restore wrote alpha: %v", err)
	}
}

func TestRestoreRejectsManifestChecksumBeforeWrite(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	m.Chunks[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := writeManifestFile(dir, m); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}

	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore error = %v, want ErrCorrupt", err)
	}
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("corrupt-manifest restore wrote alpha: %v", err)
	}
}
