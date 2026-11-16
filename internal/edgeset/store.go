package edgeset

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Store is an immutable local view of an edgeset directory.
//
// It holds the manifest and a lazily populated in-memory cache of decoded
// segments. All methods are safe for concurrent use; the segment cache is
// guarded by a mutex and decoded segments are never mutated.
type Store struct {
	dir      string
	manifest Manifest
	metas    map[uint16]SegmentMeta

	mu    sync.RWMutex
	cache map[uint16]*segment
}

func newStore(dir string, m *Manifest) *Store {
	manifest := cloneManifest(m)
	metas := make(map[uint16]SegmentMeta, len(manifest.Segments))
	for _, meta := range manifest.Segments {
		metas[meta.Prefix] = meta
	}
	return &Store{
		dir:      dir,
		manifest: *manifest,
		metas:    metas,
		cache:    make(map[uint16]*segment),
	}
}

// Build creates an immutable store from hashes, writes it to opts.SegmentDir
// and returns both the opened Store and its Manifest.
//
// The input slice is copied, deduplicated and sorted; the caller may reuse it.
// Segment files are deterministic for a given hash set, options and signing
// key. Build refuses a non-empty destination directory.
func Build(hashes []Hash, opts BuildOptions) (*Store, *Manifest, error) {
	prefixBytes, falsePositiveRate, compression, err := normalizeBuildOptions(opts)
	if err != nil {
		return nil, nil, err
	}

	work := append([]Hash(nil), hashes...)
	sort.Slice(work, func(i, j int) bool { return hashLess(work[i], work[j]) })
	unique := work[:0]
	for _, h := range work {
		if len(unique) == 0 || unique[len(unique)-1] != h {
			unique = append(unique, h)
		}
	}
	if uint64(len(unique)) > uint64(^uint32(0)) {
		return nil, nil, fmt.Errorf("%w: %d hashes exceed the segment count field", ErrInvalidOption, len(unique))
	}

	target, err := newBuildTarget(opts.SegmentDir)
	if err != nil {
		return nil, nil, err
	}
	published := false
	defer func() {
		if !published {
			target.abort()
		}
	}()

	if err := os.MkdirAll(filepath.Join(target.staging, "segments"), 0o755); err != nil {
		return nil, nil, fmt.Errorf("edgeset: create segments directory: %w", err)
	}

	// One encoder is reused for every compressed segment; EncodeAll itself is
	// deterministic, so builds with the same inputs produce identical files.
	var encoder *zstd.Encoder
	if compression != compressionNoneMode {
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, fmt.Errorf("edgeset: create zstd encoder: %w", err)
		}
		defer func() { _ = encoder.Close() }()
	}

	segments := make([]SegmentMeta, 0)
	for start := 0; start < len(unique); {
		prefix := prefixOf(unique[start], prefixBytes)
		end := start + 1
		for end < len(unique) && prefixOf(unique[end], prefixBytes) == prefix {
			end++
		}
		group := unique[start:end]

		bloom, err := newBloomFilter(len(group), falsePositiveRate)
		if err != nil {
			return nil, nil, err
		}
		for _, h := range group {
			bloom.add(h)
		}
		levels := merkleLevels(group)
		if len(levels) == 0 {
			return nil, nil, fmt.Errorf("%w: empty segment %04x", ErrCorruptSegment, prefix)
		}
		root := levels[len(levels)-1][0]

		rawSize := len(bloom.bits) + len(group)*segmentHashSize
		data, compressionName, err := encodeSegmentV2(prefix, group, bloom, compression.compress(rawSize), encoder)
		if err != nil {
			return nil, nil, err
		}
		checksum := sha256.Sum256(data)
		relative := segmentFileFor(prefix)
		if err := os.WriteFile(filepath.Join(target.staging, filepath.FromSlash(relative)), data, 0o644); err != nil {
			return nil, nil, fmt.Errorf("edgeset: write segment %04x: %w", prefix, err)
		}

		segments = append(segments, SegmentMeta{
			Prefix:      prefix,
			Count:       len(group),
			BloomBits:   int(bloom.m),
			BloomK:      bloom.k,
			Root:        root,
			File:        relative,
			Size:        int64(len(data)),
			SHA256:      checksum,
			Compression: compressionName,
		})
		start = end
	}

	manifest := &Manifest{
		Version:     ManifestVersion,
		PrefixBytes: prefixBytes,
		Segments:    segments,
	}
	switch {
	case len(opts.Ed25519PrivateKey) > 0:
		if err := signManifestEd25519(manifest, opts.Ed25519PrivateKey); err != nil {
			return nil, nil, err
		}
	case len(opts.SigningKey) > 0:
		if err := signManifest(manifest, opts.SigningKey); err != nil {
			return nil, nil, err
		}
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("edgeset: encode manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')
	if err := os.WriteFile(filepath.Join(target.staging, manifestFileName), manifestJSON, 0o644); err != nil {
		return nil, nil, fmt.Errorf("edgeset: write manifest: %w", err)
	}
	if err := target.commit(); err != nil {
		return nil, nil, err
	}
	published = true

	return newStore(target.dir, manifest), manifest, nil
}

// LoadStore opens an existing store using the legacy HMAC-SHA256 verifier.
//
// It validates the manifest structure, verifies its HMAC when key is non-empty
// (an empty key disables signature verification), and verifies every segment
// file's size, SHA-256, header, compression, ordering and Merkle root before
// returning. Individual segments are nevertheless rechecked when first cached,
// so files changed after LoadStore cannot answer lookups.
//
// Ed25519-signed stores must be opened with LoadStoreVerified. Use LoadStore
// only for HMAC-signed or unsigned stores.
func LoadStore(dir string, key []byte) (*Store, *Manifest, error) {
	return LoadStoreVerified(dir, HMACVerifier{Key: key})
}

// LoadStoreVerified opens an existing store and authenticates its manifest
// with verifier. A nil verifier skips signature verification but still checks
// the manifest structure and every segment file.
//
// The verifier decides which signature algorithms are acceptable. Passing an
// Ed25519Verifier rejects HMAC manifests (and vice versa), which prevents an
// attacker from silently downgrading the manifest signature algorithm.
func LoadStoreVerified(dir string, verifier Verifier) (*Store, *Manifest, error) {
	if dir == "" {
		return nil, nil, fmt.Errorf("%w: empty store directory", ErrInvalidOption)
	}
	clean := filepath.Clean(dir)
	manifestJSON, err := os.ReadFile(filepath.Join(clean, manifestFileName))
	if err != nil {
		return nil, nil, fmt.Errorf("edgeset: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, nil, fmt.Errorf("%w: decode manifest: %w", ErrInvalidManifest, err)
	}
	if err := validateManifest(&manifest); err != nil {
		return nil, nil, err
	}
	if verifier != nil {
		if err := verifier.VerifyManifest(&manifest); err != nil {
			return nil, nil, err
		}
	}

	store := newStore(clean, &manifest)
	if err := store.verifyAllSegments(); err != nil {
		return nil, nil, err
	}
	return store, &manifest, nil
}

func normalizeBuildOptions(opts BuildOptions) (prefixBytes int, falsePositiveRate float64, compression compressionMode, err error) {
	prefixBytes = opts.PrefixBytes
	if prefixBytes == 0 {
		prefixBytes = defaultPrefixBytes
	}
	if prefixBytes < minPrefixBytes || prefixBytes > maxPrefixBytes {
		return 0, 0, 0, fmt.Errorf("%w: PrefixBytes %d outside %d..%d", ErrInvalidOption, prefixBytes, minPrefixBytes, maxPrefixBytes)
	}

	falsePositiveRate = opts.FalsePositiveRate
	if falsePositiveRate == 0 {
		falsePositiveRate = defaultFalsePositiveRate
	}
	if math.IsNaN(falsePositiveRate) || math.IsInf(falsePositiveRate, 0) ||
		falsePositiveRate <= 0 || falsePositiveRate >= 1 {
		return 0, 0, 0, fmt.Errorf("%w: FalsePositiveRate %v must be in (0,1)", ErrInvalidOption, falsePositiveRate)
	}

	if len(opts.SigningKey) > 0 && len(opts.Ed25519PrivateKey) > 0 {
		return 0, 0, 0, fmt.Errorf("%w: SigningKey and Ed25519PrivateKey are mutually exclusive", ErrInvalidOption)
	}
	if len(opts.Ed25519PrivateKey) > 0 && len(opts.Ed25519PrivateKey) != ed25519.PrivateKeySize {
		return 0, 0, 0, fmt.Errorf("%w: Ed25519PrivateKey must be %d bytes, got %d", ErrInvalidOption, ed25519.PrivateKeySize, len(opts.Ed25519PrivateKey))
	}

	compression, err = parseCompression(opts.Compression)
	if err != nil {
		return 0, 0, 0, err
	}
	return prefixBytes, falsePositiveRate, compression, nil
}

func (s *Store) verifyAllSegments() error {
	for _, meta := range s.manifest.Segments {
		if _, err := readSegmentFile(s.segmentPath(meta), meta, s.manifest.PrefixBytes); err != nil {
			return fmt.Errorf("edgeset: verify segment %04x: %w", meta.Prefix, err)
		}
	}
	return nil
}

func (s *Store) segmentPath(meta SegmentMeta) string {
	return filepath.Join(s.dir, filepath.FromSlash(meta.File))
}

// Dir returns the directory backing the store.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Manifest returns a deep copy of the store's manifest.
func (s *Store) Manifest() *Manifest {
	if s == nil {
		return nil
	}
	return cloneManifest(&s.manifest)
}

// PrefixFor returns the segment prefix that Contains would route h to.
func (s *Store) PrefixFor(h Hash) uint16 {
	if s == nil {
		return 0
	}
	return prefixOf(h, s.manifest.PrefixBytes)
}

// SegmentMeta returns metadata for a segment prefix.
func (s *Store) SegmentMeta(prefix uint16) (SegmentMeta, bool) {
	if s == nil {
		return SegmentMeta{}, false
	}
	meta, ok := s.metas[prefix]
	return meta, ok
}

// SegmentCount returns the number of non-empty segments in the manifest.
func (s *Store) SegmentCount() int {
	if s == nil {
		return 0
	}
	return len(s.manifest.Segments)
}

// TotalCount returns the number of unique hashes across all segments.
func (s *Store) TotalCount() int {
	if s == nil {
		return 0
	}
	total := 0
	for _, meta := range s.manifest.Segments {
		total += meta.Count
	}
	return total
}

// SegmentPath returns the filesystem path of a segment file without reading
// it. It is the cheap accessor for a file-serving HTTP handler; Segment
// returns the verified bytes instead.
func (s *Store) SegmentPath(prefix uint16) (string, error) {
	if s == nil {
		return "", errors.New("edgeset: nil store")
	}
	meta, ok := s.metas[prefix]
	if !ok {
		return "", fmt.Errorf("%w: %04x", ErrSegmentNotFound, prefix)
	}
	return s.segmentPath(meta), nil
}

// Segment returns a copy of the verified raw bytes of one segment file. The
// bytes can be served verbatim to a cache client.
func (s *Store) Segment(prefix uint16) ([]byte, error) {
	seg, err := s.loadSegment(prefix)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(seg.data))
	copy(out, seg.data)
	return out, nil
}

// Contains performs an exact lookup.
//
// A missing segment prefix returns (false, nil, nil) without touching disk.
// Otherwise the segment is loaded and checked against its Bloom filter; a
// Bloom negative returns (false, nil, nil), while a Bloom positive triggers a
// binary search. A hit returns (true, proof, nil) and a miss returns
// (false, nil, nil), so Bloom false positives never surface as set membership.
// Proofs are over the segment Merkle root from the manifest; use SegmentMeta
// or Manifest to obtain that root for VerifyProof.
func (s *Store) Contains(h Hash) (bool, *Proof, error) {
	if s == nil {
		return false, nil, errors.New("edgeset: nil store")
	}
	prefix := s.PrefixFor(h)
	if _, ok := s.metas[prefix]; !ok {
		return false, nil, nil
	}
	seg, err := s.loadSegment(prefix)
	if err != nil {
		return false, nil, err
	}
	if !seg.bloom.maybe(h) {
		return false, nil, nil
	}
	index := sort.Search(len(seg.hashes), func(i int) bool {
		return !hashLess(seg.hashes[i], h)
	})
	if index == len(seg.hashes) || seg.hashes[index] != h {
		return false, nil, nil
	}
	return true, proofFor(seg.hashes, seg.levels, index), nil
}

// MayContain reports whether the segment Bloom filter thinks h may be present.
//
// A false result is definitive: h is not in the set and can be answered
// locally without a server lookup. A true result is only a hint and must be
// resolved by an exact server lookup (or Store.Contains). A missing segment
// prefix is definitely absent. MayContain reads the Bloom bitset but never
// binary-searches the hash list.
func (s *Store) MayContain(h Hash) (bool, error) {
	if s == nil {
		return false, errors.New("edgeset: nil store")
	}
	prefix := s.PrefixFor(h)
	if _, ok := s.metas[prefix]; !ok {
		return false, nil
	}
	seg, err := s.loadSegment(prefix)
	if err != nil {
		return false, err
	}
	return seg.bloom.maybe(h), nil
}

func (s *Store) loadSegment(prefix uint16) (*segment, error) {
	meta, ok := s.metas[prefix]
	if !ok {
		return nil, fmt.Errorf("%w: %04x", ErrSegmentNotFound, prefix)
	}

	s.mu.RLock()
	seg := s.cache[prefix]
	s.mu.RUnlock()
	if seg != nil {
		return seg, nil
	}

	seg, err := readSegmentFile(s.segmentPath(meta), meta, s.manifest.PrefixBytes)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[uint16]*segment)
	}
	if existing := s.cache[prefix]; existing != nil {
		seg = existing
	} else {
		s.cache[prefix] = seg
	}
	s.mu.Unlock()
	return seg, nil
}
