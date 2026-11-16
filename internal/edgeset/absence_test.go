package edgeset

import (
	"errors"
	"testing"
)

func absenceTestHashes(prefix uint16) []Hash {
	tails := []byte{10, 20, 30, 40, 60, 70, 80, 90}
	out := make([]Hash, len(tails))
	for i, tail := range tails {
		out[i] = prefixedHash(prefix, tail)
	}
	return out
}

func TestAbsenceProofBoundaries(t *testing.T) {
	const prefix = uint16(0x3333)
	hashes := absenceTestHashes(prefix)
	store, manifest := mustBuild(t, hashes, BuildOptions{})
	if len(manifest.Segments) != 1 {
		t.Fatalf("test needs one segment, got %d", len(manifest.Segments))
	}
	root := manifest.Segments[0].Root
	if root != segmentRoot(t, store, hashes[0]) {
		t.Fatal("segment root lookup mismatch")
	}

	type testCase struct {
		name       string
		h          Hash
		leftIndex  int
		rightIndex int
		hasLeft    bool
		hasRight   bool
	}
	tests := []testCase{
		{name: "before first", h: prefixedHash(prefix, 5), rightIndex: 0, hasRight: true},
		{name: "between", h: prefixedHash(prefix, 50), leftIndex: 3, rightIndex: 4, hasLeft: true, hasRight: true},
		{name: "after last", h: prefixedHash(prefix, 99), leftIndex: len(hashes) - 1, hasLeft: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := store.ProveAbsent(tc.h)
			if err != nil {
				t.Fatalf("ProveAbsent: %v", err)
			}
			if proof == nil {
				t.Fatal("ProveAbsent returned nil proof")
			}
			if proof.Prefix != prefix || proof.Root != root {
				t.Fatalf("proof header = %04x/%s, want %04x/%s", proof.Prefix, proof.Root, prefix, root)
			}
			if (proof.Left != nil) != tc.hasLeft || (proof.Right != nil) != tc.hasRight {
				t.Fatalf("proof sides = left %v right %v, want %v/%v", proof.Left != nil, proof.Right != nil, tc.hasLeft, tc.hasRight)
			}
			if proof.Left != nil && proof.Left.Index != tc.leftIndex {
				t.Fatalf("left index = %d, want %d", proof.Left.Index, tc.leftIndex)
			}
			if proof.Right != nil && proof.Right.Index != tc.rightIndex {
				t.Fatalf("right index = %d, want %d", proof.Right.Index, tc.rightIndex)
			}
			if !VerifyAbsence(root, tc.h, proof) {
				t.Fatal("VerifyAbsence rejected a valid proof")
			}
			// Contains must still answer exact membership with no proof for
			// an absent hash.
			ok, inclusion, err := store.Contains(tc.h)
			if err != nil {
				t.Fatalf("Contains: %v", err)
			}
			if ok || inclusion != nil {
				t.Fatalf("Contains(absent) = %v, %v", ok, inclusion)
			}
			// Copying the proof and verifying the copy must not depend on
			// internal aliasing.
			if !VerifyAbsence(root, tc.h, copyAbsenceProof(proof)) {
				t.Fatal("VerifyAbsence rejected a copied valid proof")
			}
		})
	}

	for _, h := range hashes {
		if _, err := store.ProveAbsent(h); !errors.Is(err, ErrPresent) {
			t.Fatalf("ProveAbsent(present %s) error = %v, want ErrPresent", h, err)
		}
		if ok, _, err := store.Contains(h); err != nil || !ok {
			t.Fatalf("Contains(present %s) = %v, %v", h, ok, err)
		}
	}
}

func TestAbsenceProofSingleLeaf(t *testing.T) {
	const prefix = uint16(0x5151)
	present := prefixedHash(prefix, 10)
	store, manifest := mustBuild(t, []Hash{present}, BuildOptions{})
	root := manifest.Segments[0].Root

	before := prefixedHash(prefix, 5)
	proof, err := store.ProveAbsent(before)
	if err != nil {
		t.Fatalf("ProveAbsent(before): %v", err)
	}
	if proof.Left != nil || proof.Right == nil || proof.Right.Index != 0 {
		t.Fatalf("before proof = %+v", proof)
	}
	if !VerifyAbsence(root, before, proof) {
		t.Fatal("before-first proof does not verify for a single leaf")
	}

	after := prefixedHash(prefix, 20)
	proof, err = store.ProveAbsent(after)
	if err != nil {
		t.Fatalf("ProveAbsent(after): %v", err)
	}
	if proof.Right != nil || proof.Left == nil || proof.Left.Index != 0 {
		t.Fatalf("after proof = %+v", proof)
	}
	if !VerifyAbsence(root, after, proof) {
		t.Fatal("after-last proof does not verify for a single leaf")
	}

	if _, err := store.ProveAbsent(present); !errors.Is(err, ErrPresent) {
		t.Fatalf("present hash error = %v, want ErrPresent", err)
	}
}

func TestAbsenceProofTampering(t *testing.T) {
	const prefix = uint16(0x3333)
	hashes := absenceTestHashes(prefix)
	store, manifest := mustBuild(t, hashes, BuildOptions{})
	root := manifest.Segments[0].Root

	h := prefixedHash(prefix, 50)
	proof, err := store.ProveAbsent(h)
	if err != nil {
		t.Fatalf("ProveAbsent: %v", err)
	}
	if proof.Left == nil || proof.Right == nil {
		t.Fatal("test needs a between proof with two sides")
	}
	if !VerifyAbsence(root, h, proof) {
		t.Fatal("original proof does not verify")
	}

	seg, err := store.loadSegment(proof.Prefix)
	if err != nil {
		t.Fatalf("loadSegment: %v", err)
	}
	if seg.hashes[0] != hashes[0] {
		t.Fatal("decoded segment hashes differ from input")
	}

	nonAdjacent := &AbsenceProof{
		Prefix: prefix,
		Root:   root,
		Left:   proofFor(seg.hashes, seg.levels, 0),
		Right:  proofFor(seg.hashes, seg.levels, 2),
	}
	// This pair sorts around the hash but skips one leaf, which must be caught.
	hBetweenNonAdjacent := prefixedHash(prefix, 25)
	if !VerifyProof(root, nonAdjacent.Left.Hash, nonAdjacent.Left) ||
		!VerifyProof(root, nonAdjacent.Right.Hash, nonAdjacent.Right) {
		t.Fatal("test constructed invalid leaf proofs")
	}

	notLast := &AbsenceProof{
		Prefix: prefix,
		Root:   root,
		Left:   proofFor(seg.hashes, seg.levels, len(seg.hashes)-2),
	}
	afterAll := prefixedHash(prefix, 255)
	if !VerifyProof(root, notLast.Left.Hash, notLast.Left) {
		t.Fatal("test constructed an invalid non-last left proof")
	}

	notFirst := &AbsenceProof{
		Prefix: prefix,
		Root:   root,
		Right:  proofFor(seg.hashes, seg.levels, 1),
	}
	beforeAll := prefixedHash(prefix, 1)
	if !VerifyProof(root, notFirst.Right.Hash, notFirst.Right) {
		t.Fatal("test constructed an invalid non-first right proof")
	}

	tests := []struct {
		name string
		h    Hash
		p    *AbsenceProof
	}{
		{name: "nil proof", h: h, p: nil},
		{name: "hash outside interval", h: prefixedHash(prefix, 5), p: proof},
		{name: "both sides missing", h: h, p: &AbsenceProof{Prefix: prefix, Root: root}},
		{name: "wrong proof root", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Root[0] ^= 0xaa
			return p
		}()},
		{name: "wrong prefix", h: h, p: &AbsenceProof{Prefix: 0xdead, Root: root, Left: proof.Left, Right: proof.Right}},
		{name: "tampered left path", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Left.Path[0][0] ^= 1
			return p
		}()},
		{name: "tampered right path", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Right.Path[0][0] ^= 1
			return p
		}()},
		{name: "tampered left index", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Left.Index++
			return p
		}()},
		{name: "tampered right index", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Right.Index--
			return p
		}()},
		{name: "swapped sides", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Left, p.Right = p.Right, p.Left
			return p
		}()},
		{name: "duplicated side", h: h, p: &AbsenceProof{Prefix: prefix, Root: root, Left: proof.Left, Right: copyProof(proof.Left)}},
		{name: "left hash equals h", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Left.Hash = h
			return p
		}()},
		{name: "right hash equals h", h: h, p: func() *AbsenceProof {
			p := copyAbsenceProof(proof)
			p.Right.Hash = h
			return p
		}()},
		{name: "non-adjacent indexes", h: hBetweenNonAdjacent, p: nonAdjacent},
		{name: "after-last with non-last leaf", h: afterAll, p: notLast},
		{name: "before-first with non-first leaf", h: beforeAll, p: notFirst},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyAbsence(root, tc.h, tc.p) {
				t.Fatalf("VerifyAbsence accepted %s", tc.name)
			}
		})
	}

	if !VerifyAbsence(root, h, proof) {
		t.Fatal("original proof stopped verifying after tamper checks")
	}
	wrongRoot := root
	wrongRoot[0] ^= 1
	if VerifyAbsence(wrongRoot, h, proof) {
		t.Fatal("VerifyAbsence accepted a proof under a wrong root argument")
	}
}

func TestAbsenceEmptyStoreAndMissingPrefix(t *testing.T) {
	emptyStore, emptyManifest := mustBuild(t, nil, BuildOptions{})
	if len(emptyManifest.Segments) != 0 {
		t.Fatalf("empty store has %d segments", len(emptyManifest.Segments))
	}
	target := HashOf([]byte("absent"))
	if proof, err := emptyStore.ProveAbsent(target); !errors.Is(err, ErrSegmentNotFound) || proof != nil {
		t.Fatalf("ProveAbsent(empty store) = %v, %v, want ErrSegmentNotFound", proof, err)
	}
	if VerifyAbsence(Hash{}, target, nil) {
		t.Fatal("VerifyAbsence accepted a nil proof")
	}
	if VerifyAbsence(Hash{}, target, &AbsenceProof{Prefix: 0, Root: Hash{}}) {
		t.Fatal("VerifyAbsence accepted an empty proof")
	}

	const presentPrefix = uint16(0x1111)
	hashes := forcedPrefixHashes(10, presentPrefix, 81)
	store, _ := mustBuild(t, hashes, BuildOptions{})
	missing := prefixedHash(0x2222, 1)
	if proof, err := store.ProveAbsent(missing); !errors.Is(err, ErrSegmentNotFound) || proof != nil {
		t.Fatalf("ProveAbsent(missing prefix) = %v, %v, want ErrSegmentNotFound", proof, err)
	}
	if ok, _, err := store.Contains(missing); err != nil || ok {
		t.Fatalf("Contains(missing prefix) = %v, %v", ok, err)
	}

	// VerifyAbsence must not accept a proof for a different segment root.
	absent := prefixedHash(presentPrefix, 0xff)
	proof, err := store.ProveAbsent(absent)
	if err != nil {
		t.Fatalf("ProveAbsent in existing segment: %v", err)
	}
	meta, _ := store.SegmentMeta(presentPrefix)
	var wrongRoot Hash
	wrongRoot[0] ^= 0xff
	if VerifyAbsence(wrongRoot, absent, proof) {
		t.Fatal("VerifyAbsence accepted a proof under a wrong root")
	}
	if VerifyAbsence(meta.Root, missing, proof) {
		t.Fatal("VerifyAbsence accepted a proof for another prefix")
	}
}
