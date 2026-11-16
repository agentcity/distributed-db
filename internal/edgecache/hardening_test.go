package edgecache_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/edgecache"
	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

type blockSegmentsHandler struct {
	inner   http.Handler
	started chan struct{}
	once    sync.Once
}

func (h *blockSegmentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/segment/") {
		h.once.Do(func() { close(h.started) })
		<-r.Context().Done()
		return
	}
	h.inner.ServeHTTP(w, r)
}

func newServerWithVerifier(t *testing.T, dir string, verifier edgeset.Verifier) *edgehttp.Server {
	t.Helper()
	srv, err := edgehttp.NewServerWithOptions(dir, edgehttp.Options{Verifier: verifier})
	if err != nil {
		t.Fatalf("NewServerWithOptions: %v", err)
	}
	return srv
}

func generationSegmentPath(cache *edgecache.Cache, prefix uint16) string {
	return filepath.Join(cache.GenerationDir(), "segments", fmt.Sprintf("%04x.seg", prefix))
}

func currentPointer(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "CURRENT"))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

func TestCrashDuringSyncKeepsPreviousGeneration(t *testing.T) {
	key := []byte("crash-key")
	oldHash := hash(0x1111, 1)
	newHash := hash(0x1111, 2)
	oldDir, _ := buildStore(t, []edgeset.Hash{oldHash}, key)
	newDir, _ := buildStore(t, []edgeset.Hash{newHash}, key)

	handler := newSwitchHandler(edgeServer(t, oldDir, key).Handler())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	cache := newCache(t, ts.URL, t.TempDir(), key, edgecache.Options{})
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	beforeGeneration := cache.CurrentGeneration()
	beforeDir := cache.GenerationDir()
	if beforeGeneration == "" {
		t.Fatal("no current generation after Sync")
	}

	blocked := &blockSegmentsHandler{
		inner:   edgeServer(t, newDir, key).Handler(),
		started: make(chan struct{}),
	}
	handler.Set(blocked)

	syncCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cache.Sync(syncCtx) }()

	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("segment download did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted Sync returned nil")
		}
		if !errorsIsCanceled(err) {
			t.Fatalf("interrupted Sync error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted Sync did not return")
	}

	if got := cache.CurrentGeneration(); got != beforeGeneration {
		t.Fatalf("CURRENT generation changed after interrupted sync: %q -> %q", beforeGeneration, got)
	}
	if got := cache.GenerationDir(); got != beforeDir {
		t.Fatalf("active generation changed: %q -> %q", beforeDir, got)
	}
	if !cache.Synced() {
		t.Fatalf("cache lost its valid generation: %v", cache.LoadError())
	}
	if present, err := cache.Contains(ctx, oldHash); err != nil || !present {
		t.Fatalf("old Contains after crash = %v, %v", present, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(cache.Dir(), ".staging-*")); len(matches) != 0 {
		t.Fatalf("stale staging directories after interrupted sync: %v", matches)
	}
}

func errorsIsCanceled(err error) bool {
	return err != nil && (strings.Contains(err.Error(), context.Canceled.Error()) ||
		strings.Contains(err.Error(), "context canceled"))
}

func TestStartupFallsBackFromCorruptCurrentGeneration(t *testing.T) {
	key := []byte("fallback-key")
	oldHash := hash(0x1111, 1)
	newHash := hash(0x1111, 2)
	oldDir, _ := buildStore(t, []edgeset.Hash{oldHash}, key)
	newDir, newManifest := buildStore(t, []edgeset.Hash{newHash}, key)

	handler := newSwitchHandler(edgeServer(t, oldDir, key).Handler())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	handler.Set(edgeServer(t, newDir, key).Handler())
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	corruptGeneration := cache.CurrentGeneration()
	if corruptGeneration == "" {
		t.Fatal("no generation after sync")
	}

	// Corrupt the active generation's segment. The checksum check must reject
	// it and startup must fall back to the previous generation.
	path := filepath.Join(cache.GenerationDir(), filepath.FromSlash(newManifest.Segments[0].File))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	reopened := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	t.Cleanup(func() { _ = reopened.Close() })
	if !reopened.Synced() {
		t.Fatalf("reopened cache is not synced after fallback: %v", reopened.LoadError())
	}
	if reopened.CurrentGeneration() == corruptGeneration {
		t.Fatal("reopened cache still points at the corrupt generation")
	}
	if got := currentPointer(t, cacheDir); got != reopened.CurrentGeneration() {
		t.Fatalf("CURRENT = %q, want repaired %q", got, reopened.CurrentGeneration())
	}
	if present, err := reopened.Contains(ctx, oldHash); err != nil || !present {
		t.Fatalf("fallback Contains old hash = %v, %v", present, err)
	}
}

func TestInvalidCurrentPointerFallsBack(t *testing.T) {
	key := []byte("pointer-key")
	oldHash := hash(0x1111, 1)
	newHash := hash(0x1111, 2)
	oldDir, _ := buildStore(t, []edgeset.Hash{oldHash}, key)
	newDir, _ := buildStore(t, []edgeset.Hash{newHash}, key)

	handler := newSwitchHandler(edgeServer(t, oldDir, key).Handler())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	handler.Set(edgeServer(t, newDir, key).Handler())
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	wantGeneration := cache.CurrentGeneration()
	_ = cache.Close()

	if err := os.WriteFile(filepath.Join(cacheDir, "CURRENT"), []byte("gen-does-not-exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopened := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	t.Cleanup(func() { _ = reopened.Close() })
	if !reopened.Synced() {
		t.Fatalf("cache not synced after pointer fallback: %v", reopened.LoadError())
	}
	if reopened.CurrentGeneration() != wantGeneration {
		t.Fatalf("fallback generation = %q, want newest valid %q", reopened.CurrentGeneration(), wantGeneration)
	}
	if got := currentPointer(t, cacheDir); got != wantGeneration {
		t.Fatalf("CURRENT = %q, want repaired %q", got, wantGeneration)
	}
	if present, err := reopened.Contains(ctx, newHash); err != nil || !present {
		t.Fatalf("Contains after pointer fallback = %v, %v", present, err)
	}
}

func TestPartialStagingIsIgnoredAndCleaned(t *testing.T) {
	key := []byte("staging-key")
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, _ := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	if err := cache.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	wantGeneration := cache.CurrentGeneration()
	_ = cache.Close()

	stale := filepath.Join(cacheDir, ".staging-interrupted")
	if err := os.MkdirAll(filepath.Join(stale, "segments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	reopened := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	t.Cleanup(func() { _ = reopened.Close() })
	if !reopened.Synced() {
		t.Fatalf("cache not synced with partial staging present: %v", reopened.LoadError())
	}
	if reopened.CurrentGeneration() != wantGeneration {
		t.Fatalf("generation = %q, want %q", reopened.CurrentGeneration(), wantGeneration)
	}
	if _, err := os.Stat(stale); !errorsIsNotExist(err) {
		t.Fatalf("stale staging directory was not removed: %v", err)
	}
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

func TestEvictionByMaxSegmentsAndRemoteFallback(t *testing.T) {
	key := []byte("evict-count-key")
	hashes := []edgeset.Hash{
		hash(0x1111, 1), hash(0x1111, 2),
		hash(0x2222, 1), hash(0x2222, 2),
	}
	storeDir, _ := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	counter := &countingHandler{next: srv.Handler()}
	ts := httptest.NewServer(counter)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{MaxSegments: 1})
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	stats := cache.Stats()
	if stats.Segments != 1 {
		t.Fatalf("segments = %d, want 1", stats.Segments)
	}
	if stats.Evictions != 1 {
		t.Fatalf("evictions = %d, want 1", stats.Evictions)
	}
	evicted := uint16(0)
	if _, ok := cache.Store().SegmentMeta(0x1111); !ok {
		evicted = 0x1111
	} else if _, ok := cache.Store().SegmentMeta(0x2222); !ok {
		evicted = 0x2222
	}
	if evicted == 0 {
		t.Fatal("no segment was evicted")
	}
	if _, err := os.Stat(generationSegmentPath(cache, evicted)); !errorsIsNotExist(err) {
		t.Fatalf("evicted segment file still exists: %v", err)
	}

	before := counter.n.Load()
	present, err := cache.Contains(ctx, hash(evicted, 1))
	if err != nil {
		t.Fatalf("remote fallback Contains: %v", err)
	}
	if !present {
		t.Fatal("remote fallback contains = false, want true")
	}
	if counter.n.Load() <= before {
		t.Fatal("evicted segment did not fall back to the server")
	}
}

func TestEvictionByMaxBytes(t *testing.T) {
	key := []byte("evict-bytes-key")
	hashes := []edgeset.Hash{
		hash(0x1111, 1), hash(0x1111, 2),
		hash(0x2222, 1), hash(0x2222, 2),
	}
	storeDir, manifest := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	maxBytes := int64(0)
	for _, meta := range manifest.Segments {
		if meta.Size > maxBytes {
			maxBytes = meta.Size
		}
	}
	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{MaxCacheBytes: maxBytes})
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	stats := cache.Stats()
	if stats.Bytes > maxBytes {
		t.Fatalf("cached bytes = %d, want <= %d", stats.Bytes, maxBytes)
	}
	if stats.Segments != 1 || stats.Evictions == 0 {
		t.Fatalf("stats = %+v, want one segment and at least one eviction", stats)
	}
}

func TestTTLEvictionAndRemoteFallback(t *testing.T) {
	key := []byte("evict-ttl-key")
	hashes := []edgeset.Hash{hash(0x1111, 1), hash(0x2222, 1)}
	storeDir, _ := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	counter := &countingHandler{next: srv.Handler()}
	ts := httptest.NewServer(counter)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{SegmentTTL: 50 * time.Millisecond})
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if cache.Stats().Segments == 0 {
		t.Fatal("TTL evicted segments immediately")
	}
	time.Sleep(120 * time.Millisecond)
	if err := cache.Evict(); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	stats := cache.Stats()
	if stats.Segments != 0 {
		t.Fatalf("segments after TTL = %d, want 0", stats.Segments)
	}
	if stats.Evictions < 2 {
		t.Fatalf("evictions = %d, want at least 2", stats.Evictions)
	}

	before := counter.n.Load()
	present, err := cache.Contains(ctx, hashes[0])
	if err != nil {
		t.Fatalf("remote fallback after TTL: %v", err)
	}
	if !present {
		t.Fatal("remote fallback after TTL = false")
	}
	if counter.n.Load() <= before {
		t.Fatal("TTL-evicted segment did not fall back to the server")
	}
}

func TestLRUEvictionKeepsRecentlyUsedSegment(t *testing.T) {
	key := []byte("evict-lru-key")
	hashes := []edgeset.Hash{hash(0x1111, 1), hash(0x2222, 1)}
	storeDir, _ := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{SegmentTTL: 250 * time.Millisecond})
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	// Touch only prefix 1111, making 2222 the oldest segment.
	if present, err := cache.Contains(ctx, hash(0x1111, 1)); err != nil || !present {
		t.Fatalf("touch Contains = %v, %v", present, err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := cache.Evict(); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, ok := cache.Store().SegmentMeta(0x1111); !ok {
		t.Fatal("recently used segment 1111 was evicted")
	}
	if _, ok := cache.Store().SegmentMeta(0x2222); ok {
		t.Fatal("stale segment 2222 was not evicted")
	}
}

func TestEd25519CacheVerifierAndCompression(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hashes := []edgeset.Hash{hash(0x1111, 1), hash(0x2222, 1)}
	storeDir := t.TempDir()
	if _, _, err := edgeset.Build(hashes, edgeset.BuildOptions{
		SegmentDir:        storeDir,
		Ed25519PrivateKey: priv,
		Compression:       "zstd",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	srv := newServerWithVerifier(t, storeDir, edgeset.NewEd25519Verifier(pub))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	verifier := edgeset.NewEd25519Verifier(pub)
	cache := newCache(t, ts.URL, cacheDir, nil, edgecache.Options{Verifier: verifier})
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if present, err := cache.Contains(ctx, hashes[0]); err != nil || !present {
		t.Fatalf("Contains = %v, %v", present, err)
	}
	loaded, manifest, err := edgeset.LoadStoreVerified(cache.GenerationDir(), verifier)
	if err != nil {
		t.Fatalf("LoadStoreVerified(generation): %v", err)
	}
	if loaded.TotalCount() != len(hashes) {
		t.Fatalf("cached total = %d", loaded.TotalCount())
	}
	if manifest.SignatureAlg != edgeset.SignatureAlgEd25519 {
		t.Fatalf("signature_alg = %q", manifest.SignatureAlg)
	}
	for _, meta := range manifest.Segments {
		if meta.Compression != edgeset.CompressionZstd {
			t.Fatalf("segment %04x compression = %q, want zstd", meta.Prefix, meta.Compression)
		}
	}

	// A different public key must be rejected.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrong := newCache(t, ts.URL, t.TempDir(), nil, edgecache.Options{
		Verifier: edgeset.NewEd25519Verifier(otherPub),
	})
	t.Cleanup(func() { _ = wrong.Close() })
	if err := wrong.Sync(ctx); !errorsIsManifestSignature(err) {
		t.Fatalf("Sync with wrong public key = %v, want manifest signature error", err)
	}
	if wrong.Synced() {
		t.Fatal("wrong-key cache is synced")
	}
}

func errorsIsManifestSignature(err error) bool {
	return err != nil && (strings.Contains(err.Error(), edgeset.ErrManifestSignature.Error()) ||
		strings.Contains(err.Error(), "signature"))
}

func TestCacheAuthToken(t *testing.T) {
	key := []byte("auth-cache-key")
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, _ := buildStore(t, hashes, key)
	srv, err := edgehttp.NewServerWithOptions(storeDir, edgehttp.Options{
		Verifier:  edgeset.NewHMACVerifier(key),
		AuthToken: "cache-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cache := newCache(t, ts.URL, t.TempDir(), key, edgecache.Options{AuthToken: "cache-token"})
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.Sync(context.Background()); err != nil {
		t.Fatalf("Sync with token: %v", err)
	}
	if present, err := cache.Contains(context.Background(), hashes[0]); err != nil || !present {
		t.Fatalf("Contains = %v, %v", present, err)
	}

	noToken := newCache(t, ts.URL, t.TempDir(), key, edgecache.Options{})
	t.Cleanup(func() { _ = noToken.Close() })
	if err := noToken.Sync(context.Background()); err == nil {
		t.Fatal("Sync without token succeeded against an auth server")
	}
}
