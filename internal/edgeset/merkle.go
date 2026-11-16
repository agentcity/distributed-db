package edgeset

import "crypto/sha256"

const (
	leafDomain         = "edgeset:merkle-leaf:v1\x00"
	nodeDomain         = "edgeset:merkle-node:v1\x00"
	emptyLeafDomain    = "edgeset:merkle-empty-leaf:v1"
	maxMerkleTreeDepth = 62
)

// emptyLeaf pads the last level of a segment Merkle tree up to a power of two.
// Using a domain distinct from leafDomain means a caller can never prove that
// an ordinary hash occupies a padding slot.
var emptyLeaf = func() Hash {
	sum := sha256.Sum256([]byte(emptyLeafDomain))
	return sum
}()

// leafHash domain-separates Merkle leaves from internal nodes. Without the
// prefixes a 64-byte node could be interpreted as two concatenated hashes.
func leafHash(h Hash) Hash {
	var buf [len(leafDomain) + len(h)]byte
	copy(buf[:], leafDomain)
	copy(buf[len(leafDomain):], h[:])
	return sha256.Sum256(buf[:])
}

func nodeHash(left, right Hash) Hash {
	var buf [len(nodeDomain) + 2*len(left)]byte
	copy(buf[:], nodeDomain)
	copy(buf[len(nodeDomain):], left[:])
	copy(buf[len(nodeDomain)+len(left):], right[:])
	return sha256.Sum256(buf[:])
}

// merkleLevels builds a complete power-of-two tree over sorted hashes. Missing
// trailing leaves are filled with emptyLeaf. levels[0] is the leaf level and
// levels[len-1][0] is the root. For an empty hash list it returns nil.
func merkleLevels(hashes []Hash) [][]Hash {
	if len(hashes) == 0 {
		return nil
	}

	size := 1
	for size < len(hashes) {
		if size > 1<<maxMerkleTreeDepth {
			// Unreachable for real slices; keeps the shift below safe.
			return nil
		}
		size <<= 1
	}

	leaves := make([]Hash, size)
	for i, h := range hashes {
		leaves[i] = leafHash(h)
	}
	for i := len(hashes); i < size; i++ {
		leaves[i] = emptyLeaf
	}

	levels := make([][]Hash, 0, 1+maxMerkleTreeDepth)
	levels = append(levels, leaves)
	for len(leaves) > 1 {
		next := make([]Hash, len(leaves)/2)
		for i := range next {
			next[i] = nodeHash(leaves[2*i], leaves[2*i+1])
		}
		levels = append(levels, next)
		leaves = next
	}
	return levels
}

// merkleRoot returns the root of the segment tree, or emptyLeaf for no hashes.
func merkleRoot(hashes []Hash) Hash {
	levels := merkleLevels(hashes)
	if len(levels) == 0 {
		return emptyLeaf
	}
	return levels[len(levels)-1][0]
}

// proofFor builds the index-th inclusion proof from precomputed levels.
func proofFor(hashes []Hash, levels [][]Hash, index int) *Proof {
	if index < 0 || index >= len(hashes) || len(levels) == 0 {
		return nil
	}
	depth := len(levels) - 1
	p := &Proof{
		Hash:  hashes[index],
		Index: index,
		Path:  make([]Hash, 0, depth),
		Right: make([]bool, 0, depth),
	}
	idx := index
	for level := 0; level < depth; level++ {
		sibling := idx ^ 1
		p.Path = append(p.Path, levels[level][sibling])
		p.Right = append(p.Right, idx&1 == 0)
		idx >>= 1
	}
	return p
}

// VerifyProof reports whether p proves that h is a leaf of the Merkle tree with
// the given root. Path and Right must describe one sibling per level, and Index
// must match the left/right pattern encoded in Right. The latter check makes
// index tampering detectable even though verification itself only needs Path.
func VerifyProof(root Hash, h Hash, p *Proof) bool {
	if p == nil || p.Hash != h {
		return false
	}
	depth := len(p.Path)
	if depth != len(p.Right) || depth > maxMerkleTreeDepth {
		return false
	}
	if p.Index < 0 || uint64(p.Index) >= uint64(1)<<uint(depth) {
		return false
	}
	for i := 0; i < depth; i++ {
		leftChild := (p.Index>>uint(i))&1 == 0
		if p.Right[i] != leftChild {
			return false
		}
	}

	node := leafHash(h)
	for i, sibling := range p.Path {
		if p.Right[i] {
			node = nodeHash(node, sibling)
		} else {
			node = nodeHash(sibling, node)
		}
	}
	return node == root
}
