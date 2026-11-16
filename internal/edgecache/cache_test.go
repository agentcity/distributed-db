package edgecache_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"distributed-db/internal/edgecache"
	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

func hash(prefix uint16, tail uint32) edgeset.Hash {
	var h edgeset.Hash
	binary.BigEndian.PutUint16(h[0:2], prefix)
	binary.BigEndian.PutUint32(h[28:32], tail)
	return h
}

func buildStore(t *testing.T, hashes []edgeset.Hash, key []byte) (string, *edgeset.Manifest) {
	t.Helper()
	dir := t.TempDir()
	_, manifest, err := edgeset.Build(hashes, edgeset.BuildOptions{
		SegmentDir: dir,
		SigningKey: key,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dir, manifest
}

func edgeServer(t *testing.T, dir string, key []byte) *edgehttp.Server {
	t.Helper()
	srv, err := edgehttp.NewServer(dir, key)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

type countingHandler struct {
	next http.Handler
	n    atomic.Int64
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.n.Add(1)
	h.next.ServeHTTP(w, r)
}

type switchHandler struct {
	handler atomic.Value
}

func newSwitchHandler(next http.Handler) *switchHandler {
	h := &switchHandler{}
	h.handler.Store(next)
	return h
}

func (h *switchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.Load().(http.Handler).ServeHTTP(w, r)
}

func (h *switchHandler) Set(next http.Handler) {
	h.handler.Store(next)
}

func newCache(t *testing.T, serverURL, dir string, key []byte, opts edgecache.Options) *edgecache.Cache {
	t.Helper()
	cache, err := edgecache.New(serverURL, dir, key, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache
}

func TestSyncAllAndLocalLookups(t *testing.T) {
	key := []byte("sync-key")
	hashes := []edgeset.Hash{
		hash(0x1111, 1), hash(0x1111, 2), hash(0x1111, 3),
		hash(0x2222, 1), hash(0x2222, 2),
	}
	storeDir, manifest := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	counter := &countingHandler{next: srv.Handler()}
	ts := httptest.NewServer(counter)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !cache.Synced() {
		t.Fatal("cache not synced after Sync")
	}

	// With the default all-segment policy the materialized cache is a fully
	// signed edgeset store loadable with the key.
	materialized, loaded, err := edgeset.LoadStore(cacheDir, key)
	if err != nil {
		t.Fatalf("LoadStore(materialized): %v", err)
	}
	if materialized.TotalCount() != len(hashes) {
		t.Fatalf("cached total = %d, want %d", materialized.TotalCount(), len(hashes))
	}
	if loaded.Version != manifest.Version || len(loaded.Segments) != len(manifest.Segments) {
		t.Fatalf("cached manifest = %+v", loaded)
	}
	if cache.Manifest() == nil || len(cache.Manifest().Segments) != len(manifest.Segments) {
		t.Fatal("Cache.Manifest does not expose the full manifest")
	}

	before := counter.n.Load()
	present, err := cache.Contains(ctx, hashes[1])
	if err != nil {
		t.Fatalf("Contains present: %v", err)
	}
	if !present {
		t.Fatal("Contains present = false")
	}
	if got := counter.n.Load(); got != before {
		t.Fatalf("local positive used HTTP: requests %d -> %d", before, got)
	}

	// Find a hash in an existing segment for which the Bloom filter says no.
	local := cache.Store()
	var bloomNegative edgeset.Hash
	for tail := uint32(100); tail < 500; tail++ {
		candidate := hash(0x1111, tail)
		maybe, err := local.MayContain(candidate)
		if err != nil {
			t.Fatalf("MayContain: %v", err)
		}
		if !maybe {
			bloomNegative = candidate
			break
		}
	}
	if bloomNegative == (edgeset.Hash{}) {
		t.Skip("no Bloom-negative candidate found")
	}
	before = counter.n.Load()
	present, err = cache.Contains(ctx, bloomNegative)
	if err != nil {
		t.Fatalf("Contains Bloom negative: %v", err)
	}
	if present {
		t.Fatal("Bloom-negative hash reported present")
	}
	if got := counter.n.Load(); got != before {
		t.Fatalf("Bloom negative used HTTP: requests %d -> %d", before, got)
	}

	// A missing prefix is definitively absent and also stays local.
	before = counter.n.Load()
	present, err = cache.Contains(ctx, hash(0x9999, 1))
	if err != nil {
		t.Fatalf("Contains missing prefix: %v", err)
	}
	if present {
		t.Fatal("hash with missing prefix reported present")
	}
	if got := counter.n.Load(); got != before {
		t.Fatalf("missing prefix used HTTP: requests %d -> %d", before, got)
	}
}

func TestSyncPrefixFilterAndRemoteLookups(t *testing.T) {
	key := []byte("filter-key")
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
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{
		Prefixes: []uint16{0x2222},
	})
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Only the selected segment is materialized. The partial manifest is
	// unsigned, so it is loaded without a key.
	local, materialized, err := edgeset.LoadStore(cacheDir, nil)
	if err != nil {
		t.Fatalf("LoadStore(filtered): %v", err)
	}
	if len(materialized.Segments) != 1 || materialized.Segments[0].Prefix != 0x2222 {
		t.Fatalf("materialized segments = %+v", materialized.Segments)
	}
	if local.TotalCount() != 2 {
		t.Fatalf("filtered total = %d, want 2", local.TotalCount())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "segments", "1111.seg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected segment was downloaded: %v", err)
	}

	// Cached positive and cached negative answers need no network.
	before := counter.n.Load()
	if present, err := cache.Contains(ctx, hashes[2]); err != nil || !present {
		t.Fatalf("local filtered Contains = %v, %v", present, err)
	}
	for tail := uint32(100); tail < 500; tail++ {
		candidate := hash(0x2222, tail)
		maybe, err := cache.Store().MayContain(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if !maybe {
			if present, err := cache.Contains(ctx, candidate); err != nil || present {
				t.Fatalf("local filtered negative = %v, %v", present, err)
			}
			break
		}
	}
	if got := counter.n.Load(); got != before {
		t.Fatalf("local filtered lookups used HTTP: %d -> %d", before, got)
	}

	// An excluded prefix is resolved remotely and the proof is verified.
	before = counter.n.Load()
	present, err := cache.Contains(ctx, hashes[0])
	if err != nil {
		t.Fatalf("remote Contains: %v", err)
	}
	if !present {
		t.Fatal("remote Contains present = false")
	}
	if got := counter.n.Load(); got <= before {
		t.Fatal("remote Contains did not contact the server")
	}

	// The same lookup without a materialized cache returns a verifiable proof.
	result, err := edgecache.RemoteLookup(ctx, ts.URL, key, hashes[0], edgecache.Options{})
	if err != nil {
		t.Fatalf("RemoteLookup: %v", err)
	}
	if !result.Present || result.Proof == nil {
		t.Fatalf("RemoteLookup = %+v", result)
	}
	if !edgeset.VerifyProof(result.Root, hashes[0], result.Proof) {
		t.Fatal("RemoteLookup proof does not verify")
	}

	// Absent hash in an excluded prefix: remote present=false.
	present, err = cache.Contains(ctx, hash(0x1111, 999))
	if err != nil {
		t.Fatalf("remote absent Contains: %v", err)
	}
	if present {
		t.Fatal("remote absent Contains = true")
	}

	// Reopening the cache must restore the verified full manifest and the
	// partial local store.
	reopened := newCache(t, ts.URL, cacheDir, key, edgecache.Options{Prefixes: []uint16{0x2222}})
	if !reopened.Synced() {
		t.Fatalf("reopened cache not synced: %v", reopened.LoadError())
	}
	if present, err := reopened.Contains(ctx, hashes[2]); err != nil || !present {
		t.Fatalf("reopened local Contains = %v, %v", present, err)
	}
	if present, err := reopened.Contains(ctx, hashes[0]); err != nil || !present {
		t.Fatalf("reopened remote Contains = %v, %v", present, err)
	}
}

func TestSyncStaleManifestUpdate(t *testing.T) {
	key := []byte("stale-key")
	oldHash := hash(0x1111, 1)
	newHash := hash(0x1111, 2)

	oldDir, oldManifest := buildStore(t, []edgeset.Hash{oldHash}, key)
	newDir, newManifest := buildStore(t, []edgeset.Hash{newHash}, key)
	if oldManifest.Segments[0].SHA256 == newManifest.Segments[0].SHA256 {
		t.Fatal("test stores unexpectedly share a segment checksum")
	}

	handler := newSwitchHandler(edgeServer(t, oldDir, key).Handler())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	cache := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	ctx := context.Background()
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if present, err := cache.Contains(ctx, oldHash); err != nil || !present {
		t.Fatalf("old Contains = %v, %v", present, err)
	}

	handler.Set(edgeServer(t, newDir, key).Handler())
	if err := cache.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if present, err := cache.Contains(ctx, newHash); err != nil || !present {
		t.Fatalf("new Contains = %v, %v", present, err)
	}
	if present, err := cache.Contains(ctx, oldHash); err != nil {
		t.Fatalf("old hash after update: %v", err)
	} else if present {
		t.Fatal("old hash still reported present after update")
	}

	// The single segment file must have been replaced, not duplicated.
	files, err := filepath.Glob(filepath.Join(cacheDir, "segments", "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("cached segment files = %v, want one", files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if edgeset.HashOf(body) != newManifest.Segments[0].SHA256 {
		t.Fatal("cached segment was not replaced with the new generation")
	}
	if got := cache.Manifest(); got == nil || got.Segments[0].Root != newManifest.Segments[0].Root {
		t.Fatal("Cache.Manifest did not advance to the new generation")
	}
}

func TestSyncRejectsWrongKey(t *testing.T) {
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, _ := buildStore(t, hashes, []byte("right-key"))
	srv := edgeServer(t, storeDir, []byte("right-key"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cache := newCache(t, ts.URL, t.TempDir(), []byte("wrong-key"), edgecache.Options{})
	if err := cache.Sync(context.Background()); !errors.Is(err, edgeset.ErrManifestSignature) {
		t.Fatalf("Sync error = %v, want manifest signature failure", err)
	}
	if cache.Synced() {
		t.Fatal("cache is synced despite a rejected manifest")
	}
}

func TestSyncRejectsTamperedServerSegment(t *testing.T) {
	key := []byte("tamper-key")
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, manifest := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// The server verified the store at startup but has not read the segment
	// yet. Modify it before the cache asks for it.
	path := filepath.Join(storeDir, filepath.FromSlash(manifest.Segments[0].File))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newCache(t, ts.URL, t.TempDir(), key, edgecache.Options{})
	if err := cache.Sync(context.Background()); err == nil {
		t.Fatal("Sync accepted a tampered server segment")
	}
	if cache.Synced() {
		t.Fatal("cache is synced despite a failed segment download")
	}
}

func TestLocalTamperRejectedThenRepaired(t *testing.T) {
	key := []byte("local-tamper-key")
	hashes := []edgeset.Hash{hash(0x1111, 1), hash(0x1111, 2)}
	storeDir, manifest := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cacheDir := t.TempDir()
	first := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	ctx := context.Background()
	if err := first.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	path := filepath.Join(cacheDir, filepath.FromSlash(manifest.Segments[0].File))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tampered := newCache(t, ts.URL, cacheDir, key, edgecache.Options{})
	if tampered.Synced() {
		t.Fatal("tampered local cache was loaded")
	}
	if _, err := tampered.Contains(ctx, hashes[0]); !errors.Is(err, edgecache.ErrNotSynced) {
		t.Fatalf("Contains error = %v, want ErrNotSynced", err)
	}

	if err := tampered.Sync(ctx); err != nil {
		t.Fatalf("repair Sync: %v", err)
	}
	if !tampered.Synced() {
		t.Fatal("cache not synced after repair")
	}
	if present, err := tampered.Contains(ctx, hashes[0]); err != nil || !present {
		t.Fatalf("Contains after repair = %v, %v", present, err)
	}
}

func TestRemoteLookupRejectsTamperedProof(t *testing.T) {
	key := []byte("proof-key")
	hashes := []edgeset.Hash{hash(0x1111, 1), hash(0x1111, 2), hash(0x1111, 3)}
	storeDir, _ := buildStore(t, hashes, key)
	srv := edgeServer(t, storeDir, key)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/lookup/") {
			srv.Handler().ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, r)
		body := recorder.Body.Bytes()
		var wire map[string]any
		if err := json.Unmarshal(body, &wire); err != nil || wire["present"] != true {
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(body)
			return
		}
		proof, ok := wire["proof"].(map[string]any)
		if !ok {
			http.Error(w, "lookup response has no proof object", http.StatusInternalServerError)
			return
		}
		path, ok := proof["Path"].([]any)
		if !ok || len(path) == 0 {
			http.Error(w, "lookup proof has no path", http.StatusInternalServerError)
			return
		}
		fake := hash(0x1111, 999).String()
		path[0] = fake
		proof["Path"] = path
		wire["proof"] = proof
		tampered, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tampered)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	_, err := edgecache.RemoteLookup(context.Background(), ts.URL, key, hashes[0], edgecache.Options{})
	if !errors.Is(err, edgecache.ErrInvalidProof) {
		t.Fatalf("RemoteLookup error = %v, want ErrInvalidProof", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := edgecache.New("://bad", t.TempDir(), nil, edgecache.Options{}); err == nil {
		t.Fatal("New accepted a malformed URL")
	}
	if _, err := edgecache.New("ftp://example.com", t.TempDir(), nil, edgecache.Options{}); err == nil {
		t.Fatal("New accepted a non-HTTP URL")
	}
	if _, err := edgecache.New("http://example.com", t.TempDir(), nil, edgecache.Options{Timeout: -time.Second}); err == nil {
		t.Fatal("New accepted a negative timeout")
	}
	if err := newCache(t, "http://127.0.0.1:1", t.TempDir(), nil, edgecache.Options{}).Sync(context.Background()); err == nil {
		t.Fatal("Sync unexpectedly succeeded against an unused port")
	}
}

func TestSyncContextCanceled(t *testing.T) {
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, _ := buildStore(t, hashes, nil)
	srv := edgeServer(t, storeDir, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cache := newCache(t, ts.URL, t.TempDir(), nil, edgecache.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cache.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync error = %v, want context.Canceled", err)
	}
}

func TestRemoteLookupAbsent(t *testing.T) {
	hashes := []edgeset.Hash{hash(0x1111, 1)}
	storeDir, _ := buildStore(t, hashes, nil)
	srv := edgeServer(t, storeDir, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	result, err := edgecache.RemoteLookup(context.Background(), ts.URL, nil, hash(0x1111, 77), edgecache.Options{})
	if err != nil {
		t.Fatalf("RemoteLookup: %v", err)
	}
	if result.Present || result.Proof != nil {
		t.Fatalf("absent RemoteLookup = %+v", result)
	}
}
