package edgeset

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestBuildContainsProofs(t *testing.T) {
	hashes := randomHashes(10_000, 1)
	store, manifest := mustBuild(t, hashes, BuildOptions{})

	if manifest.Version != ManifestVersion {
		t.Fatalf("manifest version = %d, want %d", manifest.Version, ManifestVersion)
	}
	if manifest.PrefixBytes != 2 {
		t.Fatalf("manifest PrefixBytes = %d, want default 2", manifest.PrefixBytes)
	}
	if store.TotalCount() != len(hashes) {
		t.Fatalf("TotalCount = %d, want %d", store.TotalCount(), len(hashes))
	}
	if store.SegmentCount() != len(manifest.Segments) {
		t.Fatalf("SegmentCount = %d, manifest has %d", store.SegmentCount(), len(manifest.Segments))
	}

	var previous uint16
	for i, meta := range manifest.Segments {
		if i > 0 && meta.Prefix <= previous {
			t.Fatalf("segments are not strictly sorted: %04x after %04x", meta.Prefix, previous)
		}
		previous = meta.Prefix
		if meta.Count <= 0 {
			t.Fatalf("segment %04x has count %d", meta.Prefix, meta.Count)
		}
	}

	for i, h := range hashes {
		ok, proof, err := store.Contains(h)
		if err != nil {
			t.Fatalf("Contains(%s): %v", h, err)
		}
		if !ok {
			t.Fatalf("Contains(%s) = false for built hash %d", h, i)
		}
		if proof == nil {
			t.Fatalf("Contains(%s) returned no proof", h)
		}
		if proof.Hash != h {
			t.Fatalf("proof hash = %s, want %s", proof.Hash, h)
		}
		if !VerifyProof(segmentRoot(t, store, h), h, proof) {
			t.Fatalf("proof for %s does not verify", h)
		}
	}

	present := make(map[Hash]struct{}, len(hashes))
	for _, h := range hashes {
		present[h] = struct{}{}
	}
	for _, h := range randomHashes(10_000, 2) {
		if _, ok := present[h]; ok {
			continue
		}
		ok, proof, err := store.Contains(h)
		if err != nil {
			t.Fatalf("Contains(absent %s): %v", h, err)
		}
		if ok {
			t.Fatalf("Contains(absent %s) = true", h)
		}
		if proof != nil {
			t.Fatalf("Contains(absent %s) returned a proof", h)
		}
	}
}

func TestPrefixRoutingAndBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		prefixBytes int
		prefixes    []uint16
		want        []uint16
	}{
		{
			name:        "one byte",
			prefixBytes: 1,
			prefixes:    []uint16{0x0011, 0x7f22, 0xff33},
			want:        []uint16{0x00, 0x7f, 0xff},
		},
		{
			name:        "two bytes boundary",
			prefixBytes: 2,
			prefixes:    []uint16{0x0000, 0x1234, 0xffff},
			want:        []uint16{0x0000, 0x1234, 0xffff},
		},
		{
			name:        "three bytes accepted",
			prefixBytes: 3,
			prefixes:    []uint16{0x0100, 0xabcd, 0xffff},
			want:        []uint16{0x0100, 0xabcd, 0xffff},
		},
		{
			name:        "four bytes accepted",
			prefixBytes: 4,
			prefixes:    []uint16{0x0001, 0x8000, 0xffff},
			want:        []uint16{0x0001, 0x8000, 0xffff},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hashes := make([]Hash, len(tc.prefixes))
			for i, prefix := range tc.prefixes {
				hashes[i] = Hash{}
				binary.BigEndian.PutUint16(hashes[i][0:2], prefix)
				binary.BigEndian.PutUint32(hashes[i][28:32], uint32(i+1))
			}
			store, manifest := mustBuild(t, hashes, BuildOptions{PrefixBytes: tc.prefixBytes})
			if manifest.PrefixBytes != tc.prefixBytes {
				t.Fatalf("PrefixBytes = %d, want %d", manifest.PrefixBytes, tc.prefixBytes)
			}
			seen := make(map[uint16]bool)
			for _, meta := range manifest.Segments {
				seen[meta.Prefix] = true
			}
			for i, h := range hashes {
				if got := store.PrefixFor(h); got != tc.want[i] {
					t.Fatalf("PrefixFor(%s) = %04x, want %04x", h, got, tc.want[i])
				}
				ok, proof, err := store.Contains(h)
				if err != nil || !ok || proof == nil {
					t.Fatalf("Contains(%s) = %v, %v, %v", h, ok, proof, err)
				}
				if !VerifyProof(segmentRoot(t, store, h), h, proof) {
					t.Fatalf("proof for %s does not verify", h)
				}
			}
			for _, prefix := range tc.want {
				if !seen[prefix] {
					t.Fatalf("manifest has no segment for expected prefix %04x", prefix)
				}
			}
			mustLoad(t, store.Dir(), nil)
		})
	}
}

func TestEmptyStore(t *testing.T) {
	key := []byte("empty-store-key")
	dir := t.TempDir()
	store, manifest, err := Build(nil, BuildOptions{SegmentDir: dir, SigningKey: key})
	if err != nil {
		t.Fatalf("Build(empty): %v", err)
	}
	if manifest.Version != ManifestVersion || manifest.PrefixBytes != 2 {
		t.Fatalf("unexpected empty manifest: %+v", manifest)
	}
	if len(manifest.Segments) != 0 {
		t.Fatalf("empty store has %d segments", len(manifest.Segments))
	}
	if len(manifest.Signature) != sha256.Size {
		t.Fatalf("signature length = %d, want %d", len(manifest.Signature), sha256.Size)
	}
	if err := VerifyManifest(manifest, key); err != nil {
		t.Fatalf("empty signed manifest does not verify: %v", err)
	}
	if cloned := store.Manifest(); cloned.Segments == nil {
		t.Fatal("Store.Manifest turned an empty segment slice into nil")
	} else if err := VerifyManifest(cloned, key); err != nil {
		t.Fatalf("cloned empty manifest does not verify: %v", err)
	}
	if store.SegmentCount() != 0 || store.TotalCount() != 0 {
		t.Fatalf("empty store counts = %d/%d", store.SegmentCount(), store.TotalCount())
	}

	h := HashOf([]byte("not present"))
	ok, proof, err := store.Contains(h)
	if err != nil || ok || proof != nil {
		t.Fatalf("Contains on empty store = %v, %v, %v", ok, proof, err)
	}
	if _, err := store.Segment(0); !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Segment(0) error = %v, want ErrSegmentNotFound", err)
	}

	loaded, loadedManifest := mustLoad(t, dir, key)
	if len(loadedManifest.Segments) != 0 {
		t.Fatalf("reloaded empty store has %d segments", len(loadedManifest.Segments))
	}
	if ok, _, err := loaded.Contains(h); err != nil || ok {
		t.Fatalf("reloaded Contains = %v, %v", ok, err)
	}
}

func TestDuplicateHashesDeduped(t *testing.T) {
	base := randomHashes(40, 7)
	input := make([]Hash, 0, len(base)*3)
	input = append(input, base[3], base[1], base[0], base[2])
	input = append(input, base...)
	input = append(input, base[0], base[39], base[10])
	original := append([]Hash(nil), input...)

	store, manifest := mustBuild(t, input, BuildOptions{})
	if !reflect.DeepEqual(input, original) {
		t.Fatal("Build mutated the input hash slice")
	}
	if store.TotalCount() != len(base) {
		t.Fatalf("TotalCount = %d, want %d unique hashes", store.TotalCount(), len(base))
	}
	if got := len(manifest.Segments); got != store.SegmentCount() {
		t.Fatalf("SegmentCount = %d, want %d", got, store.SegmentCount())
	}
	for _, h := range base {
		ok, proof, err := store.Contains(h)
		if err != nil || !ok || !VerifyProof(segmentRoot(t, store, h), h, proof) {
			t.Fatalf("Contains(%s) after dedup = %v, %v, %v", h, ok, proof, err)
		}
	}

	// Force all duplicates into one segment and inspect its count directly.
	forced := forcedPrefixHashes(10, 0x7777, 8)
	duplicated := append(append([]Hash(nil), forced...), forced...)
	dupStore, dupManifest := mustBuild(t, duplicated, BuildOptions{})
	if dupManifest.Segments[0].Prefix != 0x7777 || dupManifest.Segments[0].Count != len(forced) {
		t.Fatalf("duplicate segment = %+v, want prefix 7777 and count %d", dupManifest.Segments[0], len(forced))
	}
	if dupStore.TotalCount() != len(forced) {
		t.Fatalf("duplicate TotalCount = %d, want %d", dupStore.TotalCount(), len(forced))
	}
}

func TestPersistenceRestart(t *testing.T) {
	hashes := randomHashes(800, 9)
	key := []byte("persistent-key")
	dir := t.TempDir()

	built, builtManifest := mustBuild(t, hashes, BuildOptions{SegmentDir: dir, SigningKey: key})
	if built.Dir() != dir {
		t.Fatalf("Store.Dir() = %q, want %q", built.Dir(), dir)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestFileName)); err != nil {
		t.Fatalf("manifest.json is missing: %v", err)
	}

	reloaded, manifest := mustLoad(t, dir, key)
	if manifest.Version != builtManifest.Version || manifest.PrefixBytes != builtManifest.PrefixBytes {
		t.Fatalf("reloaded manifest header differs: %+v vs %+v", manifest, builtManifest)
	}
	if !bytes.Equal(manifest.Signature, builtManifest.Signature) {
		t.Fatal("reloaded manifest signature differs")
	}
	if !reflect.DeepEqual(manifest.Segments, builtManifest.Segments) {
		t.Fatal("reloaded manifest segments differ")
	}
	if reloaded.TotalCount() != len(hashes) {
		t.Fatalf("reloaded TotalCount = %d, want %d", reloaded.TotalCount(), len(hashes))
	}

	for _, h := range hashes {
		ok, proof, err := reloaded.Contains(h)
		if err != nil || !ok {
			t.Fatalf("reloaded Contains(%s) = %v, %v", h, ok, err)
		}
		if !VerifyProof(segmentRoot(t, reloaded, h), h, proof) {
			t.Fatalf("reloaded proof for %s does not verify", h)
		}
	}

	// A second load exercises a fresh cache, like a process restart.
	again, _ := mustLoad(t, dir, key)
	if ok, _, err := again.Contains(hashes[0]); err != nil || !ok {
		t.Fatalf("second reload Contains = %v, %v", ok, err)
	}
}

func TestSegmentAccessorAndLayout(t *testing.T) {
	hashes := forcedPrefixHashes(120, 0x1234, 10)
	store, manifest := mustBuild(t, hashes, BuildOptions{})

	entries, err := os.ReadDir(filepath.Join(store.Dir(), "segments"))
	if err != nil {
		t.Fatalf("read segments dir: %v", err)
	}
	if len(entries) != len(manifest.Segments) {
		t.Fatalf("segment directory has %d files, manifest has %d segments", len(entries), len(manifest.Segments))
	}
	if len(entries) != 1 || entries[0].Name() != "1234.seg" {
		t.Fatalf("unexpected segment files: %v", entries)
	}

	for _, meta := range manifest.Segments {
		data, err := store.Segment(meta.Prefix)
		if err != nil {
			t.Fatalf("Segment(%04x): %v", meta.Prefix, err)
		}
		if int64(len(data)) != meta.Size {
			t.Fatalf("Segment(%04x) size = %d, want %d", meta.Prefix, len(data), meta.Size)
		}
		if sum := sha256.Sum256(data); sum != meta.SHA256 {
			t.Fatalf("Segment(%04x) checksum mismatch", meta.Prefix)
		}
		if !bytes.HasPrefix(data, []byte(segmentMagic)) {
			t.Fatalf("Segment(%04x) lacks magic header", meta.Prefix)
		}

		path, err := store.SegmentPath(meta.Prefix)
		if err != nil {
			t.Fatalf("SegmentPath(%04x): %v", meta.Prefix, err)
		}
		fromDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(fromDisk, data) {
			t.Fatalf("Segment(%04x) differs from its file", meta.Prefix)
		}

		// Segment returns a copy; mutating it must not poison the cache.
		data[0] ^= 0xff
		again, err := store.Segment(meta.Prefix)
		if err != nil || !bytes.Equal(again, fromDisk) {
			t.Fatalf("Segment(%04x) returned cached mutable data: %v", meta.Prefix, err)
		}
	}

	missing := uint16(0)
	for {
		if _, ok := store.SegmentMeta(missing); !ok {
			break
		}
		missing++
	}
	if _, err := store.Segment(missing); !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("Segment(missing) error = %v, want ErrSegmentNotFound", err)
	}
	if _, err := store.SegmentPath(missing); !errors.Is(err, ErrSegmentNotFound) {
		t.Fatalf("SegmentPath(missing) error = %v, want ErrSegmentNotFound", err)
	}

	reloaded, _ := mustLoad(t, store.Dir(), nil)
	if data, err := reloaded.Segment(0x1234); err != nil || !bytes.HasPrefix(data, []byte(segmentMagic)) {
		t.Fatalf("reloaded Segment = %v, %v", data, err)
	}
}

func TestConcurrentContains(t *testing.T) {
	hashes := forcedPrefixHashes(400, 0x4444, 11)
	store, _ := mustBuild(t, hashes, BuildOptions{})

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := offset; i < len(hashes); i += workers {
				h := hashes[i]
				ok, proof, err := store.Contains(h)
				if err != nil {
					errs <- fmt.Errorf("Contains(%s): %w", h, err)
					return
				}
				if !ok || proof == nil {
					errs <- fmt.Errorf("Contains(%s) = %v with proof %v", h, ok, proof)
					return
				}
				meta, _ := store.SegmentMeta(store.PrefixFor(h))
				if !VerifyProof(meta.Root, h, proof) {
					errs <- fmt.Errorf("proof for %s does not verify", h)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDeterministicBuild(t *testing.T) {
	hashes := randomHashes(100, 12)
	key := []byte("deterministic-key")
	firstStore, first := mustBuild(t, hashes, BuildOptions{SigningKey: key})
	secondStore, second := mustBuild(t, hashes, BuildOptions{SigningKey: key})

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("manifests differ across builds:\n%+v\n%+v", first, second)
	}
	for i, meta := range first.Segments {
		a, err := firstStore.Segment(meta.Prefix)
		if err != nil {
			t.Fatalf("first Segment(%04x): %v", meta.Prefix, err)
		}
		b, err := secondStore.Segment(second.Segments[i].Prefix)
		if err != nil {
			t.Fatalf("second Segment(%04x): %v", second.Segments[i].Prefix, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("segment %04x differs across builds", meta.Prefix)
		}
	}
}

func TestBuildRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts BuildOptions
	}{
		{name: "negative prefix bytes", opts: BuildOptions{PrefixBytes: -1}},
		{name: "too many prefix bytes", opts: BuildOptions{PrefixBytes: 5}},
		{name: "negative fpr", opts: BuildOptions{FalsePositiveRate: -0.1}},
		{name: "fpr of one", opts: BuildOptions{FalsePositiveRate: 1}},
		{name: "nan fpr", opts: BuildOptions{FalsePositiveRate: math.NaN()}},
		{name: "inf fpr", opts: BuildOptions{FalsePositiveRate: math.Inf(1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.SegmentDir = t.TempDir()
			if _, _, err := Build(nil, tc.opts); !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Build error = %v, want ErrInvalidOption", err)
			}
		})
	}

	t.Run("non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Build(nil, BuildOptions{SegmentDir: dir}); !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("Build error = %v, want ErrInvalidOption", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
			t.Fatalf("Build touched a non-empty directory: %v", err)
		}
	})
}

func TestBuildWithTemporaryStoreDir(t *testing.T) {
	hashes := randomHashes(10, 14)
	store, manifest, err := Build(hashes, BuildOptions{})
	if err != nil {
		t.Fatalf("Build without SegmentDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(store.Dir()) })
	if store.Dir() == "" {
		t.Fatal("Build without SegmentDir returned an empty Dir")
	}
	if len(manifest.Segments) == 0 {
		t.Fatal("Build with hashes produced no segments")
	}
	reloaded, _ := mustLoad(t, store.Dir(), nil)
	if ok, _, err := reloaded.Contains(hashes[0]); err != nil || !ok {
		t.Fatalf("reloaded Contains = %v, %v", ok, err)
	}
}

func TestManifestAccessorReturnsCopy(t *testing.T) {
	store, manifest := mustBuild(t, randomHashes(10, 13), BuildOptions{})
	copy := store.Manifest()
	if copy == nil {
		t.Fatal("Manifest() returned nil")
	}
	copy.Segments[0].Count++
	copy.Segments[0].Root[0] ^= 0xff

	if reflect.DeepEqual(copy.Segments, manifest.Segments) {
		t.Fatal("mutating the returned manifest did not change the copy")
	}
	meta, _ := store.SegmentMeta(manifest.Segments[0].Prefix)
	if meta.Count != manifest.Segments[0].Count || meta.Root != manifest.Segments[0].Root {
		t.Fatal("mutating Store.Manifest() changed Store metadata")
	}
}
