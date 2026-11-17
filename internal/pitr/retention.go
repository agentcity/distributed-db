package pitr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RetentionPolicy controls prefix GC. MaxAge and MaxBytes are independent:
// MaxAge reclaims old segments, then MaxBytes reclaims more until the archive
// fits. Both only ever remove a contiguous prefix of each range, never a hole.
//
// ProtectTSO is the replay floor that must remain recoverable. When it is
// non-zero, a segment is eligible for deletion only if its MaxTSO is non-zero
// and <= ProtectTSO; such a segment cannot contain a record above the base
// snapshot. A zero ProtectTSO disables that safety check and may delete
// segments a restore still needs. Callers should set ProtectTSO to the TSO of
// the oldest base backup they intend to keep.
type RetentionPolicy struct {
	MaxAge     time.Duration
	MaxBytes   int64
	ProtectTSO uint64
}

// GCResult describes one retention pass.
type GCResult struct {
	Deleted           []SegmentInfo
	BytesFreed        int64
	RemainingBytes    int64
	RemainingSegments int
}

// GC applies p to the archive. The manifest is rewritten atomically before any
// segment file is unlinked, so a crash leaves either the old manifest with all
// files or the new manifest with some unreferenced files (removed by the next
// Writer open).
func (w *Writer) GC(ctx context.Context, p RetentionPolicy) (*GCResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.MaxAge < 0 {
		return nil, fmt.Errorf("pitr: retention MaxAge must not be negative")
	}
	if p.MaxBytes < 0 {
		return nil, fmt.Errorf("pitr: retention MaxBytes must not be negative")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, ErrClosed
	}
	result := &GCResult{RemainingSegments: len(w.manifest.Segments)}
	for _, s := range w.manifest.Segments {
		result.RemainingBytes += s.SizeBytes
	}
	if p.MaxAge == 0 && p.MaxBytes == 0 {
		return result, nil
	}
	now := w.now().UTC()
	segs := w.manifest.Segments
	keep := make([]bool, len(segs))
	for i := range keep {
		keep[i] = true
	}
	// Group ascending indices per range. The manifest is already sorted.
	groups := make(map[uint64][]int)
	for i, s := range segs {
		groups[s.RangeID] = append(groups[s.RangeID], i)
	}
	reclaimable := func(s SegmentInfo) bool {
		if p.ProtectTSO > 0 && (s.MaxTSO == 0 || s.MaxTSO > p.ProtectTSO) {
			return false
		}
		return true
	}

	var deletedIdx []int
	var freed int64
	deleteIdx := func(i int) {
		if !keep[i] {
			return
		}
		keep[i] = false
		deletedIdx = append(deletedIdx, i)
		freed += segs[i].SizeBytes
	}

	if p.MaxAge > 0 {
		cutoff := now.Add(-p.MaxAge)
		for _, idxs := range groups {
			for _, i := range idxs {
				if !segs[i].SealedAt.After(cutoff) && reclaimable(segs[i]) {
					deleteIdx(i)
					continue
				}
				// A non-reclaimable segment blocks older? It is the current
				// prefix head; later segments must not be deleted out of order.
				break
			}
		}
	}

	remaining := result.RemainingBytes - freed
	if p.MaxBytes > 0 && remaining > p.MaxBytes {
		for remaining > p.MaxBytes {
			type candidate struct {
				idx int
				at  time.Time
			}
			var cands []candidate
			for _, idxs := range groups {
				for _, i := range idxs {
					if !keep[i] {
						continue
					}
					if reclaimable(segs[i]) {
						cands = append(cands, candidate{idx: i, at: segs[i].SealedAt})
					}
					break // only the first retained segment of each range
				}
			}
			if len(cands) == 0 {
				break
			}
			sort.Slice(cands, func(i, j int) bool {
				if !cands[i].at.Equal(cands[j].at) {
					return cands[i].at.Before(cands[j].at)
				}
				return cands[i].idx < cands[j].idx
			})
			progress := false
			for _, c := range cands {
				deleteIdx(c.idx)
				remaining -= segs[c.idx].SizeBytes
				progress = true
				if remaining <= p.MaxBytes {
					break
				}
			}
			if !progress {
				break
			}
		}
	}

	if len(deletedIdx) == 0 {
		return result, nil
	}
	sort.Slice(deletedIdx, func(i, j int) bool { return deletedIdx[i] < deletedIdx[j] })

	next := w.manifest.Clone()
	kept := make([]SegmentInfo, 0, len(segs)-len(deletedIdx))
	for i, s := range segs {
		if keep[i] {
			kept = append(kept, s)
		} else {
			result.Deleted = append(result.Deleted, s)
		}
	}
	next.Segments = kept
	trimmed := make(map[uint64]TrimmedRange, len(next.Trimmed))
	for _, t := range next.Trimmed {
		trimmed[t.RangeID] = t
	}
	for _, i := range deletedIdx {
		s := segs[i]
		t := trimmed[s.RangeID]
		t.RangeID = s.RangeID
		if s.EndIndex > t.ThroughIndex {
			t.ThroughIndex = s.EndIndex
		}
		if s.MaxTSO > t.MaxTSO {
			t.MaxTSO = s.MaxTSO
		}
		trimmed[s.RangeID] = t
	}
	next.Trimmed = next.Trimmed[:0]
	for _, t := range trimmed {
		next.Trimmed = append(next.Trimmed, t)
	}
	sortTrimmed(next.Trimmed)
	touchManifest(next, now)
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := writeManifestDir(w.dir, next); err != nil {
		return nil, err
	}
	w.manifest = next
	w.refreshRangeStatesLocked()

	// The manifest no longer references these files. Unlink after publication;
	// a crash here is recovered by reconcileArchive on the next open.
	for _, i := range deletedIdx {
		p, err := segmentFilePath(w.dir, segs[i])
		if err != nil {
			return nil, err
		}
		if err := removeIfExists(p); err != nil {
			return nil, err
		}
		_ = syncDir(filepath.Dir(p))
	}
	result.BytesFreed = freed
	result.RemainingBytes = 0
	for _, s := range next.Segments {
		result.RemainingBytes += s.SizeBytes
	}
	result.RemainingSegments = len(next.Segments)
	return result, nil
}

// refreshRangeStatesLocked reconciles per-range append cursors with the
// current manifest after GC. Buffered records keep their cursor; only the
// chain tail is refreshed.
func (w *Writer) refreshRangeStatesLocked() {
	for id, rs := range w.ranges {
		segs := w.manifest.SegmentsForRange(id)
		if len(rs.pending) > 0 {
			if len(segs) > 0 {
				last := segs[len(segs)-1]
				rs.lastSHA = last.SHA256
				rs.lastTerm = last.EndTerm
			} else {
				rs.lastSHA = ""
			}
			continue
		}
		if len(segs) > 0 {
			last := segs[len(segs)-1]
			rs.nextIndex = last.EndIndex + 1
			rs.lastTerm = last.EndTerm
			rs.lastSHA = last.SHA256
			continue
		}
		if t, ok := w.manifest.TrimmedFor(id); ok {
			rs.nextIndex = t.ThroughIndex + 1
			rs.lastTerm = 0
			rs.lastSHA = ""
			continue
		}
		rs.nextIndex = 0
		rs.lastTerm = 0
		rs.lastSHA = ""
	}
	for _, s := range w.manifest.Segments {
		if _, ok := w.ranges[s.RangeID]; !ok {
			rs := &rangeState{lastSHA: s.SHA256, lastTerm: s.EndTerm, nextIndex: s.EndIndex + 1}
			w.ranges[s.RangeID] = rs
		}
	}
	for _, t := range w.manifest.Trimmed {
		if _, ok := w.ranges[t.RangeID]; !ok {
			w.ranges[t.RangeID] = &rangeState{nextIndex: t.ThroughIndex + 1}
		}
	}
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pitr: remove segment %s: %w", p, err)
	}
	return nil
}
