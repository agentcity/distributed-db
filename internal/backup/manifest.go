// Package backup implements logical, point-in-time backups of the v2 KV
// plane. A backup is a directory containing one JSON manifest and a sequence
// of chunk files. Every chunk is a small, checksummed snapshot of one range;
// chunks hold at most ChunkKeys keys or ChunkBytes of key+value payload, so
// backup and restore use memory proportional to one chunk, not one range.
//
// The format is deliberately self-contained: it records the meta range
// descriptors and the TSO at which every range was scanned. Restore can
// therefore validate the target's range layout before writing anything.
//
// # Snapshot consistency
//
// A backup is a cut at one TSO, not a long-running history. It is consistent
// only when the scan is lock-aware: mvcc.Store.ScanForBackup reads the lock
// column before the write column from one engine snapshot and fails with
// ErrLocked if a lock with startTS <= the snapshot TSO exists. Because a commit
// installs its data/write records and removes the lock in one atomic batch, a
// key is either visibly locked (the backup aborts and retries) or visibly
// committed in that same snapshot, so a committed value at or below the TSO
// cannot be silently omitted. Backup applies a bounded retry budget around that
// abort; a persistent lock becomes a clear error.
//
// The default v2client path cannot enumerate the lock column: it can only
// probe the locks of keys returned by Txn.Scan through CheckTxnStatus. Supply
// BackupConfig.Scanner (for example StoreScanner for an in-process store) to
// get the full lock-aware guarantee, and set RequireLockAwareScan to reject
// the fallback. Independently, a commit allocated at or below the snapshot TSO
// whose atomic write batch has not applied yet (notably 1PC) needs an applied-
// commit barrier. BackupConfig.Barrier is called with the snapshot TSO after
// it is obtained and before the first scan; StoreScanner supplies it
// implicitly through mvcc.Store.WaitApplied. RequireLockAwareScan fails closed
// with ErrBackupBarrierUnavailable when no barrier is available. The barrier
// cannot close the timestamp-allocation window by itself: the allocator must
// hold an mvcc.CommitLease across allocation and apply for that guarantee.
//
// # Point-in-time scope
//
// This package is not a complete PITR implementation. It has no continuous
// Raft-log archive, no global checkpoint or safe point, no meta/TSO archive,
// no encryption/retention policy, and no atomic cluster cutover. Restore is a
// best-effort logical import into an existing target: it overwrites keys
// present in the backup and does not delete keys absent from it, so it is
// idempotent for an unchanged backup only. Use RequireEmpty, a
// dedicated/fenced target cluster and the VerifyHook for an MVP restore drill.
// internal/pitr can pair this manifest with an external archive and produce a
// deterministic restore-to-TSO plan, but it does not make the base restore or
// the cutover atomic; see docs/adr/0006-pitr.md for the remaining design.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// FormatName identifies the backup on-disk format.
	FormatName = "distributed-db-kvbackup"
	// FormatVersion is the manifest schema version understood by this package.
	FormatVersion = 1
	// ManifestFile is the manifest file name inside a backup directory.
	ManifestFile = "manifest.json"
	// ChunksDir is the chunk directory inside a backup directory.
	ChunksDir = "chunks"

	// DefaultChunkKeys bounds the number of keys in one chunk.
	DefaultChunkKeys = 1000
	// DefaultChunkBytes bounds the key+value payload of one chunk (1 MiB).
	DefaultChunkBytes = 1 << 20
)

var (
	// ErrCorrupt is returned when a backup file fails validation.
	ErrCorrupt = errors.New("backup: corrupt backup")
	// ErrManifest is returned when a manifest is missing or invalid.
	ErrManifest = errors.New("backup: invalid manifest")
	// ErrRangeMismatch is returned when the target range layout does not match
	// the layout recorded in the manifest.
	ErrRangeMismatch = errors.New("backup: range layout mismatch")
	// ErrTargetNotEmpty is returned by Restore with RequireEmpty when the
	// target already has a visible key.
	ErrTargetNotEmpty = errors.New("backup: restore target is not empty")
)

// Range is one meta range descriptor captured at backup time.
//
// Start is inclusive and End is exclusive. A nil Start means -inf and a nil
// End means +inf. Encoding a nil []byte as JSON null (and a non-nil empty
// slice as "") keeps the two cases distinct; empty boundaries are not valid
// for a real range map except at the ends.
type Range struct {
	ID       uint64   `json:"id"`
	Start    []byte   `json:"start"`
	End      []byte   `json:"end"`
	Replicas []string `json:"replicas,omitempty"`
	Leader   string   `json:"leader,omitempty"`
	Version  uint64   `json:"version"`
}

// Chunk describes one immutable chunk file.
type Chunk struct {
	// Index is the global, zero-based chunk order.
	Index int `json:"index"`
	// RangeID is the meta range the chunk was scanned from.
	RangeID uint64 `json:"range_id"`
	// File is the slash-separated path relative to the backup directory.
	File string `json:"file"`
	// KeyCount is the number of key/value records in the chunk.
	KeyCount int `json:"key_count"`
	// PayloadBytes is the sum of len(key)+len(value) over the chunk.
	PayloadBytes int64 `json:"payload_bytes"`
	// SizeBytes is the complete chunk file size, including framing and digest.
	SizeBytes int64 `json:"size_bytes"`
	// FirstKey and LastKey are the inclusive chunk bounds.
	FirstKey []byte `json:"first_key"`
	LastKey  []byte `json:"last_key"`
	// SHA256 is the lowercase hex digest of the chunk contents (header and
	// records, excluding the trailer digest).
	SHA256 string `json:"sha256"`
}

// Manifest is the root of a backup directory.
type Manifest struct {
	Format        string    `json:"format"`
	FormatVersion int       `json:"format_version"`
	CreatedAt     time.Time `json:"created_at"`
	// SnapshotTS is the v2 TSO at which every range was scanned.
	SnapshotTS uint64 `json:"snapshot_ts"`
	// ChunkKeys and ChunkBytes record the limits used to produce the chunks.
	ChunkKeys  int     `json:"chunk_keys"`
	ChunkBytes int64   `json:"chunk_bytes"`
	Ranges     []Range `json:"ranges"`
	Chunks     []Chunk `json:"chunks"`
	TotalKeys  int64   `json:"total_keys"`
	TotalBytes int64   `json:"total_bytes"`
}

// ChunkPath returns the absolute path of c.File inside dir after validating
// that the relative path is local to the backup directory.
func ChunkPath(dir string, c Chunk) (string, error) {
	if err := validateChunkFile(c.File); err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(c.File)), nil
}

func validateChunkFile(file string) error {
	if file == "" {
		return fmt.Errorf("%w: empty chunk file name", ErrManifest)
	}
	if strings.Contains(file, "\\") {
		return fmt.Errorf("%w: chunk file %q must use forward slashes", ErrManifest, file)
	}
	if path.IsAbs(file) || filepath.IsAbs(file) {
		return fmt.Errorf("%w: chunk file %q must be relative", ErrManifest, file)
	}
	clean := path.Clean(file)
	if clean != file || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: chunk file %q escapes the backup directory", ErrManifest, file)
	}
	if !strings.HasPrefix(clean, ChunksDir+"/") {
		return fmt.Errorf("%w: chunk file %q is outside %s/", ErrManifest, file, ChunksDir)
	}
	if !filepath.IsLocal(filepath.FromSlash(clean)) {
		return fmt.Errorf("%w: chunk file %q is not local", ErrManifest, file)
	}
	return nil
}

// LoadManifest reads and validates dir/manifest.json.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrManifest, path, err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrManifest, path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks all manifest invariants that do not require reading chunks.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrManifest)
	}
	if m.Format != FormatName {
		return fmt.Errorf("%w: format %q, want %q", ErrManifest, m.Format, FormatName)
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("%w: format_version %d, want %d", ErrManifest, m.FormatVersion, FormatVersion)
	}
	if m.SnapshotTS == 0 {
		return fmt.Errorf("%w: snapshot_ts is zero", ErrManifest)
	}
	if m.ChunkKeys < 0 || m.ChunkBytes < 0 {
		return fmt.Errorf("%w: negative chunk limits", ErrManifest)
	}
	if err := validateRangeList(m.Ranges); err != nil {
		return fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if m.TotalKeys < 0 || m.TotalBytes < 0 {
		return fmt.Errorf("%w: negative totals", ErrManifest)
	}

	byID := make(map[uint64]Range, len(m.Ranges))
	rangeIndex := make(map[uint64]int, len(m.Ranges))
	for i, r := range m.Ranges {
		byID[r.ID] = r
		rangeIndex[r.ID] = i
	}

	var keys, bytes int64
	lastRange := -1
	var prevLast []byte
	seenFiles := make(map[string]struct{}, len(m.Chunks))
	for i, c := range m.Chunks {
		if c.Index != i {
			return fmt.Errorf("%w: chunk %d has index %d", ErrManifest, i, c.Index)
		}
		r, ok := byID[c.RangeID]
		if !ok {
			return fmt.Errorf("%w: chunk %d references unknown range %d", ErrManifest, i, c.RangeID)
		}
		if err := validateChunkFile(c.File); err != nil {
			return err
		}
		if _, ok := seenFiles[c.File]; ok {
			return fmt.Errorf("%w: chunk %d reuses file %q", ErrManifest, i, c.File)
		}
		seenFiles[c.File] = struct{}{}
		if c.KeyCount <= 0 {
			return fmt.Errorf("%w: chunk %d has key_count %d", ErrManifest, i, c.KeyCount)
		}
		if c.PayloadBytes < 0 || c.SizeBytes <= 0 {
			return fmt.Errorf("%w: chunk %d has invalid sizes", ErrManifest, i)
		}
		if compareBytes(c.FirstKey, c.LastKey) > 0 {
			return fmt.Errorf("%w: chunk %d first_key > last_key", ErrManifest, i)
		}
		if !inRange(r, c.FirstKey) || !inRange(r, c.LastKey) {
			return fmt.Errorf("%w: chunk %d keys outside range %d", ErrManifest, i, c.RangeID)
		}
		if _, err := hex.DecodeString(c.SHA256); err != nil || len(c.SHA256) != sha256.Size*2 {
			return fmt.Errorf("%w: chunk %d has invalid sha256", ErrManifest, i)
		}
		idx := rangeIndex[c.RangeID]
		switch {
		case idx < lastRange:
			return fmt.Errorf("%w: chunk %d is out of range order", ErrManifest, i)
		case idx == lastRange:
			if compareBytes(prevLast, c.FirstKey) >= 0 {
				return fmt.Errorf("%w: chunk %d overlaps or is out of order", ErrManifest, i)
			}
		default:
			lastRange = idx
		}
		prevLast = c.LastKey
		keys += int64(c.KeyCount)
		bytes += c.PayloadBytes
	}
	if keys != m.TotalKeys {
		return fmt.Errorf("%w: total_keys %d, chunks sum to %d", ErrManifest, m.TotalKeys, keys)
	}
	if bytes != m.TotalBytes {
		return fmt.Errorf("%w: total_bytes %d, chunks sum to %d", ErrManifest, m.TotalBytes, bytes)
	}
	return nil
}

func validateRangeList(ranges []Range) error {
	if len(ranges) == 0 {
		return errors.New("no ranges")
	}
	seen := make(map[uint64]struct{}, len(ranges))
	for i, r := range ranges {
		if r.ID == 0 {
			return fmt.Errorf("range %d has zero id", i)
		}
		if _, ok := seen[r.ID]; ok {
			return fmt.Errorf("duplicate range id %d", r.ID)
		}
		seen[r.ID] = struct{}{}
		if i == 0 {
			if r.Start != nil && len(r.Start) != 0 {
				return errors.New("first range must start at -inf")
			}
		} else {
			prev := ranges[i-1]
			if len(r.Start) == 0 || compareBytes(r.Start, prev.End) != 0 {
				return fmt.Errorf("range %d is not adjacent to range %d", r.ID, prev.ID)
			}
		}
		if i == len(ranges)-1 {
			if r.End != nil && len(r.End) != 0 {
				return errors.New("last range must end at +inf")
			}
		} else if len(r.End) == 0 || compareBytes(r.Start, r.End) >= 0 {
			return fmt.Errorf("range %d has invalid bounds", r.ID)
		}
	}
	return nil
}

// sortRanges orders ranges by start key, treating nil as -inf.
func sortRanges(ranges []Range) {
	sort.Slice(ranges, func(i, j int) bool {
		return compareBytes(ranges[i].Start, ranges[j].Start) < 0
	})
}

func compareBytes(a, b []byte) int {
	// nil is -inf, which is smaller than every non-nil key (including an
	// empty key).
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

// inRange reports whether key is in [r.Start, r.End).
func inRange(r Range, key []byte) bool {
	if r.Start != nil && compareBytes(key, r.Start) < 0 {
		return false
	}
	if r.End != nil && compareBytes(key, r.End) >= 0 {
		return false
	}
	return true
}
