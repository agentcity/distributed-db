package edgeset

import (
	"errors"
	"fmt"
	"sort"
)

// AbsenceProof is a Merkle proof that a hash is not in one segment.
//
// Prefix and Root identify the segment the proof is about. Root must match the
// segment root from the authenticated manifest; VerifyAbsence also accepts a
// zero Root for hand-built proofs that rely on its explicit root argument.
// Left and Right are inclusion proofs for the sorted neighbours of the absent
// hash:
//
//   - h sorts before the first hash: Left is nil, Right proves index 0 and
//     h < Right.Hash.
//   - h sorts after the last hash: Right is nil and Left proves the last leaf;
//     h > Left.Hash. The verifier additionally checks that every right-hand
//     sibling on the path is an all-empty subtree, so a non-last leaf cannot be
//     used for this boundary case.
//   - h sorts between two hashes: both proofs are present for adjacent indexes
//     i-1 and i, and Left.Hash < h < Right.Hash.
//
// An absence proof is only as fresh as the manifest it was built from. A proof
// against an old manifest says nothing about a newer store that may contain h;
// verifiers must bind the manifest version/root they checked to the answer they
// cache or forward.
type AbsenceProof struct {
	Prefix uint16
	Root   Hash
	Left   *Proof
	Right  *Proof
}

// ProveAbsent returns a non-membership proof for h in the segment selected by
// h's prefix.
//
// Contains remains the exact membership API; ProveAbsent returns ErrPresent if
// h is in the set. A hash whose prefix has no segment returns
// ErrSegmentNotFound: the manifest is itself evidence that no segment covers
// it, but no Merkle proof can be built for a segment that does not exist.
//
// The proof is only as fresh as the Store's manifest. A newer manifest may
// contain h even though this proof verifies against the current root.
func (s *Store) ProveAbsent(h Hash) (*AbsenceProof, error) {
	if s == nil {
		return nil, errors.New("edgeset: nil store")
	}
	prefix := s.PrefixFor(h)
	meta, ok := s.metas[prefix]
	if !ok {
		return nil, fmt.Errorf("%w: %04x", ErrSegmentNotFound, prefix)
	}
	seg, err := s.loadSegment(prefix)
	if err != nil {
		return nil, err
	}

	index := sort.Search(len(seg.hashes), func(i int) bool {
		return !hashLess(seg.hashes[i], h)
	})
	if index < len(seg.hashes) && seg.hashes[index] == h {
		return nil, fmt.Errorf("%w: %s", ErrPresent, h)
	}

	proof := &AbsenceProof{Prefix: prefix, Root: meta.Root}
	switch {
	case index == 0:
		proof.Right = proofFor(seg.hashes, seg.levels, 0)
	case index == len(seg.hashes):
		proof.Left = proofFor(seg.hashes, seg.levels, len(seg.hashes)-1)
	default:
		proof.Left = proofFor(seg.hashes, seg.levels, index-1)
		proof.Right = proofFor(seg.hashes, seg.levels, index)
	}
	if proof.Left == nil && proof.Right == nil {
		return nil, fmt.Errorf("%w: prefix %04x: cannot build absence proof", ErrCorruptSegment, prefix)
	}
	return proof, nil
}

// VerifyAbsence reports whether p proves that h is absent from the segment
// whose Merkle root is root.
//
// Both neighbour proofs must verify against root at their claimed indexes.
// When both are present their indexes must be adjacent and the hash must sort
// strictly between them. Boundary proofs use index 0 for "before first" and a
// last-leaf proof for "after last". A proof that claims a different non-zero
// root, or whose non-zero prefix does not route h to the segment, is rejected.
//
// root must be the root of the segment selected by h's prefix, taken from an
// authenticated manifest. Absence is only as fresh as that manifest.
func VerifyAbsence(root Hash, h Hash, p *AbsenceProof) bool {
	if p == nil {
		return false
	}
	// Root and Prefix are produced by ProveAbsent and are checked when set. A
	// zero Root (a hand-built proof that relies on the root argument) is
	// accepted; an explicitly different root is not.
	if p.Root != (Hash{}) && p.Root != root {
		return false
	}
	if p.Prefix != 0 && !absencePrefixMatches(p.Prefix, h) {
		return false
	}

	switch {
	case p.Left == nil && p.Right == nil:
		return false

	case p.Left == nil:
		right := p.Right
		if right == nil || !VerifyProof(root, right.Hash, right) || right.Index != 0 {
			return false
		}
		return hashLess(h, right.Hash)

	case p.Right == nil:
		left := p.Left
		if left == nil || !VerifyProof(root, left.Hash, left) {
			return false
		}
		if !proofShowsLastLeaf(left) {
			return false
		}
		return hashLess(left.Hash, h)

	default:
		left, right := p.Left, p.Right
		if !VerifyProof(root, left.Hash, left) || !VerifyProof(root, right.Hash, right) {
			return false
		}
		if left.Index < 0 || right.Index != left.Index+1 {
			return false
		}
		return hashLess(left.Hash, h) && hashLess(h, right.Hash)
	}
}

// absencePrefixMatches checks that prefix could have routed h. Segment ids use
// either the first byte (PrefixBytes == 1) or the first two bytes; the proof
// itself does not carry PrefixBytes, so both canonical forms are accepted.
func absencePrefixMatches(prefix uint16, h Hash) bool {
	if prefix == uint16(h[0]) {
		return true
	}
	return prefix == uint16(h[0])<<8|uint16(h[1])
}
