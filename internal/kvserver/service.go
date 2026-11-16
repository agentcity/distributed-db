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
}

// stateMachine applies committed Raft commands to MVCC. Exactly one goroutine
// calls Apply (Raft's apply loop), so no extra mutex is needed.
type stateMachine struct {
	store       *mvcc.Store
	engine      storage.Engine
	lastApplied atomic.Uint64

	splitMu    sync.RWMutex
	splitKey   []byte
	mergeLeft  uint64
	mergeRight uint64
}

func (m *stateMachine) Apply(entry model.Entry) error {
	err := m.apply(entry)
	m.lastApplied.Store(entry.Index)
	return err
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
		return m.store.Prewrite(cmd.Mutations, cmd.Primary, cmd.StartTS)
	case model.CommandCommit:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode commit: %w", err)
		}
		return m.store.Commit(cmd.Keys, cmd.StartTS, cmd.CommitTS)
	case model.CommandPrewriteCommit:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode prewrite_commit: %w", err)
		}
		if err := m.store.Prewrite(cmd.Mutations, cmd.Primary, cmd.StartTS); err != nil {
			return err
		}
		return m.store.Commit(cmd.Keys, cmd.StartTS, cmd.CommitTS)
	case model.CommandRollback:
		var cmd applyCommand
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode rollback: %w", err)
		}
		return m.store.Rollback(cmd.Keys, cmd.StartTS)
	case model.CommandSplitPrepare:
		var cmd struct {
			SplitKey []byte `json:"split_key"`
		}
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode split prepare: %w", err)
		}
		m.splitMu.Lock()
		m.splitKey = append([]byte(nil), cmd.SplitKey...)
		m.splitMu.Unlock()
		return nil
	case model.CommandMergePrepare:
		var cmd struct {
			Left  uint64 `json:"left"`
			Right uint64 `json:"right"`
		}
		if err := json.Unmarshal(entry.Value, &cmd); err != nil {
			return fmt.Errorf("kvserver: decode merge prepare: %w", err)
		}
		m.splitMu.Lock()
		m.mergeLeft, m.mergeRight = cmd.Left, cmd.Right
		m.splitMu.Unlock()
		return nil
	default:
		return fmt.Errorf("kvserver: unknown command type %q", entry.Type)
	}
}

func (m *stateMachine) splitPrepared() []byte {
	m.splitMu.RLock()
	defer m.splitMu.RUnlock()
	return append([]byte(nil), m.splitKey...)
}

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

func (m *stateMachine) clearMerge() {
	m.splitMu.Lock()
	m.mergeLeft, m.mergeRight = 0, 0
	m.splitMu.Unlock()
}

// snapshotPayload is the durable state-machine snapshot. It contains every
// key/value in the MVCC keyspace plus the applied Raft index.
type snapshotPayload struct {
	Index   uint64 `json:"index"`
	Entries []struct {
		Key   []byte `json:"k"`
		Value []byte `json:"v"`
	} `json:"entries"`
}

// Snapshot implements raft.Snapshotter.
func (m *stateMachine) Snapshot() ([]byte, uint64, error) {
	snap, err := m.engine.Snapshot()
	if err != nil {
		return nil, 0, err
	}
	defer snap.Close()
	it, err := snap.NewIter(nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer it.Close()

	payload := snapshotPayload{Index: m.lastApplied.Load()}
	for it.Valid() {
		key := append([]byte(nil), it.Key()...)
		value := append([]byte(nil), it.Value()...)
		payload.Entries = append(payload.Entries, struct {
			Key   []byte `json:"k"`
			Value []byte `json:"v"`
		}{Key: key, Value: value})
		it.Next()
	}
	if err := it.Error(); err != nil {
		return nil, 0, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return data, payload.Index, nil
}

// RestoreSnapshot implements raft.SnapshotRestorer. It replaces the whole
// MVCC keyspace; this is acceptable for M1 (small snapshots) and will be
// replaced by a Pebble snapshot/ingest path later.
func (m *stateMachine) RestoreSnapshot(data []byte) error {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("kvserver: decode snapshot: %w", err)
	}
	batch := m.engine.NewBatch()
	defer batch.Close()

	it, err := m.engine.NewIter(nil, nil)
	if err != nil {
		return err
	}
	var oldKeys [][]byte
	for it.Valid() {
		oldKeys = append(oldKeys, append([]byte(nil), it.Key()...))
		it.Next()
	}
	if err := it.Error(); err != nil {
		it.Close()
		return err
	}
	_ = it.Close()
	for _, key := range oldKeys {
		if err := batch.Delete(key); err != nil {
			return err
		}
	}
	for _, e := range payload.Entries {
		if err := batch.Set(e.Key, e.Value); err != nil {
			return err
		}
	}
	if err := batch.Commit(); err != nil {
		return err
	}
	m.lastApplied.Store(payload.Index)
	return nil
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
	if err := checkEpoch(ctx, g); err != nil {
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
		if st.Committed || st.RolledBack {
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
	if err := checkEpoch(ctx, g); err != nil {
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
	if err := checkEpoch(ctx, g); err != nil {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if g.isFrozen() {
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(ErrRangeFrozen)}, nil
	}
	if len(req.GetPrimary()) > 0 && !g.containsKey(req.GetPrimary()) {
		err := fmt.Errorf("%w: primary %q is outside range %d", ErrRangeMismatch, req.GetPrimary(), g.cfg.ID)
		return &pbv2.PrewriteResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS:   req.GetStartTs(),
		Primary:   req.GetPrimary(),
		Mutations: mutations,
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
	if err := checkEpoch(ctx, g); err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if g.isFrozen() {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(ErrRangeFrozen)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(), CommitTS: req.GetCommitTs(),
		Primary: req.GetPrimary(), Mutations: mutations, Keys: keys,
	})
	if err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: "encode"}, nil
	}
	if _, err := g.node.Propose(ctx, model.Entry{Type: model.CommandPrewriteCommit, Value: payload}); err != nil {
		return &pbv2.CommitSingleResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	return &pbv2.CommitSingleResponse{Success: true, Leader: g.node.Leader()}, nil
}

func (k *kvService) Commit(ctx context.Context, req *pbv2.CommitRequest) (*pbv2.CommitResponse, error) {
	g, err := k.s.groupForKeys(req.GetKeys())
	if err != nil {
		return &pbv2.CommitResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	if err := checkEpoch(ctx, g); err != nil {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if g.isFrozen() {
		return &pbv2.CommitResponse{Leader: g.node.Leader(), Error: opError(ErrRangeFrozen)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS:  req.GetStartTs(),
		CommitTS: req.GetCommitTs(),
		Keys:     req.GetKeys(),
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
	if err := checkEpoch(ctx, g); err != nil {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if g.isFrozen() {
		return &pbv2.RollbackResponse{Leader: g.node.Leader(), Error: opError(ErrRangeFrozen)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(),
		Keys:    req.GetKeys(),
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
	if err := checkEpoch(ctx, g); err != nil {
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
	if err := checkEpoch(ctx, g); err != nil {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: opError(err)}, nil
	}
	if g.isFrozen() {
		return &pbv2.ResolveLockResponse{Leader: g.node.Leader(), Error: opError(ErrRangeFrozen)}, nil
	}
	payload, err := encodeCommand(applyCommand{
		StartTS: req.GetStartTs(), CommitTS: req.GetCommitTs(), Keys: req.GetKeys(),
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
	if g.isFrozen() {
		return ErrRangeFrozen
	}
	keys := st.Keys
	if len(keys) == 0 {
		keys = [][]byte{fallbackKey}
	}
	payload, err := encodeCommand(applyCommand{StartTS: st.LockTS, CommitTS: st.CommitTS, Keys: keys})
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
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.SplitRangeResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	leader := g.node.Leader()
	if req.GetNewRangeId() == 0 || len(req.GetSplitKey()) == 0 {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: "bad split request"}, nil
	}
	if req.GetEpoch() != 0 && req.GetEpoch() != g.cfg.Epoch {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: opError(ErrStaleEpoch)}, nil
	}
	if _, ok := k.s.groups[req.GetNewRangeId()]; ok {
		return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
	}
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
	if _, ok := k.s.groups[req.GetNewRangeId()]; ok {
		g.sm.clearSplit()
		return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
	}
	if _, err := k.s.splitLocalLocked(g, req.GetSplitKey(), req.GetNewRangeId(), false); err != nil {
		return &pbv2.SplitRangeResponse{Leader: leader, Error: opError(err)}, nil
	}
	g.sm.clearSplit()
	return &pbv2.SplitRangeResponse{Success: true, Leader: leader}, nil
}

// MergeRanges is the admin RPC for a replicated merge. The leaders of both
// groups replicate a prepare command; every replica then moves the right half
// into the left group and stops the right Raft group.
func (k *kvService) MergeRanges(ctx context.Context, req *pbv2.MergeRangesRequest) (*pbv2.MergeRangesResponse, error) {
	left, err := k.s.groupForID(req.GetLeftRangeId())
	if err != nil {
		return &pbv2.MergeRangesResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}
	right, err := k.s.groupForID(req.GetRightRangeId())
	if err != nil {
		// A partial merge may already have stopped the right group on this
		// replica. If the left end covers the requested right end, the local
		// merge is complete and the retry is idempotent.
		if errors.Is(err, ErrRangeNotFound) && len(req.GetRightEnd()) > 0 && bytes.Equal(left.cfg.End, req.GetRightEnd()) {
			return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
		}
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
	}
	if len(req.GetRightEnd()) > 0 && !bytes.Equal(right.cfg.End, req.GetRightEnd()) {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "right end mismatch"}, nil
	}
	if left == right {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "same range"}, nil
	}
	if req.GetEpoch() != 0 && req.GetEpoch() != left.cfg.Epoch {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(ErrStaleEpoch)}, nil
	}
	payload, err := json.Marshal(struct {
		Left  uint64 `json:"left"`
		Right uint64 `json:"right"`
	}{Left: left.cfg.ID, Right: right.cfg.ID})
	if err != nil {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: "encode"}, nil
	}
	if left.node.IsLeader() {
		if err := proposeWhileLeader(ctx, left, model.Entry{Type: model.CommandMergePrepare, Value: payload}); err != nil {
			return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
		}
	}
	if right.node.IsLeader() {
		if err := proposeWhileLeader(ctx, right, model.Entry{Type: model.CommandMergePrepare, Value: payload}); err != nil {
			return &pbv2.MergeRangesResponse{Leader: right.node.Leader(), Error: opError(err)}, nil
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
	if _, ok := k.s.groups[right.cfg.ID]; !ok {
		left.sm.clearMerge()
		return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
	}
	if _, err := k.s.mergeLocalLocked(left, right); err != nil {
		return &pbv2.MergeRangesResponse{Leader: left.node.Leader(), Error: opError(err)}, nil
	}
	left.sm.clearMerge()
	return &pbv2.MergeRangesResponse{Success: true, Leader: left.node.Leader()}, nil
}

func (k *kvService) Status(_ context.Context, req *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return nil, err
	}
	st := g.node.Status()
	return &pbv2.StatusResponse{
		Id:          st.ID,
		Role:        string(st.Role),
		Term:        st.Term,
		Leader:      st.Leader,
		CommitIndex: st.CommitIndex,
		LastApplied: st.LastApplied,
		RangeId:     g.cfg.ID,
		RangeStart:  cloneBytes(g.cfg.Start),
		RangeEnd:    cloneBytes(g.cfg.End),
		Epoch:       g.cfg.Epoch,
	}, nil
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
			Index: req.GetSnapshot().GetIndex(),
			Term:  req.GetSnapshot().GetTerm(),
			Data:  req.GetSnapshot().GetData(),
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

func checkEpoch(ctx context.Context, g *rangeGroup) error {
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
	if epoch != 0 && epoch != g.cfg.Epoch {
		return fmt.Errorf("%w: have %d, request %d", ErrStaleEpoch, g.cfg.Epoch, epoch)
	}
	return nil
}
