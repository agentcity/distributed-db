// Package edgecache implements a local, immutable-segment cache for an
// edgehash HTTP server.
//
// A Cache keeps a materialized edgeset Store under a local directory. Sync
// downloads the signed manifest, verifies it, and atomically installs only the
// segment files that are missing or whose checksum changed. Contains first
// consults the local Bloom filter; a negative answer is returned without any
// network I/O. Bloom positives are resolved by an exact local lookup and,
// when the segment is not cached (filtered or evicted), by a remote lookup
// whose Merkle proof is verified against the root from the locally cached
// manifest.
//
// # Crash-atomic generations
//
// Every sync writes a complete new generation directory
// (<cache>/gen-<timestamp>-<seq>/) containing the selected segments, the
// materialized manifest and the signed source manifest. The generation is
// verified with edgeset before it is renamed into place, all files are fsynced,
// and only then is the CURRENT pointer atomically replaced with a
// write-temp+fsync+rename. Readers therefore see either the old generation or
// the new one, never a mixture. The previous generation is retained as a
// fallback; if CURRENT is missing, malformed or points at a corrupt generation,
// New falls back to the newest generation that verifies.
//
// # Eviction
//
// Options.MaxCacheBytes, MaxSegments, SegmentTTL and EvictInterval bound the
// cached segment set. Eviction is LRU-ish: segments that have not been used for
// the longest time are removed first, and a segment whose last use is older
// than SegmentTTL is removed regardless of the byte/count limits. The manifest
// itself is never evicted. An evicted segment is resolved through the server on
// the next lookup, with the returned Merkle proof checked against the cached
// full manifest.
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"distributed-db/internal/edgeset"
)

const (
	cacheManifestFile       = "manifest.json"
	cacheSourceManifestFile = "source-manifest.json"
	cacheSegmentsDir        = "segments"
	cacheCurrentFile        = "CURRENT"
	cacheGenerationPrefix   = "gen-"
	cacheStagingPrefix      = ".staging-"
	cacheLegacyStaging      = ".sync-"

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

	errNoLocalCache = errors.New("edgecache: no local cache")
)

// generationSequence disambiguates generations created in the same nanosecond.
var generationSequence atomic.Uint64

// Options configures a Cache. The zero value downloads every segment with a
// 15 second HTTP timeout and performs no size/TTL eviction.
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

	// Verifier authenticates the manifest. When nil, the key argument to New
	// and RemoteLookup is wrapped in a legacy HMAC-SHA256 verifier (an empty
	// key disables signature verification). Set this to
	// edgeset.NewEd25519Verifier for Ed25519-signed stores.
	Verifier edgeset.Verifier

	// MaxCacheBytes is the maximum total stored segment size. Zero is
	// unlimited. Negative values are rejected.
	MaxCacheBytes int64

	// MaxSegments is the maximum number of cached segments. Zero is unlimited.
	// Negative values are rejected.
	MaxSegments int

	// SegmentTTL evicts segments unused for longer than this duration. Zero
	// disables age-based eviction. Negative values are rejected.
	SegmentTTL time.Duration

	// EvictInterval runs eviction periodically. Zero disables the background
	// loop; Sync and Evict still enforce the configured limits.
	EvictInterval time.Duration

	// AuthToken is sent as a Bearer token on every request. It is the client
	// counterpart to edgehttp.Options.AuthToken.
	AuthToken string
}

// LookupResult is a verified answer to a remote lookup.
type LookupResult struct {
	// Present reports set membership.
	Present bool
	// Proof is the verified Merkle inclusion proof; nil when absent.
	Proof *edgeset.Proof
	// AbsenceProof is the verified non-membership proof returned by a server
	// that supports ?absence=1; nil when absent is proven only by the manifest
	// (no segment covers the hash) or the server did not return one.
	AbsenceProof *edgeset.AbsenceProof
	// Root is the verified segment Merkle root; zero when the prefix has no
	// segment in the manifest.
	Root edgeset.Hash
	// Prefix is the 16-bit segment routing prefix of the queried hash.
	Prefix uint16
}

// Stats is a snapshot of cache usage.
type Stats struct {
	// Bytes is the total stored size of materialized segment files.
	Bytes int64
	// Segments is the number of materialized segments.
	Segments int
	// Evictions is the cumulative number of segments evicted by this Cache.
	Evictions int64
	// Generations is the number of on-disk generation directories.
	Generations int
}

// usageInfo tracks LRU/TTL state for one materialized segment.
type usageInfo struct {
	lastUsed time.Time
	added    time.Time
}

// Cache owns a materialized local store and its authoritative full manifest.
type Cache struct {
	baseURL   string
	dir       string
	verifier  edgeset.Verifier
	client    *http.Client
	authToken string
	filter    map[uint16]struct{} // nil means all segments

	maxCacheBytes int64
	maxSegments   int
	segmentTTL    time.Duration
	evictInterval time.Duration

	syncMu sync.Mutex

	mu         sync.RWMutex
	store      *edgeset.Store
	full       *edgeset.Manifest
	fullMetas  map[uint16]edgeset.SegmentMeta
	generation string // generation basename; "" means legacy root layout
	usage      map[uint16]usageInfo
	loadErr    error
	stats      Stats
	evictions  int64

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// New creates a cache rooted at cacheDir. It validates the server URL, creates
// the directory, and loads an existing local generation when one is present and
// consistent. A missing local cache is not an error: call Sync to populate it.
//
// A corrupted or tampered local cache is never used for lookups; the error is
// retained and Contains reports ErrNotSynced until a successful Sync rebuilds
// the cache. If the CURRENT generation is invalid but an older generation
// verifies, that older generation is used and CURRENT is repaired.
func New(serverURL, cacheDir string, key []byte, opts Options) (*Cache, error) {
	baseURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cacheDir) == "" {
		return nil, fmt.Errorf("edgecache: empty cache directory")
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
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
		baseURL:       baseURL,
		dir:           dir,
		verifier:      verifierFor(key, opts),
		client:        client,
		authToken:     opts.AuthToken,
		filter:        prefixFilter(opts.Prefixes),
		maxCacheBytes: opts.MaxCacheBytes,
		maxSegments:   opts.MaxSegments,
		segmentTTL:    opts.SegmentTTL,
		evictInterval: opts.EvictInterval,
		done:          make(chan struct{}),
	}
	c.cleanupStaleStaging()
	c.loadLocal()
	// Enforce limits/TTL against a cache restored from disk and drop stale
	// generations. This also makes a restart apply newly configured limits.
	c.mu.Lock()
	if c.store != nil {
		_ = c.evictLocked(time.Now())
	}
	c.pruneGenerationsLocked(c.generation)
	c.updateStatsLocked()
	c.mu.Unlock()
	if c.evictInterval > 0 {
		c.wg.Add(1)
		go c.evictLoop()
	}
	return c, nil
}

func validateOptions(opts Options) error {
	if opts.Timeout < 0 {
		return fmt.Errorf("edgecache: negative HTTP timeout %v", opts.Timeout)
	}
	if opts.MaxCacheBytes < 0 {
		return fmt.Errorf("edgecache: MaxCacheBytes must not be negative")
	}
	if opts.MaxSegments < 0 {
		return fmt.Errorf("edgecache: MaxSegments must not be negative")
	}
	if opts.SegmentTTL < 0 {
		return fmt.Errorf("edgecache: SegmentTTL must not be negative")
	}
	if opts.EvictInterval < 0 {
		return fmt.Errorf("edgecache: EvictInterval must not be negative")
	}
	return nil
}

// verifierFor implements the key-constructor compatibility rule: an explicit
// Verifier wins; otherwise key becomes a legacy HMAC verifier.
func verifierFor(key []byte, opts Options) edgeset.Verifier {
	if opts.Verifier != nil {
		return opts.Verifier
	}
	return edgeset.NewHMACVerifier(append([]byte(nil), key...))
}

// Close stops the background eviction loop. It is safe to call more than once.
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
	})
	c.wg.Wait()
	return nil
}

// Dir returns the cache root directory. The active materialized store lives in
// GenerationDir().
func (c *Cache) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// GenerationDir returns the directory of the active generation, which is a
// complete edgeset store. Before the first Sync it is empty.
func (c *Cache) GenerationDir() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generationPathLocked()
}

// RootDir is an alias for Dir.
func (c *Cache) RootDir() string {
	return c.Dir()
}

// CurrentDir is an alias for GenerationDir.
func (c *Cache) CurrentDir() string {
	return c.GenerationDir()
}

func (c *Cache) generationPathLocked() string {
	if c.generation == "" {
		return c.dir
	}
	return filepath.Join(c.dir, c.generation)
}

// CurrentGeneration returns the basename of the active generation, or "" when
// the cache is not synced or is using the legacy root layout.
func (c *Cache) CurrentGeneration() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
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

// SourceManifest is an alias for Manifest: it returns the full signed manifest,
// including segments that are not materialized locally.
func (c *Cache) SourceManifest() *edgeset.Manifest {
	return c.Manifest()
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

// Stats returns a snapshot of cache usage.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// Sync updates the local cache from the server.
//
// It downloads and verifies the full manifest, then stages every selected
// segment in a fresh generation directory. Unchanged segments are hard-linked
// or copied from the previous generation; missing or changed segments are
// downloaded and checksum-verified. The staged generation is fully validated
// with edgeset before it is renamed into place. The CURRENT pointer is then
// replaced atomically, which is the commit point: interrupted syncs leave the
// previous generation untouched.
//
// Sync serializes with other Sync/Evict calls. Contains may run concurrently:
// the new generation becomes visible only after full validation and the
// in-memory swap.
func (c *Cache) Sync(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("edgecache: nil cache")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	raw, manifest, err := fetchManifest(ctx, c.baseURL, c.client, c.authToken, c.verifier)
	if err != nil {
		return err
	}

	selected := c.selectSegments(manifest)

	c.mu.RLock()
	store := c.store
	genDir := c.generationPathLocked()
	oldUsage := cloneUsage(c.usage)
	c.mu.RUnlock()

	stage, err := os.MkdirTemp(c.dir, cacheStagingPrefix)
	if err != nil {
		return fmt.Errorf("edgecache: create staging directory: %w", err)
	}
	stageLive := true
	defer func() {
		if stageLive {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.MkdirAll(filepath.Join(stage, cacheSegmentsDir), 0o755); err != nil {
		return fmt.Errorf("edgecache: create staging segments directory: %w", err)
	}

	reused := make(map[uint16]bool, len(selected))
	for _, meta := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(stage, filepath.FromSlash(meta.File))
		if store != nil && segmentReusable(store, genDir, meta) {
			if err := reuseSegment(genDir, dst, meta); err == nil {
				reused[meta.Prefix] = true
				continue
			}
			_ = os.Remove(dst)
		}
		if err := downloadSegment(ctx, c.baseURL, c.client, c.authToken, meta, dst); err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// The signed source manifest is always retained. The materialized manifest
	// is the server's exact bytes when every segment is selected; a filtered
	// generation cannot keep the signature, so it is cross-checked against the
	// signed source at load time.
	localJSON := raw
	if len(selected) != len(manifest.Segments) {
		materialized := &edgeset.Manifest{
			Version:     manifest.Version,
			PrefixBytes: manifest.PrefixBytes,
			Segments:    append([]edgeset.SegmentMeta(nil), selected...),
		}
		localJSON, err = marshalLocalManifest(materialized)
		if err != nil {
			return err
		}
	}
	if err := writeFileAtomic(filepath.Join(stage, cacheSourceManifestFile), raw, 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(stage, cacheManifestFile), localJSON, 0o644); err != nil {
		return err
	}
	if err := syncDir(filepath.Join(stage, cacheSegmentsDir)); err != nil {
		return fmt.Errorf("edgecache: sync staged segments: %w", err)
	}
	if err := syncDir(stage); err != nil {
		return fmt.Errorf("edgecache: sync staging directory: %w", err)
	}

	stagedStore, stagedFull, err := loadGeneration(stage, c.verifier)
	if err != nil {
		return fmt.Errorf("edgecache: verify staged generation: %w", err)
	}
	if !materializedSubset(stagedStore.Manifest(), manifest) {
		return fmt.Errorf("%w: synced segments do not match the server manifest", ErrInvalidManifest)
	}

	generation := newGenerationName()
	finalDir := filepath.Join(c.dir, generation)
	if err := os.Rename(stage, finalDir); err != nil {
		return fmt.Errorf("edgecache: publish generation: %w", err)
	}
	stageLive = false
	if err := syncDir(c.dir); err != nil {
		return fmt.Errorf("edgecache: sync cache directory: %w", err)
	}
	// Re-open the published generation so the in-memory Store points at the
	// final directory rather than the now-renamed staging path. The files were
	// already verified above; this pass binds absolute paths and rechecks them.
	stagedStore, stagedFull, err = loadGeneration(finalDir, c.verifier)
	if err != nil {
		return fmt.Errorf("edgecache: open published generation: %w", err)
	}

	c.mu.Lock()
	if err := writeCurrentPointer(c.dir, generation); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("edgecache: update CURRENT pointer: %w", err)
	}
	now := time.Now()
	usage := make(map[uint16]usageInfo, len(selected))
	for _, meta := range selected {
		entry := usageInfo{}
		if reused[meta.Prefix] {
			entry = oldUsage[meta.Prefix]
		}
		if entry.lastUsed.IsZero() {
			entry.lastUsed = now
		}
		if entry.added.IsZero() {
			entry.added = now
		}
		usage[meta.Prefix] = entry
	}
	c.store = stagedStore
	c.full = stagedFull
	c.fullMetas = segmentMetaMap(stagedFull)
	c.generation = generation
	c.usage = usage
	c.loadErr = nil
	c.updateStatsLocked()
	// Eviction is best-effort after a successful publish: failing to shrink the
	// cache must not make a valid generation unusable.
	_ = c.evictLocked(now)
	c.pruneGenerationsLocked(generation)
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// Evict enforces MaxCacheBytes, MaxSegments and SegmentTTL immediately.
func (c *Cache) Evict() error {
	if c == nil {
		return fmt.Errorf("edgecache: nil cache")
	}
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.evictLocked(time.Now())
	if c.store != nil {
		c.pruneGenerationsLocked(c.generation)
		c.updateStatsLocked()
	}
	return err
}

// EvictNow runs Evict and returns the resulting stats.
func (c *Cache) EvictNow() Stats {
	_ = c.Evict()
	return c.Stats()
}

func (c *Cache) evictLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.evictInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			_ = c.Evict()
		}
	}
}

// evictLocked removes whole segments from the active generation. It rewrites
// the materialized manifest first (atomically, while keeping the signed source
// manifest), verifies the reduced store, then deletes the files. A crash
// therefore leaves either the old manifest with extra files or the new reduced
// manifest, never a manifest that references a deleted segment.
func (c *Cache) evictLocked(now time.Time) error {
	if c.store == nil {
		c.updateStatsLocked()
		return nil
	}
	if c.maxCacheBytes <= 0 && c.maxSegments <= 0 && c.segmentTTL <= 0 {
		c.updateStatsLocked()
		return nil
	}
	local := c.store.Manifest()
	if local == nil || len(local.Segments) == 0 {
		c.updateStatsLocked()
		return nil
	}
	activeDir := c.generationPathLocked()

	type candidate struct {
		meta edgeset.SegmentMeta
		last time.Time
	}
	candidates := make([]candidate, 0, len(local.Segments))
	bytesUsed := int64(0)
	for _, meta := range local.Segments {
		entry := c.usage[meta.Prefix]
		last := entry.lastUsed
		if last.IsZero() {
			last = entry.added
		}
		if last.IsZero() {
			if info, err := os.Stat(filepath.Join(activeDir, filepath.FromSlash(meta.File))); err == nil {
				last = info.ModTime()
			}
		}
		candidates = append(candidates, candidate{meta: meta, last: last})
		bytesUsed += meta.Size
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].last.Equal(candidates[j].last) {
			return candidates[i].meta.Prefix < candidates[j].meta.Prefix
		}
		return candidates[i].last.Before(candidates[j].last)
	})

	count := len(candidates)
	evicted := make(map[uint16]edgeset.SegmentMeta)
	for _, candidate := range candidates {
		expired := c.segmentTTL > 0 && !candidate.last.IsZero() && now.Sub(candidate.last) > c.segmentTTL
		overBytes := c.maxCacheBytes > 0 && bytesUsed > c.maxCacheBytes
		overCount := c.maxSegments > 0 && count > c.maxSegments
		if !expired && !overBytes && !overCount {
			break
		}
		evicted[candidate.meta.Prefix] = candidate.meta
		bytesUsed -= candidate.meta.Size
		count--
	}
	if len(evicted) == 0 {
		c.updateStatsLocked()
		return nil
	}

	kept := make([]edgeset.SegmentMeta, 0, len(local.Segments)-len(evicted))
	for _, meta := range local.Segments {
		if _, drop := evicted[meta.Prefix]; !drop {
			kept = append(kept, meta)
		}
	}
	reduced := &edgeset.Manifest{
		Version:     local.Version,
		PrefixBytes: local.PrefixBytes,
		Segments:    kept,
	}
	body, err := marshalLocalManifest(reduced)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(activeDir, cacheManifestFile)
	oldManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("edgecache: read active manifest: %w", err)
	}
	if err := writeFileAtomic(manifestPath, body, 0o644); err != nil {
		return fmt.Errorf("edgecache: write reduced manifest: %w", err)
	}
	if err := syncDir(activeDir); err != nil {
		_ = writeFileAtomic(manifestPath, oldManifest, 0o644)
		return fmt.Errorf("edgecache: sync reduced manifest: %w", err)
	}
	reducedStore, _, err := loadGeneration(activeDir, c.verifier)
	if err != nil {
		// The old manifest still describes files that were never deleted.
		_ = writeFileAtomic(manifestPath, oldManifest, 0o644)
		return fmt.Errorf("edgecache: verify reduced generation: %w", err)
	}

	// The reduced manifest is already durable and verified. Swap the in-memory
	// view before deleting files so a deletion failure leaves extra orphan
	// files, never a store that references missing segments.
	c.store = reducedStore
	for prefix := range evicted {
		delete(c.usage, prefix)
	}
	c.evictions += int64(len(evicted))
	for _, meta := range evicted {
		_ = os.Remove(filepath.Join(activeDir, filepath.FromSlash(meta.File)))
	}
	_ = syncDir(filepath.Join(activeDir, cacheSegmentsDir))
	c.updateStatsLocked()
	return nil
}

func (c *Cache) updateStatsLocked() {
	var bytesUsed int64
	segments := 0
	if c.store != nil {
		for _, meta := range c.store.Manifest().Segments {
			bytesUsed += meta.Size
			segments++
		}
	}
	c.stats = Stats{
		Bytes:       bytesUsed,
		Segments:    segments,
		Evictions:   c.evictions,
		Generations: c.countGenerationsLocked(),
	}
}

func (c *Cache) countGenerationsLocked() int {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), cacheGenerationPrefix) {
			count++
		}
	}
	return count
}

// Contains answers whether h is in the authoritative set.
//
// The local Bloom filter is authoritative for negatives: false is returned
// without any HTTP request. A Bloom positive is checked exactly against the
// local segment. If the segment is not materialized (excluded by the prefix
// filter or evicted), the server is asked for an exact answer and its Merkle
// proof is verified against the locally cached manifest root.
func (c *Cache) Contains(ctx context.Context, h edgeset.Hash) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("edgecache: nil cache")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.maybeEvictExpired()

	// Hold the read lock while local segments are consulted so a concurrent
	// Sync or eviction cannot replace the files underneath an old manifest.
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
			c.touch(prefix)
			return false, nil
		}
		present, _, err := store.Contains(h)
		if err != nil {
			c.mu.RUnlock()
			return false, fmt.Errorf("edgecache: local lookup: %w", err)
		}
		c.mu.RUnlock()
		c.touch(prefix)
		if present {
			return true, nil
		}
		// The materialized segment is complete and verified: a Bloom false
		// positive is a definitive local negative.
		return false, nil
	}

	meta, ok := metas[prefix]
	c.mu.RUnlock()
	if !ok {
		// No segment with this prefix exists in the authoritative manifest, so
		// the hash is definitively absent.
		return false, nil
	}
	c.touch(prefix)
	result, err := c.lookupRemote(ctx, h, prefix, meta)
	if err != nil {
		return false, err
	}
	return result.Present, nil
}

// maybeEvictExpired applies TTL lazily so a lookup does not keep answering from
// an expired segment when no background loop or explicit Evict ran.
func (c *Cache) maybeEvictExpired() {
	if c.segmentTTL <= 0 {
		return
	}
	now := time.Now()
	c.mu.RLock()
	expired := false
	for _, entry := range c.usage {
		last := entry.lastUsed
		if last.IsZero() {
			last = entry.added
		}
		if !last.IsZero() && now.Sub(last) > c.segmentTTL {
			expired = true
			break
		}
	}
	c.mu.RUnlock()
	if expired {
		_ = c.Evict()
	}
}

func (c *Cache) touch(prefix uint16) {
	c.mu.Lock()
	if c.usage == nil {
		c.usage = make(map[uint16]usageInfo)
	}
	entry := c.usage[prefix]
	entry.lastUsed = time.Now()
	c.usage[prefix] = entry
	c.mu.Unlock()
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

// loadLocal opens the best available generation. It never mutates a
// generation; it only repairs a stale CURRENT pointer.
func (c *Cache) loadLocal() {
	store, full, generation, err := c.openBestGeneration()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.store = nil
		c.full = nil
		c.fullMetas = nil
		c.generation = ""
		c.usage = nil
		if errors.Is(err, errNoLocalCache) {
			c.loadErr = nil
		} else {
			c.loadErr = err
		}
		c.updateStatsLocked()
		return
	}
	c.store = store
	c.full = full
	c.fullMetas = segmentMetaMap(full)
	c.generation = generation
	c.loadErr = nil
	c.initUsageLocked(store, c.generationPathLocked())
	c.updateStatsLocked()
}

// openBestGeneration tries CURRENT first, then scans generations newest-first,
// then falls back to the legacy root layout. A corrupted CURRENT generation is
// skipped in favour of any older verified generation.
func (c *Cache) openBestGeneration() (*edgeset.Store, *edgeset.Manifest, string, error) {
	current, currentErr := readCurrentPointer(c.dir)
	var firstErr error
	if current != "" {
		store, full, err := loadGeneration(filepath.Join(c.dir, current), c.verifier)
		if err == nil {
			return store, full, current, nil
		}
		firstErr = err
	}

	generations, err := listGenerations(c.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(generations)))
	for _, generation := range generations {
		if generation == current {
			continue
		}
		store, full, err := loadGeneration(filepath.Join(c.dir, generation), c.verifier)
		if err == nil {
			if current != generation {
				_ = writeCurrentPointer(c.dir, generation)
			}
			return store, full, generation, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	// Legacy readiness: caches created before generations kept the store at
	// the root. They remain readable and are replaced by a generation on the
	// next successful Sync.
	if _, statErr := os.Stat(filepath.Join(c.dir, cacheManifestFile)); statErr == nil {
		store, full, err := loadGeneration(c.dir, c.verifier)
		if err == nil {
			return store, full, "", nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	if currentErr != nil && firstErr == nil {
		firstErr = currentErr
	}
	if firstErr != nil {
		return nil, nil, "", firstErr
	}
	return nil, nil, "", errNoLocalCache
}

// loadGeneration verifies one generation directory and returns its materialized
// store plus the full signed manifest.
func loadGeneration(dir string, verifier edgeset.Verifier) (*edgeset.Store, *edgeset.Manifest, error) {
	sourcePath := filepath.Join(dir, cacheSourceManifestFile)
	rawSource, err := os.ReadFile(sourcePath)
	var full *edgeset.Manifest
	switch {
	case err == nil:
		full, err = decodeManifest(rawSource, verifier)
		if err != nil {
			return nil, nil, fmt.Errorf("source manifest: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Older caches may only have the materialized manifest.
	default:
		return nil, nil, fmt.Errorf("read source manifest: %w", err)
	}

	localPath := filepath.Join(dir, cacheManifestFile)
	rawLocal, err := os.ReadFile(localPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	local, err := decodeManifest(rawLocal, nil)
	if err != nil {
		return nil, nil, err
	}
	if full == nil {
		// Without the source manifest the materialized manifest itself must
		// carry a signature acceptable to the verifier.
		if verifier != nil {
			if err := verifier.VerifyManifest(local); err != nil {
				return nil, nil, err
			}
		}
		full = local
	}

	store, _, err := edgeset.LoadStoreVerified(dir, nil)
	if err != nil {
		return nil, nil, err
	}
	if !materializedSubset(store.Manifest(), full) {
		return nil, nil, fmt.Errorf("%w: materialized segments do not match the source manifest", ErrInvalidManifest)
	}
	return store, full, nil
}

func (c *Cache) initUsageLocked(store *edgeset.Store, dir string) {
	usage := make(map[uint16]usageInfo)
	if store != nil {
		for _, meta := range store.Manifest().Segments {
			var entry usageInfo
			if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(meta.File))); err == nil {
				entry.added = info.ModTime()
				entry.lastUsed = info.ModTime()
			}
			usage[meta.Prefix] = entry
		}
	}
	c.usage = usage
}

func (c *Cache) cleanupStaleStaging() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && isStagingDirName(name) {
			_ = os.RemoveAll(filepath.Join(c.dir, name))
			continue
		}
		if strings.HasPrefix(name, ".CURRENT.tmp-") {
			_ = os.Remove(filepath.Join(c.dir, name))
		}
	}
}

// pruneGenerationsLocked keeps the active generation and one previous valid
// generation (the startup fallback) and removes older generations and stale
// staging directories.
func (c *Cache) pruneGenerationsLocked(active string) {
	generations, err := listGenerations(c.dir)
	if err != nil {
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(generations)))
	keptPrevious := false
	for _, generation := range generations {
		if generation == active {
			continue
		}
		if !keptPrevious {
			// Retain the newest non-active generation that still verifies as
			// the startup fallback; discard corrupt or older ones.
			if _, _, err := loadGeneration(filepath.Join(c.dir, generation), c.verifier); err == nil {
				keptPrevious = true
				continue
			}
		}
		_ = os.RemoveAll(filepath.Join(c.dir, generation))
	}

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && strings.HasPrefix(name, cacheStagingPrefix) {
			_ = os.RemoveAll(filepath.Join(c.dir, name))
			continue
		}
		if strings.HasPrefix(name, ".CURRENT.tmp-") {
			_ = os.Remove(filepath.Join(c.dir, name))
		}
	}
}

func readCurrentPointer(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, cacheCurrentFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("edgecache: read CURRENT: %w", err)
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return "", fmt.Errorf("edgecache: empty CURRENT pointer")
	}
	if filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("edgecache: invalid CURRENT pointer %q", name)
	}
	return name, nil
}

func writeCurrentPointer(dir, name string) error {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("edgecache: invalid generation name %q", name)
	}
	if err := writeFileAtomic(filepath.Join(dir, cacheCurrentFile), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	return syncDir(dir)
}

func isStagingDirName(name string) bool {
	return strings.HasPrefix(name, cacheStagingPrefix) || strings.HasPrefix(name, cacheLegacyStaging)
}

func listGenerations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	generations := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), cacheGenerationPrefix) {
			generations = append(generations, entry.Name())
		}
	}
	return generations, nil
}

func newGenerationName() string {
	return fmt.Sprintf("%s%019d-%06d", cacheGenerationPrefix, time.Now().UnixNano(), generationSequence.Add(1)%1_000_000)
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
		Version:      m.Version,
		PrefixBytes:  m.PrefixBytes,
		SignatureAlg: m.SignatureAlg,
		Segments:     append([]edgeset.SegmentMeta(nil), m.Segments...),
	}
	if m.Signature != nil {
		out.Signature = append([]byte(nil), m.Signature...)
	}
	return out
}

func cloneUsage(in map[uint16]usageInfo) map[uint16]usageInfo {
	if len(in) == 0 {
		return nil
	}
	out := make(map[uint16]usageInfo, len(in))
	for prefix, entry := range in {
		out[prefix] = entry
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

// syncDir fsyncs a directory so a completed rename is durable. Some platforms
// do not support directory fsync; those errors are ignored.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, os.ErrInvalid) {
			return nil
		}
		return err
	}
	return nil
}
