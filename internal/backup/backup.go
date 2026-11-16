package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"distributed-db/internal/v2client"
)

// BackupConfig configures one logical backup.
type BackupConfig struct {
	// Client provides the TSO and snapshot scans.
	Client *v2client.Client
	// Meta lists the range map. *MetaClient implements it.
	Meta RangeLister
	// OutDir is the backup directory to create. It must not already exist.
	OutDir string
	// ChunkKeys and ChunkBytes bound one chunk. Zero selects the defaults.
	ChunkKeys  int
	ChunkBytes int64
}

type chunkLimits struct {
	keys  int
	bytes int64
}

// Backup scans every range at one TSO and writes an atomic backup directory.
//
// All ranges are read through one Txn, so every chunk observes the same
// snapshot timestamp. Memory use is bounded by one scan batch/chunk; no range
// or the full key space is ever buffered.
//
// The backup only becomes visible at OutDir after the manifest, every chunk
// and the range-map recheck have succeeded: files are written to a hidden
// staging directory in the same parent and renamed at the end.
func Backup(ctx context.Context, cfg BackupConfig) (*Manifest, error) {
	if cfg.Client == nil {
		return nil, errors.New("backup: Client is required")
	}
	if cfg.Meta == nil {
		return nil, errors.New("backup: Meta is required")
	}
	if cfg.OutDir == "" {
		return nil, errors.New("backup: OutDir is required")
	}
	limits, err := normalizeLimits(cfg.ChunkKeys, cfg.ChunkBytes)
	if err != nil {
		return nil, err
	}
	out := filepath.Clean(cfg.OutDir)
	if out == "." || out == string(filepath.Separator) {
		return nil, fmt.Errorf("backup: refusing to use %q as an output directory", cfg.OutDir)
	}
	if _, err := os.Stat(out); err == nil {
		return nil, fmt.Errorf("backup: output directory %s already exists", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("backup: stat output: %w", err)
	}
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("backup: create output parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(out)+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("backup: create staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("backup: chmod staging directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(staging, ChunksDir), 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return nil, fmt.Errorf("backup: create chunk directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	ranges, err := cfg.Meta.ListRanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: list ranges: %w", err)
	}
	ranges = cloneRanges(ranges)
	sortRanges(ranges)
	if err := validateRangeList(ranges); err != nil {
		return nil, fmt.Errorf("%w: meta range map: %v", ErrRangeMismatch, err)
	}

	txn, err := cfg.Client.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: begin snapshot transaction: %w", err)
	}
	defer func() { _ = txn.Rollback(ctx) }()

	m := &Manifest{
		Format:        FormatName,
		FormatVersion: FormatVersion,
		CreatedAt:     time.Now().UTC(),
		SnapshotTS:    txn.StartTS(),
		ChunkKeys:     limits.keys,
		ChunkBytes:    limits.bytes,
		Ranges:        ranges,
	}
	for _, r := range ranges {
		if err := backupRange(ctx, txn, r, staging, limits, m); err != nil {
			return nil, err
		}
	}

	// A split/merge during the scan can make the captured descriptor set
	// stale. The snapshot scan would already fail for a changed boundary, but
	// a range that appeared after ListRanges could silently be missed. Abort
	// rather than publish a backup with an incomplete range map.
	current, err := cfg.Meta.ListRanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: recheck range map: %w", err)
	}
	current = cloneRanges(current)
	sortRanges(current)
	if !sameRangeMap(ranges, current) {
		return nil, fmt.Errorf("%w: range map changed during backup; retry", ErrRangeMismatch)
	}

	if err := writeManifestFile(staging, m); err != nil {
		return nil, err
	}
	_ = syncDir(filepath.Join(staging, ChunksDir))
	_ = syncDir(staging)
	if err := os.Rename(staging, out); err != nil {
		return nil, fmt.Errorf("backup: publish %s: %w", out, err)
	}
	committed = true
	_ = syncDir(parent)
	return m, nil
}

// maxChunkKeys keeps one Scan response and the pending buffer bounded even if
// a caller passes an absurd -chunk-keys value.
const maxChunkKeys = 100_000

func normalizeLimits(keys int, bytesLimit int64) (chunkLimits, error) {
	if keys == 0 {
		keys = DefaultChunkKeys
	}
	if bytesLimit == 0 {
		bytesLimit = DefaultChunkBytes
	}
	if keys < 0 || keys > maxChunkKeys {
		return chunkLimits{}, fmt.Errorf("backup: ChunkKeys must be between 1 and %d, got %d", maxChunkKeys, keys)
	}
	if bytesLimit < 0 {
		return chunkLimits{}, fmt.Errorf("backup: ChunkBytes must not be negative, got %d", bytesLimit)
	}
	return chunkLimits{keys: keys, bytes: bytesLimit}, nil
}

// backupRange scans one range in bounded chunks. The txn carries the single
// backup snapshot TS, so repeated Scan calls see the same MVCC snapshot.
//
// A scan batch may contain more rows than a chunk can hold (the byte limit can
// be reached before the key limit). Those rows stay in pending and are used by
// the next chunk; no scan result is discarded or re-read.
func backupRange(ctx context.Context, txn *v2client.Txn, r Range, dir string, limits chunkLimits, m *Manifest) error {
	cur := cloneBytes(r.Start)
	var pending []v2client.KV
	var prev []byte
	havePrev := false
	exhausted := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(pending) == 0 {
			if exhausted {
				break
			}
			if cur != nil && r.End != nil && compareBytes(cur, r.End) >= 0 {
				break
			}
			rows, err := txn.Scan(ctx, cur, cloneBytes(r.End), limits.keys)
			if err != nil {
				return fmt.Errorf("backup: scan range %d at %q: %w", r.ID, cur, err)
			}
			if len(rows) == 0 {
				break
			}
			for i, kv := range rows {
				if !inRange(r, kv.Key) {
					return fmt.Errorf("%w: scan of range %d returned key %q outside its bounds", ErrRangeMismatch, r.ID, kv.Key)
				}
				if i > 0 && compareBytes(rows[i-1].Key, kv.Key) >= 0 {
					return fmt.Errorf("%w: scan of range %d returned unsorted keys", ErrCorrupt, r.ID)
				}
				if havePrev && compareBytes(prev, kv.Key) >= 0 {
					return fmt.Errorf("%w: scan of range %d duplicated or reordered key %q", ErrCorrupt, r.ID, kv.Key)
				}
			}
			pending = rows
			exhausted = len(rows) < limits.keys
		}

		chunk := make([]v2client.KV, 0, limits.keys)
		var payload int64
		for len(pending) > 0 {
			kv := pending[0]
			sz := int64(len(kv.Key) + len(kv.Value))
			if len(chunk) > 0 && (len(chunk) >= limits.keys || payload+sz > limits.bytes) {
				break
			}
			pending = pending[1:]
			chunk = append(chunk, kv)
			payload += sz
		}
		if len(chunk) == 0 {
			return fmt.Errorf("backup: range %d produced an empty chunk", r.ID)
		}
		c, err := writeChunkFile(dir, len(m.Chunks), r.ID, chunk)
		if err != nil {
			return err
		}
		m.Chunks = append(m.Chunks, c)
		m.TotalKeys += int64(c.KeyCount)
		m.TotalBytes += c.PayloadBytes
		last := chunk[len(chunk)-1].Key
		prev = last
		havePrev = true

		if len(pending) > 0 {
			next := pending[0].Key
			if compareBytes(next, last) <= 0 {
				return fmt.Errorf("backup: range %d scan did not advance past %q", r.ID, last)
			}
			cur = next
			continue
		}
		if exhausted {
			break
		}
		next := keySuccessor(last)
		if r.End != nil && compareBytes(next, r.End) >= 0 {
			break
		}
		cur = next
	}
	return nil
}

// keySuccessor returns the smallest key strictly greater than key in the
// byte-lexicographic key space: key with a zero byte appended. Appending
// (rather than incrementing) is correct for any key, including 0xff suffixes.
func keySuccessor(key []byte) []byte {
	out := make([]byte, len(key)+1)
	copy(out, key)
	return out
}

func cloneRanges(in []Range) []Range {
	out := make([]Range, len(in))
	for i, r := range in {
		out[i] = Range{
			ID: r.ID, Start: cloneBytes(r.Start), End: cloneBytes(r.End),
			Replicas: append([]string(nil), r.Replicas...), Leader: r.Leader,
			Version: r.Version,
		}
	}
	return out
}

// sameRangeMap compares the boundaries and versions that determine where data
// lives. Leader hints and replica lists may change without moving any key.
func sameRangeMap(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Version != b[i].Version {
			return false
		}
		if compareBytes(a[i].Start, b[i].Start) != 0 || compareBytes(a[i].End, b[i].End) != 0 {
			return false
		}
	}
	return true
}

func writeManifestFile(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, "."+ManifestFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("backup: open manifest for sync: %w", err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("backup: sync manifest: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("backup: close manifest: %w", closeErr)
	}
	if err := os.Rename(tmp, filepath.Join(dir, ManifestFile)); err != nil {
		return fmt.Errorf("backup: publish manifest: %w", err)
	}
	return nil
}

// syncDir flushes a directory entry table. It is best effort because some
// platforms/filesystems reject fsync on directories.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
