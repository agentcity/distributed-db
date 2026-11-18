package kvserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"
)

// Split/merge admin state machine.
//
// A range's split/merge lifecycle is kept in two places:
//
//   - The replicated data plane (Raft): CommandSplitPrepare / CommandMergePrepare
//     carry an adminOp payload whose Op field is "prepare", "finish" or "clear".
//     Applying one of those entries is the only authority that installs or
//     removes the write-freeze barrier on a replica.
//   - A durable adminRecord stored in the range's own engine prefix. It records
//     the last admin phase, the Raft index that produced it, the freeze
//     generation and enough bounds to finish or abort the operation after a
//     crash. It is written by the apply loop, so a restart can tell a stale
//     prepare (already resolved) from an operation that is genuinely in flight.
//   - A per-source-range adminIntent journal file next to ranges.json. It makes
//     the local map publication crash-consistent: the intent is fsynced before
//     the data cut / map publish and again afterwards, and recovery compares it
//     with the local ranges.json to decide finalize vs abort. This closes the
//     local window only. Meta publication remains a separate coordinator step;
//     this process cannot observe whether meta's range map was applied, so a
//     crash after ranges.json was published (but before the coordinator's meta
//     update) is finalized locally and still depends on the scheduler retry to
//     publish the same layout to meta. There is deliberately no distributed
//     commit across meta and the range hosts in this stage.
//
// Remaining gaps (deliberately not implemented in this file):
//
//   - There is no distributed two-phase commit across the meta range map. The
//     coordinator updates meta only after every replica's KV RPC succeeds, so a
//     crash between the local ranges.json write and the meta update leaves a
//     window where meta still routes to the old layout. Recovery reconciles
//     only this process's ranges.json and data; a coordinator retry drives the
//     rest. Do not treat a local split/merge success as a globally atomic map
//     transition.
//   - Installing a Raft snapshot at runtime replaces the durable adminRecord in
//     the engine, but the in-memory freeze markers/generation timeline are
//     refreshed on the next admin apply or restart, not at install time
//     (snapshot installation lives in snapshot_stream.go, outside this change).
//   - genSteps grows by one entry per completed barrier. Split/merge are rare,
//     so no compaction is performed; a range with an extreme number of admin
//     operations would carry a larger record.
//
// The record is stored under a reserved key in the range prefix. MVCC only
// scans the 'd', 'l' and 'w' column families, so the record can never be
// observed as user data, while Raft snapshots (which stream the whole prefix)
// carry it with the data.
const (
	adminKindSplit = "split"
	adminKindMerge = "merge"

	adminPhasePrepared  = "prepared"
	adminPhaseCut       = "cut"
	adminPhaseFinishing = "finishing"
	adminPhaseFinished  = "finished"
	adminPhaseAborted   = "aborted"

	adminOpPrepare = "prepare"
	adminOpFinish  = "finish"
	adminOpClear   = "clear"

	// adminFinishWait bounds how long a replica-local admin operation waits
	// for the replicated finish before settling its own barrier from durable
	// local state. It must be long enough to ride out a leader election.
	adminFinishWait = 2 * time.Second
)

var (
	// rangeAdminKey holds the durable adminRecord of one range.
	rangeAdminKey = []byte{0x00, 'k', 'v', 'a', 'd', 'm', 'i', 'n'}
	// rangeSplitCopyKey marks a child range prefix whose split data copy is
	// durably complete. The parent consults it during crash recovery to decide
	// whether it may restore the right half.
	rangeSplitCopyKey = []byte{0x00, 'k', 'v', 's', 'p', 'l', 'i', 't', 'c', 'o', 'p', 'y'}
	// splitCopyMarker is the value written under rangeSplitCopyKey.
	splitCopyMarker = []byte("copied")
)

// adminOp is the JSON payload of CommandSplitPrepare / CommandMergePrepare.
type adminOp struct {
	// Op selects the transition. The empty value is treated as "prepare" so
	// payloads written by earlier releases keep working.
	Op string `json:"op,omitempty"`

	SplitKey   []byte `json:"split_key,omitempty"`
	NewRangeID uint64 `json:"new_range_id,omitempty"`
	OldEnd     []byte `json:"old_end,omitempty"`

	Left       uint64 `json:"left,omitempty"`
	Right      uint64 `json:"right,omitempty"`
	RightStart []byte `json:"right_start,omitempty"`
	RightEnd   []byte `json:"right_end,omitempty"`
}

// genStep records the freeze generation established by one prepare entry.
// Replaying the Raft log must advance the generation at exactly the same log
// positions as the original run, or a committed write from before a barrier
// would be rejected after an unclean restart.
type genStep struct {
	Index uint64 `json:"index,omitempty"`
	Gen   uint64 `json:"gen,omitempty"`
}

// adminRecovery is the replicated transition a restarted replica still has to
// append after replaying its Raft log (an abort clear, or the finish of an
// operation whose range-map transition was already published).
type adminRecovery struct {
	typ model.CommandType
	op  adminOp
}

// adminRecord is the durable, per-replica snapshot of the split/merge state.
type adminRecord struct {
	Kind  string `json:"kind,omitempty"`
	Phase string `json:"phase,omitempty"`

	SplitKey   []byte `json:"split_key,omitempty"`
	OldEnd     []byte `json:"old_end,omitempty"`
	NewRangeID uint64 `json:"new_range_id,omitempty"`

	Left       uint64 `json:"left,omitempty"`
	Right      uint64 `json:"right,omitempty"`
	RightStart []byte `json:"right_start,omitempty"`
	RightEnd   []byte `json:"right_end,omitempty"`

	// Gen is the freeze generation. It is monotonic and must not move
	// backwards across a restart: stale write proposals are rejected by
	// comparing their stamped Gen with this value.
	Gen uint64 `json:"gen,omitempty"`
	// FinishCommitted records whether a replicated finish entry (not merely a
	// local settle) was applied. A completed range map without it means a
	// restarted replica must append the finish itself.
	FinishCommitted bool `json:"finish_committed,omitempty"`
	// ClearCommitted records whether a replicated abort/clear entry was
	// applied, so recovery does not append one on every restart.
	ClearCommitted bool `json:"clear_committed,omitempty"`
	// Index is the Raft index of the last admin entry reflected in Phase.
	Index uint64 `json:"index,omitempty"`
	// Gens is the ordered list of prepare-index/generation pairs for this
	// range's retained log. It lets replay reproduce the generation timeline
	// even though the log may start after a snapshot.
	Gens []genStep `json:"gens,omitempty"`
}

// ---------------------------------------------------------------------------
// Durable local admin intent (stage 1: no distributed 2PC)
// ---------------------------------------------------------------------------
//
// adminIntent is a per-source-range journal record written next to
// ranges.json. It is the local counterpart of the meta range-map transition:
// the meta map is only published after every replica returned success, and
// every replica publishes its local ranges.json before returning. Therefore a
// restart can decide safely from the local map file alone:
//
//   - local ranges.json already reflects the intent's target layout -> the
//     transition is final; clean up leftovers and unfreeze;
//   - local ranges.json still reflects the old layout -> the transition was
//     never acknowledged; abort it, restoring the right half from the durable
//     child copy when a split had already cut the parent.
//
// The intent is written with the same temp-file+fsync+rename discipline as
// ranges.json and is advanced before/after the range-map publication, so at
// every crash point one of the two decisions above is valid. This closes the
// local meta<->ranges.json window without pretending the meta map and the
// local map form a distributed transaction: meta publication is still a
// separate step performed by the scheduler after all replicas succeed.
const (
	adminIntentVersion = 1

	adminIntentPhasePrepared          = "prepared"
	adminIntentPhaseCopyDone          = "copy_done"
	adminIntentPhaseMapPublishPending = "map_publish_pending"
	adminIntentPhaseDone              = "done"
	adminIntentPhaseAborted           = "aborted"

	adminIntentFilePrefix = "admin-intent-"
	adminIntentFileSuffix = ".json"
)

// adminIntent is the durable, per-range record of a split/merge local stage.
// The JSON field names are part of the on-disk format; keep them stable.
type adminIntent struct {
	Version int    `json:"version"`
	Op      string `json:"op"`
	Phase   string `json:"phase"`

	// RangeID is the source range: the split parent or the merge left range.
	// RightID is the new child range for a split or the absorbed right range
	// for a merge.
	RangeID uint64 `json:"range_id"`
	RightID uint64 `json:"right_id,omitempty"`

	// Bounds before the operation. End is the source range's old end (nil
	// means the end of the keyspace). SplitKey is the split point; RightStart
	// and RightEnd are the right range's bounds (for a split these mirror
	// SplitKey/End).
	Start      []byte `json:"start,omitempty"`
	End        []byte `json:"end,omitempty"`
	SplitKey   []byte `json:"split_key,omitempty"`
	RightStart []byte `json:"right_start,omitempty"`
	RightEnd   []byte `json:"right_end,omitempty"`

	// Epoch is the source range epoch before the operation (the range version
	// meta currently has) and NewEpoch is the epoch the local map is published
	// with.
	Epoch    uint64 `json:"epoch,omitempty"`
	NewEpoch uint64 `json:"new_epoch,omitempty"`

	// Generation is the freeze generation established by the prepare
	// barrier. Recovery raises the in-memory generation to at least this
	// value so stale writes replayed from the Raft log stay rejected.
	Generation uint64 `json:"generation,omitempty"`

	// UpdatedAt is advisory status/audit data; it is not used for recovery.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// AdminIntentStatus is the public, read-only view of an adminIntent exposed to
// kvctl/metrics. It intentionally aliases the durable record so no conversion
// can silently drop a field.
type AdminIntentStatus = adminIntent

// PendingIntents returns the local split/merge intents that have not reached a
// terminal phase yet. A corrupt on-disk record is reported through the server
// log and omitted here (New fails closed on it before serving); callers that
// need the error should use AdminIntentStatuses.
func (s *Server) PendingIntents() []AdminIntentStatus {
	intents, err := s.AdminIntentStatuses()
	if err != nil {
		if s.log != nil {
			s.log.Warn("read pending admin intents", "error", err)
		}
		return nil
	}
	out := make([]AdminIntentStatus, 0, len(intents))
	for _, in := range intents {
		switch in.Phase {
		case adminIntentPhaseDone, adminIntentPhaseAborted:
			continue
		default:
			out = append(out, in)
		}
	}
	return out
}

// AdminIntentStatuses returns every durable local intent, including terminal
// phases retained until the next recovery pass.
func (s *Server) AdminIntentStatuses() ([]AdminIntentStatus, error) {
	return loadAdminIntents(s.cfg.DataDir)
}

// PendingIntentCount is the metric-friendly form of PendingIntents.
func (s *Server) PendingIntentCount() int { return len(s.PendingIntents()) }

// adminIntentPath returns the per-range intent file path. One file per source
// range avoids a shared journal and keeps per-range updates independent.
func adminIntentPath(dataDir string, rangeID uint64) string {
	return filepath.Join(dataDir, fmt.Sprintf("%s%d%s", adminIntentFilePrefix, rangeID, adminIntentFileSuffix))
}

// adminIntentTempPath is the deterministic temp file used by the atomic write
// below. Admin operations for one range are serialized by adminMu, so a fixed
// name is safe and lets recovery remove a crash-left temp file.
func adminIntentTempPath(dataDir string, rangeID uint64) string {
	return adminIntentPath(dataDir, rangeID) + ".tmp"
}

// validateAdminIntent rejects a record that cannot drive recovery. Unknown
// versions/phases/ops are corrupt for this build: fail closed instead of
// guessing whether to finalize or abort.
func validateAdminIntent(in adminIntent) error {
	if in.Version != 0 && in.Version != adminIntentVersion {
		return fmt.Errorf("unsupported version %d", in.Version)
	}
	if in.RangeID == 0 {
		return fmt.Errorf("range_id is zero")
	}
	switch in.Op {
	case adminKindSplit:
		if in.RightID == 0 {
			return fmt.Errorf("split intent has no right range id")
		}
		if len(in.SplitKey) == 0 {
			return fmt.Errorf("split intent has no split key")
		}
		if in.Start != nil && bytes.Compare(in.SplitKey, in.Start) <= 0 {
			return fmt.Errorf("split key %q is not after range start %q", in.SplitKey, in.Start)
		}
		if in.End != nil && bytes.Compare(in.SplitKey, in.End) >= 0 {
			return fmt.Errorf("split key %q is not before range end %q", in.SplitKey, in.End)
		}
	case adminKindMerge:
		if in.RightID == 0 {
			return fmt.Errorf("merge intent has no right range id")
		}
		if in.RightID == in.RangeID {
			return fmt.Errorf("merge intent has identical left/right range id %d", in.RangeID)
		}
		if in.End != nil && in.RightStart != nil && !bytes.Equal(in.End, in.RightStart) {
			return fmt.Errorf("merge right start %q does not match left end %q", in.RightStart, in.End)
		}
		if in.RightStart != nil && in.RightEnd != nil && bytes.Compare(in.RightEnd, in.RightStart) <= 0 {
			return fmt.Errorf("merge right range [%q,%q) is empty", in.RightStart, in.RightEnd)
		}
	default:
		return fmt.Errorf("unknown op %q", in.Op)
	}
	switch in.Phase {
	case adminIntentPhasePrepared, adminIntentPhaseCopyDone,
		adminIntentPhaseMapPublishPending, adminIntentPhaseDone, adminIntentPhaseAborted:
	default:
		return fmt.Errorf("unknown phase %q", in.Phase)
	}
	return nil
}

// readAdminIntent loads and validates one range's intent. A missing file is
// (zero, false, nil); malformed or semantically invalid content is an error so
// group construction can fail closed.
func readAdminIntent(dataDir string, rangeID uint64) (adminIntent, bool, error) {
	path := adminIntentPath(dataDir, rangeID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return adminIntent{}, false, nil
	}
	if err != nil {
		return adminIntent{}, false, fmt.Errorf("kvserver: read admin intent %s: %w", path, err)
	}
	var in adminIntent
	if err := json.Unmarshal(data, &in); err != nil {
		return adminIntent{}, false, fmt.Errorf("kvserver: corrupt admin intent %s: %w", path, err)
	}
	if err := validateAdminIntent(in); err != nil {
		return adminIntent{}, false, fmt.Errorf("kvserver: corrupt admin intent %s: %w", path, err)
	}
	return in, true, nil
}

// writeAdminIntent atomically replaces one range's intent and fsyncs both the
// file and its directory. Callers that want the old record to remain valid
// until the new one is durable get that for free from rename.
func writeAdminIntent(dataDir string, in adminIntent) error {
	if err := validateAdminIntent(in); err != nil {
		return fmt.Errorf("kvserver: invalid admin intent: %w", err)
	}
	in.Version = adminIntentVersion
	in.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("kvserver: encode admin intent: %w", err)
	}
	if err := writeFileAtomic(adminIntentPath(dataDir, in.RangeID), data, 0o644); err != nil {
		return fmt.Errorf("kvserver: persist admin intent for range %d: %w", in.RangeID, err)
	}
	return nil
}

// clearAdminIntent removes one range's intent (and any crash-left temp file).
// A missing record is success so recovery/cleanup is idempotent.
func clearAdminIntent(dataDir string, rangeID uint64) error {
	path := adminIntentPath(dataDir, rangeID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kvserver: remove admin intent %s: %w", path, err)
	}
	_ = os.Remove(adminIntentTempPath(dataDir, rangeID))
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("kvserver: fsync data dir after admin intent clear: %w", err)
	}
	return nil
}

// loadAdminIntents scans the data directory for per-range intents. Every file
// is validated; a corrupt record aborts the scan so New can fail closed. The
// result is ordered by source range ID for stable status output.
func loadAdminIntents(dataDir string) ([]adminIntent, error) {
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kvserver: list admin intents: %w", err)
	}
	out := make([]adminIntent, 0, 4)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, adminIntentFilePrefix) ||
			!strings.HasSuffix(name, adminIntentFileSuffix) {
			continue
		}
		rawID := strings.TrimSuffix(strings.TrimPrefix(name, adminIntentFilePrefix), adminIntentFileSuffix)
		id, err := strconv.ParseUint(rawID, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("kvserver: corrupt admin intent file name %q", name)
		}
		in, ok, err := readAdminIntent(dataDir, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if in.RangeID != id {
			return nil, fmt.Errorf("kvserver: admin intent %s records range %d", name, in.RangeID)
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RangeID < out[j].RangeID })
	return out, nil
}

// writeFileAtomic writes data to a temp file in the target directory, fsyncs
// it, renames it over path and fsyncs the directory. The rename is the commit
// point: readers see either the old or the new complete file, never a torn
// mix. It is used for both ranges.json and the intent journal.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename durable. Directory fsync is supported on the
// platforms this server targets; callers surface the error because a range-map
// transition must not be acknowledged before the rename is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// persistRangesAtomicLocked is the durable range-map writer used by
// rebuildRangeOrderErr. Caller holds hostMu. Unlike the historical helper it
// fsyncs the temp file and the parent directory, so the map file cannot be
// lost after a split/merge was acknowledged.
func (s *Server) persistRangesAtomicLocked() error {
	data, err := json.Marshal(s.cfg.Ranges)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.cfg.DataDir, "ranges.json"), data, 0o644)
}

// writeAdminIntent / clearAdminIntent are rangeGroup convenience wrappers so
// split/merge code does not have to carry dataDir and rangeID separately.
func (g *rangeGroup) writeAdminIntent(in adminIntent) error {
	return writeAdminIntent(g.dataDir, in)
}

func (g *rangeGroup) clearAdminIntent() error {
	return clearAdminIntent(g.dataDir, g.cfg.ID)
}

// abortAdminIntent records a terminal abort without deleting the record. A
// failed operation may leave partial data; keeping the aborted intent lets the
// next restart run the same idempotent abort/restore path before clearing it.
func (g *rangeGroup) abortAdminIntent(in *adminIntent) {
	in.Phase = adminIntentPhaseAborted
	if err := g.writeAdminIntent(*in); err != nil && g.log != nil {
		g.log.Warn("persist aborted admin intent", "range", g.cfg.ID, "error", err)
	}
}

// currentGeneration returns the freeze generation to stamp into an intent.
func (m *stateMachine) currentGeneration() uint64 {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	return m.freezeGen
}

// hostedGroup returns the group with exactly id, or nil. Unlike groupForID it
// does not translate 0 to the default range, because split/merge treats 0 as
// invalid.
func (s *Server) hostedGroup(id uint64) *rangeGroup {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	return s.groups[id]
}

// lockGroupPair locks the admin mutexes of two ranges in ascending ID order so
// concurrent pair operations cannot deadlock. The returned function unlocks
// them.
func lockGroupPair(a, b *rangeGroup) func() {
	if a.cfg.ID > b.cfg.ID {
		a, b = b, a
	}
	a.adminMu.Lock()
	b.adminMu.Lock()
	return func() {
		b.adminMu.Unlock()
		a.adminMu.Unlock()
	}
}

// lockRangePair resolves and locks two hosted ranges for an admin operation.
// The pair is locked in ascending ID order, and the lookup is repeated if a
// range disappears while waiting, so a concurrent merge cannot leave us
// holding a stale group. A missing right range is returned as nil with the
// left range locked; callers handle that idempotent-retry case.
func (s *Server) lockRangePair(leftID, rightID uint64) (left, right *rangeGroup, unlock func(), err error) {
	for attempt := 0; attempt < 5; attempt++ {
		left = s.hostedGroup(leftID)
		if left == nil {
			return nil, nil, nil, fmt.Errorf("%w: left id %d", ErrRangeNotFound, leftID)
		}
		right = s.hostedGroup(rightID)
		if right == nil {
			left.adminMu.Lock()
			if s.hostedGroup(leftID) == left {
				return left, nil, func() { left.adminMu.Unlock() }, nil
			}
			left.adminMu.Unlock()
			continue
		}
		release := lockGroupPair(left, right)
		if s.hostedGroup(leftID) == left && s.hostedGroup(rightID) == right {
			return left, right, release, nil
		}
		release()
	}
	return nil, nil, nil, fmt.Errorf("%w: range pair %d/%d changed while locking", ErrRangeNotFound, leftID, rightID)
}

// markMerged records that this range absorbed rightID. Callers hold adminMu.
func (g *rangeGroup) markMerged(rightID uint64) {
	if g.mergedAway == nil {
		g.mergedAway = make(map[uint64]struct{}, 1)
	}
	g.mergedAway[rightID] = struct{}{}
}

// merged reports whether this range already absorbed rightID. Callers hold
// adminMu.
func (g *rangeGroup) merged(rightID uint64) bool {
	_, ok := g.mergedAway[rightID]
	return ok
}

// ---------------------------------------------------------------------------
// Durable admin record
// ---------------------------------------------------------------------------

// readAdminRecord loads the durable admin record through the range prefix.
func (m *stateMachine) readAdminRecord() (adminRecord, bool, error) {
	if m.engine == nil {
		return adminRecord{}, false, nil
	}
	data, err := m.engine.Get(rangeAdminKey)
	if errors.Is(err, storage.ErrNotFound) {
		return adminRecord{}, false, nil
	}
	if err != nil {
		return adminRecord{}, false, fmt.Errorf("kvserver: read admin state: %w", err)
	}
	var rec adminRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return adminRecord{}, false, fmt.Errorf("kvserver: decode admin state: %w", err)
	}
	return rec, true, nil
}

// writeAdminRecord durably replaces the record. An empty Kind removes it.
func (m *stateMachine) writeAdminRecord(rec adminRecord) error {
	if m.engine == nil {
		return nil
	}
	if rec.Kind == "" {
		return m.engine.Delete(rangeAdminKey)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := m.engine.Set(rangeAdminKey, data); err != nil {
		return fmt.Errorf("kvserver: persist admin state: %w", err)
	}
	return nil
}

// recordLocked builds the record from the in-memory admin fields. Caller holds
// splitMu.
func (m *stateMachine) recordLocked() adminRecord {
	return adminRecord{
		Kind:  m.adminKind,
		Phase: m.adminPhase,
		// adminSplitKey survives the finish (which clears the active freeze
		// marker) so recovery can compare it with the persisted range end.
		SplitKey:        cloneBytes(m.adminSplitKey),
		OldEnd:          cloneBytes(m.splitOldEnd),
		NewRangeID:      m.splitNewID,
		Left:            m.mergeLeft,
		Right:           m.mergeRight,
		RightStart:      cloneBytes(m.mergeRightStart),
		RightEnd:        cloneBytes(m.mergeRightEnd),
		Gen:             m.freezeGen,
		FinishCommitted: m.finishCommitted,
		ClearCommitted:  m.clearCommitted,
		Index:           m.adminIndex,
		Gens:            append([]genStep(nil), m.genSteps...),
	}
}

// persistLocked writes the current admin record. Caller holds splitMu.
func (m *stateMachine) persistLocked() error {
	return m.writeAdminRecord(m.recordLocked())
}

// loadAdminState restores the durable record at group construction time. It is
// called before Raft starts, so no lock is contended; the in-memory markers are
// deliberately left clear until recovery decides what to do with the record.
func (m *stateMachine) loadAdminState() error {
	rec, ok, err := m.readAdminRecord()
	if err != nil || !ok {
		return err
	}
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.adminKind = rec.Kind
	m.adminPhase = rec.Phase
	m.adminIndex = rec.Index
	m.finishCommitted = rec.FinishCommitted
	m.clearCommitted = rec.ClearCommitted
	m.finishApplied = rec.FinishCommitted
	m.genSteps = append([]genStep(nil), rec.Gens...)
	m.genCursor = 0
	// The generation before the earliest retained prepare is one less than the
	// first prepare's recorded generation. Starting there (rather than at the
	// record's current Gen) makes replay reproduce the timeline: writes that
	// committed before the barrier keep their old generation and are accepted.
	// With no recorded prepare, the range epoch already carries the generation
	// (the original newRangeGroup initialisation).
	if len(m.genSteps) > 0 && m.genSteps[0].Gen > 0 {
		m.freezeGen = m.genSteps[0].Gen - 1
	}
	m.adminSplitKey = cloneBytes(rec.SplitKey)
	m.splitNewID = rec.NewRangeID
	m.splitOldEnd = cloneBytes(rec.OldEnd)
	m.mergeRightStart = cloneBytes(rec.RightStart)
	m.mergeRightEnd = cloneBytes(rec.RightEnd)
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.finishApplied = false
	m.abortPending = false
	m.preparedSplitKey = nil
	m.preparedMergeLeft, m.preparedMergeRight = 0, 0
	return nil
}

// advanceAdminGen moves freezeGen to the generation recorded for the last gen
// step at or before index. It is called once per applied entry, in Raft log
// order, so replay reproduces the generation timeline. Caller holds splitMu.
func (m *stateMachine) advanceGenLocked(index uint64) {
	if index == 0 {
		return
	}
	for m.genCursor < len(m.genSteps) && m.genSteps[m.genCursor].Index <= index {
		m.freezeGen = m.genSteps[m.genCursor].Gen
		m.genCursor++
	}
}

// hasGenStepLocked reports whether a generation step already exists for the
// exact entry index (true while replaying that prepare).
func (m *stateMachine) hasGenStepLocked(index uint64) bool {
	if index == 0 {
		return false
	}
	for i := range m.genSteps {
		if m.genSteps[i].Index == index {
			return true
		}
	}
	return false
}

// recordGenStepLocked appends the generation established by a new barrier at
// index. Local (unindexed) transitions are not recorded because they are never
// replayed from a Raft log.
func (m *stateMachine) recordGenStepLocked(index uint64) {
	if index == 0 {
		return
	}
	m.genSteps = append(m.genSteps, genStep{Index: index, Gen: m.freezeGen})
	m.genCursor = len(m.genSteps)
}

// setRecovery records the replicated admin entry this restarted replica must
// append after replay (an abort clear or a deferred finish).
func (m *stateMachine) setRecovery(typ model.CommandType, op adminOp) {
	m.splitMu.Lock()
	m.recovery = &adminRecovery{typ: typ, op: op}
	m.splitMu.Unlock()
}

// clearRecovery drops a pending recovery transition once an admin entry has
// resolved the operation.
func (m *stateMachine) clearRecoveryLocked() { m.recovery = nil }

// recoveryEntry returns the pending recovery transition, if any.
func (m *stateMachine) recoveryEntry() (adminRecovery, bool) {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	if m.recovery == nil {
		return adminRecovery{}, false
	}
	return *m.recovery, true
}

// ---------------------------------------------------------------------------
// Replicated transitions. These are called only from the apply loop (except
// the local host-only helpers, which take applyGate before calling).
// ---------------------------------------------------------------------------

// prepareSplitPlan installs a split freeze barrier and records the child plan.
// It is idempotent for a retry of the same split key: a duplicate prepare does
// not bump the freeze generation.
func (m *stateMachine) prepareSplitPlan(key []byte, newID uint64, oldEnd []byte, index uint64) error {
	if len(key) == 0 {
		return fmt.Errorf("%w: split key is empty", ErrRangeFrozen)
	}
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if m.mergeLeft != 0 || m.mergeRight != 0 {
		return fmt.Errorf("%w: merge %d/%d is already prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}
	if len(m.splitKey) > 0 {
		if !bytes.Equal(m.splitKey, key) {
			return fmt.Errorf("%w: split key %q is already prepared", ErrRangeFrozen, m.splitKey)
		}
		// Retry of the same prepare: refresh the plan without double-freezing.
		if newID != 0 {
			m.splitNewID = newID
		}
		if oldEnd != nil {
			m.splitOldEnd = cloneBytes(oldEnd)
		}
		if index != 0 {
			m.adminIndex = maxUint64(m.adminIndex, index)
		}
		m.adminKind, m.adminPhase = adminKindSplit, adminPhasePrepared
		m.preparedSplitKey = cloneBytes(key)
		m.adminSplitKey = cloneBytes(key)
		return m.persistLocked()
	}

	prevGen, prevIndex := m.freezeGen, m.adminIndex
	prevCursor := m.genCursor
	m.splitKey = cloneBytes(key)
	m.adminSplitKey = cloneBytes(key)
	m.splitNewID = newID
	m.splitOldEnd = cloneBytes(oldEnd)
	m.preparedSplitKey = cloneBytes(key)
	// During replay the generation for this index is already in genSteps; do
	// not bump twice.
	if !m.hasGenStepLocked(index) {
		m.freezeGen++
		m.recordGenStepLocked(index)
	}
	if index != 0 {
		m.adminIndex = maxUint64(m.adminIndex, index)
	}
	m.adminKind, m.adminPhase = adminKindSplit, adminPhasePrepared
	m.finishCommitted = false
	m.clearCommitted = false
	if err := m.persistLocked(); err != nil {
		// Do not install a freeze that cannot survive a restart.
		m.splitKey = nil
		m.splitNewID = 0
		m.splitOldEnd = nil
		m.freezeGen = prevGen
		m.adminIndex = prevIndex
		m.genCursor = prevCursor
		return err
	}
	return nil
}

// prepareMergePlan installs a merge freeze barrier on one of the two
// participating ranges. It is idempotent for a retry of the same pair.
func (m *stateMachine) prepareMergePlan(left, right uint64, rightStart, rightEnd []byte, index uint64) error {
	if left == 0 || right == 0 || left == right {
		return fmt.Errorf("%w: invalid merge pair %d/%d", ErrRangeFrozen, left, right)
	}
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if len(m.splitKey) > 0 {
		return fmt.Errorf("%w: split key %q is already prepared", ErrRangeFrozen, m.splitKey)
	}
	if m.mergeLeft != 0 || m.mergeRight != 0 {
		if m.mergeLeft == left && m.mergeRight == right {
			if index != 0 {
				m.adminIndex = maxUint64(m.adminIndex, index)
			}
			if rightStart != nil {
				m.mergeRightStart = cloneBytes(rightStart)
			}
			if rightEnd != nil {
				m.mergeRightEnd = cloneBytes(rightEnd)
			}
			m.adminKind, m.adminPhase = adminKindMerge, adminPhasePrepared
			m.preparedMergeLeft, m.preparedMergeRight = left, right
			return m.persistLocked()
		}
		return fmt.Errorf("%w: merge %d/%d is already prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}

	prevGen, prevIndex := m.freezeGen, m.adminIndex
	prevCursor := m.genCursor
	m.mergeLeft, m.mergeRight = left, right
	m.mergeRightStart = cloneBytes(rightStart)
	m.mergeRightEnd = cloneBytes(rightEnd)
	m.preparedMergeLeft, m.preparedMergeRight = left, right
	if !m.hasGenStepLocked(index) {
		m.freezeGen++
		m.recordGenStepLocked(index)
	}
	if index != 0 {
		m.adminIndex = maxUint64(m.adminIndex, index)
	}
	m.adminKind, m.adminPhase = adminKindMerge, adminPhasePrepared
	m.finishCommitted = false
	m.clearCommitted = false
	if err := m.persistLocked(); err != nil {
		m.mergeLeft, m.mergeRight = 0, 0
		m.mergeRightStart, m.mergeRightEnd = nil, nil
		m.freezeGen = prevGen
		m.adminIndex = prevIndex
		m.genCursor = prevCursor
		return err
	}
	return nil
}

// applySplitFinish applies a replicated split-finish entry. If this replica's
// local data move is still running, the entry only records that the finish is
// committed; the freeze is cleared by markLocalAdminDone when the copy and the
// range-map cut are durable. Otherwise the freeze is cleared immediately.
func (m *stateMachine) applySplitFinish(op adminOp, index uint64) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if m.adminKind == adminKindMerge && (m.mergeLeft != 0 || m.mergeRight != 0) {
		return fmt.Errorf("%w: split finish while merge %d/%d is prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}
	if len(op.SplitKey) > 0 && len(m.splitKey) > 0 && !bytes.Equal(m.splitKey, op.SplitKey) {
		return fmt.Errorf("%w: split finish key %q does not match prepared %q",
			ErrRangeFrozen, op.SplitKey, m.splitKey)
	}
	m.adminKind = adminKindSplit
	if len(op.SplitKey) > 0 {
		m.splitKey = cloneBytes(op.SplitKey)
		m.adminSplitKey = cloneBytes(op.SplitKey)
	}
	if op.NewRangeID != 0 {
		m.splitNewID = op.NewRangeID
	}
	m.finishApplied = true
	m.finishCommitted = true
	m.clearCommitted = false
	m.clearRecoveryLocked()
	if index != 0 {
		m.adminIndex = maxUint64(m.adminIndex, index)
	}
	if m.localKind == adminKindSplit && !m.localDone {
		// Data movement is still in flight on this replica; clearing the
		// barrier now would let a write slip between the copy and the cut.
		m.adminPhase = adminPhaseFinishing
		return m.persistLocked()
	}
	m.adminPhase = adminPhaseFinished
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	return m.persistLocked()
}

// applyMergeFinish applies a replicated merge-finish entry on the left range.
func (m *stateMachine) applyMergeFinish(op adminOp, index uint64) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if len(m.splitKey) > 0 {
		return fmt.Errorf("%w: merge finish while split key %q is prepared",
			ErrRangeFrozen, m.splitKey)
	}
	m.adminKind = adminKindMerge
	if op.Left != 0 {
		m.mergeLeft = op.Left
	}
	if op.Right != 0 {
		m.mergeRight = op.Right
	}
	if len(op.RightStart) > 0 {
		m.mergeRightStart = cloneBytes(op.RightStart)
	}
	if len(op.RightEnd) > 0 {
		m.mergeRightEnd = cloneBytes(op.RightEnd)
	}
	m.finishApplied = true
	m.finishCommitted = true
	m.clearCommitted = false
	m.clearRecoveryLocked()
	if index != 0 {
		m.adminIndex = maxUint64(m.adminIndex, index)
	}
	if m.localKind == adminKindMerge && !m.localDone {
		m.adminPhase = adminPhaseFinishing
		return m.persistLocked()
	}
	m.adminPhase = adminPhaseFinished
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	return m.persistLocked()
}

// applyAdminClear applies a replicated clear/abort entry. A clear that arrives
// while this replica is still moving data is remembered; the local operation
// will persist the abort when it stops.
func (m *stateMachine) applyAdminClear(kind string, index uint64) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.clearRecoveryLocked()
	m.clearCommitted = true
	m.finishCommitted = false
	if index != 0 {
		m.adminIndex = maxUint64(m.adminIndex, index)
	}
	if m.localKind != "" && !m.localDone {
		m.abortPending = true
		return m.persistLocked()
	}
	if kind != "" {
		m.adminKind = kind
	}
	m.adminPhase = adminPhaseAborted
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.finishApplied = false
	m.abortPending = false
	return m.persistLocked()
}

// ---------------------------------------------------------------------------
// Replica-local progress. These are in-memory only: the durable evidence that
// a local split/merge completed is the persisted range map plus the record.
// ---------------------------------------------------------------------------

// beginLocalSplit marks that this replica is about to move data for a split.
// The marker prevents a replicated finish from clearing the freeze during the
// window between applying prepare and finishing the local copy.
func (m *stateMachine) beginLocalSplit(key []byte, newID uint64, oldEnd []byte) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if len(m.splitKey) > 0 && !bytes.Equal(m.splitKey, key) {
		return fmt.Errorf("%w: split key %q is already prepared", ErrRangeFrozen, m.splitKey)
	}
	if m.mergeLeft != 0 || m.mergeRight != 0 {
		return fmt.Errorf("%w: merge %d/%d is already prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}
	if m.splitNewID != 0 && newID != 0 && m.splitNewID != newID {
		return fmt.Errorf("%w: split new range id %d already prepared", ErrRangeFrozen, m.splitNewID)
	}
	m.localKind = adminKindSplit
	m.localDone = false
	m.finishApplied = false
	m.finishCommitted = false
	m.abortPending = false
	m.recovery = nil
	if newID != 0 {
		m.splitNewID = newID
	}
	if oldEnd != nil {
		m.splitOldEnd = cloneBytes(oldEnd)
	}
	return nil
}

// beginLocalMerge marks that this replica is about to move data for a merge.
func (m *stateMachine) beginLocalMerge(left, right uint64, rightStart, rightEnd []byte) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if len(m.splitKey) > 0 {
		return fmt.Errorf("%w: split key %q is already prepared", ErrRangeFrozen, m.splitKey)
	}
	if m.mergeLeft != 0 && (m.mergeLeft != left || m.mergeRight != right) {
		return fmt.Errorf("%w: merge %d/%d is already prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}
	// Only replica-local progress is set here. The freeze marker itself must
	// come from the replicated prepare: installing it in the RPC goroutine
	// could reject an already-committed write that Raft has not applied yet.
	m.localKind = adminKindMerge
	m.localDone = false
	m.finishApplied = false
	m.finishCommitted = false
	m.abortPending = false
	m.recovery = nil
	m.mergeRightStart = cloneBytes(rightStart)
	m.mergeRightEnd = cloneBytes(rightEnd)
	return nil
}

// markSplitCut persists that the parent's right half has been removed. It is
// called after the data cut; the child copy marker is the evidence that the
// cut can be undone if the process crashes before the range map is published.
func (m *stateMachine) markSplitCut() error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if len(m.splitKey) == 0 || m.adminKind != adminKindSplit {
		return nil
	}
	m.adminPhase = adminPhaseCut
	return m.persistLocked()
}

// markLocalAdminDone records that this replica's copy and range-map update are
// durable. If a replicated finish already arrived, the freeze is cleared now;
// otherwise the range stays frozen until it does (or until recovery).
func (m *stateMachine) markLocalAdminDone() error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.localDone = true
	if m.abortPending {
		m.adminPhase = adminPhaseAborted
		m.splitKey = nil
		m.mergeLeft, m.mergeRight = 0, 0
		m.localKind = ""
		m.localDone = false
		m.finishApplied = false
		m.finishCommitted = false
		m.abortPending = false
		return m.persistLocked()
	}
	if !m.finishApplied {
		return nil
	}
	m.adminPhase = adminPhaseFinished
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	return m.persistLocked()
}

// abortLocalAdmin clears replica-local progress after a failed local operation.
// The caller is responsible for proposing a replicated clear when it is the
// leader.
func (m *stateMachine) abortLocalAdmin() {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.localKind = ""
	m.localDone = false
	m.finishApplied = false
	m.abortPending = false
}

// adminFinishApplied reports whether a replicated finish (or the equivalent
// recovery decision) has been applied to this replica.
func (m *stateMachine) adminFinishApplied() bool {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	return m.finishApplied
}

// forceFinishedSplit marks a locally completed split finished without waiting
// for a replicated finish. It is used for the host-local API and as the
// idempotent retry/unfreeze path once the child group is already published.
func (m *stateMachine) forceFinishedSplit() {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.adminKind = adminKindSplit
	m.adminPhase = adminPhaseFinished
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.abortPending = false
	m.finishApplied = true
	_ = m.persistLocked()
}

// forceFinishedMerge is forceFinishedSplit for a merge that already removed the
// right range from the local map.
func (m *stateMachine) forceFinishedMerge() {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	m.adminKind = adminKindMerge
	m.adminPhase = adminPhaseFinished
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.abortPending = false
	m.finishApplied = true
	_ = m.persistLocked()
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// recoverAdminState reconciles the durable admin record with the persisted
// range map during group construction (before Raft starts and before any
// client can reach the range). A split/merge whose range-map transition was
// not published is aborted: copied data is removed (or, for a split whose
// parent cut already happened, restored from the durable child copy) and the
// range is left writable. A transition that was published is marked finished.
//
// The local adminIntent is validated first (a corrupt record fails closed) and
// then reconciled after the replicated admin record. The two records describe
// the same operation from different angles: the admin record is replicated and
// owns the freeze/generation timeline, while the intent is the local map-
// publication journal. Running both keeps old crash images (no intent) working
// and makes repeated recovery idempotent.
func recoverAdminState(g *rangeGroup) error {
	intent, hasIntent, err := readAdminIntent(g.dataDir, g.cfg.ID)
	if err != nil {
		return err
	}
	rec, ok, err := g.sm.readAdminRecord()
	if err != nil {
		return err
	}
	if !ok {
		// A published child range may still carry the split copy marker if the
		// parent crashed between publishing the map and deleting it. It has
		// served its recovery purpose once the child is being constructed.
		if marker, err := g.engine.Get(rangeSplitCopyKey); err == nil && len(marker) > 0 {
			_ = g.engine.Delete(rangeSplitCopyKey)
		}
	} else {
		switch rec.Kind {
		case adminKindSplit:
			if err := recoverSplit(g, rec); err != nil {
				return err
			}
		case adminKindMerge:
			if err := recoverMerge(g, rec); err != nil {
				return err
			}
		default:
			// Unknown/newer record: clear it rather than freezing the range.
			if err := g.sm.recoverAdminAborted(rec); err != nil {
				return err
			}
		}
	}
	if hasIntent {
		return recoverAdminIntent(g, intent)
	}
	return nil
}

// recoverAdminIntent reconciles the local intent with the just-loaded local
// range map. The decision is made from the map file, which every replica makes
// durable before acknowledging a split/merge to the meta scheduler:
//
//   - target layout present -> finalize (clean leftovers, unfreeze);
//   - old layout present    -> abort (restore the right half for a split,
//     remove copied records for a merge, discard scratch state, unfreeze).
//
// Every step is idempotent. The terminal phase is fsynced before the journal
// file is removed, so a crash during recovery merely repeats the same path.
func recoverAdminIntent(g *rangeGroup, in adminIntent) error {
	if in.RangeID != g.cfg.ID {
		return fmt.Errorf("kvserver: admin intent %s for range %d found while recovering range %d",
			adminIntentPath(g.dataDir, in.RangeID), in.RangeID, g.cfg.ID)
	}
	// A crash during the atomic write may have left the temp file behind; the
	// committed file is authoritative.
	_ = os.Remove(adminIntentTempPath(g.dataDir, g.cfg.ID))

	finished := false
	switch in.Op {
	case adminKindSplit:
		published := len(in.SplitKey) > 0 && bytes.Equal(g.cfg.End, in.SplitKey)
		if published {
			finished = true
			// The child is live. Only its recovery marker and any scratch raft
			// log left by a failed earlier attempt are disposable.
			child := storage.Prefix(g.shared, rangePrefix(in.RightID))
			if marker, err := child.Get(rangeSplitCopyKey); err == nil && len(marker) > 0 {
				_ = child.Delete(rangeSplitCopyKey)
			}
		} else {
			// The parent may already have cut the right half. The durable child
			// copy is the only complete copy in that case; restore it before
			// discarding the scratch prefix.
			copied, err := childCopyComplete(g.shared, in.RightID)
			if err != nil {
				return err
			}
			if copied {
				if err := copyPrefixRecords(g.shared, in.RightID, g.cfg.ID, true); err != nil {
					return fmt.Errorf("kvserver: recover split intent for range %d: restore right half: %w", g.cfg.ID, err)
				}
			}
			discardScratchChild(g.dataDir, g.shared, in.RightID)
		}
	case adminKindMerge:
		// RightEnd may be nil for a merge that absorbs the end of the keyspace;
		// bytes.Equal still distinguishes the old left end (non-nil, equal to
		// RightStart) from the published wider end (nil).
		published := in.RightID != 0 && bytes.Equal(g.cfg.End, in.RightEnd)
		if published {
			finished = true
			// The left range owns the merged keyspace; the right prefix/log are
			// redundant and must be removed idempotently.
			if err := deletePrefixRecords(g.shared, in.RightID); err != nil {
				return fmt.Errorf("kvserver: recover merge intent for range %d: discard right prefix: %w", g.cfg.ID, err)
			}
			discardRangeRaftLog(g.dataDir, in.RightID)
			g.markMerged(in.RightID)
		} else {
			// The merge was not published. Remove records this left range
			// copied from the right range; the right range still owns them.
			if err := g.store.DeleteRange(in.RightStart, in.RightEnd); err != nil {
				return fmt.Errorf("kvserver: recover merge intent for range %d: remove copied right half: %w", g.cfg.ID, err)
			}
		}
	default:
		return fmt.Errorf("kvserver: admin intent for range %d has unknown op %q", g.cfg.ID, in.Op)
	}

	if err := g.sm.recoverIntentState(in, finished); err != nil {
		return err
	}
	if finished {
		in.Phase = adminIntentPhaseDone
		if g.log != nil {
			g.log.Info("finalized published admin intent after restart",
				"range", g.cfg.ID, "op", in.Op, "right", in.RightID)
		}
	} else {
		in.Phase = adminIntentPhaseAborted
		if g.log != nil {
			g.log.Warn("aborted unpublished admin intent after restart",
				"range", g.cfg.ID, "op", in.Op, "right", in.RightID, "phase", in.Phase)
		}
	}
	if err := g.writeAdminIntent(in); err != nil {
		return err
	}
	return g.clearAdminIntent()
}

// recoverIntentState writes the terminal local admin record implied by the
// intent and clears every freeze marker. It raises the freeze generation to at
// least the value the intent observed, so stale writes replayed from the Raft
// log remain rejected after a restart.
func (m *stateMachine) recoverIntentState(in adminIntent, finished bool) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	gen := in.Generation
	if in.NewEpoch > 0 && in.NewEpoch-1 > gen {
		gen = in.NewEpoch - 1
	}
	if gen > m.freezeGen {
		m.freezeGen = gen
	}
	m.adminKind = in.Op
	if finished {
		m.adminPhase = adminPhaseFinished
		m.finishApplied = true
	} else {
		m.adminPhase = adminPhaseAborted
		m.finishApplied = false
	}
	switch in.Op {
	case adminKindSplit:
		m.adminSplitKey = cloneBytes(in.SplitKey)
		m.splitNewID = in.RightID
		m.splitOldEnd = cloneBytes(in.End)
	case adminKindMerge:
		m.mergeRightStart = cloneBytes(in.RightStart)
		m.mergeRightEnd = cloneBytes(in.RightEnd)
	}
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.abortPending = false
	return m.persistLocked()
}

func recoverSplit(g *rangeGroup, rec adminRecord) error {
	completed := len(rec.SplitKey) > 0 && bytes.Equal(g.cfg.End, rec.SplitKey)
	if completed {
		// The range map was published before the crash. The child range owns
		// the right half; do not touch its prefix. If no replicated finish was
		// applied before the crash, replay of the prepare would freeze the
		// range again, so queue a finish to append after replay.
		if err := g.sm.recoverAdminFinished(rec); err != nil {
			return err
		}
		if !rec.FinishCommitted {
			g.sm.setRecovery(model.CommandSplitPrepare, adminOp{
				Op: adminOpFinish, SplitKey: rec.SplitKey, NewRangeID: rec.NewRangeID,
			})
		}
		return nil
	}
	// The map was not published. Abort idempotently. When the durable copy
	// marker is present the parent cut may already have removed the right
	// half, so restore it from the child prefix before discarding it.
	if copied, err := childCopyComplete(g.shared, rec.NewRangeID); err != nil {
		return err
	} else if copied {
		if err := copyPrefixRecords(g.shared, rec.NewRangeID, g.cfg.ID, true); err != nil {
			return fmt.Errorf("kvserver: recover split range %d: restore right half: %w", g.cfg.ID, err)
		}
	}
	discardScratchChild(g.dataDir, g.shared, rec.NewRangeID)
	if rec.Phase != adminPhaseAborted {
		g.log.Warn("aborted unfinished split after restart",
			"range", g.cfg.ID, "split_key", string(rec.SplitKey), "new_range", rec.NewRangeID, "phase", rec.Phase)
	}
	if err := g.sm.recoverAdminAborted(rec); err != nil {
		return err
	}
	// Replaying the prepare (if it is still in the log) re-installs the
	// freeze; append a clear so the aborted operation resolves on every
	// replica. A committed clear needs no second one.
	if !rec.ClearCommitted {
		g.sm.setRecovery(model.CommandSplitPrepare, adminOp{
			Op: adminOpClear, SplitKey: rec.SplitKey, NewRangeID: rec.NewRangeID,
		})
	}
	return nil
}

func recoverMerge(g *rangeGroup, rec adminRecord) error {
	if rec.Right == 0 || rec.Right == g.cfg.ID {
		// This replica is the right-hand range. If it is still in the map the
		// merge was not published; its data is untouched. Replaying the
		// prepare would freeze it, so queue a clear.
		if err := g.sm.recoverAdminAborted(rec); err != nil {
			return err
		}
		if !rec.ClearCommitted {
			g.sm.setRecovery(model.CommandMergePrepare, adminOp{Op: adminOpClear, Left: rec.Left, Right: rec.Right})
		}
		return nil
	}
	// RightEnd is legitimately nil when the merged right range is unbounded;
	// compare it directly so a published merge of the last range is finalized.
	merged := rec.Right != 0 && bytes.Equal(g.cfg.End, rec.RightEnd)
	if merged {
		// The left range already covers the right end: the merge completed on
		// this host. The right prefix is redundant; remove it idempotently.
		if err := deletePrefixRecords(g.shared, rec.Right); err != nil {
			return fmt.Errorf("kvserver: recover merge range %d: discard right prefix: %w", g.cfg.ID, err)
		}
		discardRangeRaftLog(g.dataDir, rec.Right)
		g.markMerged(rec.Right)
		if err := g.sm.recoverAdminFinished(rec); err != nil {
			return err
		}
		if !rec.FinishCommitted {
			g.sm.setRecovery(model.CommandMergePrepare, adminOp{
				Op: adminOpFinish, Left: rec.Left, Right: rec.Right,
				RightStart: rec.RightStart, RightEnd: rec.RightEnd,
			})
		}
		return nil
	}
	// The merge was not published. Remove any records this range copied from
	// the right-hand range; the right range still owns them.
	if err := g.store.DeleteRange(rec.RightStart, rec.RightEnd); err != nil {
		return fmt.Errorf("kvserver: recover merge range %d: remove copied right half: %w", g.cfg.ID, err)
	}
	if rec.Phase != adminPhaseAborted {
		g.log.Warn("aborted unfinished merge after restart",
			"left", g.cfg.ID, "right", rec.Right, "phase", rec.Phase)
	}
	if err := g.sm.recoverAdminAborted(rec); err != nil {
		return err
	}
	if !rec.ClearCommitted {
		g.sm.setRecovery(model.CommandMergePrepare, adminOp{
			Op: adminOpClear, Left: rec.Left, Right: rec.Right,
			RightStart: rec.RightStart, RightEnd: rec.RightEnd,
		})
	}
	return nil
}

// recoverAdminFinished marks a published transition as finished and unfrozen.
func (m *stateMachine) recoverAdminFinished(rec adminRecord) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if rec.Gen > m.freezeGen {
		m.freezeGen = rec.Gen
	}
	m.adminKind = rec.Kind
	m.adminPhase = adminPhaseFinished
	m.adminSplitKey = cloneBytes(rec.SplitKey)
	m.adminIndex = maxUint64(m.adminIndex, rec.Index)
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.abortPending = false
	m.finishApplied = true
	return m.persistLocked()
}

// recoverAdminAborted resolves an unpublished transition as aborted, keeping
// the freeze generation so stale write proposals replayed from the Raft log are
// still rejected.
func (m *stateMachine) recoverAdminAborted(rec adminRecord) error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if rec.Gen > m.freezeGen {
		m.freezeGen = rec.Gen
	}
	m.adminKind = rec.Kind
	m.adminPhase = adminPhaseAborted
	m.adminSplitKey = cloneBytes(rec.SplitKey)
	m.adminIndex = maxUint64(m.adminIndex, rec.Index)
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.localKind = ""
	m.localDone = false
	m.abortPending = false
	m.finishApplied = false
	return m.persistLocked()
}

// ---------------------------------------------------------------------------
// Prefix helpers used by recovery and cleanup. They operate on the unprefixed
// shared engine so a range that is not (or no longer) hosted can be cleaned.
// ---------------------------------------------------------------------------

func childCopyComplete(shared storage.Engine, id uint64) (bool, error) {
	if id == 0 || shared == nil {
		return false, nil
	}
	child := storage.Prefix(shared, rangePrefix(id))
	if _, err := child.Get(rangeSplitCopyKey); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isMVCCKey reports whether key belongs to one of the MVCC column families.
func isMVCCKey(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	switch key[0] {
	case 'd', 'l', 'w':
		return true
	default:
		return false
	}
}

// copyPrefixRecords copies records from one range prefix to another. When
// mvccOnly is set, the admin/copy control keys are skipped so a child prefix's
// recovery marker is never mistaken for the parent's admin record.
func copyPrefixRecords(shared storage.Engine, fromID, toID uint64, mvccOnly bool) error {
	if shared == nil || fromID == 0 || toID == 0 {
		return nil
	}
	src := storage.Prefix(shared, rangePrefix(fromID))
	dst := storage.Prefix(shared, rangePrefix(toID))
	it, err := src.NewIter(nil, nil)
	if err != nil {
		return err
	}
	defer it.Close()

	batch := dst.NewBatch()
	pending := 0
	commit := func() error {
		if pending == 0 {
			return nil
		}
		if err := batch.Commit(); err != nil {
			return err
		}
		pending = 0
		batch = dst.NewBatch()
		return nil
	}
	for it.Valid() {
		key := append([]byte(nil), it.Key()...)
		value := append([]byte(nil), it.Value()...)
		if !mvccOnly || isMVCCKey(key) {
			if err := batch.Set(key, value); err != nil {
				batch.Close()
				return err
			}
			pending++
		}
		it.Next()
		if pending >= 1024 {
			if err := commit(); err != nil {
				batch.Close()
				return err
			}
		}
	}
	if err := it.Error(); err != nil {
		batch.Close()
		return err
	}
	err = commit()
	batch.Close()
	return err
}

// deletePrefixRecords removes every key under a range prefix in bounded
// batches.
func deletePrefixRecords(shared storage.Engine, id uint64) error {
	if shared == nil || id == 0 {
		return nil
	}
	engine := storage.Prefix(shared, rangePrefix(id))
	it, err := engine.NewIter(nil, nil)
	if err != nil {
		return err
	}
	defer it.Close()
	batch := engine.NewBatch()
	pending := 0
	commit := func() error {
		if pending == 0 {
			return nil
		}
		if err := batch.Commit(); err != nil {
			return err
		}
		pending = 0
		batch = engine.NewBatch()
		return nil
	}
	for it.Valid() {
		key := append([]byte(nil), it.Key()...)
		if err := batch.Delete(key); err != nil {
			batch.Close()
			return err
		}
		pending++
		it.Next()
		if pending >= 1024 {
			if err := commit(); err != nil {
				batch.Close()
				return err
			}
		}
	}
	if err := it.Error(); err != nil {
		batch.Close()
		return err
	}
	err = commit()
	batch.Close()
	return err
}

// discardScratchChild removes a child range that was never published in the
// range map: its engine records and its raft log directory.
func discardScratchChild(dataDir string, shared storage.Engine, id uint64) {
	if id == 0 {
		return
	}
	_ = deletePrefixRecords(shared, id)
	discardRangeRaftLog(dataDir, id)
}

// discardRangeRaftLog removes the Raft log directory of a range that is no
// longer hosted. Callers must have stopped the group first.
func discardRangeRaftLog(dataDir string, id uint64) {
	if dataDir == "" || id == 0 {
		return
	}
	_ = os.RemoveAll(filepath.Join(dataDir, "raft", fmt.Sprintf("range-%d", id)))
}

// ---------------------------------------------------------------------------
// Host-local split/merge APIs
// ---------------------------------------------------------------------------

// SplitRange creates a new Raft group for [splitKey, old.End) on this server
// and moves the matching MVCC records from the old group into it.
//
// M2.3 host-local scope: this method performs the data movement on one
// RangeHost. Use the SplitRange RPC + meta SetRanges for a replicated split.
// The admin prepare/finish state is still persisted locally so a restart does
// not re-freeze a completed range.
func (s *Server) SplitRange(ctx context.Context, rangeID uint64, splitKey []byte, newID uint64) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g, err := s.groupForID(rangeID)
	if err != nil {
		return nil, err
	}

	g.adminMu.Lock()
	defer g.adminMu.Unlock()

	if g, err = s.groupForID(rangeID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("kvserver: stopped")
	}
	if !g.node.IsLeader() {
		return nil, fmt.Errorf("kvserver: split range %d: not leader", rangeID)
	}
	if newID == 0 {
		return nil, fmt.Errorf("kvserver: split range %d: new range id %d is invalid", rangeID, newID)
	}
	// A retry that arrives after the split finished must be idempotent: the
	// new group already owns the right half and no data is copied again.
	if existing := s.hostedGroup(newID); existing != nil {
		if bytes.Equal(existing.cfg.Start, splitKey) {
			g.sm.forceFinishedSplit()
			return s.Ranges(), nil
		}
		return nil, fmt.Errorf("kvserver: split range %d: new range id %d is invalid", rangeID, newID)
	}
	s.hostMu.RLock()
	oldEnd := cloneBytes(g.cfg.End)
	s.hostMu.RUnlock()
	if len(splitKey) == 0 || !g.containsKey(splitKey) || bytes.Equal(splitKey, g.cfg.Start) {
		return nil, fmt.Errorf("kvserver: split range %d: split key %q is not inside", rangeID, splitKey)
	}
	if err := g.sm.beginLocalSplit(splitKey, newID, oldEnd); err != nil {
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}
	if err := g.sm.prepareSplitPlan(splitKey, newID, oldEnd, 0); err != nil {
		g.sm.abortLocalAdmin()
		g.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}
	ranges, err := s.splitLocalLocked(ctx, g, splitKey, newID, true, false)
	if err != nil {
		g.sm.abortLocalAdmin()
		g.sm.clearFreeze()
		return nil, err
	}
	// Host-local splits have no replicated finish; the local copy and map cut
	// are the whole operation, so mark it finished (and unfreeze).
	g.sm.forceFinishedSplit()
	return ranges, nil
}

// splitLocalLocked performs the data copy/new-group/cut/range-map work. Caller
// holds s.mu and the source group's adminMu.
//
// When replicateFinish is set, the split-finish command is proposed after the
// parent cut is durable and before the new range map is published. Every
// replica that has finished its own data move clears its freeze when the
// finish applies; a replica still copying stays frozen (applySplitFinish only
// records the pending finish). If the finish cannot be replicated within
// adminFinishWait (for example because leadership moved and no replica has
// proposed it yet), the local barrier is settled from the durable local state
// so the range never remains permanently frozen; the coordinator retry/leader
// RPC replicates the finish for the rest of the group.
func (s *Server) splitLocalLocked(ctx context.Context, g *rangeGroup, splitKey []byte, newID uint64, snapshotOld, replicateFinish bool) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rangeID := g.cfg.ID
	s.hostMu.RLock()
	oldStart := cloneBytes(g.cfg.Start)
	oldEnd := cloneBytes(g.cfg.End)
	oldEpoch := g.cfg.Epoch
	s.hostMu.RUnlock()

	// Journal the local intent before touching data. The prepared phase is
	// enough for recovery to decide from ranges.json alone; later phases make
	// the crash point explicit for status and for the restore path.
	intent := adminIntent{
		Op: adminKindSplit, Phase: adminIntentPhasePrepared,
		RangeID: rangeID, RightID: newID,
		Start: oldStart, End: oldEnd,
		SplitKey: cloneBytes(splitKey), RightStart: cloneBytes(splitKey), RightEnd: cloneBytes(oldEnd),
		Epoch: oldEpoch, NewEpoch: oldEpoch + 1,
		Generation: g.sm.currentGeneration(),
	}
	if err := g.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}
	committed := false
	defer func() {
		if !committed {
			g.abortAdminIntent(&intent)
		}
	}()

	// A previous attempt against this range ID may have left a partial child
	// prefix or Raft log from a process that crashed before publishing the
	// split. It is not in the range map (the early-return above handles a
	// published child), so it can be discarded before copying.
	discardScratchChild(g.dataDir, g.shared, newID)

	newGroup, err := newRangeGroup(s.cfg, g.shared, RangeConfig{
		ID: newID, Start: cloneBytes(splitKey), End: oldEnd,
		Peers: cloneStrings(g.cfg.Peers), Epoch: oldEpoch + 1,
	}, s.peerCreds)
	if err != nil {
		return nil, fmt.Errorf("kvserver: split range %d: create group: %w", rangeID, err)
	}
	cleanup := func() {
		_ = newGroup.stop()
		discardScratchChild(g.dataDir, g.shared, newID)
	}
	batch := 0
	if err := g.store.StreamRange(splitKey, oldEnd, 1024, func(records []mvcc.Record) error {
		if g.splitCopyHook != nil {
			if err := g.splitCopyHook(batch); err != nil {
				return err
			}
		}
		batch++
		return newGroup.store.ImportRecords(records)
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: stream copy: %w", rangeID, err)
	}
	// Make the copy durable and record its completion before the child is
	// started or the parent is cut. A crash after this point can restore the
	// right half; a crash before it leaves the parent untouched.
	if err := newGroup.engine.Flush(); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: flush copy: %w", rangeID, err)
	}
	if err := newGroup.engine.Set(rangeSplitCopyKey, splitCopyMarker); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: mark copy: %w", rangeID, err)
	}
	// The copy marker is the evidence recovery uses to restore the right half
	// after a parent cut. Flush again so the marker itself is durable before
	// the parent is modified.
	if err := newGroup.engine.Flush(); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: flush copy marker: %w", rangeID, err)
	}
	intent.Phase = adminIntentPhaseCopyDone
	if err := g.writeAdminIntent(intent); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}
	if err := newGroup.start(context.Background()); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: start new group: %w", rangeID, err)
	}

	// The new group owns the right side; the old one keeps only the left.
	if err := g.store.DeleteRange(splitKey, oldEnd); err != nil {
		// The child copy is still durable and marked. Restore the right half
		// before discarding the scratch prefix so a partial delete cannot lose
		// acknowledged data.
		_ = copyPrefixRecords(g.shared, newID, rangeID, true)
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: delete right: %w", rangeID, err)
	}
	if err := g.sm.markSplitCut(); err != nil {
		// The copy is durable; put the right half back so a retry starts from
		// a consistent parent.
		_ = copyPrefixRecords(g.shared, newID, rangeID, true)
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: record cut: %w", rangeID, err)
	}

	// The data cut is durable. Replicate the finish before publishing the new
	// range map so the barrier can only be cleared after the finish has been
	// committed for the group.
	var finishErr error
	if replicateFinish {
		finishErr = proposeSplitFinish(ctx, g, splitKey, newID, adminFinishWait)
		if finishErr != nil {
			s.log.Warn("split finish did not replicate; settling locally",
				"range", rangeID, "new_range", newID, "error", finishErr)
		}
	}

	// The map publication is the local commit point. Advance the intent and
	// fsync it before the in-memory map update / ranges.json write; recovery
	// then sees either the old map (abort) or the new map (finalize), never an
	// unjournaled cut.
	intent.Phase = adminIntentPhaseMapPublishPending
	intent.Generation = g.sm.currentGeneration()
	if err := g.writeAdminIntent(intent); err != nil {
		_ = copyPrefixRecords(g.shared, newID, rangeID, true)
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}

	// Publish the new range map in one hostMu critical section so readers that
	// route by range bounds never observe the group added with the old end (or
	// the old end without the new group).
	s.hostMu.Lock()
	g.cfg.End = cloneBytes(splitKey)
	g.cfg.Epoch = oldEpoch + 1
	s.groups[newID] = newGroup
	s.hostMu.Unlock()
	if err := s.rebuildRangeOrderErr(); err != nil {
		// The map file is the durable authority. Roll the in-memory map back
		// and restore the right half from the child copy so this server keeps
		// serving the original (unsplit) range; the operation can be retried.
		s.hostMu.Lock()
		g.cfg.End = oldEnd
		g.cfg.Epoch = oldEpoch
		delete(s.groups, newID)
		s.hostMu.Unlock()
		_ = s.rebuildRangeOrderErr()
		_ = copyPrefixRecords(g.shared, newID, rangeID, true)
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: persist range map: %w", rangeID, err)
	}
	// The child copy marker has served its recovery purpose. Remove it so it
	// does not leak into child snapshots forever.
	_ = newGroup.engine.Delete(rangeSplitCopyKey)
	// Persist the finished phase now that the map is durable. Recovery also
	// derives "finished" from the map bounds, but writing it keeps the replay
	// index guard accurate without a restart.
	_ = g.sm.markSplitFinished()
	// The replica-local move is durable. If the replicated finish arrived,
	// this clears the barrier now. Otherwise settle the local barrier from the
	// durable local state (the range map is published, so no write can be lost)
	// rather than leaving the range frozen forever.
	_ = g.sm.markLocalAdminDone()
	if !g.sm.adminFinishApplied() {
		g.sm.forceFinishedSplit()
	}

	// Record the terminal phase, then remove the journal record. A crash
	// between these two steps leaves a "done" intent; recovery sees the new
	// map and finalizes/clears it idempotently.
	intent.Phase = adminIntentPhaseDone
	intent.Generation = g.sm.currentGeneration()
	if err := g.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: split range %d: record completed intent: %w", rangeID, err)
	}
	committed = true
	if err := g.clearAdminIntent(); err != nil {
		s.log.Warn("clear completed split intent", "range", rangeID, "error", err)
	}

	// Host-local split can compact the old log because no other replica needs
	// the prepare entry. The distributed path must keep it until every replica
	// has applied the prepare command.
	if snapshotOld {
		if _, err := g.node.Snapshot(); err != nil {
			return nil, fmt.Errorf("kvserver: split range %d: snapshot old range: %w", rangeID, err)
		}
	}
	return s.Ranges(), nil
}

// markSplitFinished persists the finished phase without clearing the freeze;
// callers clear it through the replicated finish (or forceFinishedSplit for the
// host-local path).
func (m *stateMachine) markSplitFinished() error {
	m.splitMu.Lock()
	defer m.splitMu.Unlock()
	if m.adminKind != adminKindSplit {
		return nil
	}
	m.adminPhase = adminPhaseFinished
	return m.persistLocked()
}

// MergeRanges copies all data from right into left and removes the right Raft
// group. Ranges must be adjacent and hosted in this process.
func (s *Server) MergeRanges(ctx context.Context, leftID, rightID uint64) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if leftID == rightID {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are the same", leftID, rightID)
	}
	left, right, unlock, err := s.lockRangePair(leftID, rightID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if right == nil {
		// Retry after this merge already finished: the right group is gone and
		// the left group recorded the absorption.
		if left.merged(rightID) {
			left.sm.forceFinishedMerge()
			return s.Ranges(), nil
		}
		left.sm.clearFreeze()
		return nil, fmt.Errorf("%w: right id %d", ErrRangeNotFound, rightID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("kvserver: stopped")
	}
	// Re-check under s.mu: membership operations remove ranges while holding
	// s.mu and do not take the admin lock.
	if s.hostedGroup(leftID) != left {
		return nil, fmt.Errorf("%w: left id %d", ErrRangeNotFound, leftID)
	}
	if s.hostedGroup(rightID) != right {
		left.sm.clearFreeze()
		if left.merged(rightID) {
			return s.Ranges(), nil
		}
		return nil, fmt.Errorf("%w: right id %d", ErrRangeNotFound, rightID)
	}
	if left == right {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are the same", leftID, rightID)
	}
	if !left.node.IsLeader() {
		return nil, fmt.Errorf("kvserver: merge: left range %d is not leader", leftID)
	}
	if !bytes.Equal(left.cfg.End, right.cfg.Start) {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are not adjacent", left.cfg.ID, right.cfg.ID)
	}
	rightStart := cloneBytes(right.cfg.Start)
	rightEnd := cloneBytes(right.cfg.End)
	if err := left.sm.beginLocalMerge(left.cfg.ID, right.cfg.ID, rightStart, rightEnd); err != nil {
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	if err := right.sm.beginLocalMerge(left.cfg.ID, right.cfg.ID, rightStart, rightEnd); err != nil {
		left.sm.abortLocalAdmin()
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	if err := left.sm.prepareMergePlan(left.cfg.ID, right.cfg.ID, rightStart, rightEnd, 0); err != nil {
		left.sm.abortLocalAdmin()
		right.sm.abortLocalAdmin()
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	if err := right.sm.prepareMergePlan(left.cfg.ID, right.cfg.ID, rightStart, rightEnd, 0); err != nil {
		left.sm.abortLocalAdmin()
		right.sm.abortLocalAdmin()
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	ranges, err := s.mergeLocalLocked(ctx, left, right, false)
	if err != nil {
		left.sm.abortLocalAdmin()
		right.sm.abortLocalAdmin()
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return nil, err
	}
	left.sm.forceFinishedMerge()
	return ranges, nil
}

// mergeLocalLocked moves right's records into left, stops right and publishes
// the wider left range. Caller holds s.mu and both range admin locks.
//
// When replicateFinish is set the merge-finish command is replicated after the
// right group is stopped and the copied data is durable, but before the wider
// left map is published. Each replica clears its freeze when finish applies
// (or at local completion if finish already applied), so removing the right
// range and unfreezing are both gated on the replicated finish. A timeout is
// settled locally from durable state to avoid a permanent freeze.
func (s *Server) mergeLocalLocked(ctx context.Context, left, right *rangeGroup, replicateFinish bool) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !bytes.Equal(left.cfg.End, right.cfg.Start) {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are not adjacent", left.cfg.ID, right.cfg.ID)
	}
	rightID := right.cfg.ID
	rightEnd := cloneBytes(right.cfg.End)
	op := adminOp{
		Left: left.cfg.ID, Right: rightID,
		RightStart: cloneBytes(right.cfg.Start), RightEnd: rightEnd,
	}

	// Journal the local intent on the surviving (left) range before copying.
	// The right range disappears from ranges.json on success, so the left
	// record is the one a restart can rely on.
	intent := adminIntent{
		Op: adminKindMerge, Phase: adminIntentPhasePrepared,
		RangeID: left.cfg.ID, RightID: rightID,
		Start: cloneBytes(left.cfg.Start), End: cloneBytes(left.cfg.End),
		RightStart: cloneBytes(right.cfg.Start), RightEnd: cloneBytes(rightEnd),
		Epoch: left.cfg.Epoch, NewEpoch: left.cfg.Epoch + 1,
		Generation: left.sm.currentGeneration(),
	}
	if err := left.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			left.abortAdminIntent(&intent)
		}
	}()
	batch := 0
	if err := right.store.StreamRange(nil, nil, 1024, func(records []mvcc.Record) error {
		if right.mergeCopyHook != nil {
			if err := right.mergeCopyHook(batch); err != nil {
				return err
			}
		}
		batch++
		return left.store.ImportRecords(records)
	}); err != nil {
		return nil, fmt.Errorf("kvserver: merge: stream copy right: %w", err)
	}
	// The copied records must be durable before the right range is stopped and
	// removed: after the map update they are the only copy in this host.
	if err := left.engine.Flush(); err != nil {
		return nil, fmt.Errorf("kvserver: merge: flush copy: %w", err)
	}
	intent.Phase = adminIntentPhaseCopyDone
	if err := left.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}

	// Stop the right group before publishing the wider left end. Once stop
	// returns, Raft is halted and the log is closed; keeping the group
	// routable would send requests to a stopped node. Even a stop error must
	// therefore finish the map update so a retry is idempotent.
	stopErr := right.stop()

	// The right group can no longer accept writes. Replicate the finish now:
	// it is the barrier that lets each replica unfreeze, and it is applied by
	// every replica before that replica publishes its wider map.
	var finishErr error
	if replicateFinish {
		finishErr = proposeMergeFinish(ctx, left, op, adminFinishWait)
		if finishErr != nil {
			s.log.Warn("merge finish did not replicate; settling locally",
				"left", left.cfg.ID, "right", rightID, "error", finishErr)
		}
	}

	// Fsync the pending map-publication intent before the local commit point.
	// A crash after this write but before ranges.json is updated aborts the
	// merge; a crash after the update finalizes it.
	intent.Phase = adminIntentPhaseMapPublishPending
	intent.Generation = left.sm.currentGeneration()
	if err := left.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}

	s.hostMu.Lock()
	left.cfg.End = cloneBytes(rightEnd)
	left.cfg.Epoch++
	left.markMerged(rightID)
	delete(s.groups, rightID)
	s.hostMu.Unlock()
	if err := s.rebuildRangeOrderErr(); err != nil {
		// Keep the in-memory map consistent (the right group is stopped) and
		// report the failure. The right prefix is not deleted, so a restart can
		// recover the pre-merge map and data.
		left.sm.forceFinishedMerge()
		return nil, fmt.Errorf("kvserver: merge: persist range map: %w", err)
	}
	right.sm.abortLocalAdmin()
	// Persist the merged marker on the left before discarding the right prefix:
	// a restart then knows the absorption is complete even if a later cleanup
	// was interrupted.
	if err := left.sm.markLocalAdminDone(); err != nil {
		return nil, fmt.Errorf("kvserver: merge: record finish: %w", err)
	}
	if !left.sm.adminFinishApplied() {
		left.sm.forceFinishedMerge()
	}
	// The right prefix is now redundant. Remove it in bounded batches; a crash
	// before this finishes leaves it recoverable (left recovery removes it).
	if err := deletePrefixRecords(left.shared, rightID); err != nil {
		s.log.Warn("merge: discard right prefix failed", "left", left.cfg.ID, "right", rightID, "error", err)
	}
	discardRangeRaftLog(left.dataDir, rightID)

	// The wider map and the absorbed data are durable. Record the terminal
	// phase and clear the journal; a crash between the two leaves a "done"
	// intent that restart finalizes idempotently.
	intent.Phase = adminIntentPhaseDone
	intent.Generation = left.sm.currentGeneration()
	if err := left.writeAdminIntent(intent); err != nil {
		return nil, fmt.Errorf("kvserver: merge: record completed intent: %w", err)
	}
	committed = true
	if err := left.clearAdminIntent(); err != nil {
		s.log.Warn("clear completed merge intent", "left", left.cfg.ID, "error", err)
	}
	if stopErr != nil {
		return nil, fmt.Errorf("kvserver: merge: stop right: %w", stopErr)
	}
	return s.Ranges(), nil
}

// rebuildRangeOrder refreshes the sorted hosted range list. Caller holds s.mu.
func (s *Server) rebuildRangeOrder() {
	if err := s.rebuildRangeOrderErr(); err != nil {
		s.log.Error("persist range map failed", "error", err)
	}
}

// rebuildRangeOrderErr is rebuildRangeOrder with the persistence error
// returned, for split/merge which must not report success before the map is
// durable.
func (s *Server) rebuildRangeOrderErr() error {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	ids := make([]uint64, 0, len(s.groups))
	for id := range s.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.groups[ids[i]].cfg, s.groups[ids[j]].cfg
		switch {
		case a.Start == nil && b.Start == nil:
			return a.ID < b.ID
		case a.Start == nil:
			return true
		case b.Start == nil:
			return false
		default:
			return bytes.Compare(a.Start, b.Start) < 0
		}
	})
	s.groupIDs = ids
	if len(ids) > 0 {
		s.defaultID = ids[0]
	} else {
		s.defaultID = 0
	}
	s.cfg.Ranges = s.cfg.Ranges[:0]
	for _, id := range ids {
		g := s.groups[id]
		s.cfg.Ranges = append(s.cfg.Ranges, g.cfg)
	}
	// The map file is the local authority recovery compares intents against.
	// Use the fsynced atomic writer (temp+rename+dir sync) rather than the
	// historical helper so an acknowledged split/merge cannot lose ranges.json.
	return s.persistRangesAtomicLocked()
}

// ---------------------------------------------------------------------------
// RPC helpers shared by the replicated split/merge handlers.
// ---------------------------------------------------------------------------

func encodeAdminOp(op adminOp) ([]byte, error) { return json.Marshal(op) }

func adminEntry(typ model.CommandType, op adminOp) (model.Entry, error) {
	payload, err := encodeAdminOp(op)
	if err != nil {
		return model.Entry{}, err
	}
	return model.Entry{Type: typ, Value: payload}, nil
}

// proposeAdminEntry proposes one admin entry and returns its index once it has
// committed and applied on the local replica. It retries while leadership
// moves.
func proposeAdminEntry(ctx context.Context, g *rangeGroup, entry model.Entry) (uint64, error) {
	for {
		if !g.node.IsLeader() {
			return 0, raft.ErrNotLeader
		}
		index, err := g.node.Propose(ctx, entry)
		if err == nil {
			return index, nil
		}
		if !errors.Is(err, raft.ErrNotLeader) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitSplitPrepared waits until this replica has applied a split prepare for
// key. It returns the applied key even if a finish has already cleared the
// active freeze marker. While waiting it re-proposes the prepare when this
// replica (re)gains leadership.
func waitSplitPrepared(ctx context.Context, g *rangeGroup, key []byte, entry model.Entry) error {
	deadline := time.Now().Add(30 * time.Second)
	lastTry := time.Time{}
	for {
		if bytes.Equal(g.sm.lastSplitPrepared(), key) {
			return nil
		}
		if time.Since(lastTry) > 250*time.Millisecond {
			tryPrepare(ctx, g, entry)
			lastTry = time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("split prepare timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitMergePrepared waits until both participating ranges applied the prepare
// for the given pair, re-proposing on leadership changes.
func waitMergePrepared(ctx context.Context, left, right *rangeGroup, leftID, rightID uint64, entry model.Entry) error {
	deadline := time.Now().Add(30 * time.Second)
	lastTry := time.Time{}
	for {
		ll, lr := left.sm.lastMergePrepared()
		rl, rr := right.sm.lastMergePrepared()
		if ll == leftID && lr == rightID && rl == leftID && rr == rightID {
			return nil
		}
		if time.Since(lastTry) > 250*time.Millisecond {
			tryPrepare(ctx, left, entry)
			tryPrepare(ctx, right, entry)
			lastTry = time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("merge prepare timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// awaitAdminFinish waits until the replicated finish for the current admin
// operation has been applied locally.
func awaitAdminFinish(ctx context.Context, g *rangeGroup, wait time.Duration) error {
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		if g.sm.adminFinishApplied() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("admin finish did not replicate")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// proposeSplitFinish replicates the split finish and waits for it to apply.
// While this replica is (or becomes) the leader it retries the proposal; a
// leadership change is not fatal because another replica's SplitRange call
// proposes the same finish. If the barrier cannot be replicated within wait,
// the caller settles the local barrier instead of leaving a permanent freeze.
func proposeSplitFinish(ctx context.Context, g *rangeGroup, splitKey []byte, newID uint64, wait time.Duration) error {
	if wait <= 0 {
		wait = 30 * time.Second
	}
	entry, err := adminEntry(model.CommandSplitPrepare, adminOp{
		Op: adminOpFinish, SplitKey: splitKey, NewRangeID: newID,
	})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		if g.sm.adminFinishApplied() {
			return nil
		}
		if g.node.IsLeader() {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := g.node.Propose(pctx, entry)
			cancel()
			if err == nil {
				continue
			}
			if !errors.Is(err, raft.ErrNotLeader) &&
				!errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, context.Canceled) {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("split finish did not replicate")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// proposeMergeFinish replicates the merge finish on the left range and waits
// for it to apply. It follows leadership changes like proposeSplitFinish.
func proposeMergeFinish(ctx context.Context, left *rangeGroup, op adminOp, wait time.Duration) error {
	if wait <= 0 {
		wait = 30 * time.Second
	}
	op.Op = adminOpFinish
	entry, err := adminEntry(model.CommandMergePrepare, op)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		if left.sm.adminFinishApplied() {
			return nil
		}
		if left.node.IsLeader() {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := left.node.Propose(pctx, entry)
			cancel()
			if err == nil {
				continue
			}
			if !errors.Is(err, raft.ErrNotLeader) &&
				!errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, context.Canceled) {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("merge finish did not replicate")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// proposeAdminClear replicates a clear/abort so every replica unfreezes. It is
// best effort: the local abort already happened, and a metadata retry will
// propose a fresh prepare/finish if needed.
func proposeAdminClear(ctx context.Context, g *rangeGroup, typ model.CommandType, op adminOp) {
	if ctx == nil {
		ctx = context.Background()
	}
	op.Op = adminOpClear
	entry, err := adminEntry(typ, op)
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = proposeAdminEntry(pctx, g, entry)
}
