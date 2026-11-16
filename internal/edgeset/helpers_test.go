package edgeset

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

func randomHashes(n int, seed int64) []Hash {
	rng := rand.New(rand.NewSource(seed))
	out := make([]Hash, n)
	for i := range out {
		out[i] = randomHash(rng)
	}
	return out
}

func randomHash(rng *rand.Rand) Hash {
	var h Hash
	for off := 0; off < len(h); off += 8 {
		binary.BigEndian.PutUint64(h[off:off+8], rng.Uint64())
	}
	return h
}

// forcedPrefixHashes returns n unique hashes that all share the given segment
// prefix. The remaining bytes stay random so lookups stay representative.
func forcedPrefixHashes(n int, prefix uint16, seed int64) []Hash {
	rng := rand.New(rand.NewSource(seed))
	out := make([]Hash, n)
	for i := range out {
		out[i] = randomHash(rng)
		binary.BigEndian.PutUint16(out[i][0:2], prefix)
		// Make the tail unique even if math/rand collides.
		binary.BigEndian.PutUint32(out[i][28:32], uint32(i))
	}
	return out
}

func mustBuild(t *testing.T, hashes []Hash, opts BuildOptions) (*Store, *Manifest) {
	t.Helper()
	if opts.SegmentDir == "" {
		opts.SegmentDir = t.TempDir()
	}
	store, manifest, err := Build(hashes, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if store == nil || manifest == nil {
		t.Fatal("Build returned a nil store or manifest")
	}
	return store, manifest
}

func mustLoad(t *testing.T, dir string, key []byte) (*Store, *Manifest) {
	t.Helper()
	store, manifest, err := LoadStore(dir, key)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store, manifest
}

func segmentRoot(t *testing.T, store *Store, h Hash) Hash {
	t.Helper()
	meta, ok := store.SegmentMeta(store.PrefixFor(h))
	if !ok {
		t.Fatalf("no segment metadata for prefix %04x", store.PrefixFor(h))
	}
	return meta.Root
}

func copyProof(p *Proof) *Proof {
	if p == nil {
		return nil
	}
	out := &Proof{Hash: p.Hash, Index: p.Index}
	out.Path = append([]Hash(nil), p.Path...)
	out.Right = append([]bool(nil), p.Right...)
	return out
}
