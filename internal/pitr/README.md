# `internal/pitr` — PITR archive MVP

Storage core for continuous Raft-log archiving and point-in-time restore
planning. This package is intentionally **not** wired into the cluster and does
not claim RPO 0.

## Layout

```text
<archive>/
  manifest.json
  segments/
    range-0000000000000001/
      00000000000000000001-00000000000000000042-1a2b3c4d5e6f.seg
```

`manifest.json` is the atomically replaced index: cluster id, incarnation,
timestamps, sorted segment descriptors, and per-range GC trim frontiers.

## Segment format v1

| Part | Fields / checksum |
|---|---|
| Prologue (20 B) | `DDBPITR1`, u16 version, u16 flags, u32 header length, u32 CRC32C of header body |
| Header | cluster id, range id, incarnation, start/end index, start/end term, entry count, min/max observed TSO, writer epoch, payload bytes, seal time, previous segment SHA-256 |
| Records | `u32 payload length + u32 CRC32C + payload`; payload is index, term, TSO, type/key/value |
| Footer (40 B) | `DDBPIT1E` + SHA-256 of all preceding bytes |
| Manifest descriptor | complete-file SHA-256, size, path, terms, TSO bounds, chain predecessor |

Readers verify the full-file SHA-256, footer body hash, header CRC and every
record CRC/field before delivering records. Indices must be contiguous,
terms non-decreasing, and the header must agree with the manifest.

## API

```go
w, _ := pitr.NewWriter(pitr.WriterConfig{
    Dir: "/archive", ClusterID: "cluster-a", Incarnation: 1,
    SegmentMaxBytes: 4 << 20, SegmentMaxAge: 30 * time.Second,
})
// Caller supplies committed entries and the commit TSO it observed.
w.Append(ctx, rangeID, []pitr.LogRecord{{TSO: commitTS, Entry: entry}})
w.Seal(ctx)
w.Close()

r, _ := pitr.OpenReader("/archive")
r.ReplayRange(ctx, rangeID, pitr.Target{TSO: target}, func(rec pitr.LogRecord) error { ... })
r.Verify(ctx)

planner, _ := pitr.NewPlanner(r)
plan, _ := planner.Plan(ctx, baseManifest, targetTSO)
```

`Writer.GC(ctx, pitr.RetentionPolicy{MaxAge: 7*24*time.Hour, MaxBytes: 1<<40,
ProtectTSO: oldestBaseTSO})` trims prefixes by age and size. `ProtectTSO` is the
safety floor: only segments whose `MaxTSO <= ProtectTSO` may be removed.

## Guarantees

- Segment temp file is fsynced; rename and directory are fsynced before the
  segment is referenced by the manifest.
- Manifest is rewritten via temp + fsync + rename + directory fsync.
- A crash between segment rename and manifest publication leaves no torn index;
  the next `NewWriter` removes the unacknowledged orphan and stale temp files.
- Manifest validation rejects per-range index gaps, hash-chain breaks, path
  escapes, duplicate paths and invalid descriptors.
- GC rewrites the manifest before unlinking, only trims contiguous prefixes,
  and respects `ProtectTSO`.
- `Reader` and `Planner` use no mutable package-level state; writers, clocks,
  buffers and manifests are independent instances.
- Planner output is deterministic: same base + archive + target => same record
  order, segment list and SHA-256 plan hash.

## Tests

`go test -race ./internal/pitr/...` covers:

- append/read round trip, reopen and chain continuation;
- crash simulation between temp/rename and between rename/manifest, plus retry;
- corrupted/truncated segment, bad manifest, checksum rejection;
- gap detection (missing middle segment and gapped manifest);
- retention by age and size, protection of needed segments, post-trim append;
- restore plan ordering, target filtering, range subset, determinism/hash,
  repeated apply, missing/corrupt/trimmed range rejection;
- independent writers/readers running concurrently (no global state).

## Remaining integration work

- Tail the committed Raft log prefix and handle truncation/leadership changes;
  record the observed commit TSO (or an explicit `TSO=0` for metadata).
- Persist and publish per-range/global safe points, TSO high-water, and the
  meta/range-map archive. Without them the planner cannot prove a target is
  fully archived.
- Split/merge lineage, range epoch fencing, duplicate-writer arbitration.
- Cross-range transaction reconstruction, MVCC command application, lock
  staging and resolution.
- Base chunk import into an isolated target, restore lease/resume, verification
  drills and atomic cutover.
- Object-store upload, compression, encryption/KMS, signatures and
  anti-rollback. Checksums detect corruption, not tampering.
- RPO 0 is **not** claimed: unsealed records are lost on a crash and archive lag
  is not tracked or enforced.
