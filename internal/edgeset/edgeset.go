// Package edgeset implements the immutable hash set used by edge caches.
//
// An authoritative server builds a Store from a collection of 32-byte hashes.
// The set is split into deterministic shards ("segments") by the high bytes of
// each hash. Every segment contains a Bloom filter, a sorted hash list and a
// Merkle tree over that list. A manifest lists the segment metadata and is
// optionally authenticated with HMAC-SHA256.
//
// Stores are immutable: Build writes the complete directory and never mutates
// it afterwards. A LoadStore/Contains caller may share a Store between
// goroutines. Segment files are verified against the manifest checksum both at
// load time and when they are first cached in memory.
//
// The package deliberately does not implement networking, cache eviction or
// deletion. Those belong to the edge server/cache layers built on top.
package edgeset

const (
	defaultPrefixBytes       = 2
	minPrefixBytes           = 1
	maxPrefixBytes           = 4
	defaultFalsePositiveRate = 0.01
)

// Hash is a SHA-256 digest. Input hashes are assumed to already be SHA-256
// values; the package never re-hashes them for sharding or ordering.
type Hash [32]byte

// BuildOptions controls Build.
type BuildOptions struct {
	// PrefixBytes is the number of high hash bytes used to address a segment.
	// Zero selects the default of 2. Values 1..4 are accepted. Because
	// SegmentMeta.Prefix is a uint16 (the on-disk segment id), values 3 and 4
	// are accepted for forward compatibility but segment identity is capped at
	// the first two bytes: one byte yields 256 segments and two or more yield
	// 65536 segments.
	PrefixBytes int

	// FalsePositiveRate is the Bloom filter target. Zero selects 0.01.
	FalsePositiveRate float64

	// SigningKey is an optional HMAC-SHA256 key for the manifest. When empty,
	// the manifest is stored unsigned.
	SigningKey []byte

	// SegmentDir is the output store directory. It must not exist (or must be
	// an empty directory). When empty, Build creates a temporary directory;
	// Store.Dir reports it.
	SegmentDir string
}

// SegmentMeta describes one immutable segment file.
type SegmentMeta struct {
	Prefix    uint16 `json:"prefix"`
	Count     int    `json:"count"`
	BloomBits int    `json:"bloom_bits"`
	BloomK    int    `json:"bloom_k"`
	Root      Hash   `json:"root"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	SHA256    Hash   `json:"sha256"`
}

// Manifest is the canonical description of a Store.
//
// Version and PrefixBytes describe the store; Segments is sorted by Prefix.
// Signature is the HMAC-SHA256 of the canonical JSON encoding of every other
// field, or nil for an unsigned store.
type Manifest struct {
	Version     uint64        `json:"version"`
	PrefixBytes int           `json:"prefix_bytes"`
	Segments    []SegmentMeta `json:"segments"`
	Signature   []byte        `json:"signature,omitempty"`
}

// Proof is a Merkle inclusion proof for one hash in one segment.
//
// Path contains the sibling hashes from the leaf level upwards. Right[i]
// reports that Path[i] is the right-hand sibling at that level. Index is the
// position of the hash in the segment's sorted hash list and therefore also
// fixes the left/right pattern (bit i of Index is 0 for a left child).
type Proof struct {
	Hash  Hash
	Index int
	Path  []Hash
	Right []bool
}
