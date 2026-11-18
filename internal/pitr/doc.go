// Package pitr implements the storage half of a point-in-time recovery (PITR)
// archive: immutable, checksummed log segments, an atomic manifest, a reader
// with integrity and gap checks, retention/GC, adapters from committed
// Raft/WAL-like entries and a local Raft log store, a meta/TSO checkpoint
// record, and a deterministic restore planner and executor over a base backup
// plus archived records.
//
// # Scope
//
// This package is an MVP archive component, not cluster-integrated PITR. It
// reads a local Raft log store in read-only mode, but it does not tail the log,
// fence leadership, gate truncation or coordinate multiple archivers. It does
// not read the meta log, MVCC store, TSO allocator or the existing backup
// chunks; a caller supplies the captured high-water TSO and range metadata.
// ArchiveAdapter consumes a committed, gap-free entry source supplied by the
// caller; RaftLogSource is one concrete source over raft.LogStore.Load.
// Planner pairs an internal/backup manifest with the archive, and RestoreToTS
// feeds the resulting plan to a caller-supplied RestoreSink. Those wirings are
// narrow interfaces, not a running cluster integration. The remaining open
// work is listed under "Integration gaps" below.
//
// # Archive layout
//
//	<dir>/manifest.json
//	<dir>/segments/range-<16-hex>/<start>-<end>-<sha-prefix>.seg
//
// The manifest records the cluster id, incarnation, timestamps, retained
// segment descriptors, per-range GC trim frontiers and an optional meta/TSO
// checkpoint (high-water TSO plus the range map observed at capture).
// Segments are immutable and sorted by (range id, start index).
//
// # Segment format (version 1)
//
// A segment is:
//
//	20-byte prologue: "DDBPITR1", u16 version, u16 flags, u32 header length,
//	                  u32 CRC32C of the header body
//	header body:      cluster id, range id, incarnation, start/end index,
//	                  start/end term, entry count, min/max observed TSO,
//	                  writer epoch, payload bytes, seal time, previous
//	                  segment SHA-256
//	records:           framed as u32 payload length, u32 CRC32C, then a
//	                  big-endian payload: index, term, TSO, type/key/value
//	40-byte footer:   "DDBPIT1E" and SHA-256 of all preceding bytes
//
// The manifest's segment SHA-256 covers the complete file, including the
// footer. The footer body hash lets the reader verify the header and records
// before delivering any record. Records are validated for framing, CRC,
// contiguous indices, non-decreasing terms, header terms, TSO bounds, count
// and payload bytes.
//
// # Durability and atomic publish
//
// A segment is written to a temporary file in its final range directory,
// fsynced, closed, renamed into place, and the directory is fsynced. Only then
// is the manifest rewritten through temp-file + fsync + rename + directory
// fsync. A crash can therefore leave a valid old manifest or a valid new one,
// never a torn manifest. A crash between segment rename and manifest
// publication leaves an unreferenced segment; NewWriter removes it (the append
// that produced it was never acknowledged) and also removes stale temp files.
//
// # Retention
//
// GC only removes contiguous prefixes per range and rewrites the manifest
// before unlinking files. RetentionPolicy.ProtectTSO must be set to the TSO of
// the oldest base backup that must remain restorable: a segment is eligible
// only when its MaxTSO is non-zero and <= ProtectTSO, so it cannot contain a
// record needed by that base. A zero ProtectTSO disables the safety check.
//
// # Restore planning and execution
//
// Planner combines an internal/backup manifest (SnapshotTS and range map) with
// the archive. It emits records in base range order (by start key, then range
// id) and Raft index order within each range, filtering to
// BaseTSO < TSO <= TargetTSO. It rejects a target before the base, unknown
// requested ranges, a needed range absent from the checkpoint range map, a
// trimmed prefix that may contain records above the base, and
// missing/corrupt/non-contiguous segments needed by the requested ranges.
//
// RestoreToTS builds that plan and feeds it to a RestoreSink in plan order. It
// fails closed before the first sink call when the target is above the safe
// high-water: the smallest of the manifest checkpoint, per-needed-range archive
// coverage and PlanRequest.BarrierFrontier (the applied frontier reported by
// the commit-lease barrier). PlanRequest.AllowUnprovenTarget is an explicit
// opt-out. A target at or below the base is always allowed because the base is
// independently complete. RestoreToTS does not execute commands, resolve locks,
// delete keys absent from the base, or perform an atomic cutover.
//
// The plan is a pure function of its inputs: building it twice yields the same
// records, order, segment list and SHA-256 Hash. Applying the plan twice is
// idempotent only when the sink writes at the recorded commit TSO and upserts
// by (range, key, commitTS); this package deliberately does not implement
// command semantics, lock resolution or storage writes.
//
// # Entry feed
//
// ArchiveAdapter translates entries from an EntrySource into records and
// appends them through Writer. CommitTSFromEntry understands the v2
// transactional JSON payloads (commit, prewrite_commit, txn_batch) and returns
// zero for entries that carry no single commit timestamp. The adapter requires
// ascending, gap-free indices: Raft delivers apply entries with gaps when it
// consumes membership commands itself, so an integration must feed the
// committed log sequence (including those commands) rather than the state-
// machine apply stream, or fill the gaps before calling the adapter.
//
// RaftLogSource adapts raft.LogStore.Load (for example *raft.FileStore) to
// EntrySource. It takes one read-only Load snapshot per call, validates
// index/term ordering, clamps the result to a caller-supplied commit index
// (typically raft.Node.Status().CommitIndex), and returns entries in index
// order. A requested index below the first retained entry returns *GapError
// with Compacted set and FirstAvailable reported; *raft.FileStore's snapshot
// metadata lets the adapter report that frontier even when no entries remain.
//
// # Meta/TSO checkpoint
//
// CaptureCheckpoint reads a high-water TSO and range map from narrow caller
// interfaces, optionally waits on an applied-commit barrier at that TSO, and
// returns a validated Checkpoint. Writer.SetCheckpoint persists it atomically
// in the manifest; checkpoints are monotonic in TSO. Reader.CheckTargetTSO
// rejects a target above the checkpoint high-water (or, without a checkpoint,
// above the newest observed archived TSO). This is a declarative bound, not a
// proven global T_safe.
//
// # Guarantees
//
//   - Every published segment and manifest is fsynced before success.
//   - The manifest is replaced atomically and references only complete
//     segments; the reader rejects missing, truncated, reordered or checksum-
//     damaged segments.
//   - Segments of a range are index-contiguous and hash-chained while retained,
//     except at a recorded GC trim boundary.
//   - ArchiveAdapter rejects gaps, duplicates, term regressions and malformed
//     commit TSOs before calling Writer.Append.
//   - RaftLogSource never returns an entry above the caller's commit index and
//     reports a compacted prefix as a typed gap with the first retained index.
//   - A checkpoint is validated (TSO, capture time, sorted contiguous range
//     map) before it is written and again when the manifest is loaded.
//   - RestoreToTS performs no sink call until the plan has been fully read,
//     verified against segment checksums and checked for checkpoint, archive
//     and barrier coverage.
//   - GC never deletes a segment whose MaxTSO is above a non-zero ProtectTSO
//     and never creates a hole inside a range's retained suffix.
//   - Reader and Planner are safe for concurrent use and share no mutable
//     package-level state. Clock and filesystem effects are per-instance.
//
// # Integration gaps
//
//   - The local Raft reader is a snapshot-per-call adapter, not a tailer: it
//     does not follow leadership/epoch changes, detect truncation between
//     calls, gate TruncatePrefix, or retry a compacted prefix. A caller must
//     archive before truncation and treat *GapError as a hard failure.
//   - The checkpoint is supplied by the caller; this package does not
//     coordinate per-range frontiers, publish a global T_safe, or archive the
//     meta Raft log itself. Range coverage remains a conservative heuristic.
//   - No RPO 0 claim. Unsealed records are lost on a writer crash, archive lag
//     is invisible to the planner, and strict-acknowledgement mode does not
//     exist.
//   - No split/merge lineage, range-epoch conflict detection or multi-writer
//     arbitration. A needed range absent from the checkpoint map fails closed;
//     archive ranges absent from the base are skipped with a warning.
//   - No cross-range transaction reconstruction, MVCC command semantics, lock
//     staging/resolution or imported base chunks. RestoreSink is the caller's
//     executor.
//   - No isolated target provisioning, restore lease, verification drill or
//     atomic cutover. RestoreToTS writes only through the supplied sink; it
//     never contacts or cuts over a serving cluster.
//   - No object-store backend, compression, encryption, KMS or signatures.
//     Checksums detect corruption, not tampering or rollback.
//
// See docs/adr/0006-pitr.md for the full design and the remaining production
// work.
package pitr
