package kvserver

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"distributed-db/internal/mvcc"
)

// SplitRange creates a new Raft group for [splitKey, old.End) on this server
// and moves the matching MVCC records from the old group into it.
//
// M2.3 host-local scope: this method performs the data movement on one
// RangeHost. Use the SplitRange RPC + meta SetRanges for a replicated split.
func (s *Server) SplitRange(ctx context.Context, rangeID uint64, splitKey []byte, newID uint64) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("kvserver: stopped")
	}
	g, ok := s.groups[rangeID]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrRangeNotFound, rangeID)
	}
	if !g.node.IsLeader() {
		return nil, fmt.Errorf("kvserver: split range %d: not leader", rangeID)
	}
	if _, ok := s.groups[newID]; ok || newID == 0 {
		return nil, fmt.Errorf("kvserver: split range %d: new range id %d is invalid", rangeID, newID)
	}
	if len(splitKey) == 0 || !g.containsKey(splitKey) || bytes.Equal(splitKey, g.cfg.Start) {
		return nil, fmt.Errorf("kvserver: split range %d: split key %q is not inside", rangeID, splitKey)
	}
	if !g.frozen.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("kvserver: split range %d: already frozen", rangeID)
	}
	defer g.frozen.Store(false)
	return s.splitLocalLocked(g, splitKey, newID, true)
}

// splitLocalLocked performs the data copy/new-group work. Caller holds s.mu.
func (s *Server) splitLocalLocked(g *rangeGroup, splitKey []byte, newID uint64, snapshotOld bool) ([]RangeConfig, error) {
	rangeID := g.cfg.ID
	oldEnd := cloneBytes(g.cfg.End)
	newGroup, err := newRangeGroup(s.cfg, s.engine, RangeConfig{
		ID: newID, Start: cloneBytes(splitKey), End: oldEnd,
		Peers: cloneStrings(g.cfg.Peers), Epoch: g.cfg.Epoch + 1,
	}, s.peerCreds)
	if err != nil {
		return nil, fmt.Errorf("kvserver: split range %d: create group: %w", rangeID, err)
	}
	cleanup := func() {
		_ = newGroup.stop()
	}
	if err := g.store.StreamRange(splitKey, oldEnd, 1024, func(batch []mvcc.Record) error {
		return newGroup.store.ImportRecords(batch)
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: stream copy: %w", rangeID, err)
	}
	if err := newGroup.start(context.Background()); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: start new group: %w", rangeID, err)
	}

	// The new group owns the right side; the old one keeps only the left.
	if err := g.store.DeleteRange(splitKey, oldEnd); err != nil {
		_ = newGroup.stop()
		return nil, fmt.Errorf("kvserver: split range %d: delete right: %w", rangeID, err)
	}
	g.cfg.End = cloneBytes(splitKey)
	g.cfg.Epoch++
	s.hostMu.Lock()
	s.groups[newID] = newGroup
	s.hostMu.Unlock()
	s.rebuildRangeOrder()

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

// MergeRanges copies all data from right into left and removes the right Raft
// group. Ranges must be adjacent and hosted in this process.
func (s *Server) MergeRanges(ctx context.Context, leftID, rightID uint64) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("kvserver: stopped")
	}
	left, ok := s.groups[leftID]
	if !ok {
		return nil, fmt.Errorf("%w: left id %d", ErrRangeNotFound, leftID)
	}
	right, ok := s.groups[rightID]
	if !ok {
		return nil, fmt.Errorf("%w: right id %d", ErrRangeNotFound, rightID)
	}
	if !left.node.IsLeader() {
		return nil, fmt.Errorf("kvserver: merge: left range %d is not leader", leftID)
	}
	return s.mergeLocalLocked(left, right)
}

// mergeLocalLocked moves right's records into left and stops right. Caller
// holds s.mu.
func (s *Server) mergeLocalLocked(left, right *rangeGroup) ([]RangeConfig, error) {
	if !bytes.Equal(left.cfg.End, right.cfg.Start) {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are not adjacent", left.cfg.ID, right.cfg.ID)
	}
	if err := right.store.StreamRange(nil, nil, 1024, func(batch []mvcc.Record) error {
		return left.store.ImportRecords(batch)
	}); err != nil {
		return nil, fmt.Errorf("kvserver: merge: stream copy right: %w", err)
	}
	left.cfg.End = cloneBytes(right.cfg.End)
	left.cfg.Epoch++
	if err := right.stop(); err != nil {
		return nil, fmt.Errorf("kvserver: merge: stop right: %w", err)
	}
	s.hostMu.Lock()
	delete(s.groups, right.cfg.ID)
	s.hostMu.Unlock()
	s.rebuildRangeOrder()
	return s.Ranges(), nil
}

// rebuildRangeOrder refreshes the sorted hosted range list. Caller holds s.mu.
func (s *Server) rebuildRangeOrder() {
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
	if err := s.persistRangesLocked(); err != nil {
		s.log.Error("persist range map failed", "error", err)
	}
}
