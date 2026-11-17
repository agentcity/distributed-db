package pitr

import (
	"context"
	"fmt"
	"path/filepath"
)

// Target bounds a replay. A zero field means unbounded. TSO and Index are
// applied together: a record is delivered only when it satisfies both bounds.
// TSO order is not assumed to follow Raft index order, so records with a TSO
// above the target are skipped rather than ending the replay.
type Target struct {
	TSO   uint64
	Index uint64
}

// Segment is one fully read and validated segment.
type Segment struct {
	Header  SegmentHeader
	Records []LogRecord
}

// VerifyReport summarizes a full archive verification pass.
type VerifyReport struct {
	ClusterID    string
	Ranges       int
	Segments     int
	Records      int64
	PayloadBytes int64
	SizeBytes    int64
	MinTSO       uint64
	MaxTSO       uint64
}

// Reader reads one immutable archive directory. It is safe for concurrent use;
// its manifest is copied at Open and never mutated.
type Reader struct {
	dir      string
	manifest *Manifest
}

// OpenReader loads and validates dir/manifest.json. Segment files are checked
// lazily by ReadSegment, ReplayRange, ReplayAll and Verify.
func OpenReader(dir string) (*Reader, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty archive directory", ErrManifest)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	return &Reader{dir: filepath.Clean(dir), manifest: m}, nil
}

// Dir returns the archive root directory.
func (r *Reader) Dir() string { return r.dir }

// Manifest returns a deep copy of the reader's manifest.
func (r *Reader) Manifest() *Manifest { return r.manifest.Clone() }

// ListSegments returns all retained segments sorted by range id and start
// index.
func (r *Reader) ListSegments() []SegmentInfo {
	out := make([]SegmentInfo, len(r.manifest.Segments))
	copy(out, r.manifest.Segments)
	return out
}

// ListRangeSegments returns the retained segments of rangeID in index order.
func (r *Reader) ListRangeSegments(rangeID uint64) []SegmentInfo {
	return r.manifest.SegmentsForRange(rangeID)
}

// ReadSegment reads and validates one segment descriptor and returns all of
// its records. The whole segment, including its footer, is verified before any
// record is returned.
func (r *Reader) ReadSegment(ctx context.Context, info SegmentInfo) (*Segment, error) {
	if err := info.Validate(); err != nil {
		return nil, err
	}
	seg := &Segment{}
	var recs []LogRecord
	h, err := readSegmentFile(ctx, r.dir, r.manifest.ClusterID, info, func(rec LogRecord) error {
		recs = append(recs, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	seg.Header = *h
	seg.Records = recs
	return seg, nil
}

// ReplayRange validates and replays one range's segments in index order,
// applying target bounds. It returns ErrGap/ErrMissing when a referenced
// segment is absent and ErrCorrupt on any checksum or framing failure.
func (r *Reader) ReplayRange(ctx context.Context, rangeID uint64, target Target, sink func(LogRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		sink = func(LogRecord) error { return nil }
	}
	for _, info := range r.manifest.SegmentsForRange(rangeID) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if target.Index > 0 && info.StartIndex > target.Index {
			break
		}
		_, err := readSegmentFile(ctx, r.dir, r.manifest.ClusterID, info, func(rec LogRecord) error {
			if target.Index > 0 && rec.Entry.Index > target.Index {
				return nil
			}
			if target.TSO > 0 && rec.TSO > target.TSO {
				return nil
			}
			return sink(rec)
		})
		if err != nil {
			return fmt.Errorf("pitr: replay range %d: %w", rangeID, err)
		}
	}
	return nil
}

// ReplayAll validates and replays every range present in the manifest, ordered
// by range id and then by Raft index. Ranges that GC fully trimmed have no
// records and therefore do not appear.
func (r *Reader) ReplayAll(ctx context.Context, target Target, sink func(rangeID uint64, rec LogRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		sink = func(uint64, LogRecord) error { return nil }
	}
	for _, rangeID := range r.manifest.SegmentRanges() {
		id := rangeID
		if err := r.ReplayRange(ctx, id, target, func(rec LogRecord) error {
			return sink(id, rec)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Verify reads every retained segment and validates its full-file SHA-256,
// footer body hash, header CRC, record CRCs, index contiguity, term ordering
// and manifest descriptor agreement.
func (r *Reader) Verify(ctx context.Context) (*VerifyReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := &VerifyReport{ClusterID: r.manifest.ClusterID, Ranges: len(r.manifest.SegmentRanges())}
	first := true
	for _, info := range r.manifest.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := readSegmentFile(ctx, r.dir, r.manifest.ClusterID, info, nil)
		if err != nil {
			return nil, fmt.Errorf("pitr: verify segment %s: %w", info.Path, err)
		}
		report.Segments++
		report.Records += int64(h.EntryCount)
		report.PayloadBytes += int64(h.PayloadBytes)
		report.SizeBytes += info.SizeBytes
		if first {
			report.MinTSO, report.MaxTSO = h.MinTSO, h.MaxTSO
			first = false
		} else {
			if h.MinTSO < report.MinTSO {
				report.MinTSO = h.MinTSO
			}
			if h.MaxTSO > report.MaxTSO {
				report.MaxTSO = h.MaxTSO
			}
		}
	}
	return report, nil
}

// VerifyFile validates one segment file against its descriptor without a
// Reader, useful for object-store download checks.
func VerifyFile(dir string, clusterID string, info SegmentInfo) error {
	_, err := readSegmentFile(context.Background(), dir, clusterID, info, nil)
	return err
}
