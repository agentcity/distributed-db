package edgeset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// HashOf returns the SHA-256 digest of data. It is a convenience for callers
// that start from a key or URL rather than a precomputed hash.
func HashOf(data []byte) Hash {
	return sha256.Sum256(data)
}

// String returns the lowercase hexadecimal form of h.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// MarshalText implements encoding.TextMarshaler so hashes are rendered as hex
// strings in the manifest JSON instead of arrays of numbers.
func (h Hash) MarshalText() ([]byte, error) {
	dst := make([]byte, hex.EncodedLen(len(h)))
	hex.Encode(dst, h[:])
	return dst, nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	if h == nil {
		return fmt.Errorf("edgeset: cannot unmarshal hash into nil receiver")
	}
	parsed, err := ParseHash(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// ParseHash parses exactly 64 hexadecimal characters into a Hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != hex.EncodedLen(len(h)) {
		return h, fmt.Errorf("edgeset: hash must be %d hex characters, got %d", hex.EncodedLen(len(h)), len(s))
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return Hash{}, fmt.Errorf("edgeset: invalid hash: %w", err)
	}
	return h, nil
}

// hashLess orders hashes lexicographically by their bytes, which is the same
// as big-endian unsigned comparison.
func hashLess(a, b Hash) bool {
	return bytes.Compare(a[:], b[:]) < 0
}
