// Package edgeset implements the immutable hash set used by edge caches.
//
// An authoritative server builds a Store from a collection of 32-byte hashes.
// The set is split into deterministic shards ("segments") by the high bytes of
// each hash. Every segment contains a Bloom filter, a sorted hash list and a
// Merkle tree over that list. A manifest lists the segment metadata and is
// optionally authenticated with HMAC-SHA256 (legacy) or Ed25519.
//
// Stores are immutable: Build writes the complete directory and never mutates
// it afterwards. A LoadStore/Contains caller may share a Store between
// goroutines. Segment files are verified against the manifest checksum both at
// load time and when they are first cached in memory.
//
// Segment files are written in format v2, which adds optional zstd compression
// of the Bloom+hash payload. The decoder also accepts the legacy uncompressed
// format v1, so stores built by older versions remain readable.
//
// Contains gives exact membership. ProveAbsent additionally returns a Merkle
// non-membership proof for a hash that is absent from the segment selected by
// its prefix. An absence proof is only as fresh as the manifest it was built
// from: a later manifest may contain the hash.
//
// The package deliberately does not implement networking, cache eviction or
// deletion. Those belong to the edge server/cache layers built on top.
package edgeset

import "crypto/ed25519"

const (
	defaultPrefixBytes       = 2
	minPrefixBytes           = 1
	maxPrefixBytes           = 4
	defaultFalsePositiveRate = 0.01

	// defaultCompressionThreshold is the uncompressed Bloom+hash payload size
	// at or above which BuildOptions.Compression == "" selects zstd. Smaller
	// segments are stored uncompressed because the zstd frame overhead would
	// dominate them.
	defaultCompressionThreshold = 4 << 10
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
	// the manifest is not HMAC-signed. It is mutually exclusive with
	// Ed25519PrivateKey.
	SigningKey []byte

	// Ed25519PrivateKey is an optional Ed25519 private key for the manifest.
	// When set, the manifest is signed with Ed25519 and SignatureAlg is
	// "ed25519". It is mutually exclusive with SigningKey; setting both makes
	// Build return ErrInvalidOption. A non-nil key must be a
	// crypto/ed25519.PrivateKey (64 bytes).
	Ed25519PrivateKey ed25519.PrivateKey

	// Compression selects the segment payload encoding: "none", "zstd", or ""
	// for automatic selection. Auto uses zstd for segments whose uncompressed
	// Bloom+hash payload is at least 4 KiB and "none" below. Unknown values
	// make Build return ErrInvalidOption.
	Compression string

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

	// Compression is "none" or "zstd" for format v2 segment files. It is
	// empty for legacy format v1 segments, which are always uncompressed.
	// Size and SHA256 always cover the stored (possibly compressed) file.
	Compression string `json:"compression,omitempty"`
}

// Manifest is the canonical description of a Store.
//
// Version and PrefixBytes describe the store; Segments is sorted by Prefix.
// Signature is the HMAC-SHA256 or Ed25519 signature of the canonical JSON
// encoding of every other field, or nil for an unsigned store. SignatureAlg is
// empty (legacy HMAC-SHA256) or "hmac-sha256" for HMAC-signed manifests, and
// "ed25519" for Ed25519-signed manifests.
type Manifest struct {
	Version     uint64        `json:"version"`
	PrefixBytes int           `json:"prefix_bytes"`
	Segments    []SegmentMeta `json:"segments"`
	Signature   []byte        `json:"signature,omitempty"`

	// SignatureAlg identifies the signature algorithm. It is omitted from
	// JSON for legacy HMAC-SHA256 manifests, preserving their canonical
	// encoding and signatures.
	SignatureAlg string `json:"signature_alg,omitempty"`
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
