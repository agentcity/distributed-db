package pitr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultSegmentMaxBytes = int64(4 << 20)
	defaultSegmentMaxAge   = 30 * time.Second
)

// WriterConfig configures one archive writer. A directory must have at most
// one live Writer.
type WriterConfig struct {
	// Dir is the archive root. It is created if it does not exist.
	Dir string
	// ClusterID identifies the source cluster. Every segment and the manifest
	// carry it; a mismatch with an existing archive is refused.
	ClusterID string
	// Incarnation distinguishes cluster lifetimes. Zero adopts the value from
	// an existing manifest, or zero for a new archive.
	Incarnation uint64
	// WriterEpoch identifies the archiver instance. It is recorded in each
	// segment for split-brain diagnostics; this MVP does not enforce epochs.
	WriterEpoch uint64
	// SegmentMaxBytes bounds buffered records before a seal is triggered. It
	// is a target, not a hard limit: one record may exceed it. The hard cap is
	// 256 MiB. Zero selects 4 MiB.
	SegmentMaxBytes int64
	// SegmentMaxAge seals buffered records on the next Append/Seal once the
	// oldest buffered record is this old. Zero selects 30 seconds.
	SegmentMaxAge time.Duration
	// Now supplies time for seal timestamps and retention. Nil selects
	// time.Now. It exists so independent writers can be tested without global
	// clocks.
	Now func() time.Time
}

// Writer appends committed Raft log entries to immutable segments and
// atomically publishes them in the manifest.
//
// Records are buffered in memory until Seal, Close or a size/age trigger. A
// crash therefore loses records that were never sealed; this MVP does not
// claim RPO 0. A sealed segment is fsynced before its manifest entry is
// fsynced and published.
type Writer struct {
	mu          sync.Mutex
	dir         string
	clusterID   string
	incarnation uint64
	writerEpoch uint64
	maxBytes    int64
	maxAge      time.Duration
	now         func() time.Time

	manifest *Manifest
	ranges   map[uint64]*rangeState
	closed   bool

	hooks writerHooks
}

type rangeState struct {
	nextIndex    uint64 // 0 means the first index has not been chosen yet
	lastTerm     uint64
	lastSHA      string
	pending      []LogRecord
	pendingBytes int64
	firstAt      time.Time
}

// writerHooks is a test seam. It is per-Writer, never global.
type writerHooks struct {
	beforeRename func(tmp, final string) error
}

// NewWriter opens or initializes an archive directory. It removes stale temp
// files and unreferenced segments left by a crash between rename and manifest
// publication, and refuses a manifest that references a missing segment.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	if cfg.Dir == "" {
		return nil, errors.New("pitr: WriterConfig.Dir is required")
	}
	if cfg.ClusterID == "" {
		return nil, errors.New("pitr: WriterConfig.ClusterID is required")
	}
	if len(cfg.ClusterID) > maxClusterID {
		return nil, errors.New("pitr: WriterConfig.ClusterID is too long")
	}
	maxBytes := cfg.SegmentMaxBytes
	if maxBytes == 0 {
		maxBytes = defaultSegmentMaxBytes
	}
	if maxBytes < 0 || maxBytes > maxSegmentBytes {
		return nil, fmt.Errorf("pitr: SegmentMaxBytes %d is outside (0,%d]", maxBytes, maxSegmentBytes)
	}
	maxAge := cfg.SegmentMaxAge
	if maxAge == 0 {
		maxAge = defaultSegmentMaxAge
	}
	if maxAge < 0 {
		return nil, errors.New("pitr: SegmentMaxAge must not be negative")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	w := &Writer{
		dir:         filepath.Clean(cfg.Dir),
		clusterID:   cfg.ClusterID,
		incarnation: cfg.Incarnation,
		writerEpoch: cfg.WriterEpoch,
		maxBytes:    maxBytes,
		maxAge:      maxAge,
		now:         now,
		ranges:      make(map[uint64]*rangeState),
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return nil, fmt.Errorf("pitr: create archive directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(w.dir, SegmentsDir), 0o755); err != nil {
		return nil, fmt.Errorf("pitr: create segments directory: %w", err)
	}
	if err := syncDir(filepath.Dir(w.dir)); err != nil {
		return nil, fmt.Errorf("pitr: sync archive parent directory: %w", err)
	}
	if err := syncDir(w.dir); err != nil {
		return nil, fmt.Errorf("pitr: sync archive directory: %w", err)
	}
	m, exists, err := loadManifestIfExists(w.dir)
	if err != nil {
		return nil, err
	}
	if exists {
		if m.ClusterID != cfg.ClusterID {
			return nil, fmt.Errorf("%w: archive cluster %q, writer cluster %q",
				ErrManifest, m.ClusterID, cfg.ClusterID)
		}
		if cfg.Incarnation != 0 && m.Incarnation != cfg.Incarnation {
			return nil, fmt.Errorf("%w: archive incarnation %d, writer incarnation %d",
				ErrManifest, m.Incarnation, cfg.Incarnation)
		}
		w.incarnation = m.Incarnation
		if err := reconcileArchive(w.dir, m); err != nil {
			return nil, err
		}
		w.manifest = m
	} else {
		if err := ensureNoSegments(w.dir); err != nil {
			return nil, err
		}
		nowUTC := now().UTC()
		if nowUTC.IsZero() {
			return nil, errors.New("pitr: WriterConfig.Now returned zero time")
		}
		w.manifest = &Manifest{
			Format:        FormatName,
			FormatVersion: FormatVersion,
			ClusterID:     cfg.ClusterID,
			Incarnation:   cfg.Incarnation,
			CreatedAt:     nowUTC,
			UpdatedAt:     nowUTC,
		}
		if err := writeManifestDir(w.dir, w.manifest); err != nil {
			return nil, err
		}
	}
	w.initRangeStatesLocked()
	return w, nil
}

// Manifest returns a deep copy of the current manifest.
func (w *Writer) Manifest() *Manifest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.manifest.Clone()
}

// Append buffers committed Raft entries for one range. Entries must be
// contiguous with the last sealed index (or the last buffered index) of that
// range. The records are copied, so the caller may reuse the slice and values.
func (w *Writer) Append(ctx context.Context, rangeID uint64, records []LogRecord) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if rangeID == 0 {
		return fmt.Errorf("%w: range id is zero", ErrRecord)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if len(records) == 0 {
		return nil
	}
	rs := w.ranges[rangeID]
	if rs == nil {
		rs = &rangeState{}
		w.ranges[rangeID] = rs
	}
	// Validate the whole batch before mutating writer state so a bad record
	// does not leave a partially buffered retry.
	expect := rs.nextIndex
	lastTerm := rs.lastTerm
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rec.Validate(); err != nil {
			return err
		}
		if expect != 0 && rec.Entry.Index != expect {
			return fmt.Errorf("%w: %w: range %d index %d, want %d",
				ErrGap, ErrIndex, rangeID, rec.Entry.Index, expect)
		}
		if lastTerm != 0 && rec.Entry.Term < lastTerm {
			return fmt.Errorf("%w: range %d term regresses at index %d", ErrRecord, rangeID, rec.Entry.Index)
		}
		expect = rec.Entry.Index + 1
		lastTerm = rec.Entry.Term
	}
	now := w.now().UTC()
	if err := w.sealIfAgedLocked(ctx, rangeID, rs, now); err != nil {
		return err
	}
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, err := encodedRecordSize(rec)
		if err != nil {
			return err
		}
		if len(rs.pending) > 0 && rs.pendingBytes+size > w.maxBytes {
			if err := w.sealRangeLocked(ctx, rangeID, rs, now); err != nil {
				return err
			}
		}
		if len(rs.pending) == 0 {
			rs.firstAt = now
		}
		rs.pending = append(rs.pending, cloneLogRecord(rec))
		rs.pendingBytes += size
		rs.nextIndex = rec.Entry.Index + 1
		rs.lastTerm = rec.Entry.Term
	}
	if rs.pendingBytes >= w.maxBytes {
		if err := w.sealRangeLocked(ctx, rangeID, rs, w.now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// Seal publishes every buffered range as one segment per range. The Writer
// remains usable.
func (w *Writer) Seal(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.sealAllLocked(ctx)
}

// Close seals all buffered records and closes the writer. It is idempotent.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.sealAllLocked(context.Background()); err != nil {
		return err
	}
	w.closed = true
	return nil
}

func (w *Writer) sealAllLocked(ctx context.Context) error {
	ids := make([]uint64, 0, len(w.ranges))
	for id, rs := range w.ranges {
		if len(rs.pending) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	now := w.now().UTC()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.sealRangeLocked(ctx, id, w.ranges[id], now); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) sealIfAgedLocked(ctx context.Context, rangeID uint64, rs *rangeState, now time.Time) error {
	if len(rs.pending) == 0 || w.maxAge <= 0 || rs.firstAt.IsZero() {
		return nil
	}
	if now.Sub(rs.firstAt) < w.maxAge {
		return nil
	}
	return w.sealRangeLocked(ctx, rangeID, rs, now)
}

func (w *Writer) sealRangeLocked(ctx context.Context, rangeID uint64, rs *rangeState, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rs.pending) == 0 {
		return nil
	}
	recs := rs.pending
	h := SegmentHeader{
		ClusterID:   w.clusterID,
		RangeID:     rangeID,
		Incarnation: w.incarnation,
		StartIndex:  recs[0].Entry.Index,
		EndIndex:    recs[len(recs)-1].Entry.Index,
		StartTerm:   recs[0].Entry.Term,
		EndTerm:     recs[len(recs)-1].Entry.Term,
		EntryCount:  uint64(len(recs)),
		MinTSO:      recs[0].TSO,
		MaxTSO:      recs[0].TSO,
		WriterEpoch: w.writerEpoch,
		SealedAt:    now,
		PrevSHA256:  rs.lastSHA,
	}
	for i, rec := range recs {
		if rec.TSO < h.MinTSO {
			h.MinTSO = rec.TSO
		}
		if rec.TSO > h.MaxTSO {
			h.MaxTSO = rec.TSO
		}
		payload, err := encodeLogRecordPayload(rec)
		if err != nil {
			return err
		}
		h.PayloadBytes += uint64(len(payload))
		if i > 0 && rec.Entry.Index != recs[i-1].Entry.Index+1 {
			return fmt.Errorf("%w: range %d buffered indices are not contiguous", ErrGap, rangeID)
		}
	}
	data, sha, err := encodeSegment(h, recs)
	if err != nil {
		return err
	}
	info := SegmentInfo{
		RangeID:      h.RangeID,
		StartIndex:   h.StartIndex,
		EndIndex:     h.EndIndex,
		StartTerm:    h.StartTerm,
		EndTerm:      h.EndTerm,
		EntryCount:   h.EntryCount,
		MinTSO:       h.MinTSO,
		MaxTSO:       h.MaxTSO,
		WriterEpoch:  h.WriterEpoch,
		PayloadBytes: h.PayloadBytes,
		SizeBytes:    int64(len(data)),
		SHA256:       sha,
		PrevSHA256:   h.PrevSHA256,
		Path:         segmentRelPath(rangeID, h.StartIndex, h.EndIndex, sha),
		SealedAt:     h.SealedAt,
	}
	if err := info.Validate(); err != nil {
		return err
	}
	rangeDir := filepath.Join(w.dir, SegmentsDir, rangeDirName(rangeID))
	if err := os.MkdirAll(rangeDir, 0o755); err != nil {
		return fmt.Errorf("pitr: create range segment directory: %w", err)
	}
	if err := syncDir(filepath.Join(w.dir, SegmentsDir)); err != nil {
		return fmt.Errorf("pitr: sync segments directory: %w", err)
	}
	finalPath := filepath.Join(w.dir, filepath.FromSlash(info.Path))
	tmp, err := os.CreateTemp(rangeDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("pitr: create segment temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("pitr: chmod segment temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("pitr: write segment temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("pitr: sync segment temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pitr: close segment temp: %w", err)
	}
	if w.hooks.beforeRename != nil {
		if err := w.hooks.beforeRename(tmpName, finalPath); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return fmt.Errorf("pitr: publish segment: %w", err)
	}
	if err := syncDir(rangeDir); err != nil {
		_ = os.Remove(finalPath)
		_ = syncDir(rangeDir)
		return fmt.Errorf("pitr: sync range segment directory: %w", err)
	}

	next := w.manifest.Clone()
	next.Segments = append(next.Segments, info)
	sortSegments(next.Segments)
	touchManifest(next, now)
	if err := next.Validate(); err != nil {
		_ = os.Remove(finalPath)
		_ = syncDir(rangeDir)
		return err
	}
	if err := writeManifestDir(w.dir, next); err != nil {
		_ = os.Remove(finalPath)
		_ = syncDir(rangeDir)
		return err
	}
	w.manifest = next
	rs.pending = nil
	rs.pendingBytes = 0
	rs.firstAt = time.Time{}
	rs.lastSHA = info.SHA256
	rs.lastTerm = info.EndTerm
	rs.nextIndex = info.EndIndex + 1
	return nil
}

func (w *Writer) initRangeStatesLocked() {
	w.ranges = make(map[uint64]*rangeState)
	for _, s := range w.manifest.Segments {
		rs := w.ranges[s.RangeID]
		if rs == nil {
			rs = &rangeState{}
			w.ranges[s.RangeID] = rs
		}
		rs.nextIndex = s.EndIndex + 1
		rs.lastTerm = s.EndTerm
		rs.lastSHA = s.SHA256
	}
	for _, t := range w.manifest.Trimmed {
		rs := w.ranges[t.RangeID]
		if rs == nil {
			rs = &rangeState{}
			w.ranges[t.RangeID] = rs
		}
		if rs.nextIndex == 0 {
			rs.nextIndex = t.ThroughIndex + 1
		}
	}
}

func encodedRecordSize(rec LogRecord) (int64, error) {
	payload, err := encodeLogRecordPayload(rec)
	if err != nil {
		return 0, err
	}
	return int64(len(payload) + 8), nil
}

func cloneLogRecord(rec LogRecord) LogRecord {
	out := rec
	out.Entry.Value = append([]byte(nil), rec.Entry.Value...)
	return out
}

func touchManifest(m *Manifest, now time.Time) {
	now = now.UTC()
	if now.IsZero() {
		return
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() || now.After(m.UpdatedAt) {
		m.UpdatedAt = now
	}
	if m.UpdatedAt.Before(m.CreatedAt) {
		m.UpdatedAt = m.CreatedAt
	}
}

// reconcileArchive removes crash leftovers and verifies that every manifest
// segment exists with the descriptor's size. Unreferenced .seg files are the
// result of a crash after segment rename and before manifest publication; the
// append that produced them was never acknowledged, so they are removed.
func reconcileArchive(dir string, m *Manifest) error {
	referenced := make(map[string]SegmentInfo, len(m.Segments))
	for _, s := range m.Segments {
		referenced[s.Path] = s
	}
	seen := make(map[string]bool, len(m.Segments))
	root := filepath.Join(dir, SegmentsDir)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("pitr: scan archive segments: %w", walkErr)
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") || strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("pitr: remove stale temp %s: %w", p, err)
			}
			return nil
		}
		if !strings.HasSuffix(name, ".seg") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return fmt.Errorf("pitr: relative segment path: %w", err)
		}
		rel = filepath.ToSlash(rel)
		info, ok := referenced[rel]
		if !ok {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("pitr: remove orphan segment %s: %w", p, err)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("pitr: stat segment %s: %w", p, err)
		}
		if fi.Size() != info.SizeBytes {
			return fmt.Errorf("%w: segment %s size %d, manifest says %d",
				ErrCorrupt, rel, fi.Size(), info.SizeBytes)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	for path := range referenced {
		if !seen[path] {
			return fmt.Errorf("%w: %w: segment %s", ErrGap, ErrMissing, path)
		}
	}
	return nil
}

func ensureNoSegments(dir string) error {
	root := filepath.Join(dir, SegmentsDir)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(p)
			return nil
		}
		if strings.HasSuffix(name, ".seg") {
			return fmt.Errorf("%w: refusing to initialize over existing segment %s", ErrManifest, p)
		}
		return nil
	})
}
