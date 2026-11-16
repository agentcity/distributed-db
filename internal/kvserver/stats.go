package kvserver

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// Range statistics are deliberately approximate and cheap: the meta scheduler
// only needs a threshold decision and a safe split point, not an exact count.
const (
	// rangeStatsMaxWriteRecords bounds how many MVCC write records one
	// RangeStats call inspects. A range with more records reports the key count
	// and split_key of the scanned prefix, so a stats call cannot turn into an
	// unbounded full-range scan. This bound is a milestone tradeoff: a huge
	// range may be split at a key below its true median, but repeated
	// auto-split passes still make progress.
	rangeStatsMaxWriteRecords = 1 << 16

	// rangeStatsWriteOverhead is the approximate per-key overhead of the MVCC
	// write and data records: write CF prefix + version + value header and a
	// data CF prefix + version. It is used only by the fallback size estimate.
	rangeStatsWriteOverhead = 1 + 8 + 10 + 1 + 8
)

// rangeStats is the local result of estimateRangeStats.
type rangeStats struct {
	sizeBytes uint64
	keyCount  uint64
	splitKey  []byte
	// truncated is true when the bounded scan stopped before the range end.
	truncated bool
}

// RangeStats implements pbv2.KVServiceServer. It reports approximate storage
// size and key count for one hosted range and proposes a split key near the
// median user key. The caller normally is the meta scheduler.
//
// Size comes from the per-range storage engine's Pebble metrics
// (DiskSpaceUsage plus the active memtable size); it is an estimate of the
// whole range, not a sum of the response's values. If those metrics are
// unavailable (both zero), the size falls back to approximate record bytes for
// the bounded key scan.
//
// Key count and split_key come from one bounded iterator over the range's MVCC
// write records: at most rangeStatsMaxWriteRecords records are examined. Each
// distinct user key in [start, end) counts once. split_key is the median of the
// scanned user keys, so it is exact for ranges with at most that many records
// and a lower-biased estimate above it. A range needs at least two distinct
// user keys and the chosen median must be strictly after start; otherwise
// split_key is empty. The scan never writes and does not hold the split/merge lock, so it
// does not block client writes, but a very large range can keep one request
// busy; the record cap bounds that work.
func (k *kvService) RangeStats(ctx context.Context, req *pbv2.RangeStatsRequest) (*pbv2.RangeStatsResponse, error) {
	if req == nil {
		return &pbv2.RangeStatsResponse{Leader: k.s.defaultLeader(), Error: "missing request"}, nil
	}
	g, err := k.s.groupForID(req.GetRangeId())
	if err != nil {
		return &pbv2.RangeStatsResponse{Leader: k.s.defaultLeader(), Error: opError(err)}, nil
	}

	// Snapshot the descriptor under s.mu: split/merge mutates g.cfg while
	// holding it. The scan itself runs outside the lock.
	start, end, _ := k.s.rangeView(g)
	leader := g.node.Leader()
	if err := k.s.acceptEpoch(g, req.GetEpoch()); err != nil {
		return &pbv2.RangeStatsResponse{Leader: leader, Error: opError(err)}, nil
	}
	if err := k.s.checkEpochHeader(ctx, g); err != nil {
		return &pbv2.RangeStatsResponse{Leader: leader, Error: opError(err)}, nil
	}
	if !g.node.IsLeader() {
		return &pbv2.RangeStatsResponse{Leader: leader, Error: opError(raft.ErrNotLeader)}, nil
	}
	if err := g.node.ReadBarrier(ctx); err != nil {
		return &pbv2.RangeStatsResponse{Leader: leader, Error: opError(err)}, nil
	}

	stats, err := g.estimateRangeStats(start, end)
	if err != nil {
		return &pbv2.RangeStatsResponse{Leader: leader, Error: opError(err)}, nil
	}
	return &pbv2.RangeStatsResponse{
		Success:   true,
		Leader:    leader,
		SizeBytes: stats.sizeBytes,
		KeyCount:  stats.keyCount,
		SplitKey:  stats.splitKey,
		Start:     start,
		End:       end,
	}, nil
}

// rangeView returns a consistent copy of a group's bounds and epoch. Split and
// merge mutate g.cfg under s.mu, so taking s.mu here keeps the copy race-free.
func (s *Server) rangeView(g *rangeGroup) (start, end []byte, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBytes(g.cfg.Start), cloneBytes(g.cfg.End), g.cfg.Epoch
}

// estimateRangeStats computes approximate size/key statistics and a split key.
func (g *rangeGroup) estimateRangeStats(start, end []byte) (rangeStats, error) {
	engineStats := g.engine.Stats()
	var size uint64
	if engineStats.DiskSize > 0 {
		size += uint64(engineStats.DiskSize)
	}
	if engineStats.MemTableSize > 0 {
		size += uint64(engineStats.MemTableSize)
	}

	keys, truncated, err := g.scanUserKeys(start, end, rangeStatsMaxWriteRecords)
	if err != nil {
		return rangeStats{}, err
	}
	if size == 0 {
		// Metrics unavailable: fall back to a conservative lower bound based on
		// the scanned user keys.
		for _, key := range keys {
			size += uint64(len(key)) + rangeStatsWriteOverhead
		}
	}
	return rangeStats{
		sizeBytes: size,
		keyCount:  uint64(len(keys)),
		splitKey:  medianSplitKey(keys, start, end),
		truncated: truncated,
	}, nil
}

// scanUserKeys walks the MVCC write column family in [start, end) and returns
// the distinct user keys in ascending order, up to limit records. The write
// column is used because every committed mutation (including deletes) has a
// write record there, and its user key is the leading part of the raw key.
//
// The raw key layout is documented by package mvcc: 'w' | userKey |
// bigEndian(commitTS). This helper intentionally decodes only that fixed
// envelope; it does not depend on MVCC value encodings.
func (g *rangeGroup) scanUserKeys(start, end []byte, limit int) ([][]byte, bool, error) {
	lower := []byte{'w'}
	if start != nil {
		lower = append(lower, start...)
	}
	upper := []byte{'w' + 1}
	iter, err := g.engine.NewIter(lower, upper)
	if err != nil {
		return nil, false, err
	}
	defer iter.Close()

	keys := make([][]byte, 0, 64)
	seenKeys := make(map[string]struct{}, 64)
	records := 0
	truncated := false
	for iter.Valid() {
		if limit > 0 && records >= limit {
			truncated = true
			break
		}
		rawKey := iter.Key()
		// Strip the column-family byte and the trailing 8-byte commit TS.
		if len(rawKey) < 1+8 || rawKey[0] != 'w' {
			iter.Next()
			continue
		}
		userKey := rawKey[1 : len(rawKey)-8]
		if start != nil && bytes.Compare(userKey, start) < 0 {
			iter.Next()
			continue
		}
		if end != nil && bytes.Compare(userKey, end) >= 0 {
			iter.Next()
			continue
		}
		records++
		if _, ok := seenKeys[string(userKey)]; !ok {
			seenKeys[string(userKey)] = struct{}{}
			keys = append(keys, append([]byte(nil), userKey...))
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return nil, false, fmt.Errorf("range stats scan: %w", err)
	}
	// Raw write keys are userKey||version, so a user key that is a prefix of
	// another can interleave in raw order. Sort explicitly to get true user-key
	// order before choosing the median.
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys, truncated, nil
}

// medianSplitKey picks the upper median of the scanned user keys. A key equal
// to the inclusive range start is skipped in favour of the next key, because
// splitting at start would leave the left range empty. The result is nil when
// fewer than two keys exist (or no key is strictly inside the bounds), so the
// caller never asks SplitRange for a boundary that cannot form two ranges.
func medianSplitKey(keys [][]byte, start, end []byte) []byte {
	if len(keys) < 2 {
		return nil
	}
	idx := len(keys) / 2
	key := keys[idx]
	if start != nil && bytes.Compare(key, start) <= 0 {
		idx++
		if idx >= len(keys) {
			return nil
		}
		key = keys[idx]
	}
	if start != nil && bytes.Compare(key, start) <= 0 {
		return nil
	}
	if end != nil && bytes.Compare(key, end) >= 0 {
		return nil
	}
	return cloneBytes(key)
}
