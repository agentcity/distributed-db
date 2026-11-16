package kvserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	"distributed-db/internal/mvcc"
	"distributed-db/internal/raft"
	"distributed-db/internal/storage"
)

// rangeGroup is one independently replicated range hosted by a Server. Every
// group owns its own Pebble engine, MVCC store, Raft log, Raft node, peer
// transport and state machine. Only the gRPC listener/server is shared by all
// groups in a process.
type rangeGroup struct {
	cfg RangeConfig
	log *slog.Logger

	engine    storage.Engine
	store     *mvcc.Store
	logStore  *raft.FileStore
	node      *raft.Node
	transport *peerTransport
	sm        *stateMachine

	// frozen blocks client writes while split/merge copies data.
	frozen   atomic.Bool
	stopOnce sync.Once
	stopErr  error
}

// newRangeGroup opens the per-range storage and Raft log and builds the Raft
// node. The returned group is not running until start is called.
func newRangeGroup(cfg Config, rc RangeConfig) (*rangeGroup, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger.With("node", cfg.ID, "range", rc.ID)
	dbDir := filepath.Join(cfg.DataDir, "db", fmt.Sprintf("range-%d", rc.ID))
	raftDir := filepath.Join(cfg.DataDir, "raft", fmt.Sprintf("range-%d", rc.ID))

	engine, err := storage.Open(dbDir, storage.Options{
		CacheSize:    cfg.CacheSize,
		MemTableSize: cfg.MemTableSize,
		Compression:  "snappy",
		SyncWrites:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("kvserver: range %d: open storage: %w", rc.ID, err)
	}
	logStore, err := raft.NewFileStore(raftDir)
	if err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("kvserver: range %d: open raft log: %w", rc.ID, err)
	}

	store := mvcc.New(engine)
	sm := &stateMachine{store: store, engine: engine}
	tr := newPeerTransport(cfg.Logger, rc.ID)
	node, err := raft.New(raft.Config{
		ID:                 cfg.ID,
		Peers:              append([]string(nil), rc.Peers...),
		DataDir:            raftDir,
		ElectionTimeoutMin: cfg.ElectionTimeoutMin,
		ElectionTimeoutMax: cfg.ElectionTimeoutMax,
		HeartbeatInterval:  cfg.HeartbeatInterval,
		Logger:             cfg.Logger,
	}, logStore, sm, tr)
	if err != nil {
		_ = logStore.Close()
		_ = engine.Close()
		tr.Close()
		return nil, fmt.Errorf("kvserver: range %d: create raft: %w", rc.ID, err)
	}

	return &rangeGroup{
		cfg:       rc,
		log:       log,
		engine:    engine,
		store:     store,
		logStore:  logStore,
		node:      node,
		transport: tr,
		sm:        sm,
	}, nil
}

func (g *rangeGroup) start(ctx context.Context) error {
	if err := g.node.Start(ctx); err != nil {
		return fmt.Errorf("kvserver: range %d: start raft: %w", g.cfg.ID, err)
	}
	return nil
}

// stop halts Raft and closes the log store, engine and peer transport. It is
// idempotent and safe to call on a partially constructed group.
func (g *rangeGroup) stop() error {
	g.stopOnce.Do(func() {
		if g.node != nil {
			g.node.Stop()
		}
		var errs []error
		if g.logStore != nil {
			if err := g.logStore.Close(); err != nil {
				errs = append(errs, fmt.Errorf("range %d: close raft log: %w", g.cfg.ID, err))
			}
		}
		if g.engine != nil {
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

// isFrozen reports whether client writes must be rejected. A replicated split
// prepare freezes every replica; local split/merge uses the frozen flag.
func (g *rangeGroup) isFrozen() bool {
	left, right := g.sm.mergePrepared()
	return g.frozen.Load() || len(g.sm.splitPrepared()) > 0 || left != 0 || right != 0
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
