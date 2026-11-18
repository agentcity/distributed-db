package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/backup"
	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/pitr"
	"distributed-db/internal/storage"
	"distributed-db/internal/v2client"
)

// This file is the automated logical backup/restore drill. It starts one
// in-process meta server and one in-process KV node, commits a known data set,
// takes a logical backup through the production internal/backup API, restores
// it into a fresh in-process cluster, and verifies every expected key/value.
//
// The drill deliberately makes no claim about production readiness. It does
// not start an external cluster, does not exercise TLS/auth, multi-node
// quorum, network or disk faults, and does not implement PITR. The JSON report
// records an RPO proxy (backup snapshot TSO minus the last committed TSO) and
// an RTO proxy (the in-process restore call duration). See
// docs/RESTORE-DRILL.md for the exact scope and caveats.

const (
	drillSeedKeys     = 48
	drillChunkKeys    = 7
	drillPITRKeyCount = 3
)

type drillCluster struct {
	metaAddr string
	nodeAddr string
	metaSrv  *metaserver.Server
	nodeSrv  *kvserver.Server
	client   *v2client.Client
	meta     *backup.MetaClient
}

type drillReport struct {
	Drill      string            `json:"drill"`
	Version    int               `json:"report_version"`
	StartedAt  string            `json:"started_at"`
	FinishedAt string            `json:"finished_at,omitempty"`
	DurationMS int64             `json:"duration_ms"`
	Status     string            `json:"status"`
	Scope      drillScope        `json:"scope"`
	Backup     drillBackup       `json:"backup"`
	Restore    drillRestore      `json:"restore"`
	RPO        drillRPO          `json:"rpo"`
	RTO        drillRTO          `json:"rto"`
	Verify     drillVerification `json:"verification"`
	PITR       drillPITRReport   `json:"pitr"`
	Limits     []string          `json:"limits"`
}

type drillScope struct {
	Test               string `json:"test"`
	InProcessCluster   bool   `json:"in_process_cluster"`
	ExternalCluster    bool   `json:"external_cluster"`
	ProductionCodeEdit bool   `json:"production_code_changed"`
	SeedKeys           int    `json:"seed_keys"`
}

type drillBackup struct {
	Dir                string `json:"dir,omitempty"`
	SnapshotTS         uint64 `json:"snapshot_ts"`
	Ranges             int    `json:"ranges"`
	Chunks             int    `json:"chunks"`
	Keys               int64  `json:"keys"`
	PayloadBytes       int64  `json:"payload_bytes"`
	DurationMS         int64  `json:"duration_ms"`
	VerifyOK           bool   `json:"verify_ok"`
	LockAwareScan      bool   `json:"lock_aware_scan"`
	AppliedBarrierUsed bool   `json:"applied_commit_barrier"`
}

type drillRestore struct {
	Target       string `json:"target"`
	DurationMS   int64  `json:"duration_ms"`
	Ranges       int    `json:"ranges"`
	Chunks       int    `json:"chunks"`
	Keys         int64  `json:"keys"`
	Checked      bool   `json:"checked"`
	RequireEmpty bool   `json:"require_empty"`
}

type drillRPO struct {
	Definition             string `json:"definition"`
	BackupSnapshotTS       uint64 `json:"backup_snapshot_ts"`
	LastCommittedTS        uint64 `json:"last_committed_ts"`
	TSOLag                 uint64 `json:"tso_lag"`
	LastCommitToBackupMS   int64  `json:"last_commit_to_backup_start_ms"`
	NoMissingCommittedKeys bool   `json:"no_missing_committed_keys_at_snapshot"`
}

type drillRTO struct {
	RestoreMS int64 `json:"restore_ms"`
	TotalMS   int64 `json:"total_drill_ms"`
}

type drillVerification struct {
	Status                 string `json:"status"`
	ExpectedKeys           int    `json:"expected_keys"`
	RestoredKeys           int    `json:"restored_keys"`
	MissingKeys            int    `json:"missing_keys"`
	ValueMismatches        int    `json:"value_mismatches"`
	UnexpectedKeys         int    `json:"unexpected_keys"`
	DeletedKeyAbsent       bool   `json:"deleted_key_absent"`
	ReapplyOK              bool   `json:"reapply_ok"`
	NoMissingCommittedKeys bool   `json:"no_missing_committed_keys"`
}

type drillPITRReport struct {
	Enabled           bool   `json:"enabled"`
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	Mode              string `json:"mode,omitempty"`
	ClusterIntegrated bool   `json:"cluster_integrated"`
	ArchiveRecords    int    `json:"archive_records,omitempty"`
	ArchiveMaxTSO     uint64 `json:"archive_max_tso,omitempty"`
	TargetTSO         uint64 `json:"target_tso,omitempty"`
	PlanRecords       int    `json:"plan_records_at_target,omitempty"`
	Applied           int    `json:"applied_at_target,omitempty"`
	FullTargetTSO     uint64 `json:"full_target_tso,omitempty"`
	FullApplied       int    `json:"applied_full_target,omitempty"`
	ValuesVerified    int    `json:"values_verified,omitempty"`
	FutureKeyHidden   bool   `json:"future_key_hidden,omitempty"`
	Note              string `json:"note,omitempty"`
}

// drillPITRCommand mirrors the v2 transactional payload consumed by
// kvserver's apply loop. The PITR path constructs it only for an optional,
// explicitly enabled archive-level replay drill.
type drillPITRCommand struct {
	StartTS   uint64          `json:"start_ts,omitempty"`
	CommitTS  uint64          `json:"commit_ts,omitempty"`
	Primary   []byte          `json:"primary,omitempty"`
	Mutations []mvcc.Mutation `json:"mutations,omitempty"`
}

func drillLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// drillFreeAddr reserves a loopback port and releases it. Test-only: the
// process starts immediately afterwards, so a lost race fails loudly.
func drillFreeAddr(t *testing.T) string {
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

// startDrillCluster starts one meta server and one KV node hosting range 1.
// All state lives under t.TempDir(), so the source and restore targets are
// independent and disappear with the test.
func startDrillCluster(t *testing.T, ctx context.Context) *drillCluster {
	t.Helper()
	nodeAddr := drillFreeAddr(t)
	metaAddr := drillFreeAddr(t)

	ms, err := metaserver.New(metaserver.Config{
		Addr: metaAddr,
		Ranges: []meta.Range{{
			ID: 1, Start: nil, End: nil, Replicas: []string{nodeAddr}, Version: 1,
		}},
		Logger: drillLogger(),
	})
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}
	if err := ms.Start(ctx); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })

	srv, err := kvserver.New(kvserver.Config{
		ID:                 nodeAddr,
		RaftAddr:           nodeAddr,
		DataDir:            t.TempDir(),
		Ranges:             []kvserver.RangeConfig{{ID: 1, Epoch: 1}},
		ElectionTimeoutMin: 50 * time.Millisecond,
		ElectionTimeoutMax: 100 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             drillLogger(),
	})
	if err != nil {
		t.Fatalf("new kv node: %v", err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start kv node: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(stopCtx)
	})

	client, err := v2client.New(metaAddr, []string{nodeAddr})
	if err != nil {
		t.Fatalf("new v2 client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	mc, err := backup.OpenMeta([]string{metaAddr})
	if err != nil {
		t.Fatalf("new backup meta client: %v", err)
	}
	t.Cleanup(func() { _ = mc.Close() })

	c := &drillCluster{
		metaAddr: metaAddr, nodeAddr: nodeAddr,
		metaSrv: ms, nodeSrv: srv, client: client, meta: mc,
	}
	waitDrillLeader(t, nodeAddr, []uint64{1})
	return c
}

func waitDrillLeader(t *testing.T, addr string, ids []uint64) {
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

// drillPut commits one key/value pair and returns its start and commit TSOs.
func drillPut(t *testing.T, ctx context.Context, c *v2client.Client, key, value string) (uint64, uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		txn, err := c.Begin(ctx)
		if err == nil {
			startTS := txn.StartTS()
			txn.Set([]byte(key), []byte(value))
			if err := txn.Commit(ctx); err == nil {
				return startTS, txn.CommitTS()
			} else {
				lastErr = err
				_ = txn.Rollback(ctx)
			}
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("put %q: %v", key, lastErr)
	return 0, 0
}

// drillDelete commits one delete and returns its start and commit TSOs.
func drillDelete(t *testing.T, ctx context.Context, c *v2client.Client, key string) (uint64, uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		txn, err := c.Begin(ctx)
		if err == nil {
			startTS := txn.StartTS()
			txn.Delete([]byte(key))
			if err := txn.Commit(ctx); err == nil {
				return startTS, txn.CommitTS()
			} else {
				lastErr = err
				_ = txn.Rollback(ctx)
			}
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("delete %q: %v", key, lastErr)
	return 0, 0
}

func drillGet(t *testing.T, ctx context.Context, c *v2client.Client, key string) ([]byte, error) {
	t.Helper()
	txn, err := c.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin read %q: %w", key, err)
	}
	defer func() { _ = txn.Rollback(ctx) }()
	return txn.Get(ctx, []byte(key))
}

func drillScanAll(t *testing.T, ctx context.Context, c *v2client.Client) map[string]string {
	t.Helper()
	txn, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin scan: %v", err)
	}
	defer func() { _ = txn.Rollback(ctx) }()
	rows, err := txn.Scan(ctx, nil, nil, 0)
	if err != nil {
		t.Fatalf("scan target: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, kv := range rows {
		out[string(kv.Key)] = string(kv.Value)
	}
	return out
}

func drillOpenMVCC(t *testing.T) *mvcc.Store {
	t.Helper()
	engine, err := storage.Open(t.TempDir(), storage.Options{
		CacheSize:    4 << 20,
		MemTableSize: 1 << 20,
		Compression:  "none",
	})
	if err != nil {
		t.Fatalf("open PITR store: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close PITR store: %v", err)
		}
	})
	return mvcc.New(engine)
}

func writeDrillReport(path string, report drillReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".restore-drill-report-*.tmp")
	if err != nil {
		return fmt.Errorf("create report temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}

// TestRestoreDrill is the default-on, fast logical backup/restore drill.
//
// It asserts that every key committed before the backup snapshot is present
// with the expected value in a fresh restore target, that a key deleted before
// the snapshot stays absent, and that a second restore replays deleted target
// keys (restore is an upsert; it never deletes keys absent from the backup).
//
// The optional PITR path runs only when RESTORE_DRILL_PITR=1. It is an
// archive-level replay drill: internal/pitr is fed synthetic but schema-valid
// committed entries and applies the plan to an isolated mvcc.Store. It is not
// cluster-integrated PITR and does not tail the Raft WAL.
func TestRestoreDrill(t *testing.T) {
	started := time.Now()
	reportPath := os.Getenv("RESTORE_DRILL_REPORT")
	if reportPath == "" {
		reportPath = filepath.Join(t.TempDir(), "restore-drill-report.json")
	}
	report := drillReport{
		Drill:     "logical-backup-restore",
		Version:   1,
		StartedAt: started.UTC().Format(time.RFC3339Nano),
		Status:    "fail",
		Scope: drillScope{
			Test:               "TestRestoreDrill",
			InProcessCluster:   true,
			ExternalCluster:    false,
			ProductionCodeEdit: false,
			SeedKeys:           drillSeedKeys,
		},
		Verify: drillVerification{Status: "fail"},
		PITR: drillPITRReport{
			Enabled:           false,
			Status:            "skipped",
			ClusterIntegrated: false,
		},
		Limits: []string{
			"single in-process node; no external cluster, TLS/auth, multi-node quorum, network, or disk fault",
			"RPO is a TSO-gap proxy, not a wall-clock data-loss window",
			"RTO covers only the in-process restore call, not provisioning or cutover",
			"restore upserts; it does not delete keys absent from the backup, so the target must be fresh",
			"the optional PITR path is archive-level synthetic replay, not cluster WAL integration",
		},
	}
	t.Cleanup(func() {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		report.DurationMS = time.Since(started).Milliseconds()
		report.RTO.TotalMS = report.DurationMS
		if err := writeDrillReport(reportPath, report); err != nil {
			t.Errorf("write restore drill report: %v", err)
			return
		}
		t.Logf("restore drill report: %s", reportPath)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	src := startDrillCluster(t, ctx)
	target := startDrillCluster(t, ctx)

	// Seed a deterministic data set, then update one key and delete another.
	// The expected map is the exact state at the backup snapshot.
	expected := make(map[string]string, drillSeedKeys)
	var lastCommitTS uint64
	var lastCommitAt time.Time
	recordCommit := func(commitTS uint64) {
		if commitTS > lastCommitTS {
			lastCommitTS = commitTS
			lastCommitAt = time.Now()
		}
	}
	for i := 0; i < drillSeedKeys; i++ {
		key := fmt.Sprintf("drill/key-%03d", i)
		value := fmt.Sprintf("value-%03d-%s", i, strings.Repeat("x", 80))
		_, commitTS := drillPut(t, ctx, src.client, key, value)
		expected[key] = value
		recordCommit(commitTS)
	}

	updatedKey := "drill/key-007"
	updatedValue := "value-007-updated-" + strings.Repeat("u", 64)
	_, commitTS := drillPut(t, ctx, src.client, updatedKey, updatedValue)
	expected[updatedKey] = updatedValue
	recordCommit(commitTS)

	deletedKey := "drill/key-013"
	_, commitTS = drillDelete(t, ctx, src.client, deletedKey)
	delete(expected, deletedKey)
	recordCommit(commitTS)

	// The backup uses the production lock-aware remote scanner. The server has
	// no applied-commit barrier RPC, so the report records that the barrier is
	// not used rather than claiming a stronger guarantee than was exercised.
	backupDir := filepath.Join(t.TempDir(), "backup")
	backupStartedAt := time.Now()
	m, err := backup.Backup(ctx, backup.BackupConfig{
		Client:    src.client,
		Meta:      src.meta,
		OutDir:    backupDir,
		ChunkKeys: drillChunkKeys,
		Scanner:   backup.RemoteScanner{Client: src.client},
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	backupDuration := time.Since(backupStartedAt)

	vr, err := backup.Verify(ctx, backupDir)
	if err != nil {
		t.Fatalf("offline backup verify: %v", err)
	}
	report.Backup = drillBackup{
		Dir:                filepath.Base(backupDir),
		SnapshotTS:         m.SnapshotTS,
		Ranges:             len(m.Ranges),
		Chunks:             len(m.Chunks),
		Keys:               m.TotalKeys,
		PayloadBytes:       m.TotalBytes,
		DurationMS:         backupDuration.Milliseconds(),
		VerifyOK:           vr.Keys == m.TotalKeys && vr.SnapshotTS == m.SnapshotTS,
		LockAwareScan:      true,
		AppliedBarrierUsed: false,
	}
	if !report.Backup.VerifyOK {
		t.Fatalf("backup verify report mismatch: %+v vs manifest %+v", vr, m)
	}
	if m.SnapshotTS < lastCommitTS {
		t.Fatalf("backup snapshot TSO %d precedes last committed TSO %d", m.SnapshotTS, lastCommitTS)
	}
	report.RPO = drillRPO{
		Definition:             "backup_snapshot_ts - last_committed_ts (TSO units); not wall-clock time",
		BackupSnapshotTS:       m.SnapshotTS,
		LastCommittedTS:        lastCommitTS,
		TSOLag:                 m.SnapshotTS - lastCommitTS,
		LastCommitToBackupMS:   backupStartedAt.Sub(lastCommitAt).Milliseconds(),
		NoMissingCommittedKeys: false,
	}

	// Restore into a genuinely fresh cluster. RequireEmpty fails before the
	// first write if the target is not empty.
	restoreStartedAt := time.Now()
	rr, err := backup.Restore(ctx, backup.RestoreConfig{
		Client:       target.client,
		Meta:         target.meta,
		InDir:        backupDir,
		RequireEmpty: true,
		Check:        true,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restoreDuration := time.Since(restoreStartedAt)
	report.Restore = drillRestore{
		Target:       "fresh_in_process_cluster",
		DurationMS:   restoreDuration.Milliseconds(),
		Ranges:       rr.Ranges,
		Chunks:       rr.Chunks,
		Keys:         rr.Keys,
		Checked:      rr.Checked,
		RequireEmpty: true,
	}
	if !rr.Checked || rr.SnapshotTS != m.SnapshotTS {
		t.Fatalf("restore report = %+v, want checked restore at snapshot %d", rr, m.SnapshotTS)
	}
	if rr.Keys != int64(len(expected)) {
		t.Fatalf("restore keys = %d, expected %d", rr.Keys, len(expected))
	}

	// Compare the complete restored key space, not a sample.
	got := drillScanAll(t, ctx, target.client)
	missing, mismatch, unexpected := 0, 0, 0
	for key, want := range expected {
		gotValue, ok := got[key]
		if !ok {
			missing++
			continue
		}
		if gotValue != want {
			mismatch++
		}
	}
	for key := range got {
		if _, ok := expected[key]; !ok {
			unexpected++
		}
	}
	_, deletedKeyPresent := got[deletedKey]
	report.Verify = drillVerification{
		Status:                 "fail",
		ExpectedKeys:           len(expected),
		RestoredKeys:           len(got),
		MissingKeys:            missing,
		ValueMismatches:        mismatch,
		UnexpectedKeys:         unexpected,
		DeletedKeyAbsent:       !deletedKeyPresent,
		NoMissingCommittedKeys: missing == 0 && mismatch == 0 && unexpected == 0 && !deletedKeyPresent,
	}
	report.RPO.NoMissingCommittedKeys = report.Verify.NoMissingCommittedKeys
	if missing != 0 || mismatch != 0 || unexpected != 0 {
		t.Fatalf("restored state mismatch: missing=%d value_mismatches=%d unexpected=%d", missing, mismatch, unexpected)
	}
	if v, err := drillGet(t, ctx, target.client, deletedKey); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("deleted key %q after restore = %q, err %v; want not found", deletedKey, v, err)
	}

	// Restore is an upsert. Deleting a restored key and restoring again must
	// bring it back; this also proves a second restore is harmless.
	reapplyKey := "drill/key-000"
	reapplyValue := expected[reapplyKey]
	drillDelete(t, ctx, target.client, reapplyKey)
	if v, err := drillGet(t, ctx, target.client, reapplyKey); !errors.Is(err, v2client.ErrNotFound) {
		t.Fatalf("reapply target key %q after delete = %q, err %v; want not found", reapplyKey, v, err)
	}
	if _, err := backup.Restore(ctx, backup.RestoreConfig{
		Client: target.client,
		Meta:   target.meta,
		InDir:  backupDir,
		Check:  true,
	}); err != nil {
		t.Fatalf("reapply restore: %v", err)
	}
	if v, err := drillGet(t, ctx, target.client, reapplyKey); err != nil || string(v) != reapplyValue {
		t.Fatalf("reapply key %q = %q, err %v; want %q", reapplyKey, v, err, reapplyValue)
	}
	report.Verify.ReapplyOK = true

	// Optional PITR path. It is off by default and never touches the main
	// restore target; see the report reason for the exact limitation.
	report.PITR = runDrillPITR(t, ctx, src, m, expected)

	report.Verify.Status = "pass"
	report.Status = "pass"
	report.RTO = drillRTO{
		RestoreMS: restoreDuration.Milliseconds(),
		TotalMS:   time.Since(started).Milliseconds(),
	}
}

// runDrillPITR runs the env-gated archive-level PITR replay. When disabled it
// returns an explicit "skipped" record instead of silently omitting the path.
//
// The drill writes three more transactions to the source cluster after the base
// backup, builds a real internal/pitr archive from schema-valid prewrite_commit
// entries, and replays the plan at the second commit TSO into an isolated
// mvcc.Store pre-seeded with the base values. A third transaction must remain
// invisible at that target and become visible only when the target advances to
// the third commit TSO. This proves planner filtering, archive verification and
// recorded-TSO sink semantics; it does not prove WAL integration, a global safe
// point, lock handling or cutover.
func runDrillPITR(t *testing.T, ctx context.Context, src *drillCluster, base *backup.Manifest, baseValues map[string]string) drillPITRReport {
	t.Helper()
	enabled := os.Getenv("RESTORE_DRILL_PITR")
	if enabled != "1" && !strings.EqualFold(enabled, "true") {
		return drillPITRReport{
			Enabled:           false,
			Status:            "skipped",
			Reason:            "RESTORE_DRILL_PITR is not set; enable it to run the archive-level replay path. Cluster WAL -> PITR archive wiring and a global safe point do not exist yet.",
			ClusterIntegrated: false,
		}
	}
	if len(base.Ranges) != 1 {
		t.Fatalf("PITR drill expects exactly one base range, got %d", len(base.Ranges))
	}
	if base.SnapshotTS < uint64(len(baseValues)*2+10) {
		t.Fatalf("PITR drill needs a larger base TSO, got %d", base.SnapshotTS)
	}

	type pitrWrite struct {
		key      string
		value    string
		startTS  uint64
		commitTS uint64
	}
	writes := make([]pitrWrite, 0, drillPITRKeyCount)
	records := make([]pitr.LogRecord, 0, drillPITRKeyCount)
	for i := 0; i < drillPITRKeyCount; i++ {
		key := fmt.Sprintf("pitr/key-%02d", i)
		value := fmt.Sprintf("pitr-value-%02d-%s", i, strings.Repeat("p", 48))
		startTS, commitTS := drillPut(t, ctx, src.client, key, value)
		if commitTS <= base.SnapshotTS {
			t.Fatalf("PITR write %q commitTS %d is not above base TSO %d", key, commitTS, base.SnapshotTS)
		}
		payload, err := json.Marshal(drillPITRCommand{
			StartTS:   startTS,
			CommitTS:  commitTS,
			Primary:   []byte(key),
			Mutations: []mvcc.Mutation{{Key: []byte(key), Value: []byte(value)}},
		})
		if err != nil {
			t.Fatalf("encode PITR command: %v", err)
		}
		writes = append(writes, pitrWrite{key: key, value: value, startTS: startTS, commitTS: commitTS})
		records = append(records, pitr.LogRecord{
			TSO: commitTS,
			Entry: model.Entry{
				Term: 1, Index: uint64(i + 1),
				Type: model.CommandPrewriteCommit, Value: payload,
			},
		})
	}
	for _, w := range writes {
		got, err := drillGet(t, ctx, src.client, w.key)
		if err != nil || string(got) != w.value {
			t.Fatalf("source PITR key %q = %q, err %v; want %q", w.key, got, err, w.value)
		}
	}

	archiveDir := t.TempDir()
	w, err := pitr.NewWriter(pitr.WriterConfig{
		Dir:             archiveDir,
		ClusterID:       "restore-drill",
		SegmentMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("new PITR writer: %v", err)
	}
	if err := w.Append(ctx, base.Ranges[0].ID, records); err != nil {
		t.Fatalf("append PITR records: %v", err)
	}
	if err := w.Seal(ctx); err != nil {
		t.Fatalf("seal PITR archive: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close PITR writer: %v", err)
	}

	reader, err := pitr.OpenReader(archiveDir)
	if err != nil {
		t.Fatalf("open PITR archive: %v", err)
	}
	archiveReport, err := reader.Verify(ctx)
	if err != nil {
		t.Fatalf("verify PITR archive: %v", err)
	}
	planner, err := pitr.NewPlanner(reader)
	if err != nil {
		t.Fatalf("new PITR planner: %v", err)
	}

	// Seed an isolated store with the base values at TSOs below the base
	// snapshot, simulating the base-backup restore step without adding a
	// cluster-side import API.
	store := drillOpenMVCC(t)
	keys := make([]string, 0, len(baseValues))
	for key := range baseValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seedStart := base.SnapshotTS - uint64(len(keys)*2+10)
	for i, key := range keys {
		startTS := seedStart + uint64(i*2)
		if err := store.CommitSingle(
			[]mvcc.Mutation{{Key: []byte(key), Value: []byte(baseValues[key])}},
			[]byte(key), startTS, startTS+1,
		); err != nil {
			t.Fatalf("seed PITR base key %q: %v", key, err)
		}
	}

	sink := pitr.RestoreSinkFunc(func(ctx context.Context, rec pitr.PlannedRecord) error {
		var cmd drillPITRCommand
		if err := json.Unmarshal(rec.Entry.Value, &cmd); err != nil {
			return fmt.Errorf("decode planned command at index %d: %w", rec.Entry.Index, err)
		}
		if cmd.CommitTS != rec.TSO {
			return fmt.Errorf("planned command commit_ts %d differs from record TSO %d", cmd.CommitTS, rec.TSO)
		}
		return store.CommitSingle(cmd.Mutations, cmd.Primary, cmd.StartTS, rec.TSO)
	})

	targetTSO := writes[1].commitTS
	result, err := planner.RestoreToTS(ctx, pitr.PlanRequest{Base: base, TargetTSO: targetTSO}, sink)
	if err != nil {
		t.Fatalf("PITR restore to %d: %v", targetTSO, err)
	}
	if result.Applied != 2 || len(result.Plan.Records) != 2 {
		t.Fatalf("PITR target plan = %d records, applied %d; want 2", len(result.Plan.Records), result.Applied)
	}
	for i := 0; i < 2; i++ {
		got, err := store.Get([]byte(writes[i].key), targetTSO)
		if err != nil || string(got) != writes[i].value {
			t.Fatalf("PITR target key %q at %d = %q, err %v; want %q", writes[i].key, targetTSO, got, err, writes[i].value)
		}
	}
	if _, err := store.Get([]byte(writes[2].key), targetTSO); !errors.Is(err, mvcc.ErrKeyNotFound) {
		t.Fatalf("PITR future key %q visible at %d, err %v; want not found", writes[2].key, targetTSO, err)
	}
	if got, err := store.Get([]byte(keys[0]), targetTSO); err != nil || string(got) != baseValues[keys[0]] {
		t.Fatalf("PITR base key %q at %d = %q, err %v; want %q", keys[0], targetTSO, got, err, baseValues[keys[0]])
	}

	fullTSO := writes[2].commitTS
	full, err := planner.RestoreToTS(ctx, pitr.PlanRequest{Base: base, TargetTSO: fullTSO}, sink)
	if err != nil {
		t.Fatalf("PITR restore to %d: %v", fullTSO, err)
	}
	if full.Applied != drillPITRKeyCount {
		t.Fatalf("PITR full target applied = %d, want %d", full.Applied, drillPITRKeyCount)
	}
	for _, w := range writes {
		got, err := store.Get([]byte(w.key), fullTSO)
		if err != nil || string(got) != w.value {
			t.Fatalf("PITR full target key %q at %d = %q, err %v; want %q", w.key, fullTSO, got, err, w.value)
		}
	}

	return drillPITRReport{
		Enabled:           true,
		Status:            "pass",
		Mode:              "archive_level_synthetic_feed",
		ClusterIntegrated: false,
		ArchiveRecords:    len(records),
		ArchiveMaxTSO:     archiveReport.MaxTSO,
		TargetTSO:         targetTSO,
		PlanRecords:       len(result.Plan.Records),
		Applied:           result.Applied,
		FullTargetTSO:     fullTSO,
		FullApplied:       full.Applied,
		ValuesVerified:    drillPITRKeyCount + 1,
		FutureKeyHidden:   true,
		Note:              "Proves planner filtering, archive verification and recorded-TSO sink semantics only; no Raft WAL tailing, global safe point, lock/cross-range handling or atomic cutover.",
	}
}
