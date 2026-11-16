// Package edgecache implements a local, immutable-segment cache for an
// edgehash HTTP server.
//
// A Cache keeps a materialized edgeset Store under a local directory. Sync
// downloads the signed manifest, verifies it, and atomically installs only the
// segment files that are missing or whose checksum changed. Contains first
// consults the local Bloom filter; a negative answer is returned without any
// network I/O. Bloom positives are resolved by an exact local lookup and,
// when the segment is not cached, by a remote lookup whose Merkle proof is
// verified against the root from the locally cached manifest.
//
// The cache is not a source of truth. Callers should Sync before relying on a
// local negative result: an old Bloom filter can produce a false negative for
// a hash added in a newer generation of the immutable set.
package edgecache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"distributed-db/internal/edgeset"
)

const (
	cacheManifestFile       = "manifest.json"
	cacheSourceManifestFile = "source-manifest.json"
	cacheSegmentsDir        = "segments"

	defaultHTTPTimeout = 15 * time.Second

	// maxManifestBytes bounds the manifest response. A manifest for every
	// possible two-byte prefix is well below this limit.
	maxManifestBytes int64 = 64 << 20
	// maxLookupBytes bounds the small JSON lookup response.
	maxLookupBytes int64 = 1 << 20
	// maxSegmentBytes is a defensive bound on downloaded segment files.
	maxSegmentBytes int64 = 4 << 30
	// maxErrorBodyBytes bounds error text read from a failed HTTP response.
	maxErrorBodyBytes int64 = 512
)

var (
	// ErrNotSynced reports that Contains was called before a local store was
	// loaded. Call Sync first.
	ErrNotSynced = errors.New("edgecache: cache is not synced")

	// ErrInvalidManifest reports a malformed or untrusted manifest.
	ErrInvalidManifest = errors.New("edgecache: invalid manifest")

	// ErrInvalidProof reports a missing or invalid remote Merkle proof.
	ErrInvalidProof = errors.New("edgecache: invalid Merkle proof")

	// ErrStaleManifest reports that the server returned a proof for a
	// different manifest generation than the one cached locally. Sync and retry.
	ErrStaleManifest = errors.New("edgecache: server manifest differs from the cached manifest")
)

// Options configures a Cache. The zero value downloads every segment with a
// 15 second HTTP timeout.
type Options struct {
	// HTTPClient is an optional client. It is shallow-copied so the caller's
	// client is never mutated.
	HTTPClient *http.Client

	// Timeout overrides the HTTP timeout. Zero keeps the supplied client's
	// timeout, or the default of 15 seconds. A negative value is an error.
	Timeout time.Duration

	// Prefixes limits Sync to the listed segment prefixes. Empty means every
	// segment. Excluded prefixes are resolved through the server.
	Prefixes []uint16
}

// LookupResult is a verified answer to a remote lookup.
type LookupResult struct {
	// Present reports set membership.
	Present bool
	// Proof is the verified Merkle inclusion proof; nil when absent.
	Proof *edgeset.Proof
	// Root is the verified segment Merkle root; zero when absent.
	Root edgeset.Hash
	// Prefix is the 16-bit segment routing prefix of the queried hash.
	Prefix uint16
}

// Cache owns a materialized local store and its authoritative full manifest.
type Cache struct {
	baseURL string
	dir     string
	key     []byte
	client  *http.Client
	filter  map[uint16]struct{} // nil means all segments

	syncMu sync.Mutex

	mu        sync.RWMutex
	store     *edgeset.Store
	full      *edgeset.Manifest
	fullMetas map[uint16]edgeset.SegmentMeta
	loadErr   error
}

// New creates a cache rooted at cacheDir. It validates the server URL, creates
// the directory, and loads an existing local store when one is present and
// consistent. A missing local cache is not an error: call Sync to populate it.
//
// A corrupted or tampered local cache is never used for lookups; the error is
// retained and Contains reports ErrNotSynced until a successful Sync rebuilds
// the cache.
func New(serverURL, cacheDir string, key []byte, opts Options) (*Cache, error) {
	baseURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cacheDir) == "" {
		return nil, fmt.Errorf("edgecache: empty cache directory")
	}
	client, err := newHTTPClient(opts)
	if err != nil {
		return nil, err
	}
	dir := filepath.Clean(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("edgecache: create cache directory: %w", err)
	}

	c := &Cache{
		baseURL: baseURL,
		dir:     dir,
		key:     append([]byte(nil), key...),
		client:  client,
		filter:  prefixFilter(opts.Prefixes),
	}
	c.loadLocal()
	return c, nil
}

// Dir returns the materialized cache directory.
func (c *Cache) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// Manifest returns a deep copy of the verified full server manifest known to
// the cache, or nil before the first successful Sync.
func (c *Cache) Manifest() *edgeset.Manifest {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneManifest(c.full)
}

// Store returns the materialized local store, or nil if the cache is not
// synced or its local data was rejected during loading.
func (c *Cache) Store() *edgeset.Store {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store
}

// Synced reports whether a verified local store is currently loaded.
func (c *Cache) Synced() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store != nil
}

// LoadError returns the reason the last local cache load was rejected, if any.
// It is informational; a successful Sync clears it.
func (c *Cache) LoadError() error {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadErr
}

// Sync updates the local cache from the server.
//
// It downloads and verifies the manifest, then fetches only segments that are
// missing or whose checksum changed. Segment files are written with
// write-to-temp + rename and the manifest is replaced last. The new store is
// fully validated with edgeset.LoadStore before it becomes visible to
// Contains.
//
// Sync serializes with other Sync calls. Contains may run concurrently: local
// reads are serialized against file installation, and the new store becomes
// visible only after full validation. A remote lookup that races with Sync can
// observe a newer server generation and return ErrStaleManifest; retry after
// Sync in that case.
func (c *Cache) Sync(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("edgecache: nil cache")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	raw, manifest, err := fetchManifest(ctx, c.baseURL, c.client, c.key)
	if err != nil {
		return err
	}

	selected := c.selectSegments(manifest)
	keep := make(map[string]struct{}, len(selected))
	for _, meta := range selected {
		keep[filepath.Base(meta.File)] = struct{}{}
	}

	c.mu.RLock()
	store, localFull := c.store, c.full
	c.mu.RUnlock()
	reuse := store != nil && localFull != nil &&
		localFull.Version == manifest.Version &&
		localFull.PrefixBytes == manifest.PrefixBytes

	stage, err := os.MkdirTemp(c.dir, ".sync-*")
	if err != nil {
		return fmt.Errorf("edgecache: create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	type stagedSegment struct {
		meta edgeset.SegmentMeta
		path string
	}
	var staged []stagedSegment
	for _, meta := range selected {
		if reuse && segmentReusable(store, c.dir, meta) {
			continue
		}
		path := filepath.Join(stage, filepath.Base(meta.File))
		if err := c.downloadSegment(ctx, meta, path); err != nil {
			return err
		}
		staged = append(staged, stagedSegment{meta: meta, path: path})
	}

	materialized := &edgeset.Manifest{
		Version:     manifest.Version,
		PrefixBytes: manifest.PrefixBytes,
		Segments:    append([]edgeset.SegmentMeta(nil), selected...),
	}
	manifestJSON := raw
	if len(selected) != len(manifest.Segments) {
		// A filtered materialized manifest cannot keep the server signature:
		// the HMAC covers every segment. The signed source manifest stored
		// alongside it is the integrity anchor instead.
		manifestJSON, err = marshalLocalManifest(materialized)
		if err != nil {
			return err
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	segDir := filepath.Join(c.dir, cacheSegmentsDir)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return c.failLocked(fmt.Errorf("edgecache: create segments directory: %w", err))
	}
	for _, seg := range staged {
		dest := filepath.Join(c.dir, filepath.FromSlash(seg.meta.File))
		if err := os.Rename(seg.path, dest); err != nil {
			return c.failLocked(fmt.Errorf("edgecache: install segment %04x: %w", seg.meta.Prefix, err))
		}
	}
	if err := writeFileAtomic(filepath.Join(c.dir, cacheSourceManifestFile), raw, 0o644); err != nil {
		return c.failLocked(err)
	}
	if err := writeFileAtomic(filepath.Join(c.dir, cacheManifestFile), manifestJSON, 0o644); err != nil {
		return c.failLocked(err)
	}

	synced, _, err := edgeset.LoadStore(c.dir, nil)
	if err != nil {
		return c.failLocked(fmt.Errorf("edgecache: load synced store: %w", err))
	}
	if !materializedSubset(synced.Manifest(), manifest) {
		return c.failLocked(fmt.Errorf("%w: synced segments do not match the server manifest", ErrInvalidManifest))
	}

	c.pruneLocked(keep)
	c.store = synced
	c.full = manifest
	c.fullMetas = segmentMetaMap(manifest)
	c.loadErr = nil
	return nil
}

// Contains answers whether h is in the authoritative set.
//
// The local Bloom filter is authoritative for negatives: false is returned
// without any HTTP request. A Bloom positive is checked exactly against the
// local segment. If the hash is not found locally, or the segment is excluded
// by the configured prefix filter, the server is asked for an exact answer and
// its Merkle proof is verified against the locally cached manifest root.
func (c *Cache) Contains(ctx context.Context, h edgeset.Hash) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("edgecache: nil cache")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Hold the read lock while local segments are consulted so a concurrent
	// Sync cannot replace the files underneath an old manifest. The remote
	// call is made after unlocking.
	c.mu.RLock()
	store, full, metas, loadErr := c.store, c.full, c.fullMetas, c.loadErr
	if store == nil || full == nil {
		c.mu.RUnlock()
		if loadErr != nil {
			return false, fmt.Errorf("%w: %w", ErrNotSynced, loadErr)
		}
		return false, ErrNotSynced
	}

	prefix := prefixFor(h, full.PrefixBytes)
	if _, cached := store.SegmentMeta(prefix); cached {
		maybe, err := store.MayContain(h)
		if err != nil {
			c.mu.RUnlock()
			return false, fmt.Errorf("edgecache: local Bloom lookup: %w", err)
		}
		if !maybe {
			c.mu.RUnlock()
			return false, nil
		}
		present, _, err := store.Contains(h)
		if err != nil {
			c.mu.RUnlock()
			return false, fmt.Errorf("edgecache: local lookup: %w", err)
		}
		if present {
			c.mu.RUnlock()
			return true, nil
		}
	}

	meta, ok := metas[prefix]
	c.mu.RUnlock()
	if !ok {
		// No segment with this prefix exists in the authoritative manifest, so
		// the hash is definitively absent.
		return false, nil
	}
	result, err := c.lookupRemote(ctx, h, prefix, meta)
	if err != nil {
		return false, err
	}
	return result.Present, nil
}

func (c *Cache) selectSegments(manifest *edgeset.Manifest) []edgeset.SegmentMeta {
	if c.filter == nil {
		return append([]edgeset.SegmentMeta(nil), manifest.Segments...)
	}
	selected := make([]edgeset.SegmentMeta, 0, len(manifest.Segments))
	for _, meta := range manifest.Segments {
		if _, ok := c.filter[meta.Prefix]; ok {
			selected = append(selected, meta)
		}
	}
	return selected
}

func (c *Cache) loadLocal() {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		full    *edgeset.Manifest
		loadErr error
	)

	sourcePath := filepath.Join(c.dir, cacheSourceManifestFile)
	raw, err := os.ReadFile(sourcePath)
	switch {
	case err == nil:
		manifest, decodeErr := decodeManifest(raw, c.key)
		if decodeErr != nil {
			loadErr = decodeErr
		} else {
			full = manifest
		}
	case !errors.Is(err, os.ErrNotExist):
		loadErr = fmt.Errorf("edgecache: read source manifest: %w", err)
	}

	var store *edgeset.Store
	var storeErr error
	switch {
	case full != nil:
		// With a prefix filter the materialized manifest is unsigned, so it is
		// loaded structurally and cross-checked against the signed source.
		store, _, storeErr = edgeset.LoadStore(c.dir, nil)
	case len(c.key) == 0:
		store, _, storeErr = edgeset.LoadStore(c.dir, nil)
	default:
		// Without a trusted source manifest only a fully signed materialized
		// manifest can establish integrity.
		store, _, storeErr = edgeset.LoadStore(c.dir, c.key)
	}
	if storeErr != nil {
		if loadErr == nil && !errors.Is(storeErr, os.ErrNotExist) {
			loadErr = storeErr
		}
		store = nil
	} else if full == nil {
		candidate := store.Manifest()
		if err := edgeset.VerifyManifest(candidate, c.key); err != nil {
			store = nil
			loadErr = err
		} else {
			full = candidate
			loadErr = nil
		}
	}
	if store != nil && full != nil && !materializedSubset(store.Manifest(), full) {
		store = nil
		loadErr = fmt.Errorf("%w: local segments do not match the source manifest", ErrInvalidManifest)
	}

	c.store = store
	c.full = full
	c.fullMetas = segmentMetaMap(full)
	c.loadErr = loadErr
}

// failLocked invalidates the in-memory store after a partially applied Sync.
// The next Sync sees no reusable store and downloads every selected segment.
func (c *Cache) failLocked(err error) error {
	c.store = nil
	c.loadErr = err
	return err
}

func (c *Cache) pruneLocked(keep map[string]struct{}) {
	segDir := filepath.Join(c.dir, cacheSegmentsDir)
	entries, err := os.ReadDir(segDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".seg") {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		_ = os.Remove(filepath.Join(segDir, name))
	}
}

func segmentReusable(store *edgeset.Store, dir string, remote edgeset.SegmentMeta) bool {
	local, ok := store.SegmentMeta(remote.Prefix)
	if !ok || local != remote {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(remote.File)))
	return err == nil && info.Mode().IsRegular() && info.Size() == remote.Size
}

func materializedSubset(part, full *edgeset.Manifest) bool {
	if part == nil || full == nil {
		return false
	}
	if part.Version != full.Version || part.PrefixBytes != full.PrefixBytes {
		return false
	}
	byPrefix := make(map[uint16]edgeset.SegmentMeta, len(full.Segments))
	for _, meta := range full.Segments {
		byPrefix[meta.Prefix] = meta
	}
	for _, meta := range part.Segments {
		want, ok := byPrefix[meta.Prefix]
		if !ok || want != meta {
			return false
		}
	}
	return true
}

func segmentMetaMap(manifest *edgeset.Manifest) map[uint16]edgeset.SegmentMeta {
	if manifest == nil {
		return nil
	}
	out := make(map[uint16]edgeset.SegmentMeta, len(manifest.Segments))
	for _, meta := range manifest.Segments {
		out[meta.Prefix] = meta
	}
	return out
}

func cloneManifest(m *edgeset.Manifest) *edgeset.Manifest {
	if m == nil {
		return nil
	}
	out := &edgeset.Manifest{
		Version:     m.Version,
		PrefixBytes: m.PrefixBytes,
		Segments:    append([]edgeset.SegmentMeta(nil), m.Segments...),
	}
	if m.Signature != nil {
		out.Signature = append([]byte(nil), m.Signature...)
	}
	return out
}

func prefixFilter(prefixes []uint16) map[uint16]struct{} {
	if len(prefixes) == 0 {
		return nil
	}
	out := make(map[uint16]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		out[prefix] = struct{}{}
	}
	return out
}
