package kvserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"

	"google.golang.org/grpc/credentials"
)

// rangeGroup is one independently replicated range hosted by a Server. All
// groups in one process share a single Pebble engine; each group sees only its
// own keyspace through a storage.Prefix wrapper. Every group still owns its
// MVCC store, Raft log, Raft node, peer transport and state machine.
type rangeGroup struct {
	cfg RangeConfig
	log *slog.Logger

	// engine is this group's prefix-scoped view of the Server's shared
	// engine. It is never the real engine, so closing it is a no-op.
	engine     storage.Engine
	ownsEngine bool
	store      *mvcc.Store
	logStore   *raft.FileStore
	node       *raft.Node
	transport  *peerTransport
	sm         *stateMachine
	batcher    *commitBatcher

	// adminMu serialises split/merge administration for this range. It is
	// held for the whole operation (prepare, copy, clear) so a retry cannot
	// clear a freeze another in-flight operation still depends on or copy the
	// same records twice.
	adminMu sync.Mutex

	// mergedAway records range IDs this group absorbed, so a retry after the
	// right group has been removed is recognised as already finished. It is
	// protected by adminMu.
	mergedAway map[uint64]struct{}

	// splitCopyHook and mergeCopyHook are test seams for fault injection. They
	// are nil in production. A hook that returns an error makes the copy fail
	// exactly as a storage error would, after the range is frozen.
	splitCopyHook func(batch int) error
	mergeCopyHook func(batch int) error

	stopOnce sync.Once
	stopErr  error
}

// rangePrefix returns the fixed 10-byte keyspace prefix of a range in the
// shared engine: 'r' | big-endian uint64 range ID | 0x00.
//
// The 0x00 separator makes the prefix end unambiguous (it is the same prefix
// with the last byte incremented to 0x01), so range prefixes are disjoint for
// different range IDs and arbitrary user keys stay inside their range.
func rangePrefix(id uint64) []byte {
	prefix := make([]byte, 1+8+1)
	prefix[0] = 'r'
	binary.BigEndian.PutUint64(prefix[1:9], id)
	prefix[9] = 0x00
	return prefix
}

// sharedEngineDir is the single Pebble directory owned by a Server. The old
// per-range layout (<DataDir>/db/range-<id>) is intentionally not read or
// migrated; it predates the shared-engine layout and is ignored.
func sharedEngineDir(dataDir string) string {
	return filepath.Join(dataDir, "db", "shared")
}

// newRangeGroup builds a group over the shared engine and opens its Raft log.
// The returned group is not running until start is called. The shared engine
// is owned by the Server, so groups never close it.
func newRangeGroup(cfg Config, shared storage.Engine, rc RangeConfig, peerCreds credentials.TransportCredentials) (*rangeGroup, error) {
	if shared == nil {
		return nil, fmt.Errorf("kvserver: range %d: shared storage engine is nil", rc.ID)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger.With("node", cfg.ID, "range", rc.ID)
	raftDir := filepath.Join(cfg.DataDir, "raft", fmt.Sprintf("range-%d", rc.ID))
	engine := storage.Prefix(shared, rangePrefix(rc.ID))

	logStore, err := raft.NewFileStore(raftDir)
	if err != nil {
		return nil, fmt.Errorf("kvserver: range %d: open raft log: %w", rc.ID, err)
	}

	store := mvcc.New(engine)
	// Start the freeze generation one below the persisted range epoch. A
	// successful split/merge increments the epoch, so this is the generation
	// clients used before the barrier; replaying the prepare raises it by one.
	// Writes from before that prepare keep the baseline generation, while
	// writes proposed during the freeze carry an older one and stay rejected.
	gen := uint64(0)
	if rc.Epoch > 0 {
		gen = rc.Epoch - 1
	}
	sm := &stateMachine{store: store, engine: engine, freezeGen: gen}
	tr := newPeerTransport(cfg.Logger, rc.ID, cfg.PeerAddrs, peerCreds)
	node, err := raft.New(raft.Config{
		ID:                 cfg.ID,
		Peers:              append([]string(nil), rc.Peers...),
		DataDir:            raftDir,
		ElectionTimeoutMin: cfg.ElectionTimeoutMin,
		ElectionTimeoutMax: cfg.ElectionTimeoutMax,
		HeartbeatInterval:  cfg.HeartbeatInterval,
		Logger:             cfg.Logger,
		Learner:            rc.Learner,
		ForceLearner:       rc.ForceLearner,
	}, logStore, sm, tr)
	if err != nil {
		_ = logStore.Close()
		tr.Close()
		return nil, fmt.Errorf("kvserver: range %d: create raft: %w", rc.ID, err)
	}

	g := &rangeGroup{
		cfg:        rc,
		log:        log,
		engine:     engine,
		ownsEngine: false,
		store:      store,
		logStore:   logStore,
		node:       node,
		transport:  tr,
		sm:         sm,
	}
	g.batcher = newCommitBatcher(g)
	return g, nil
}

func (g *rangeGroup) start(ctx context.Context) error {
	if err := g.node.Start(ctx); err != nil {
		return fmt.Errorf("kvserver: range %d: start raft: %w", g.cfg.ID, err)
	}
	return nil
}

// stop halts Raft and closes the Raft log and peer transport. It only closes
// the storage engine when the group owns it; with the shared-engine layout the
// Server owns the real engine and the group's prefix wrapper is a no-op on
// Close. It is idempotent and safe to call on a partially constructed group.
func (g *rangeGroup) stop() error {
	g.stopOnce.Do(func() {
		// Stop the batcher first so no new 1PC entry is proposed while Raft is
		// shutting down; in-flight waiters are completed with an error.
		if g.batcher != nil {
			g.batcher.stop()
		}
		if g.node != nil {
			g.node.Stop()
		}
		var errs []error
		if g.logStore != nil {
			if err := g.logStore.Close(); err != nil {
				errs = append(errs, fmt.Errorf("range %d: close raft log: %w", g.cfg.ID, err))
			}
		}
		if g.engine != nil && g.ownsEngine {
			if err := g.engine.Close(); err != nil {
				errs = append(errs, fmt.Errorf("range %d: close storage: %w", g.cfg.ID, err))
			}
		}
		if g.transport != nil {
			g.transport.Close()
		}
		g.stopErr = errors.Join(errs...)
	})
	return g.stopErr
}

// isFrozen reports whether client writes must be rejected. A replicated
// split/merge prepare freezes every replica; local operations install the same
// state through freezeSplit/freezeMerge.
func (g *rangeGroup) isFrozen() bool {
	_, frozen := g.sm.writeTicket()
	return frozen
}

// writeTicket returns the generation a new write must carry, or ErrRangeFrozen
// when a split/merge barrier is active. Capturing the generation before the
// proposal is what lets Apply reject a write that raced the prepare.
func (g *rangeGroup) writeTicket() (uint64, error) {
	gen, frozen := g.sm.writeTicket()
	if frozen {
		return 0, ErrRangeFrozen
	}
	return gen, nil
}

func (g *rangeGroup) containsKey(key []byte) bool {
	if g.cfg.Start != nil && bytes.Compare(key, g.cfg.Start) < 0 {
		return false
	}
	if g.cfg.End != nil && bytes.Compare(key, g.cfg.End) >= 0 {
		return false
	}
	return true
}

// containsRange reports whether [start, end) lies entirely inside the group's
// range. A nil start means the beginning of the keyspace and a nil end means
// the end of the keyspace.
func (g *rangeGroup) containsRange(start, end []byte) bool {
	if g.cfg.Start != nil {
		if start == nil || bytes.Compare(start, g.cfg.Start) < 0 {
			return false
		}
	}
	if g.cfg.End != nil {
		if end == nil || bytes.Compare(end, g.cfg.End) > 0 {
			return false
		}
	}
	return true
}
