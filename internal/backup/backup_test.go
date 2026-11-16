package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

type testCluster struct {
	metaAddr string
	metaSrv  *metaserver.Server
	nodeAddr string
	nodeSrv  *kvserver.Server
	client   *v2client.Client
	meta     *MetaClient
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	return addr
}

// startTestCluster starts one standalone meta server and one KV node hosting
// the supplied ranges (a single default range when ranges is nil). The node
// address is substituted into empty meta replica lists so backups capture a
// realistic range descriptor.
func startTestCluster(t *testing.T, ranges []kvserver.RangeConfig, metaRanges []meta.Range) *testCluster {
	t.Helper()
	nodeAddr := freeAddr(t)
	metaAddr := freeAddr(t)
	for i := range metaRanges {
		if len(metaRanges[i].Replicas) == 0 {
			metaRanges[i].Replicas = []string{nodeAddr}
		}
	}
	ms, err := metaserver.New(metaserver.Config{Addr: metaAddr, Ranges: metaRanges, Logger: testLogger()})
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}
	if err := ms.Start(context.Background()); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	srv, err := kvserver.New(kvserver.Config{
		ID: nodeAddr, RaftAddr: nodeAddr, DataDir: t.TempDir(), Ranges: ranges,
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
	})
	if err != nil {
		_ = ms.Stop()
		t.Fatalf("new kv node: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = ms.Stop()
		t.Fatalf("start kv node: %v", err)
	}
	client, err := v2client.New(metaAddr, []string{nodeAddr})
	if err != nil {
		_ = srv.Stop(context.Background())
		_ = ms.Stop()
		t.Fatalf("new v2 client: %v", err)
	}
	mc, err := OpenMeta([]string{metaAddr})
	if err != nil {
		_ = client.Close()
		_ = srv.Stop(context.Background())
		_ = ms.Stop()
		t.Fatalf("new backup meta client: %v", err)
	}
	c := &testCluster{
		metaAddr: metaAddr, metaSrv: ms, nodeAddr: nodeAddr,
		nodeSrv: srv, client: client, meta: mc,
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = mc.Close()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(stopCtx)
		_ = ms.Stop()
	})
	ids := []uint64{1}
	if len(ranges) > 0 {
		ids = ids[:0]
		for _, r := range ranges {
			ids = append(ids, r.ID)
		}
	}
	waitRangeLeaders(t, nodeAddr, ids)
	return c
}

func waitRangeLeaders(t *testing.T, addr string, ids []uint64) {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial node: %v", err)
	}
	defer cc.Close()
	client := pbv2.NewKVServiceClient(cc)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			resp, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: id})
			cancel()
			if err != nil || !strings.EqualFold(resp.GetRole(), "leader") {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("range(s) %v did not elect a leader", ids)
}

func putKV(t *testing.T, client *v2client.Client, key, value string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		txn, err := client.Begin(ctx)
		if err != nil {
			lastErr = err
			time.Sleep(25 * time.Millisecond)
			continue
		}
		txn.Set([]byte(key), []byte(value))
		if err := txn.Commit(ctx); err == nil {
			return
		} else {
			lastErr = err
			_ = txn.Rollback(ctx)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("put %q: %v", key, lastErr)
}

func getKV(t *testing.T, client *v2client.Client, key string) ([]byte, error) {
	t.Helper()
	ctx := context.Background()
	txn, err := client.Begin(ctx)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer func() { _ = txn.Rollback(ctx) }()
	return txn.Get(ctx, []byte(key))
}

func requireKV(t *testing.T, client *v2client.Client, key, want string) {
	t.Helper()
	got, err := getKV(t, client, key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if string(got) != want {
		t.Fatalf("get %q = %q, want %q", key, got, want)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	pairs := []struct{ key, value string }{
		{"key-00", strings.Repeat("a", 80)},
		{"key-01", strings.Repeat("b", 80)},
		{"key-02", strings.Repeat("c", 80)},
		{"key-03", strings.Repeat("d", 80)},
		{"key-04", strings.Repeat("e", 80)},
	}
	for _, p := range pairs {
		putKV(t, src.client, p.key, p.value)
	}

	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{
		Client: src.client, Meta: src.meta, OutDir: dir,
		ChunkKeys: 2, ChunkBytes: 64, // byte bound wins, one key per chunk
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if m.SnapshotTS == 0 {
		t.Fatal("manifest snapshot_ts is zero")
	}
	if len(m.Ranges) != 1 {
		t.Fatalf("manifest ranges = %d, want 1", len(m.Ranges))
	}
	if len(m.Chunks) != len(pairs) {
		t.Fatalf("manifest chunks = %d, want %d (one per key)", len(m.Chunks), len(pairs))
	}
	if m.TotalKeys != int64(len(pairs)) {
		t.Fatalf("manifest total_keys = %d, want %d", m.TotalKeys, len(pairs))
	}
	for _, c := range m.Chunks {
		if c.RangeID != 1 || c.SHA256 == "" || c.SizeBytes <= 0 || c.PayloadBytes <= 0 {
			t.Fatalf("bad chunk descriptor: %+v", c)
		}
	}

	rep, err := Verify(ctx, dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Keys != int64(len(pairs)) || rep.Chunks != len(pairs) || rep.SnapshotTS != m.SnapshotTS {
		t.Fatalf("verify report = %+v", rep)
	}
	loaded, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if loaded.TotalKeys != m.TotalKeys || len(loaded.Ranges) != 1 {
		t.Fatalf("loaded manifest differs: %+v", loaded)
	}

	dst := startTestCluster(t, nil, nil)
	rr, err := Restore(ctx, RestoreConfig{
		Client: dst.client, Meta: dst.meta, InDir: dir, Check: true,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !rr.Checked || rr.Keys != int64(len(pairs)) || rr.Chunks != len(pairs) {
		t.Fatalf("restore report = %+v", rr)
	}
	for _, p := range pairs {
		requireKV(t, dst.client, p.key, p.value)
	}
	// A second restore must be harmless.
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	for _, p := range pairs {
		requireKV(t, dst.client, p.key, p.value)
	}
}

func TestBackupEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "empty")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(m.Chunks) != 0 || m.TotalKeys != 0 || m.TotalBytes != 0 {
		t.Fatalf("empty backup manifest = %+v", m)
	}
	if _, err := Verify(ctx, dir); err != nil {
		t.Fatalf("verify empty backup: %v", err)
	}
	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir, Check: true}); err != nil {
		t.Fatalf("restore empty backup: %v", err)
	}
	if _, err := getKV(t, dst.client, "absent"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("empty restore wrote a key: %v", err)
	}
}

func TestBackupRejectsExistingOutput(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	out := t.TempDir()
	if _, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: out}); err == nil {
		t.Fatal("Backup accepted an existing output directory")
	}
}

func TestVerifyRejectsCorruptChunk(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	putKV(t, src.client, "beta", "two")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir, ChunkKeys: 1})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(m.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(m.Chunks))
	}
	path, err := ChunkPath(dir, m.Chunks[0])
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
	if _, err := Verify(ctx, dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify error = %v, want ErrCorrupt", err)
	}

	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore error = %v, want ErrCorrupt", err)
	}
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("corrupt restore wrote alpha: %v", err)
	}
	if _, err := getKV(t, dst.client, "beta"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("corrupt restore wrote beta: %v", err)
	}
}

func TestManifestMissingOrCorrupt(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if _, err := Verify(ctx, base); !errors.Is(err, ErrManifest) {
		t.Fatalf("missing manifest error = %v, want ErrManifest", err)
	}

	valid := &Manifest{
		Format: FormatName, FormatVersion: FormatVersion,
		CreatedAt: time.Now().UTC(), SnapshotTS: 42,
		ChunkKeys: DefaultChunkKeys, ChunkBytes: DefaultChunkBytes,
		Ranges: []Range{{ID: 1, Version: 1}},
	}
	if err := writeManifestFile(base, valid); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := Verify(ctx, base); err != nil {
		t.Fatalf("verify valid empty manifest: %v", err)
	}

	manifestPath := filepath.Join(base, ManifestFile)
	if err := os.WriteFile(manifestPath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	if _, err := Verify(ctx, base); !errors.Is(err, ErrManifest) {
		t.Fatalf("invalid JSON error = %v, want ErrManifest", err)
	}

	bad := *valid
	bad.FormatVersion = FormatVersion + 1
	if err := writeManifestFile(base, &bad); err != nil {
		t.Fatalf("write bad version: %v", err)
	}
	if _, err := Verify(ctx, base); !errors.Is(err, ErrManifest) {
		t.Fatalf("bad version error = %v, want ErrManifest", err)
	}

	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	if _, err := Verify(ctx, base); !errors.Is(err, ErrManifest) {
		t.Fatalf("removed manifest error = %v, want ErrManifest", err)
	}
}

func TestVerifyRejectsManifestChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	m.Chunks[0].SHA256 = strings.Repeat("0", 64)
	if err := writeManifestFile(dir, m); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	if _, err := Verify(ctx, dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify error = %v, want ErrCorrupt", err)
	}
}

func TestBackupRestoreSplitRanges(t *testing.T) {
	ctx := context.Background()
	ranges := []kvserver.RangeConfig{
		{ID: 1, End: []byte("m")},
		{ID: 2, Start: []byte("m")},
	}
	metaRanges := []meta.Range{
		{ID: 1, End: []byte("m")},
		{ID: 2, Start: []byte("m")},
	}
	src := startTestCluster(t, ranges, metaRanges)
	putKV(t, src.client, "alpha", "left-1")
	putKV(t, src.client, "delta", "left-2")
	putKV(t, src.client, "mike", "right-1")
	putKV(t, src.client, "zulu", "right-2")

	dir := filepath.Join(t.TempDir(), "split")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(m.Ranges) != 2 {
		t.Fatalf("manifest ranges = %d, want 2", len(m.Ranges))
	}
	if !bytes.Equal(m.Ranges[0].End, []byte("m")) || !bytes.Equal(m.Ranges[1].Start, []byte("m")) {
		t.Fatalf("manifest range boundaries = %+v", m.Ranges)
	}
	if m.Ranges[0].Start != nil || m.Ranges[1].End != nil {
		t.Fatalf("manifest outer boundaries = %+v", m.Ranges)
	}
	if len(m.Chunks) != 2 {
		t.Fatalf("manifest chunks = %d, want 2", len(m.Chunks))
	}
	if m.Chunks[0].RangeID != 1 || m.Chunks[1].RangeID != 2 {
		t.Fatalf("chunk range ids = %d/%d, want 1/2", m.Chunks[0].RangeID, m.Chunks[1].RangeID)
	}
	if _, err := Verify(ctx, dir); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// A fresh cluster starts with the default single range. Restoring without
	// EnsureRanges must refuse before writing anything.
	dst := startTestCluster(t, ranges, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); !errors.Is(err, ErrRangeMismatch) {
		t.Fatalf("restore without EnsureRanges error = %v, want ErrRangeMismatch", err)
	}
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("rejected restore wrote alpha: %v", err)
	}

	// EnsureRanges replaces the meta range map with the backup layout and the
	// replica list with the new node address.
	rr, err := Restore(ctx, RestoreConfig{
		Client: dst.client, Meta: dst.meta, InDir: dir,
		EnsureRanges: true, Replicas: []string{dst.nodeAddr},
		StrictEpoch: true, Check: true,
	})
	if err != nil {
		t.Fatalf("split restore: %v", err)
	}
	if rr.Ranges != 2 || rr.Keys != 4 || !rr.Checked {
		t.Fatalf("split restore report = %+v", rr)
	}
	for key, want := range map[string]string{
		"alpha": "left-1", "delta": "left-2", "mike": "right-1", "zulu": "right-2",
	} {
		requireKV(t, dst.client, key, want)
	}
}

func TestBackupArgumentValidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, err := Backup(ctx, BackupConfig{Client: nil, Meta: nil, OutDir: filepath.Join(dir, "x")})
	if err == nil {
		t.Fatal("Backup accepted a nil client")
	}
	if _, err := OpenMeta(nil); err == nil {
		t.Fatal("OpenMeta accepted no addresses")
	}
}

func TestKeySuccessor(t *testing.T) {
	for _, in := range []string{"", "a", "a\x00", strings.Repeat("\xff", 4)} {
		next := keySuccessor([]byte(in))
		if bytes.Compare(next, []byte(in)) <= 0 {
			t.Fatalf("successor of %q is not greater", in)
		}
		if len(next) != len(in)+1 || next[len(in)] != 0 {
			t.Fatalf("successor of %q = %q, want prefix plus zero", in, next)
		}
	}
}

func TestVerifyRejectsMissingChunk(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	path, err := ChunkPath(dir, m.Chunks[0])
	if err != nil {
		t.Fatalf("chunk path: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove chunk: %v", err)
	}
	if _, err := Verify(ctx, dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify error = %v, want ErrCorrupt", err)
	}
	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore error = %v, want ErrCorrupt", err)
	}
	if _, err := getKV(t, dst.client, "alpha"); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("missing-chunk restore wrote alpha: %v", err)
	}
}

func TestManifestRejectsUnsafeChunkPath(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	putKV(t, src.client, "alpha", "one")
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{Client: src.client, Meta: src.meta, OutDir: dir})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	for _, file := range []string{"../escape.chunk", "/tmp/escape.chunk", "chunks/../../escape.chunk"} {
		m.Chunks[0].File = file
		if err := writeManifestFile(dir, m); err != nil {
			t.Fatalf("rewrite manifest: %v", err)
		}
		if _, err := Verify(ctx, dir); !errors.Is(err, ErrManifest) {
			t.Fatalf("Verify(%q) error = %v, want ErrManifest", file, err)
		}
	}
}

func TestBackupChunkKeyLimit(t *testing.T) {
	ctx := context.Background()
	src := startTestCluster(t, nil, nil)
	want := map[string]string{}
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("key-%02d", i)
		value := fmt.Sprintf("value-%d", i)
		want[key] = value
		putKV(t, src.client, key, value)
	}
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(ctx, BackupConfig{
		Client: src.client, Meta: src.meta, OutDir: dir,
		ChunkKeys: 2, ChunkBytes: DefaultChunkBytes,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(m.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(m.Chunks))
	}
	wantCounts := []int{2, 2, 1}
	for i, c := range m.Chunks {
		if c.KeyCount != wantCounts[i] {
			t.Fatalf("chunk %d key_count = %d, want %d", i, c.KeyCount, wantCounts[i])
		}
	}
	if _, err := Verify(ctx, dir); err != nil {
		t.Fatalf("verify: %v", err)
	}
	dst := startTestCluster(t, nil, nil)
	if _, err := Restore(ctx, RestoreConfig{Client: dst.client, Meta: dst.meta, InDir: dir, Check: true}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for key, value := range want {
		requireKV(t, dst.client, key, value)
	}
}
