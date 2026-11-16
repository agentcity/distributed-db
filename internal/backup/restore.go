package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"distributed-db/internal/v2client"
)

// VerifyReport summarizes a successful offline backup verification.
type VerifyReport struct {
	Ranges       int
	Chunks       int
	Keys         int64
	PayloadBytes int64
	SnapshotTS   uint64
}

// Verify validates the manifest and every chunk without contacting the
// cluster. It checks the chunk file size, trailer digest, manifest digest,
// record framing, key ordering, range containment and both key/byte totals.
func Verify(ctx context.Context, dir string) (*VerifyReport, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	return verifyLoaded(ctx, dir, m)
}

func verifyLoaded(ctx context.Context, dir string, m *Manifest) (*VerifyReport, error) {
	report := &VerifyReport{
		Ranges: len(m.Ranges), Chunks: len(m.Chunks),
		Keys: m.TotalKeys, PayloadBytes: m.TotalBytes, SnapshotTS: m.SnapshotTS,
	}
	byID := make(map[uint64]Range, len(m.Ranges))
	for _, r := range m.Ranges {
		byID[r.ID] = r
	}
	var keys, payload int64
	for _, c := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, err := verifiedChunk(dir, c, byID[c.RangeID])
		if err != nil {
			return nil, err
		}
		keys += int64(len(ch.rows))
		payload += ch.payloadBytes
	}
	if keys != m.TotalKeys || payload != m.TotalBytes {
		return nil, fmt.Errorf("%w: verified totals (%d keys, %d bytes) differ from manifest (%d, %d)",
			ErrCorrupt, keys, payload, m.TotalKeys, m.TotalBytes)
	}
	return report, nil
}

// verifiedChunk reads one chunk and validates it against both its manifest
// descriptor and its range. It is used before every restore write so a file
// changed after Verify cannot be written silently.
func verifiedChunk(dir string, c Chunk, r Range) (chunkData, error) {
	path, err := ChunkPath(dir, c)
	if err != nil {
		return chunkData{}, err
	}
	ch, err := readChunkFile(path)
	if err != nil {
		return chunkData{}, err
	}
	if ch.sizeBytes != c.SizeBytes {
		return chunkData{}, fmt.Errorf("%w: chunk %d size %d, manifest says %d", ErrCorrupt, c.Index, ch.sizeBytes, c.SizeBytes)
	}
	if fmt.Sprintf("%x", ch.digest) != c.SHA256 {
		return chunkData{}, fmt.Errorf("%w: chunk %d sha256 does not match manifest", ErrCorrupt, c.Index)
	}
	if ch.rangeID != c.RangeID || ch.rangeID != r.ID {
		return chunkData{}, fmt.Errorf("%w: chunk %d range %d, manifest says %d", ErrCorrupt, c.Index, ch.rangeID, c.RangeID)
	}
	if len(ch.rows) != c.KeyCount {
		return chunkData{}, fmt.Errorf("%w: chunk %d has %d keys, manifest says %d", ErrCorrupt, c.Index, len(ch.rows), c.KeyCount)
	}
	if ch.payloadBytes != c.PayloadBytes {
		return chunkData{}, fmt.Errorf("%w: chunk %d payload %d, manifest says %d", ErrCorrupt, c.Index, ch.payloadBytes, c.PayloadBytes)
	}
	for j, kv := range ch.rows {
		if !inRange(r, kv.Key) {
			return chunkData{}, fmt.Errorf("%w: chunk %d key %q is outside range %d", ErrCorrupt, c.Index, kv.Key, r.ID)
		}
		if j > 0 && bytes.Compare(ch.rows[j-1].Key, kv.Key) >= 0 {
			return chunkData{}, fmt.Errorf("%w: chunk %d keys are not strictly ascending", ErrCorrupt, c.Index)
		}
	}
	if !bytes.Equal(ch.rows[0].Key, c.FirstKey) || !bytes.Equal(ch.rows[len(ch.rows)-1].Key, c.LastKey) {
		return chunkData{}, fmt.Errorf("%w: chunk %d bounds do not match manifest", ErrCorrupt, c.Index)
	}
	return ch, nil
}

// RestoreConfig configures one logical restore.
type RestoreConfig struct {
	// Client writes chunks as transactions.
	Client *v2client.Client
	// Meta reads and optionally replaces the target range map.
	Meta RangeAdmin
	// InDir is an existing backup directory.
	InDir string
	// EnsureRanges replaces the target meta range map with the manifest's
	// layout before writing. Use this only when the target KV nodes already
	// host ranges with the same boundaries; SetRanges updates metadata, it
	// cannot start range groups.
	EnsureRanges bool
	// Replicas overrides the replica list for every range when EnsureRanges
	// is set. This is needed when the restore target has different node
	// addresses than the source. An empty list keeps the manifest replicas.
	Replicas []string
	// StrictEpoch requires the target descriptor versions to equal the
	// manifest versions before any key is written.
	StrictEpoch bool
	// Check reads every restored key back at one TSO after writing and
	// compares its value. It is slower but useful for one-off restores.
	Check bool
}

// RestoreReport summarizes a restore.
type RestoreReport struct {
	Ranges       int
	Chunks       int
	Keys         int64
	PayloadBytes int64
	SnapshotTS   uint64
	Checked      bool
}

// Restore validates the whole backup first, ensures the target range layout is
// compatible (or replaces it when EnsureRanges is set), then writes one
// transaction per chunk. Verify runs before the first write, so a corrupt
// backup never leaves a partially restored key space.
//
// Restore is idempotent for an unchanged backup: it overwrites the same keys
// with the same values. It does not delete keys that are absent from the
// backup and it is not an atomic, online operation.
func Restore(ctx context.Context, cfg RestoreConfig) (*RestoreReport, error) {
	if cfg.Client == nil {
		return nil, errors.New("backup: Client is required")
	}
	if cfg.Meta == nil {
		return nil, errors.New("backup: Meta is required")
	}
	if cfg.InDir == "" {
		return nil, errors.New("backup: InDir is required")
	}
	dir := filepath.Clean(cfg.InDir)
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	// Full checksum pass before the first write.
	if _, err := verifyLoaded(ctx, dir, m); err != nil {
		return nil, err
	}

	target, err := cfg.Meta.ListRanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("restore: list target ranges: %w", err)
	}
	target = cloneRanges(target)
	sortRanges(target)
	if err := validateRangeList(target); err != nil {
		return nil, fmt.Errorf("%w: target range map: %v", ErrRangeMismatch, err)
	}
	if !sameLayout(m.Ranges, target) {
		if !cfg.EnsureRanges {
			return nil, fmt.Errorf("%w: target has %d ranges, backup has %d (use EnsureRanges to replace the meta range map)",
				ErrRangeMismatch, len(target), len(m.Ranges))
		}
		desired := cloneRanges(m.Ranges)
		replicas := append([]string(nil), cfg.Replicas...)
		for i := range desired {
			// A leader hint from the source cluster is meaningless on the
			// target; let discovery probe the target nodes.
			desired[i].Leader = ""
			if len(replicas) > 0 {
				desired[i].Replicas = append([]string(nil), replicas...)
			}
		}
		for _, r := range desired {
			if len(r.Replicas) == 0 {
				return nil, fmt.Errorf("%w: range %d has no replicas (use Replicas to supply target nodes)", ErrRangeMismatch, r.ID)
			}
		}
		if err := cfg.Meta.SetRanges(ctx, desired); err != nil {
			return nil, fmt.Errorf("restore: set target ranges: %w", err)
		}
		target, err = cfg.Meta.ListRanges(ctx)
		if err != nil {
			return nil, fmt.Errorf("restore: re-read target ranges: %w", err)
		}
		target = cloneRanges(target)
		sortRanges(target)
		if !sameLayout(m.Ranges, target) {
			return nil, fmt.Errorf("%w: target range map still differs after SetRanges", ErrRangeMismatch)
		}
	}
	if cfg.StrictEpoch {
		if !sameVersions(m.Ranges, target) {
			return nil, fmt.Errorf("%w: target range versions differ from the backup (strict epoch)", ErrRangeMismatch)
		}
	}

	report := &RestoreReport{
		Ranges: len(m.Ranges), Chunks: len(m.Chunks),
		Keys: m.TotalKeys, PayloadBytes: m.TotalBytes, SnapshotTS: m.SnapshotTS,
	}
	byID := make(map[uint64]Range, len(m.Ranges))
	for _, r := range m.Ranges {
		byID[r.ID] = r
	}
	for i, c := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := byID[c.RangeID]
		ch, err := verifiedChunk(dir, c, r)
		if err != nil {
			return nil, err
		}
		txn, err := cfg.Client.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("restore: begin chunk %d transaction: %w", i, err)
		}
		for _, kv := range ch.rows {
			txn.Set(kv.Key, kv.Value)
		}
		if err := txn.Commit(ctx); err != nil {
			_ = txn.Rollback(ctx)
			return nil, fmt.Errorf("restore: commit chunk %d: %w", i, err)
		}
	}

	if cfg.Check {
		if err := checkRestored(ctx, cfg.Client, dir, m); err != nil {
			return nil, err
		}
		report.Checked = true
	}
	return report, nil
}

// checkRestored reads every key back at one post-restore TSO.
func checkRestored(ctx context.Context, client *v2client.Client, dir string, m *Manifest) error {
	txn, err := client.Begin(ctx)
	if err != nil {
		return fmt.Errorf("restore: begin check transaction: %w", err)
	}
	defer func() { _ = txn.Rollback(ctx) }()
	byID := make(map[uint64]Range, len(m.Ranges))
	for _, r := range m.Ranges {
		byID[r.ID] = r
	}
	for _, c := range m.Chunks {
		ch, err := verifiedChunk(dir, c, byID[c.RangeID])
		if err != nil {
			return err
		}
		for _, kv := range ch.rows {
			got, err := txn.Get(ctx, kv.Key)
			if err != nil {
				return fmt.Errorf("restore: check key %q: %w", kv.Key, err)
			}
			if !bytes.Equal(got, kv.Value) {
				return fmt.Errorf("%w: restored key %q has different value", ErrCorrupt, kv.Key)
			}
		}
	}
	return nil
}

// sameLayout compares the boundaries that determine key placement. Range IDs
// may legitimately differ between two clusters with the same split points.
func sameLayout(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if compareBytes(a[i].Start, b[i].Start) != 0 || compareBytes(a[i].End, b[i].End) != 0 {
			return false
		}
	}
	return true
}

func sameVersions(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Version != b[i].Version {
			return false
		}
	}
	return true
}
