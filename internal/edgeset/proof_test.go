package edgeset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProofVerificationAndTampering(t *testing.T) {
	hashes := forcedPrefixHashes(64, 0x2222, 41)
	store, _ := mustBuild(t, hashes, BuildOptions{})
	h := hashes[37]
	root := segmentRoot(t, store, h)

	ok, proof, err := store.Contains(h)
	if err != nil || !ok || proof == nil {
		t.Fatalf("Contains = %v, %v, %v", ok, proof, err)
	}
	if !VerifyProof(root, h, proof) {
		t.Fatal("original proof does not verify")
	}
	if proof.Index < 0 || proof.Index >= len(hashes) {
		t.Fatalf("proof index = %d, want in [0,%d)", proof.Index, len(hashes))
	}
	if len(proof.Path) != 6 || len(proof.Right) != 6 {
		t.Fatalf("proof depth = %d/%d, want 6/6", len(proof.Path), len(proof.Right))
	}

	otherHash := HashOf([]byte("some other hash"))
	wrongRoot := root
	wrongRoot[0] ^= 1
	flippedPath := copyProof(proof)
	flippedPath.Path[0][0] ^= 1
	flippedRight := copyProof(proof)
	flippedRight.Right[0] = !flippedRight.Right[0]
	swappedPath := copyProof(proof)
	swappedPath.Path[0], swappedPath.Path[1] = swappedPath.Path[1], swappedPath.Path[0]
	changedIndex := copyProof(proof)
	changedIndex.Index++
	negativeIndex := copyProof(proof)
	negativeIndex.Index = -1
	changedHash := copyProof(proof)
	changedHash.Hash = otherHash
	shortPath := copyProof(proof)
	shortPath.Path = shortPath.Path[:len(shortPath.Path)-1]
	longPath := copyProof(proof)
	longPath.Path = append(longPath.Path, proof.Path[0])
	longRight := copyProof(proof)
	longRight.Right = append(longRight.Right, false)
	truncatedBoth := copyProof(proof)
	truncatedBoth.Path = truncatedBoth.Path[:len(truncatedBoth.Path)-1]
	truncatedBoth.Right = truncatedBoth.Right[:len(truncatedBoth.Right)-1]

	tests := []struct {
		name string
		root Hash
		h    Hash
		p    *Proof
	}{
		{name: "nil proof", root: root, h: h, p: nil},
		{name: "wrong root", root: wrongRoot, h: h, p: proof},
		{name: "wrong requested hash", root: root, h: otherHash, p: proof},
		{name: "changed proof hash", root: root, h: h, p: changedHash},
		{name: "flipped path hash", root: root, h: h, p: flippedPath},
		{name: "flipped right flag", root: root, h: h, p: flippedRight},
		{name: "reordered path", root: root, h: h, p: swappedPath},
		{name: "changed index", root: root, h: h, p: changedIndex},
		{name: "negative index", root: root, h: h, p: negativeIndex},
		{name: "short path", root: root, h: h, p: shortPath},
		{name: "long path", root: root, h: h, p: longPath},
		{name: "long right", root: root, h: h, p: longRight},
		{name: "truncated both", root: root, h: h, p: truncatedBoth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyProof(tc.root, tc.h, tc.p) {
				t.Fatalf("VerifyProof accepted %s", tc.name)
			}
		})
	}

	if !VerifyProof(root, h, proof) {
		t.Fatal("original proof stopped verifying after tamper checks")
	}
}

func TestProofsAcrossTreeSizes(t *testing.T) {
	for n := 1; n <= 40; n++ {
		n := n
		t.Run("size", func(t *testing.T) {
			hashes := forcedPrefixHashes(n, 0x1010, int64(100+n))
			store, _ := mustBuild(t, hashes, BuildOptions{})
			root := segmentRoot(t, store, hashes[0])
			for _, h := range hashes {
				ok, proof, err := store.Contains(h)
				if err != nil || !ok || proof == nil {
					t.Fatalf("Contains(%s) = %v, %v, %v", h, ok, proof, err)
				}
				if !VerifyProof(root, h, proof) {
					t.Fatalf("proof for n=%d, hash %s does not verify", n, h)
				}
			}
		})
	}
}

func TestTamperedSegmentRejected(t *testing.T) {
	hashes := randomHashes(100, 42)
	key := []byte("segment-integrity-key")
	dir := t.TempDir()
	store, manifest := mustBuild(t, hashes, BuildOptions{SegmentDir: dir, SigningKey: key})
	if len(manifest.Segments) == 0 {
		t.Fatal("build produced no segments")
	}

	meta := manifest.Segments[0]
	var target Hash
	found := false
	for _, h := range hashes {
		if store.PrefixFor(h) == meta.Prefix {
			target = h
			found = true
			break
		}
	}
	if !found {
		t.Fatal("could not find a hash in the first segment")
	}

	path := filepath.Join(dir, filepath.FromSlash(meta.File))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, key); err != nil {
		t.Fatalf("baseline LoadStore: %v", err)
	}

	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0x01
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, key); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("LoadStore with tampered segment error = %v, want ErrCorruptSegment", err)
	}

	// Restore and load, then edit the file after LoadStore. The post-load edit
	// must be caught when Contains fills the cache.
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, _ := mustLoad(t, dir, key)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loaded.Contains(target); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("Contains after post-load tamper error = %v, want ErrCorruptSegment", err)
	}
	if _, err := loaded.Segment(meta.Prefix); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("Segment after post-load tamper error = %v, want ErrCorruptSegment", err)
	}

	// Failed verifications are never cached, so restoring the file makes the
	// still-running store usable again.
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, proof, err := loaded.Contains(target); err != nil || !ok || !VerifyProof(meta.Root, target, proof) {
		t.Fatalf("Contains after restore = %v, %v, %v", ok, proof, err)
	}

	// Truncation is rejected as a size mismatch.
	if err := os.WriteFile(path, original[:len(original)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, key); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("LoadStore with truncated segment error = %v, want ErrCorruptSegment", err)
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
}
