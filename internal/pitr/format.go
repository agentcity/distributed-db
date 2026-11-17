package pitr

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"distributed-db/internal/model"
)

const (
	// FormatName identifies the archive manifest format.
	FormatName = "distributed-db-pitr-archive"
	// FormatVersion is the manifest and segment schema version understood by
	// this package. A reader refuses every other version.
	FormatVersion = 1

	// ManifestFile is the manifest file name inside an archive directory.
	ManifestFile = "manifest.json"
	// SegmentsDir is the directory holding immutable segment files.
	SegmentsDir = "segments"

	// SegmentFormatVersion is the binary segment framing version.
	SegmentFormatVersion = 1

	segmentMagic       = "DDBPITR1" // 8 bytes: segment prologue
	segmentFooterMagic = "DDBPIT1E" // 8 bytes: segment epilogue
	segmentHeaderPref  = 20         // magic + version + flags + header length + header CRC
	segmentFooterLen   = 40         // footer magic + SHA-256 of the preceding bytes
	maxHeaderBody      = 64 << 10
	maxClusterID       = 1<<16 - 1
	maxRecordPayload   = 32 << 20
	// maxSegmentBytes is a hard upper bound accepted by reader and writer. It
	// keeps a single malformed descriptor from causing an unbounded read.
	maxSegmentBytes = 256 << 20
)

var (
	// ErrCorrupt is returned when a segment fails a checksum or structural
	// validation.
	ErrCorrupt = errors.New("pitr: corrupt archive")
	// ErrGap is returned when indices are not contiguous or a required
	// segment was removed.
	ErrGap = errors.New("pitr: archive gap")
	// ErrManifest is returned when the archive manifest is missing, malformed
	// or inconsistent.
	ErrManifest = errors.New("pitr: invalid archive manifest")
	// ErrMissing is returned when a manifest references an object that is not
	// present on disk.
	ErrMissing = errors.New("pitr: missing archive object")
	// ErrRecord is returned when a log record is invalid.
	ErrRecord = errors.New("pitr: invalid log record")
	// ErrIndex is returned when a Raft index is not the expected successor.
	ErrIndex = errors.New("pitr: non-contiguous raft index")
	// ErrClosed is returned by Writer methods after Close.
	ErrClosed = errors.New("pitr: archive writer closed")
	// ErrTargetBeforeBase is returned when a restore target precedes the base
	// snapshot.
	ErrTargetBeforeBase = errors.New("pitr: target TSO precedes base snapshot")
	// ErrRangeMismatch is returned when archive ranges do not match the base
	// range map requested by a restore plan.
	ErrRangeMismatch = errors.New("pitr: range mismatch")
	// ErrInvalidBase is returned when the base backup manifest is unusable.
	ErrInvalidBase = errors.New("pitr: invalid base backup manifest")
)

// crc32cTable is immutable after package initialization.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// LogRecord is one committed Raft log entry plus the commit TSO that the
// archiver observed for it. TSO may be zero for entries that carry no commit
// timestamp (for example a noop or a membership change).
type LogRecord struct {
	TSO   uint64      `json:"tso"`
	Entry model.Entry `json:"entry"`
}

// Validate checks the record's framing and identity fields.
func (r LogRecord) Validate() error {
	if r.Entry.Index == 0 {
		return fmt.Errorf("%w: raft index is zero", ErrRecord)
	}
	if r.Entry.Index == ^uint64(0) {
		return fmt.Errorf("%w: raft index overflows", ErrRecord)
	}
	if r.Entry.Term == 0 {
		return fmt.Errorf("%w: raft term is zero at index %d", ErrRecord, r.Entry.Index)
	}
	if r.Entry.Type == "" {
		return fmt.Errorf("%w: empty command type at index %d", ErrRecord, r.Entry.Index)
	}
	if len(r.Entry.Type) > 1<<16-1 {
		return fmt.Errorf("%w: command type is too long", ErrRecord)
	}
	if uint64(len(r.Entry.Key)) > 1<<32-1 {
		return fmt.Errorf("%w: key is too long", ErrRecord)
	}
	if uint64(len(r.Entry.Value)) > 1<<32-1 {
		return fmt.Errorf("%w: value is too long", ErrRecord)
	}
	payload, err := encodeLogRecordPayload(r)
	if err != nil {
		return err
	}
	if len(payload) > maxRecordPayload {
		return fmt.Errorf("%w: record payload %d exceeds %d bytes", ErrRecord, len(payload), maxRecordPayload)
	}
	return nil
}

// SegmentHeader is the self-describing prefix of one immutable segment.
// Start and End are inclusive Raft indices. MinTSO and MaxTSO are observed
// bounds and are not assumed to be monotonic in index order.
type SegmentHeader struct {
	ClusterID    string
	RangeID      uint64
	Incarnation  uint64
	StartIndex   uint64
	EndIndex     uint64
	StartTerm    uint64
	EndTerm      uint64
	EntryCount   uint64
	MinTSO       uint64
	MaxTSO       uint64
	WriterEpoch  uint64
	PayloadBytes uint64
	SealedAt     time.Time
	PrevSHA256   string
}

// Validate checks all header invariants that can be checked without records.
func (h SegmentHeader) Validate() error {
	if h.ClusterID == "" {
		return fmt.Errorf("%w: empty cluster id", ErrCorrupt)
	}
	if len(h.ClusterID) > maxClusterID {
		return fmt.Errorf("%w: cluster id is too long", ErrCorrupt)
	}
	if h.RangeID == 0 {
		return fmt.Errorf("%w: range id is zero", ErrCorrupt)
	}
	if h.StartIndex == 0 || h.EndIndex < h.StartIndex {
		return fmt.Errorf("%w: invalid index range [%d,%d]", ErrCorrupt, h.StartIndex, h.EndIndex)
	}
	if h.EntryCount != h.EndIndex-h.StartIndex+1 {
		return fmt.Errorf("%w: entry count %d does not match index range [%d,%d]",
			ErrCorrupt, h.EntryCount, h.StartIndex, h.EndIndex)
	}
	if h.StartTerm == 0 || h.EndTerm == 0 {
		return fmt.Errorf("%w: zero raft term", ErrCorrupt)
	}
	if h.MinTSO > h.MaxTSO {
		return fmt.Errorf("%w: min tso %d > max tso %d", ErrCorrupt, h.MinTSO, h.MaxTSO)
	}
	if h.SealedAt.IsZero() || h.SealedAt.UnixNano() <= 0 {
		return fmt.Errorf("%w: missing seal time", ErrCorrupt)
	}
	if h.PrevSHA256 != "" {
		if _, err := decodeSHA256(h.PrevSHA256); err != nil {
			return fmt.Errorf("%w: invalid previous segment hash: %v", ErrCorrupt, err)
		}
	}
	return nil
}

// SegmentInfo is the manifest descriptor for one published segment. SHA256
// covers the complete file bytes (header, records and footer).
type SegmentInfo struct {
	RangeID      uint64    `json:"range_id"`
	StartIndex   uint64    `json:"start_index"`
	EndIndex     uint64    `json:"end_index"`
	StartTerm    uint64    `json:"start_term"`
	EndTerm      uint64    `json:"end_term"`
	EntryCount   uint64    `json:"entry_count"`
	MinTSO       uint64    `json:"min_tso"`
	MaxTSO       uint64    `json:"max_tso"`
	WriterEpoch  uint64    `json:"writer_epoch"`
	PayloadBytes uint64    `json:"payload_bytes"`
	SizeBytes    int64     `json:"size_bytes"`
	SHA256       string    `json:"sha256"`
	PrevSHA256   string    `json:"prev_sha256,omitempty"`
	Path         string    `json:"path"`
	SealedAt     time.Time `json:"sealed_at"`
}

// Validate checks one manifest segment descriptor, including that its path is
// local, belongs to the declared range and agrees with the index range and
// hash prefix encoded in the file name.
func (s SegmentInfo) Validate() error {
	if s.RangeID == 0 {
		return fmt.Errorf("%w: segment has zero range id", ErrManifest)
	}
	if s.StartIndex == 0 || s.EndIndex < s.StartIndex {
		return fmt.Errorf("%w: segment has invalid index range [%d,%d]", ErrManifest, s.StartIndex, s.EndIndex)
	}
	if s.EntryCount != s.EndIndex-s.StartIndex+1 {
		return fmt.Errorf("%w: segment entry_count %d does not match [%d,%d]",
			ErrManifest, s.EntryCount, s.StartIndex, s.EndIndex)
	}
	if s.StartTerm == 0 || s.EndTerm == 0 {
		return fmt.Errorf("%w: segment has zero term", ErrManifest)
	}
	if s.MinTSO > s.MaxTSO {
		return fmt.Errorf("%w: segment min_tso > max_tso", ErrManifest)
	}
	if s.SizeBytes <= 0 || s.SizeBytes > maxSegmentBytes {
		return fmt.Errorf("%w: segment size %d is outside (0,%d]", ErrManifest, s.SizeBytes, maxSegmentBytes)
	}
	if s.SealedAt.IsZero() || s.SealedAt.UnixNano() <= 0 {
		return fmt.Errorf("%w: segment has no seal time", ErrManifest)
	}
	sha, err := decodeSHA256(s.SHA256)
	if err != nil {
		return fmt.Errorf("%w: segment sha256: %v", ErrManifest, err)
	}
	if s.PrevSHA256 != "" {
		if _, err := decodeSHA256(s.PrevSHA256); err != nil {
			return fmt.Errorf("%w: segment prev_sha256: %v", ErrManifest, err)
		}
	}
	if err := validateSegmentPath(s.Path, s); err != nil {
		return err
	}
	dir, name := path.Split(s.Path)
	if dir != path.Join(SegmentsDir, rangeDirName(s.RangeID))+"/" {
		return fmt.Errorf("%w: segment %q is not in the range directory", ErrManifest, s.Path)
	}
	if !strings.HasSuffix(name, ".seg") {
		return fmt.Errorf("%w: segment %q has no .seg suffix", ErrManifest, s.Path)
	}
	wantName := segmentFileName(s.StartIndex, s.EndIndex, hex.EncodeToString(sha))
	if name != wantName {
		return fmt.Errorf("%w: segment %q, want %q", ErrManifest, s.Path, path.Join(dir, wantName))
	}
	return nil
}

// TrimmedRange records the contiguous prefix of a range's archive that GC has
// removed. Any record that a restore still needs must not be below this
// frontier; MaxTSO is the largest MaxTSO among the removed segments.
type TrimmedRange struct {
	RangeID      uint64 `json:"range_id"`
	ThroughIndex uint64 `json:"through_index"`
	MaxTSO       uint64 `json:"max_tso"`
}

// Manifest is the root index of an archive directory. Segments are sorted by
// range id and start index. Trimmed ranges are sorted by range id.
type Manifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	ClusterID     string         `json:"cluster_id"`
	Incarnation   uint64         `json:"incarnation"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Segments      []SegmentInfo  `json:"segments"`
	Trimmed       []TrimmedRange `json:"trimmed,omitempty"`
}

// Validate checks the manifest's own framing, ordering, per-range
// contiguity, hash chain and trim frontier. It does not open segment files;
// Verify and Replay do that.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrManifest)
	}
	if m.Format != FormatName {
		return fmt.Errorf("%w: format %q, want %q", ErrManifest, m.Format, FormatName)
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("%w: format version %d, want %d", ErrManifest, m.FormatVersion, FormatVersion)
	}
	if m.ClusterID == "" || len(m.ClusterID) > maxClusterID {
		return fmt.Errorf("%w: invalid cluster id", ErrManifest)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: missing created_at/updated_at", ErrManifest)
	}
	if m.UpdatedAt.Before(m.CreatedAt) {
		return fmt.Errorf("%w: updated_at precedes created_at", ErrManifest)
	}

	trimmed := make(map[uint64]TrimmedRange, len(m.Trimmed))
	var prevTrim uint64
	for i, t := range m.Trimmed {
		if t.RangeID == 0 {
			return fmt.Errorf("%w: trimmed entry %d has zero range id", ErrManifest, i)
		}
		if i > 0 && t.RangeID < prevTrim {
			return fmt.Errorf("%w: trimmed entries are not sorted by range id", ErrManifest)
		}
		if i > 0 && t.RangeID == prevTrim {
			return fmt.Errorf("%w: duplicate trimmed range %d", ErrManifest, t.RangeID)
		}
		if t.ThroughIndex == 0 {
			return fmt.Errorf("%w: trimmed range %d has zero through_index", ErrManifest, t.RangeID)
		}
		trimmed[t.RangeID] = t
		prevTrim = t.RangeID
	}

	seenPaths := make(map[string]struct{}, len(m.Segments))
	var prevRange uint64
	var prevIdx int
	for i := range m.Segments {
		s := &m.Segments[i]
		if err := s.Validate(); err != nil {
			return err
		}
		if _, ok := seenPaths[s.Path]; ok {
			return fmt.Errorf("%w: segment path %q is reused", ErrManifest, s.Path)
		}
		seenPaths[s.Path] = struct{}{}
		if i > 0 {
			if s.RangeID < prevRange {
				return fmt.Errorf("%w: segments are not sorted by range id", ErrManifest)
			}
			if s.RangeID == prevRange {
				prev := &m.Segments[prevIdx]
				if s.StartIndex != prev.EndIndex+1 {
					return fmt.Errorf("%w: %w: range %d has index gap between %d and %d",
						ErrManifest, ErrGap, s.RangeID, prev.EndIndex, s.StartIndex)
				}
				if s.PrevSHA256 != prev.SHA256 {
					return fmt.Errorf("%w: %w: range %d hash chain breaks at index %d",
						ErrManifest, ErrGap, s.RangeID, s.StartIndex)
				}
			}
		}

		if s.RangeID != prevRange || i == 0 {
			// First retained segment of a range. A chain predecessor may be
			// absent only when GC has recorded a trim that ends immediately
			// before this segment.
			if t, ok := trimmed[s.RangeID]; ok {
				if t.ThroughIndex >= s.StartIndex {
					return fmt.Errorf("%w: range %d trim through %d overlaps segment starting at %d",
						ErrManifest, s.RangeID, t.ThroughIndex, s.StartIndex)
				}
				if t.ThroughIndex+1 != s.StartIndex {
					return fmt.Errorf("%w: %w: range %d is missing segments (%d..%d]",
						ErrManifest, ErrGap, s.RangeID, t.ThroughIndex, s.StartIndex)
				}
			} else if s.PrevSHA256 != "" {
				return fmt.Errorf("%w: %w: range %d first retained segment has an absent predecessor",
					ErrManifest, ErrGap, s.RangeID)
			}
		}
		prevRange = s.RangeID
		prevIdx = i
	}

	// A trimmed range with no retained segments is valid: everything for that
	// range was reclaimed. Nothing else needs checking for it here.
	return nil
}

// Clone returns a deep copy safe to mutate independently.
func (m *Manifest) Clone() *Manifest {
	if m == nil {
		return nil
	}
	out := *m
	out.Segments = append([]SegmentInfo(nil), m.Segments...)
	out.Trimmed = append([]TrimmedRange(nil), m.Trimmed...)
	return &out
}

// TrimmedFor returns the trim frontier for rangeID.
func (m *Manifest) TrimmedFor(rangeID uint64) (TrimmedRange, bool) {
	for _, t := range m.Trimmed {
		if t.RangeID == rangeID {
			return t, true
		}
	}
	return TrimmedRange{}, false
}

// SegmentRanges returns the distinct range ids present in the manifest, sorted
// ascending. The result is derived only from Segments; a fully trimmed range
// has no retained segment and is therefore not listed.
func (m *Manifest) SegmentRanges() []uint64 {
	var ids []uint64
	var last uint64
	for _, s := range m.Segments {
		if len(ids) == 0 || s.RangeID != last {
			ids = append(ids, s.RangeID)
			last = s.RangeID
		}
	}
	return ids
}

// SegmentsForRange returns the retained segments of rangeID in index order.
// The returned slice is a copy.
func (m *Manifest) SegmentsForRange(rangeID uint64) []SegmentInfo {
	var out []SegmentInfo
	for _, s := range m.Segments {
		if s.RangeID == rangeID {
			out = append(out, s)
		}
	}
	return out
}

func rangeDirName(rangeID uint64) string {
	return fmt.Sprintf("range-%016x", rangeID)
}

func segmentFileName(start, end uint64, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("%020d-%020d-%s.seg", start, end, sha)
}

func segmentRelPath(rangeID, start, end uint64, sha string) string {
	return path.Join(SegmentsDir, rangeDirName(rangeID), segmentFileName(start, end, sha))
}

func validateSegmentPath(p string, s SegmentInfo) error {
	if p == "" {
		return fmt.Errorf("%w: empty segment path", ErrManifest)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("%w: segment path %q must use forward slashes", ErrManifest, p)
	}
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return fmt.Errorf("%w: segment path %q must be relative", ErrManifest, p)
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: segment path %q escapes the archive", ErrManifest, p)
	}
	if !strings.HasPrefix(clean, SegmentsDir+"/") {
		return fmt.Errorf("%w: segment path %q is outside %s/", ErrManifest, p, SegmentsDir)
	}
	if !filepath.IsLocal(filepath.FromSlash(clean)) {
		return fmt.Errorf("%w: segment path %q is not local", ErrManifest, p)
	}
	parts := strings.Split(clean, "/")
	if len(parts) != 3 {
		return fmt.Errorf("%w: segment path %q has unexpected depth", ErrManifest, p)
	}
	if parts[1] != rangeDirName(s.RangeID) {
		return fmt.Errorf("%w: segment path %q is not in %s", ErrManifest, p, rangeDirName(s.RangeID))
	}
	return nil
}

func decodeSHA256(s string) ([]byte, error) {
	if len(s) != sha256.Size*2 {
		return nil, fmt.Errorf("want %d lowercase hex characters", sha256.Size*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// compareKeys implements archive/base key ordering: nil is -inf. An empty
// non-nil key is treated as a real (smallest) key by bytes.Compare, matching
// the backup package's representation.
func compareKeys(a, b []byte) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	return bytes.Compare(a, b)
}

// sortSegments orders segments by range id and start index.
func sortSegments(segs []SegmentInfo) {
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].RangeID != segs[j].RangeID {
			return segs[i].RangeID < segs[j].RangeID
		}
		return segs[i].StartIndex < segs[j].StartIndex
	})
}

// sortTrimmed orders trim records by range id.
func sortTrimmed(ts []TrimmedRange) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].RangeID < ts[j].RangeID })
}

// appendU16/U32/U64 and readU16/U32/U64 implement the fixed little-endian-free
// (big-endian) segment encoding. There is deliberately no JSON in the hot
// record path.
func appendU16(dst []byte, v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return append(dst, b[:]...)
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func appendU64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

func readU16(src []byte, off *int) (uint16, error) {
	if *off+2 > len(src) {
		return 0, fmt.Errorf("%w: truncated u16", ErrCorrupt)
	}
	v := binary.BigEndian.Uint16(src[*off:])
	*off += 2
	return v, nil
}

func readU32(src []byte, off *int) (uint32, error) {
	if *off+4 > len(src) {
		return 0, fmt.Errorf("%w: truncated u32", ErrCorrupt)
	}
	v := binary.BigEndian.Uint32(src[*off:])
	*off += 4
	return v, nil
}

func readU64(src []byte, off *int) (uint64, error) {
	if *off+8 > len(src) {
		return 0, fmt.Errorf("%w: truncated u64", ErrCorrupt)
	}
	v := binary.BigEndian.Uint64(src[*off:])
	*off += 8
	return v, nil
}
