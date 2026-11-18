# Restore drill: automated logical backup/restore

This document describes the automated **logical backup/restore drill** added in
Sprint 2. It is a code-level drill, not a production disaster-recovery test. It
starts no external cluster and requires no pre-existing server; the test opens
its own in-process listeners and temporary data directories.

| Artifact | Purpose |
|---|---|
| [`test/restore_drill_test.go`](../test/restore_drill_test.go) | In-process source/target clusters, seed, backup, restore, verification, JSON report, optional PITR path |
| [`scripts/restore-drill.sh`](../scripts/restore-drill.sh) | Runs the test with `-race`, captures the report, validates it, fails on mismatch |
| `docs/RESTORE-DRILL.md` | This procedure and its limits |

## 1. What the drill does

The test `TestRestoreDrill` runs by default (it is not skipped in normal or
`-short` mode) and performs one complete round trip:

1. Starts an in-process meta server and one in-process KV node with range 1,
   using temporary data directories.
2. Commits a deterministic data set: 48 keys, one overwrite, and one delete.
   The snapshot therefore contains 47 keys. The test records every commit TSO
   and the last committed TSO.
3. Takes a logical backup through `internal/backup.Backup` using the
   production lock-aware remote scanner (`backup.RemoteScanner`) with
   `ChunkKeys=7`, so the round trip covers multiple chunks.
4. Runs the offline `backup.Verify` checksum/framing verification.
5. Starts a second, fresh in-process cluster and restores with
   `RequireEmpty: true` and `Check: true`. `RequireEmpty` rejects a non-empty
   target before the first write; `Check` reads every restored key back.
6. Scans the complete target key space and compares it with the expected
   snapshot state:
   - every committed key must be present with its expected value;
   - the key deleted before the snapshot must be absent;
   - no unexpected key may exist in the fresh target.
7. Deletes one restored key on the target, confirms it is absent, restores the
   same backup again, and confirms the key is back. This proves restore is an
   upsert and a repeat restore is harmless.
8. Writes a JSON report and returns a failing test if any check fails.

The source and target use independent temporary directories, so the target is
a genuinely fresh store/cluster.

## 2. Running the drill

### Go test directly

```bash
go test -race ./test -run '^TestRestoreDrill$' -v
```

Set `RESTORE_DRILL_REPORT` to keep the JSON report at a known path:

```bash
RESTORE_DRILL_REPORT=/tmp/restore-drill/report.json \
  go test -race ./test -run '^TestRestoreDrill$' -v
```

Without the variable the report is written under the test's temporary
directory and its path is printed in the test log.

### Script (recommended)

```bash
./scripts/restore-drill.sh
./scripts/restore-drill.sh --report-dir /tmp/restore-drill
./scripts/restore-drill.sh --report-dir /tmp/restore-drill --pitr
```

Environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `DRILL_REPORT_DIR` | `mktemp -d` | Directory for `report.json` and `test.log` |
| `DRILL_PITR` | `0` | `1` enables the optional PITR path |
| `DRILL_TIMEOUT` | `90s` | `go test -timeout` |
| `GO` | `go` | Go binary |

The script starts **no external server process**, contacts no cluster, and
uses only the Go test.
It exits non-zero when the test fails, the report is missing, or the report
shows a missing/mismatched key. It prints a one-line summary and the report and
log paths on success.

Outputs:

```text
<report-dir>/report.json   structured result
<report-dir>/test.log      full go test output
```

## 3. Report

The report is a single JSON object. A successful run looks like this
(fields abbreviated):

```json
{
  "drill": "logical-backup-restore",
  "report_version": 1,
  "started_at": "2026-11-18T06:20:00Z",
  "finished_at": "2026-11-18T06:20:03Z",
  "duration_ms": 3000,
  "status": "pass",
  "scope": {
    "test": "TestRestoreDrill",
    "in_process_cluster": true,
    "external_cluster": false,
    "production_code_changed": false,
    "seed_keys": 48
  },
  "backup": {
    "snapshot_ts": 1763446800123456,
    "ranges": 1,
    "chunks": 7,
    "keys": 47,
    "payload_bytes": 7000,
    "duration_ms": 20,
    "verify_ok": true,
    "lock_aware_scan": true,
    "applied_commit_barrier": false
  },
  "restore": {
    "target": "fresh_in_process_cluster",
    "duration_ms": 60,
    "ranges": 1,
    "chunks": 7,
    "keys": 47,
    "checked": true,
    "require_empty": true
  },
  "rpo": {
    "definition": "backup_snapshot_ts - last_committed_ts (TSO units); not wall-clock time",
    "backup_snapshot_ts": 1763446800123456,
    "last_committed_ts": 1763446800123400,
    "tso_lag": 56,
    "last_commit_to_backup_start_ms": 2,
    "no_missing_committed_keys_at_snapshot": true
  },
  "rto": {
    "restore_ms": 60,
    "total_drill_ms": 3000
  },
  "verification": {
    "status": "pass",
    "expected_keys": 47,
    "restored_keys": 47,
    "missing_keys": 0,
    "value_mismatches": 0,
    "unexpected_keys": 0,
    "deleted_key_absent": true,
    "reapply_ok": true,
    "no_missing_committed_keys": true
  },
  "pitr": {
    "enabled": false,
    "status": "skipped",
    "reason": "RESTORE_DRILL_PITR is not set; ...",
    "cluster_integrated": false
  },
  "limits": ["..."]
}
```

`report.status` is `pass` only after all assertions, including the optional
PITR path when enabled, succeeded.

## 4. What the drill proves

- The production logical-backup API produces a verified, checksummed,
  multi-chunk backup at one snapshot TSO from an in-process cluster.
- Restoring that backup into a fresh empty target reproduces every key/value
  that was committed at or before the snapshot, including the latest overwrite.
- A key deleted before the backup snapshot is not resurrected by the restore.
- Restore is an idempotent upsert: a second restore of the same backup is
  harmless and replays a key deleted on the target.
- The snapshot TSO is not below the last committed TSO used to seed the data.
- The round trip completes in seconds and is safe to run in normal CI; the
  test's own context is bounded at 45 seconds.

## 5. What the drill does not prove

This is a single-node, in-process code drill. It is **not** a production DR
exercise and must not be cited as evidence for any of the following:

- a real multi-node cluster, quorum loss, rolling restart, or cross-node data
  movement;
- TLS/mTLS, admin tokens, RBAC, audit, or the `kvbackup` CLI wiring;
- network partitions, disk-full, torn writes, storage corruption, or hardware
  failure;
- backups stored off-cluster/object storage, encryption, or retention;
- multi-range layouts, split/merge during backup, range-epoch changes, or
  atomic cluster cutover;
- concurrent writers during backup. The test is quiescent, and the remote
  backup scanner has no applied-commit barrier RPC in this tree; the report
  records `applied_commit_barrier: false` rather than claiming that guarantee;
- restore of keys absent from the backup (restore never deletes target keys);
- production RPO or RTO (see below);
- PITR/WAL archiving (see the PITR path below).

A passing drill also does not replace the manual runbook in
[`docs/RUNBOOKS.md`](RUNBOOKS.md) §7, which covers the real isolated-target
procedure.

## 6. RPO and RTO caveats

### RPO proxy

The report records:

```text
tso_lag = backup_snapshot_ts - last_committed_ts
```

This is a **TSO-order proxy**: a small non-negative lag only says the backup
snapshot is newer than the last known commit in TSO order; it says nothing
about how much wall-clock time elapsed or when the next failure will occur. It
is not a wall-clock data-loss window and does not mean RPO 0:

- the actual RPO is the backup interval plus the time between the last backup
  and the failure;
- logical backup has no continuous log archive, so a failure after the last
  backup loses every commit made since that snapshot;
- `last_commit_to_backup_start_ms` is a local observation only;
- there is no PITR in the default path, so the restore point is the snapshot,
  not an arbitrary TSO.

A backup can also take time to scan; commits that arrive during the scan are
outside the snapshot by definition. The drill is quiescent, so it does not
measure that window.

### RTO proxy

`restore.duration_ms` covers only the in-process `backup.Restore` call,
including the pre-write checksum verification, chunk writes and the optional
readback. It excludes:

- cluster provisioning and scheduling;
- copying the backup from remote storage;
- DNS/service cutover, client reconnection and cache warm-up;
- verification of the full data set outside the test, and operator decisions.

The production RTO must be measured on the real target and added to the
incident/change record. Treat the drill number as a lower bound and a
regression signal, not an SLO.

## 7. Optional PITR path

The PITR path is gated by the environment:

```bash
RESTORE_DRILL_PITR=1 go test -race ./test -run '^TestRestoreDrill$' -v
# or
./scripts/restore-drill.sh --pitr
```

When the variable is unset (the default), the report contains
`"pitr": {"enabled": false, "status": "skipped", "reason": "..."}` with an
explicit reason. It is not silently omitted.

When enabled, the drill:

1. writes three more transactions to the source cluster after the base backup;
2. builds a real `internal/pitr` archive from schema-valid `prewrite_commit`
   entries with their observed commit TSOs;
3. verifies the archive and builds a restore plan over the base backup;
4. replays the plan at the second commit TSO into an isolated `mvcc.Store`
   seeded with the base values;
5. asserts the first two post-backup values are visible, the third is still
   hidden, and advancing the target to the third TSO makes it visible;
6. applies the plan again to exercise idempotent replay at the same TSOs.

What this PITR path proves:

- `internal/pitr` segment/archive verification, plan filtering by
  `(baseTSO, targetTSO]`, deterministic record order, and sink application at
  the recorded commit TSO;
- a target cannot see a commit newer than the requested restore TSO.

What it does **not** prove:

- that the cluster tails the Raft WAL into the archive (it does not; the feed is
  synthetic);
- a global safe point / archive-completeness checkpoint;
- split/merge lineage, cross-range transactions, lock resolution, base-chunk
  import into the store, or atomic cutover;
- RPO 0. Unsealed records are lost on a writer crash.

Because the repository has no cluster-integrated WAL → archive wiring, the
`cluster_integrated` field is always `false`. Do not describe this path as
production PITR.

## 8. Failure handling

If the test or script fails:

1. Keep `report.json` and `test.log`.
2. Compare `verification.missing_keys`, `value_mismatches`,
   `unexpected_keys`, `deleted_key_absent` and `reapply_ok`.
3. If the failure is in the optional PITR section, re-run without `--pitr`
   to separate the logical drill from the archive-level path.
4. Do not treat a failed restore drill as a transient test flake until the
   mismatch counters are zero and the report status is `pass`.
5. Do not use this drill as authorization to restore into a serving cluster.
