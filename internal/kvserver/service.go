package kvserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/metadata"

	"distributed-db/internal/model"
	"distributed-db/internal/mvcc"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/pbconv"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"
)

// applyCommand is the JSON payload stored in model.Entry.Value for the three
// transactional commands.
type applyCommand struct {
	StartTS   uint64          `json:"start_ts,omitempty"`
	CommitTS  uint64          `json:"commit_ts,omitempty"`
	Primary   []byte          `json:"primary,omitempty"`
	Mutations []mvcc.Mutation `json:"mutations,omitempty"`
	Keys      [][]byte        `json:"keys,omitempty"`
	// Gen is the freeze generation observed when the command was proposed.
	// Apply rejects a command whose generation is stale, which closes the
	// race where a write is appended to the Raft log after a split/merge
	// prepare but was accepted (and possibly acknowledged) before it.
	Gen uint64 `json:"gen,omitempty"`
}

// stateMachine applies committed Raft commands to MVCC. Exactly one goroutine
// calls Apply (Raft's apply loop), so no extra mutex is needed for the data
// itself.
type stateMachine struct {
	store       *mvcc.Store
	engine      storage.Engine
	lastApplied atomic.Uint64

	// applyGate serialises mutating applies with freeze barriers. Mutating
	// commands hold it for reading; a local split/merge freeze holds it for
	// writing while the freeze generation is recorded. A distributed prepare
	// arrives as a Raft entry and takes the same write lock in Apply, so
	// after it returns no mutation that passed the pre-freeze check can still
	// be in flight.
	applyGate sync.RWMutex

	splitMu    sync.RWMutex
	splitKey   []byte
	mergeLeft  uint64
	mergeRight uint64
	// freezeGen increments once per freeze barrier and is stamped into every
	// write proposal. It is initialised from the range epoch so a restart
	// does not forget an epoch already published through the range map.
	freezeGen uint64

	// batchResults holds the per-command outcome of recently applied
	// CommandTxnBatch entries until the proposing batcher consumes it.
	batchResults batchResultStore
}

func (m *stateMachine) Apply(entry model.Entry) error {
	var err error
	switch entry.Type {
	case model.CommandPrewrite, model.CommandCommit, model.CommandPrewriteCommit,
		model.CommandTxnBatch, model.CommandRollback:
		m.applyGate.RLock()
		err = m.apply(entry)
		m.applyGate.RUnlock()
	case model.CommandSplitPrepare, model.CommandMergePrepare:
		m.applyGate.Lock()
		err = m.apply(entry)
		m.applyGate.Unlock()
	default:
		err = m.apply(entry)
	}
	m.lastApplied.Store(entry.Index)
	return err
}

// checkWrite rejects a transactional command that was proposed before a
// freeze barrier but applied after it. Both conditions matter: the frozen
// state catches commands appended after an active prepare, while the
// generation catches commands appended during the freeze but applied after a
// failed operation cleared it again.
func (m *stateMachine) checkWrite(cmd applyCommand) error {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	if len(m.splitKey) > 0 || m.mergeLeft != 0 || m.mergeRight != 0 {
		return fmt.Errorf("%w: write applied after split/merge prepare", ErrRangeFrozen)
	}
	if cmd.Gen != m.freezeGen {
		return fmt.Errorf("%w: write generation %d, range generation %d",
			ErrRangeFrozen, cmd.Gen, m.freezeGen)
	}
	return nil
}

// writeTicket returns the generation a new write must stamp and whether the
// range is currently frozen. The read is atomic with respect to prepare
// applies: a caller that observes a writable generation is guaranteed that a
// later prepare raises the generation, so its proposal is rejected at apply
// time if it loses the race.
func (m *stateMachine) writeTicket() (uint64, bool) {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	frozen := len(m.splitKey) > 0 || m.mergeLeft != 0 || m.mergeRight != 0
	return m.freezeGen, frozen
}

// prepareSplit records a split freeze barrier. It is idempotent for a retry of
// the same split key and returns an error for a conflicting split/merge. The
// caller must hold applyGate for writing or be the apply loop.
func (m *stateMachine) prepareSplit(key []byte) error {
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
		if bytes.Equal(m.splitKey, key) {
			return nil // Retry of the same prepare: do not double-freeze.
		}
		return fmt.Errorf("%w: split key %q is already prepared", ErrRangeFrozen, m.splitKey)
	}
	m.splitKey = append([]byte(nil), key...)
	m.freezeGen++
	return nil
}

// prepareMerge records a merge freeze barrier on one of the two participating
// ranges. It is idempotent for a retry of the same pair.
func (m *stateMachine) prepareMerge(left, right uint64) error {
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
			return nil // Retry of the same prepare: do not double-freeze.
		}
		return fmt.Errorf("%w: merge %d/%d is already prepared",
			ErrRangeFrozen, m.mergeLeft, m.mergeRight)
	}
	m.mergeLeft, m.mergeRight = left, right
	m.freezeGen++
	return nil
}

// freezeSplit installs a local split barrier. Unlike a replicated prepare it
// only freezes this host, matching the host-local contract of Server.SplitRange.
// Holding the write side of applyGate guarantees that no earlier write is
// still mutating the store when the copy starts.
func (m *stateMachine) freezeSplit(key []byte) error {
	m.applyGate.Lock()
	defer m.applyGate.Unlock()
	return m.prepareSplit(key)
}

// freezeMerge installs a local merge barrier on this host.
func (m *stateMachine) freezeMerge(left, right uint64) error {
	m.applyGate.Lock()
	defer m.applyGate.Unlock()
	return m.prepareMerge(left, right)
}

func (m *stateMachine) apply(entry model.Entry) error {
	switch entry.Type {
	case model.CommandNoop:
		return nil
	case model.CommandPrewrite:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode prewrite: %w", err)
		}
		if err := m.checkWrite(cmd); err != nil {
			return err
		}
		return m.store.Prewrite(cmd.Mutations, cmd.Primary, cmd.StartTS)
	case model.CommandCommit:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode commit: %w", err)
		}
		if err := m.checkWrite(cmd); err != nil {
			return err
		}
		return m.store.Commit(cmd.Keys, cmd.StartTS, cmd.CommitTS)
	case model.CommandPrewriteCommit:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode prewrite_commit: %w", err)
		}
		if err := m.checkWrite(cmd); err != nil {
			return err
		}
		return m.applyOne(cmd)
	case model.CommandTxnBatch:
		var batch txnBatch
		if err := json.Unmarshal(entry.Value, &batch); err != nil {
			return fmt.Errorf("kvserver: decode txn batch: %w", err)
		}
		if len(batch.Commands) == 0 {
			return fmt.Errorf("kvserver: txn batch %d: no commands", batch.ID)
		}
		// Per-command failures are client-visible outcomes, not apply
		// failures: apply every command in order and let the batcher report
		// each error to its own waiter. Returning an error here would fail
		// the whole Raft entry for all transactions.
		errs := make([]error, len(batch.Commands))
		accepted := make([]int, 0, len(batch.Commands))
		commands := make([]mvcc.SingleCommand, 0, len(batch.Commands))
		for i, cmd := range batch.Commands {
			if err := m.checkWrite(cmd); err != nil {
				errs[i] = err
				continue
			}
			accepted = append(accepted, i)
			commands = append(commands, mvcc.SingleCommand{
				Mutations: cmd.Mutations,
				Primary:   cmd.Primary,
				StartTS:   cmd.StartTS,
				CommitTS:  cmd.CommitTS,
			})
		}
		// One Pebble batch/fsync for all accepted commands. Per-command
		// conflicts (and stale-generation rejections) remain individual
		// outcomes.
		storeErrs := m.store.CommitSingles(commands)
		for j, err := range storeErrs {
			if j < len(accepted) {
				errs[accepted[j]] = err
			}
		}
		m.batchResults.put(batch.ID, errs)
		return nil
	case model.CommandRollback:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode rollback: %w", err)
		}
		if err := m.checkWrite(cmd); err != nil {
			return err
		}
		return m.store.Rollback(cmd.Keys, cmd.StartTS)
	case model.CommandSplitPrepare:
		var cmd struct {
			SplitKey []byte `json:"split_key"`
		}
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode split prepare: %w", err)
		}
		return m.prepareSplit(cmd.SplitKey)
	case model.CommandMergePrepare:
		var cmd struct {
			Left  uint64 `json:"left"`
			Right uint64 `json:"right"`
		}
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode merge prepare: %w", err)
		}
		return m.prepareMerge(cmd.Left, cmd.Right)
	default:
		return fmt.Errorf("kvserver: unknown command type %q", entry.Type)
	}
}

// applyOne applies one 1PC transactional command (prewrite+commit in one
// step) to MVCC. It is shared by CommandPrewriteCommit and every command
// inside a CommandTxnBatch. The returned error is the transaction's own
// outcome; callers that batch commands must record it per command instead of
// failing the whole state-machine apply.
func (m *stateMachine) applyOne(cmd applyCommand) error {
	return m.store.CommitSingle(cmd.Mutations, cmd.Primary, cmd.StartTS, cmd.CommitTS)
}

func (m *stateMachine) splitPrepared() []byte {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	return append([]byte(nil), m.splitKey...)
}

// clearSplit removes the split freeze marker. It is idempotent and does not
// change the freeze generation, so a write that was proposed during the freeze
// but applies later is still rejected by checkWrite.
func (m *stateMachine) clearSplit() {
	m.splitMu.Lock()
	m.splitKey = nil
	m.splitMu.Unlock()
}

func (m *stateMachine) mergePrepared() (uint64, uint64) {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	return m.mergeLeft, m.mergeRight
}

// clearMerge removes the merge freeze marker from one range. It is idempotent
// and leaves the generation untouched for the same reason as clearSplit.
func (m *stateMachine) clearMerge() {
	m.splitMu.Lock()
	m.mergeLeft, m.mergeRight = 0, 0
	m.splitMu.Unlock()
}

// clearFreeze removes both split and merge markers. Admin retries and conflict
// paths use it so a failed or stale prepare can never permanently freeze a
// range.
func (m *stateMachine) clearFreeze() {
	m.splitMu.Lock()
	m.splitKey = nil
	m.mergeLeft, m.mergeRight = 0, 0
	m.splitMu.Unlock()
}

// Snapshot implements raft.Snapshotter. The Raft interface requires the whole
// snapshot as a []byte, so this compatibility method buffers the stream in
// memory; StreamSnapshot is the bounded-memory entry point for callers that
// can consume an io.Writer.
func (m *stateMachine) Snapshot() ([]byte, uint64, error) {
	var buf bytes.Buffer
	index, err := m.StreamSnapshot(&buf)
	if err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), index, nil
}

// RestoreSnapshot implements raft.SnapshotRestorer. It accepts the current
// streaming format produced by Snapshot/StreamSnapshot and, for snapshots
// persisted by earlier releases, the legacy single-JSON-object payload; the
// legacy path is handled by RestoreSnapshotStream.
func (m *stateMachine) RestoreSnapshot(data []byte) error {
	return m.RestoreSnapshotStream(bytes.NewReader(data))
}

func encodeCommand(cmd applyCommand) ([]byte, error) { return json.Marshal(cmd) }

type kvService struct {
	pbv2.UnimplementedKVServiceServer
	s *Server
}

func (k *kvService) Get(ctx context.Context, req *pbv2.GetRequest) (*pbv2.GetResponse, error) {
	g, err := k.s.groupForKey(req.GetKey())
	if err != nil {
		return &pbv2.GetResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.GetResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	leader := g.node.Leader()
	if err := g.node.ReadBarrier(ctx); err != nil {
		return &pbv2.GetResponse{Leader: leader, Error: opError(err)}, nil
	}
	value, err := g.store.Get(req.GetKey(), req.GetTs())
	if errors.Is(err, mvcc.ErrLocked) {
		// Resolve a lock left by a committed/rolled-back/expired transaction,
		// then retry the read. A live lock is reported to the caller.
		st, serr := g.store.CheckTxnStatus(req.GetKey(), 0, time.Now())
		if serr != nil {
			return &pbv2.GetResponse{Leader: leader, Error: opError(serr)}, nil
		}
		// A cross-range secondary lock cannot be resolved from this range's
		// local state: CheckTxnStatus sees no primary record and may classify
		// an expired lock as rolled back even though the primary range already
		// committed. Return "locked" instead; the client consults the primary
		// range and issues ResolveLock with the real outcome.
		if (st.Committed || st.RolledBack) && g.lockPrimaryInRange(st.LockPrimary) {
			if rerr := g.resolveLock(ctx, st, req.GetKey()); rerr != nil {
				return &pbv2.GetResponse{Leader: leader, Error: opError(rerr)}, nil
			}
			value, err = g.store.Get(req.GetKey(), req.GetTs())
		}
	}
	if errors.Is(err, mvcc.ErrKeyNotFound) {
		return &pbv2.GetResponse{Found: false, Leader: leader}, nil
	}
	if err != nil {
		return &pbv2.GetResponse{Leader: leader, Error: opError(err)}, nil
	}
	return &pbv2.GetResponse{Found: true, Value: value, Leader: leader}, nil
}

func (k *kvService) Scan(ctx context.Context, req *pbv2.ScanRequest) (*pbv2.ScanResponse, error) {
	g, err := k.s.groupForKey(req.GetStart())
	if err != nil {
		return &pbv2.ScanResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.ScanResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if !g.containsRange(req.GetStart(), req.GetEnd()) {
		err := fmt.Errorf("%w: scan [%q,%q) crosses range %d [%q,%q)",
			ErrRangeMismatch, req.GetStart(), req.GetEnd(), g.cfg.ID, g.cfg.Start, g.cfg.End)
		return &pbv2.ScanResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	leader := g.node.Leader()
	if err := g.node.ReadBarrier(ctx); err != nil {
		return &pbv2.ScanResponse{Leader: leader, Error: opError(err)}, nil
	}
	rows, err := g.store.Scan(req.GetStart(), req.GetEnd(), req.GetTs(), int(req.GetLimit()))
	if err != nil {
		return &pbv2.ScanResponse{Leader: leader, Error: opError(err)}, nil
	}
	out := make([]*pbv2.KV, 0, len(rows))
	for _, row := range rows {
		out = append(out, &pbv2.KV{Key: row.Key, Value: row.Value})
	}
	return &pbv2.ScanResponse{Kvs: out, Leader: leader}, nil
}

func (k *kvService) Prewrite(ctx context.Context, req *pbv2.PrewriteRequest) (*pbv2.PrewriteResponse, error) {
	if len(req.GetMutations()) == 0 {
		return &pbv2.PrewriteResponse{Leader: k.s.defaultLeader(), Error: "empty mutations"}, nil
	}
	mutations := make([]mvcc.Mutation, 0, len(req.GetMutations()))
	keys := make([][]byte, 0, len(req.GetMutations()))
	for _, m := range req.GetMutations() {
		if m == nil {
			return &pbv2.PrewriteResponse{Leader: k.s.defaultLeader(), Error: "nil mutation"}, nil
		}
		mutations = append(mutations, mvcc.Mutation{
			Key:    m.GetKey(),
			Value:  m.GetValue(),
			Delete: m.GetDelete(),
		})
		keys = append(keys, m.GetKey())
	}
	g, err := k.s.groupForKeys(keys)
	if err != nil {
		return &pbv2.PrewriteResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	gen, err := g.writeTicket()
	if err != nil {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	// The primary may live in another range for a cross-range transaction;
	// each participant only needs the primary pointer, not the primary key.
	payload, err := encodeCommand(applyCommand{
		StartTS:   req.GetStartTs(),
		Primary:   req.GetPrimary(),
		Mutations: mutations,
		Gen:       gen,
	})
	if err != nil {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: "encode"}, nil
	}
	if _, err := g.node.Propose(ctx, model.Entry{Type: model.CommandPrewrite, Value: payload}); err != nil {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.PrewriteResponse{Success: true, Leader: g.node.Leader()}, nil
}

// CommitSingle applies prewrite+commit in one Raft entry. It is used for a
// single-range transaction after the client has already obtained commitTS.
func (k *kvService) CommitSingle(ctx context.Context, req *pbv2.CommitSingleRequest) (*pbv2.CommitSingleResponse, error) {
	if len(req.GetMutations()) == 0 {
		return &pbv2.CommitSingleResponse{Leader: k.s.defaultLeader(), Error: "empty mutations"}, nil
	}
	mutations := make([]mvcc.Mutation, 0, len(req.GetMutations()))
	keys := make([][]byte, 0, len(req.GetMutations()))
	for _, m := range req.GetMutations() {
		if m == nil {
			return &pbv2.CommitSingleResponse{Leader: k.s.defaultLeader(), Error: "nil mutation"}, nil
		}
		mutations = append(mutations, mvcc.Mutation{Key: m.GetKey(), Value: m.GetValue(), Delete: m.GetDelete()})
		keys = append(keys, m.GetKey())
	}
	g, err := k.s.groupForKeys(keys)
	if err != nil {
		return &pbv2.CommitSingleResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	gen, err := g.writeTicket()
	if err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	cmd := applyCommand{
		StartTS: req.GetStartTs(), CommitTS: req.GetCommitTs(),
		Primary: req.GetPrimary(), Mutations: mutations, Keys: keys,
		Gen: gen,
	}
	// Dedicated batchers coalesce concurrent 1PC commits into one Raft entry;
	// each waiter still receives its own transaction's result.
	if err := g.batcher.submit(ctx, cmd); err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.CommitSingleResponse{Success: true, Leader: g.node.Leader()}, nil
}

func (k *kvService) Commit(ctx context.Context, req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
	g, err := k.s.groupForKeys(req.GetKeys())
	if err != nil {
		return &pbv2.CommitResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	gen, err := g.writeTicket()
	if err != nil {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS:  req.GetStartTs(),
		CommitTS: req.GetCommitTs(),
		Keys:     req.GetKeys(),
		Gen:      gen,
	})
	if err != nil {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: "encode"}, nil
	}
	if _, err := g.node.Propose(ctx, model.Entry{Type: model.CommandCommit, Value: payload}); err != nil {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.CommitResponse{Success: true, Leader: g.node.Leader()}, nil
}

func (k *kvService) Rollback(ctx context.Context, req *pbv2.RollbackRequest) (*pbv2.RollbackResponse, error) {
	g, err := k.s.groupForKeys(req.GetKeys())
	if err != nil {
		return &pbv2.RollbackResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	gen, err := g.writeTicket()
	if err != nil {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(),
		Keys:    req.GetKeys(),
		Gen:     gen,
	})
	if err != nil {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: "encode"}, nil
	}
	if _, err := g.node.Propose(ctx, model.Entry{Type: model.CommandRollback, Value: payload}); err != nil {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.RollbackResponse{Success: true, Leader: g.node.Leader()}, nil
}

func (k *kvService) CheckTxnStatus(ctx context.Context, req *pbv2.CheckTxnStatusRequest) (*pbv2.CheckTxnStatusResponse, error) {
	g, err := k.s.groupForKey(req.GetKey())
	if err != nil {
		return &pbv2.CheckTxnStatusResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.CheckTxnStatusResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	leader := g.node.Leader()
	if err := g.node.ReadBarrier(ctx); err != nil {
		return &pbv2.CheckTxnStatusResponse{Leader: leader, Error: opError(err)}, nil
	}
	st, err := g.store.CheckTxnStatus(req.GetKey(), req.GetStartTs(), time.Now())
	if err != nil {
		return &pbv2.CheckTxnStatusResponse{Leader: leader, Error: opError(err)}, nil
	}
	resp := &pbv2.CheckTxnStatusResponse{
		Committed: st.Committed, CommitTs: st.CommitTS, RolledBack: st.RolledBack,
		LockPrimary: st.LockPrimary, LockTs: st.LockTS, TtlMs: st.TTLMillis,
		Expired: st.Expired, Leader: leader,
	}
	for _, key := range st.Keys {
		resp.Keys = append(resp.Keys, key)
	}
	return resp, nil
}

func (k *kvService) ResolveLock(ctx context.Context, req *pbv2.ResolveLockRequest) (*pbv2.ResolveLockResponse, error) {
	if len(req.GetKeys()) == 0 {
		return &pbv2.ResolveLockResponse{Leader: k.s.defaultLeader(), Error: "empty keys"}, nil
	}
	g, err := k.s.groupForKeys(req.GetKeys())
	if err != nil {
		return &pbv2.ResolveLockResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	gen, err := g.writeTicket()
	if err != nil {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(), CommitTS: req.GetCommitTs(), Keys: req.GetKeys(),
		Gen: gen,
	})
	if err != nil {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: "encode"}, nil
	}
	typ := model.CommandRollback
	if req.GetCommitTs() > 0 {
		typ = model.CommandCommit
	}
	if _, err := g.node.Propose(ctx, model.Entry{Type: typ, Value: payload}); err != nil {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.ResolveLockResponse{Success: true, Leader: g.node.Leader()}, nil
}

func (g *rangeGroup) resolveLock(ctx context.Context, st mvcc.TxnStatus, fallbackKey []byte) error {
	gen, err := g.writeTicket()
	if err != nil {
		return err
	}
	keys := st.Keys
	if len(keys) == 0 {
		keys = [][]byte{fallbackKey}
	}
	payload, err := encodeCommand(applyCommand{StartTS: st.LockTS, CommitTS: st.CommitTS, Keys: keys, Gen: gen})
	if err != nil {
		return err
	}
	typ := model.CommandRollback
	if st.Committed {
		typ = model.CommandCommit
	}
	_, err = g.node.Propose(ctx, model.Entry{Type: typ, Value: payload})
	return err
}

// SplitRange is the admin RPC used by the coordinator. The leader first
// replicates a split-prepare command; every replica then copies its local
// slice and creates the new Raft group. Meta is updated by the coordinator
// after all replicas succeed.
func (k *kvService) SplitRange(ctx context.Context, req *pbv2.SplitRangeRequest) (*pbv2.SplitRangeResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.SplitRangeResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	g.adminMu.Lock()
	defer g.adminMu.Unlock()
	// The range may have been merged away while this call waited.
	if g, err = k.s.groupForID(req.GetRangeId()); err != nil {
		return &pbv2.SplitRangeResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	leader := g.node.Leader()
	if req.GetNewRangeId() == 0 || len(req.GetSplitKey()) == 0 {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: "bad split request"}, nil
	}
	// A retry after a successful split is idempotent: no prepare is needed and
	// no records are copied again.
	if existing := k.s.hostedGroup(req.GetNewRangeId()); existing != nil {
		g.sm.clearFreeze()
		if bytes.Equal(existing.cfg.Start, req.GetSplitKey()) {
			return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
		}
		return &pbv2.SplitRangeResponse{Leader: leader, Error: "split range id already in use"}, nil
	}
	if err := k.s.acceptEpoch(g, req.GetEpoch()); err != nil {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: opError(err)}, nil
	}

	// From here on the range may become frozen by our prepare. Every return
	// path, including a timeout or a failed proposal, must clear it.
	defer g.sm.clearFreeze()
	if g.node.IsLeader() {
		payload, err := json.Marshal(struct {
			SplitKey []byte `json:"split_key"`
		}{SplitKey: req.GetSplitKey()})
		if err != nil {
			return &pbv2.SplitRangeResponse{Leader: leader, Error: "encode"}, nil
		}
		if err := proposeWhileLeader(ctx, g, model.Entry{Type: model.CommandSplitPrepare, Value: payload}); err != nil {
			return &pbv2.SplitRangeResponse{Leader: leader, Error: opError(err)}, nil
		}
	}
	// Followers wait for the leader's prepare command to arrive through Raft.
	deadline := time.Now().Add(30 * time.Second)
	for !bytes.Equal(g.sm.splitPrepared(), req.GetSplitKey()) {
		if time.Now().After(deadline) {
			return &pbv2.SplitRangeResponse{Leader: leader, Error: "split prepare timeout"}, nil
		}
		select {
		case <-ctx.Done():
			return &pbv2.SplitRangeResponse{Leader: leader, Error: "canceled"}, nil
		case <-time.After(20 * time.Millisecond):
		}
	}

	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if k.s.stopped {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: "stopped"}, nil
	}
	if existing := k.s.hostedGroup(req.GetNewRangeId()); existing != nil {
		return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
	}
	if _, err := k.s.splitLocalLocked(g, req.GetSplitKey(), req.GetNewRangeId(), false); err != nil {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: opError(err)}, nil
	}
	return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
}

// MergeRanges is the admin RPC for a replicated merge. The leaders of both
// groups replicate a prepare command; every replica then moves the right half
// into the left group and stops the right Raft group.
func (k *kvService) MergeRanges(ctx context.Context, req *pbv2.MergeRangesRequest) (*pbv2.MergeRangesResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.GetLeftRangeId() == req.GetRightRangeId() {
		left, err := k.s.groupForID(req.GetLeftRangeId())
		if err != nil {
			return &pbv2.MergeRangesResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
		}
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "same range"}, nil
	}
	// Lock both ranges for the whole operation in ascending ID order. Unlike
	// a process-wide merge lock, per-range locks still let independent
	// replicas in this process propose their prepares concurrently, which the
	// distributed prepare handshake requires.
	left, right, unlock, err := k.s.lockRangePair(req.GetLeftRangeId(), req.GetRightRangeId())
	if err != nil {
		return &pbv2.MergeRangesResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	defer unlock()
	if right == nil {
		// A retry may arrive after the merge already finished: the right group
		// is gone and the left end covers the requested right end (or the left
		// group recorded the absorption). Clear any leftover freeze and report
		// success idempotently.
		left.sm.clearFreeze()
		done := len(req.GetRightEnd()) > 0 && bytes.Equal(left.cfg.End, req.GetRightEnd())
		if left.merged(req.GetRightRangeId()) {
			done = true
		}
		if done {
			return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
		}
		return &pbv2.MergeRangesResponse{
			Leader: left.node.Leader(),
			Error:  opError(fmt.Errorf("%w: right id %d", ErrRangeNotFound, req.GetRightRangeId())),
		}, nil
	}
	if len(req.GetRightEnd()) > 0 && !bytes.Equal(right.cfg.End, req.GetRightEnd()) {
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "right end mismatch"}, nil
	}
	if left == right {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "same range"}, nil
	}
	if err := k.s.acceptEpoch(left, req.GetEpoch()); err != nil {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
	}
	payload, err := json.Marshal(struct {
		Left  uint64 `json:"left"`
		Right uint64 `json:"right"`
	}{Left: left.cfg.ID, Right: right.cfg.ID})
	if err != nil {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "encode"}, nil
	}

	// Every path after this point may leave either range frozen. Always clear
	// both markers on return, including prepare timeouts, copy failures and
	// stop failures, so no range stays permanently frozen.
	defer func() {
		left.sm.clearFreeze()
		right.sm.clearFreeze()
	}()
	if left.node.IsLeader() {
		if err := proposeWhileLeader(ctx, left, model.Entry{Type: model.CommandMergePrepare, Value: payload}); err != nil {
			return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
		}
	}
	if right.node.IsLeader() {
		if err := proposeWhileLeader(ctx, right, model.Entry{Type: model.CommandMergePrepare, Value: payload}); err != nil {
			return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	lastTry := time.Time{}
	for {
		if time.Since(lastTry) > 250*time.Millisecond {
			tryPrepare(ctx, left, model.Entry{Type: model.CommandMergePrepare, Value: payload})
			tryPrepare(ctx, right, model.Entry{Type: model.CommandMergePrepare, Value: payload})
			lastTry = time.Now()
		}
		ll, lr := left.sm.mergePrepared()
		rl, rr := right.sm.mergePrepared()
		if ll == left.cfg.ID && lr == right.cfg.ID && rl == left.cfg.ID && rr == right.cfg.ID {
			break
		}
		if time.Now().After(deadline) {
			return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "merge prepare timeout"}, nil
		}
		select {
		case <-ctx.Done():
			return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "canceled"}, nil
		case <-time.After(20 * time.Millisecond):
		}
	}

	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if k.s.stopped {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "stopped"}, nil
	}
	if k.s.hostedGroup(left.cfg.ID) != left || k.s.hostedGroup(right.cfg.ID) != right {
		// Membership removed one side while this RPC waited for s.mu. Report
		// the idempotent case when the left group already covers the right end.
		if k.s.hostedGroup(left.cfg.ID) == left && left.merged(right.cfg.ID) {
			return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
		}
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "range disappeared during merge"}, nil
	}
	if _, err := k.s.mergeLocalLocked(left, right); err != nil {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
}

func (k *kvService) Status(_ context.Context, req *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	st := g.node.Status()
	start, end, epoch := k.s.rangeView(g)
	return &pbv2.StatusResponse{
		Id:          st.ID,
		Role:        string(st.Role),
		Term:        st.Term,
		Leader:      st.Leader,
		CommitIndex: st.CommitIndex,
		LastApplied: st.LastApplied,
		RangeId:     g.cfg.ID,
		RangeStart:  start,
		RangeEnd:    end,
		Epoch:       epoch,
		Voters:      st.Voters,
		Learners:    st.Learners,
		Learner:     st.Learner,
	}, nil
}

// TransferLeader moves leadership of one hosted range to target. The request
// must reach the current leader; epoch is optional (0 disables the check) and
// the x-range-epoch request header, when present, is validated as well. On
// success the response carries target as the leader hint; on failure it
// carries the current leader hint and a descriptive error.
func (k *kvService) TransferLeader(ctx context.Context, req *pbv2.TransferLeaderRequest) (*pbv2.TransferLeaderResponse, error) {
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.TransferLeaderResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := k.s.checkEpoch(ctx, g); err != nil {
		return &pbv2.TransferLeaderResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if err := k.s.acceptEpoch(g, req.GetEpoch()); err != nil {
		return &pbv2.TransferLeaderResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if req.GetTarget() == "" {
		return &pbv2.TransferLeaderResponse{Leader: g.node.Leader(), Error: "empty transfer target"}, nil
	}
	if err := g.node.TransferLeadership(ctx, req.GetTarget()); err != nil {
		return &pbv2.TransferLeaderResponse{Leader: g.node.Leader(), Error: transferError(err)}, nil
	}
	return &pbv2.TransferLeaderResponse{Success: true, Leader: req.GetTarget()}, nil
}

// transferError renders a leadership-transfer failure. The categories match
// opError, but transfer-specific errors are reported explicitly so callers can
// distinguish "not caught up" from an internal failure.
func transferError(err error) string {
	switch {
	case errors.Is(err, raft.ErrUnknownTarget):
		return "unknown transfer target"
	case errors.Is(err, raft.ErrTargetNotCaughtUp):
		return "target not caught up"
	case errors.Is(err, raft.ErrLeaderTransferUnsupported):
		return "leader transfer unsupported"
	case errors.Is(err, raft.ErrStaleTimeoutNow):
		return "stale term"
	case errors.Is(err, raft.ErrNotLeader):
		return "not leader"
	case errors.Is(err, raft.ErrStopped):
		return "stopped"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return opError(err)
	}
}

type raftService struct {
	pbv2.UnimplementedRaftServiceServer
	s *Server
}

func (r *raftService) RequestVote(ctx context.Context, req *pbv2.RequestVoteRequest) (*pbv2.RequestVoteResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil RequestVote")
	}
	g, err := r.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	resp, err := g.node.HandleRequestVote(ctx, &raft.RequestVoteRequest{
		Term:         req.GetTerm(),
		CandidateID:  req.GetCandidateId(),
		LastLogIndex: req.GetLastLogIndex(),
		LastLogTerm:  req.GetLastLogTerm(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.RequestVoteResponse{Term: resp.Term, VoteGranted: resp.VoteGranted}, nil
}

func (r *raftService) AppendEntries(ctx context.Context, req *pbv2.AppendEntriesRequest) (*pbv2.AppendEntriesResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil AppendEntries")
	}
	g, err := r.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	entries := make([]model.Entry, 0, len(req.GetEntries()))
	for _, e := range req.GetEntries() {
		if e == nil {
			continue
		}
		entries = append(entries, model.Entry{
			Term:  e.GetTerm(),
			Index: e.GetIndex(),
			Type:  model.CommandType(e.GetType()),
			Key:   string(e.GetKey()),
			Value: e.GetValue(),
		})
	}
	resp, err := g.node.HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         req.GetTerm(),
		LeaderID:     req.GetLeaderId(),
		PrevLogIndex: req.GetPrevLogIndex(),
		PrevLogTerm:  req.GetPrevLogTerm(),
		Entries:      entries,
		LeaderCommit: req.GetLeaderCommit(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.AppendEntriesResponse{
		Term:          resp.Term,
		Success:       resp.Success,
		MatchIndex:    resp.MatchIndex,
		ConflictIndex: resp.ConflictIndex,
	}, nil
}

func (r *raftService) InstallSnapshot(ctx context.Context, req *pbv2.InstallSnapshotRequest) (*pbv2.InstallSnapshotResponse, error) {
	if req == nil || req.GetSnapshot() == nil {
		return nil, errors.New("kvserver: nil InstallSnapshot")
	}
	g, err := r.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	resp, err := g.node.HandleInstallSnapshot(ctx, &raft.InstallSnapshotRequest{
		Term:     req.GetTerm(),
		LeaderID: req.GetLeaderId(),
		Snapshot: raft.Snapshot{
			Index:      req.GetSnapshot().GetIndex(),
			Term:       req.GetSnapshot().GetTerm(),
			Data:       req.GetSnapshot().GetData(),
			Membership: pbconv.SnapshotMembershipFromPB(req.GetSnapshot().GetMembership()),
		},
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.InstallSnapshotResponse{
		Term:      resp.Term,
		Success:   resp.Success,
		LastIndex: resp.LastIndex,
	}, nil
}

// InstallSnapshotChunk dispatches one chunk of a streamed snapshot to the Raft
// node hosting range_id. Protocol rejections (bad offset, checksum, restore
// failure) are returned in the response's error field with success=false;
// transport-level failures such as an unknown range or a stopped node are
// returned as gRPC errors.
func (r *raftService) InstallSnapshotChunk(ctx context.Context, req *pbv2.InstallSnapshotChunkRequest) (*pbv2.InstallSnapshotChunkResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil InstallSnapshotChunk")
	}
	g, err := r.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	resp, err := g.node.HandleInstallSnapshotChunk(ctx, &raft.InstallSnapshotChunkRequest{
		Term:         req.GetTerm(),
		LeaderID:     req.GetLeaderId(),
		Index:        req.GetIndex(),
		SnapshotTerm: req.GetSnapshotTerm(),
		Offset:       req.GetOffset(),
		Data:         req.GetData(),
		Done:         req.GetDone(),
		Size:         req.GetSize(),
		CRC32:        req.GetCrc32(),
		Membership:   pbconv.SnapshotMembershipFromPB(req.GetMembership()),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.InstallSnapshotChunkResponse{
		Term:       resp.Term,
		Success:    resp.Success,
		LastIndex:  resp.LastIndex,
		Error:      resp.Error,
		NextOffset: resp.NextOffset,
	}, nil
}

// TimeoutNow routes a leadership-transfer trigger to the Raft node hosting
// range_id. Protocol rejections (stale term, wrong role, lagging log) are
// returned in the response with success=false; transport-level failures such
// as an unknown range or a stopped node are returned as gRPC errors.
func (r *raftService) TimeoutNow(ctx context.Context, req *pbv2.TimeoutNowRequest) (*pbv2.TimeoutNowResponse, error) {
	if req == nil {
		return nil, errors.New("kvserver: nil TimeoutNow")
	}
	g, err := r.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	resp, err := g.node.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{
		Term:     req.GetTerm(),
		LeaderID: req.GetLeaderId(),
	})
	if err != nil {
		return nil, err
	}
	return &pbv2.TimeoutNowResponse{Term: resp.Term, Success: resp.Success}, nil
}

func opError(err error) string {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return "not leader"
	case errors.Is(err, raft.ErrTimeout):
		return "timeout"
	case errors.Is(err, raft.ErrStopped):
		return "stopped"
	case errors.Is(err, mvcc.ErrLocked):
		return "locked"
	case errors.Is(err, mvcc.ErrWriteConflict):
		return "write conflict"
	case errors.Is(err, mvcc.ErrAlreadyCommitted):
		return "already committed"
	case errors.Is(err, ErrRangeNotFound):
		return "not hosted"
	case errors.Is(err, ErrRangeMismatch):
		return "range mismatch"
	case errors.Is(err, ErrRangeFrozen):
		return "range frozen"
	case errors.Is(err, ErrStaleEpoch):
		return "stale epoch"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "internal error"
	}
}

// tryPrepare proposes a prepare command once if this node currently leads the
// group. It is used by the wait loop to recover from leadership changes.
func tryPrepare(ctx context.Context, g *rangeGroup, entry model.Entry) {
	if !g.node.IsLeader() {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _ = g.node.Propose(pctx, entry)
}

// proposeWhileLeader retries a prepare proposal while this node is the leader.
// If leadership moves, it returns nil and lets the new leader's RPC propose.
func proposeWhileLeader(ctx context.Context, g *rangeGroup, entry model.Entry) error {
	for {
		if !g.node.IsLeader() {
			return nil
		}
		_, err := g.node.Propose(ctx, entry)
		if err == nil {
			return nil
		}
		if !errors.Is(err, raft.ErrNotLeader) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// checkEpoch validates the optional x-range-epoch request header against the
// group's epoch. A higher epoch is adopted: the replicated meta range map is
// the authority for version bumps (membership changes, split and merge), while
// an individual KV group may not have seen the new version yet. A lower epoch
// is a stale client and is rejected.
func (s *Server) checkEpoch(ctx context.Context, g *rangeGroup) error {
	return s.checkEpochHeader(ctx, g)
}

// checkEpochHeader is checkEpoch for callers that already hold a group
// reference. It parses the optional x-range-epoch metadata and delegates the
// comparison to acceptEpoch.
func (s *Server) checkEpochHeader(ctx context.Context, g *rangeGroup) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	values := md.Get("x-range-epoch")
	if len(values) == 0 || values[0] == "" {
		return nil
	}
	epoch, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad header", ErrStaleEpoch)
	}
	return s.acceptEpoch(g, epoch)
}

// acceptEpoch compares an optional epoch with the group's current epoch. The
// meta range map is the single source of truth for range versions: the
// scheduler propagates a new version to every replica (see UpdateRangeEpoch)
// before it publishes the new map, so any mismatch here is a stale request.
// Epoch 0 disables the check.
func (s *Server) acceptEpoch(g *rangeGroup, epoch uint64) error {
	if epoch == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != g.cfg.Epoch {
		return fmt.Errorf("%w: have %d, request %d", ErrStaleEpoch, g.cfg.Epoch, epoch)
	}
	return nil
}
