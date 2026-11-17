package kvserver

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"distributed-db/internal/mvcc"
)

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

// SplitRange creates a new Raft group for [splitKey, old.End) on this server
// and moves the matching MVCC records from the old group into it.
//
// M2.3 host-local scope: this method performs the data movement on one
// RangeHost. Use the SplitRange RPC + meta SetRanges for a replicated split.
func (s *Server) SplitRange(ctx context.Context, rangeID uint64, splitKey []byte, newID uint64) ([]RangeConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g, err := s.groupForID(rangeID)
	if err != nil {
		return nil, err
	}

	// Serialise administration of this range for the whole operation. Without
	// this, two retries could each install a freeze and one could clear the
	// marker the other still relies on.
	g.adminMu.Lock()
	defer g.adminMu.Unlock()

	// The range may have been merged away while this call waited on adminMu.
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
			return s.Ranges(), nil
		}
		return nil, fmt.Errorf("kvserver: split range %d: new range id %d is invalid", rangeID, newID)
	}
	if len(splitKey) == 0 || !g.containsKey(splitKey) || bytes.Equal(splitKey, g.cfg.Start) {
		return nil, fmt.Errorf("kvserver: split range %d: split key %q is not inside", rangeID, splitKey)
	}
	if err := g.sm.freezeSplit(splitKey); err != nil {
		// adminMu is held, so no local operation owns the conflicting freeze;
		// it is stale. Clear it instead of leaving the range frozen forever.
		g.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: split range %d: %w", rangeID, err)
	}
	defer g.sm.clearSplit()
	return s.splitLocalLocked(g, splitKey, newID, true)
}

// splitLocalLocked performs the data copy/new-group work. Caller holds s.mu and
// the source group's adminMu.
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
		// A failed copy may have imported a prefix of the records. Remove
		// them before stopping the scratch group so a retry with the same
		// range ID starts from a clean prefix (ImportRecords is idempotent,
		// but a later split with different bounds must not inherit them).
		_ = newGroup.store.DeleteRange(nil, nil)
		_ = newGroup.stop()
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
	if err := newGroup.start(context.Background()); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: start new group: %w", rangeID, err)
	}

	// The new group owns the right side; the old one keeps only the left.
	if err := g.store.DeleteRange(splitKey, oldEnd); err != nil {
		cleanup()
		return nil, fmt.Errorf("kvserver: split range %d: delete right: %w", rangeID, err)
	}
	// Publish the new range map in one hostMu critical section so readers
	// that route by range bounds never observe the group added with the old
	// end (or the old end without the new group).
	s.hostMu.Lock()
	g.cfg.End = cloneBytes(splitKey)
	g.cfg.Epoch++
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
	if leftID == rightID {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are the same", leftID, rightID)
	}
	left, right, unlock, err := s.lockRangePair(leftID, rightID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if right == nil {
		// Retry after this merge already finished: the right group is gone
		// and the left group recorded the absorption.
		left.sm.clearFreeze()
		if left.merged(rightID) {
			return s.Ranges(), nil
		}
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
	if err := left.sm.freezeMerge(left.cfg.ID, right.cfg.ID); err != nil {
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	if err := right.sm.freezeMerge(left.cfg.ID, right.cfg.ID); err != nil {
		left.sm.clearFreeze()
		right.sm.clearFreeze()
		return nil, fmt.Errorf("kvserver: merge: %w", err)
	}
	defer func() {
		left.sm.clearMerge()
		right.sm.clearMerge()
	}()
	return s.mergeLocalLocked(left, right)
}

// mergeLocalLocked moves right's records into left and stops right. Caller
// holds s.mu and both range admin locks. Freeze cleanup is the caller's
// responsibility (deferred), so every error path unfreezes.
func (s *Server) mergeLocalLocked(left, right *rangeGroup) ([]RangeConfig, error) {
	if !bytes.Equal(left.cfg.End, right.cfg.Start) {
		return nil, fmt.Errorf("kvserver: merge: ranges %d and %d are not adjacent", left.cfg.ID, right.cfg.ID)
	}
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

	// Stop the right group before publishing the wider left end. Once stop
	// returns, Raft is halted and the log is closed; keeping the group
	// routable would send requests to a stopped node. Even a stop error must
	// therefore finish the map update so a retry is idempotent.
	rightID := right.cfg.ID
	stopErr := right.stop()
	s.hostMu.Lock()
	left.cfg.End = cloneBytes(right.cfg.End)
	left.cfg.Epoch++
	left.markMerged(rightID)
	delete(s.groups, rightID)
	s.hostMu.Unlock()
	s.rebuildRangeOrder()
	if stopErr != nil {
		return nil, fmt.Errorf("kvserver: merge: stop right: %w", stopErr)
	}
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
