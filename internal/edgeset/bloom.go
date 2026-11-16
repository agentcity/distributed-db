package edgeset

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	// bloomHashDomain separates Bloom positions from every other hash use.
	bloomHashDomain = "edgeset:bloom:v1\x00"

	// Bounds keep a malformed option or manifest from causing enormous
	// allocations or an unbounded probe count.
	maxBloomBits = 1 << 36
	maxBloomK    = 64
)

// bloomFilter is an immutable, non-counting Bloom filter. It is built once per
// segment and only read afterwards, so it needs no synchronization.
type bloomFilter struct {
	bits []byte
	m    uint64
	k    int
}

// bloomParams derives the bit count and probe count for n items at false
// positive rate p: m = -n*ln(p)/(ln 2)^2, k = (m/n)*ln 2.
func bloomParams(n int, p float64) (bits uint64, k int, err error) {
	if n < 0 {
		return 0, 0, fmt.Errorf("%w: negative Bloom item count %d", ErrInvalidOption, n)
	}
	if n == 0 {
		return 0, 0, nil
	}
	if math.IsNaN(p) || math.IsInf(p, 0) || p <= 0 || p >= 1 {
		return 0, 0, fmt.Errorf("%w: false positive rate %v must be in (0,1)", ErrInvalidOption, p)
	}

	m := math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2))
	if m < 1 {
		m = 1
	}
	if m > float64(maxBloomBits) {
		return 0, 0, fmt.Errorf("%w: Bloom filter needs %v bits (max %d)", ErrInvalidOption, m, maxBloomBits)
	}

	probes := int(math.Round(m / float64(n) * math.Ln2))
	if probes < 1 {
		probes = 1
	}
	if probes > maxBloomK {
		probes = maxBloomK
	}
	return uint64(m), probes, nil
}

func newBloomFilter(n int, p float64) (*bloomFilter, error) {
	bits, k, err := bloomParams(n, p)
	if err != nil {
		return nil, err
	}
	return &bloomFilter{
		bits: make([]byte, (bits+7)/8),
		m:    bits,
		k:    k,
	}, nil
}

// bloomHashes derives two independent-looking 64-bit values from SHA-256 of a
// domain-separated hash. h2 is forced odd so the double-hashing sequence has a
// non-trivial step.
func bloomHashes(h Hash) (h1, h2 uint64) {
	var buf [len(bloomHashDomain) + len(h)]byte
	copy(buf[:], bloomHashDomain)
	copy(buf[len(bloomHashDomain):], h[:])
	sum := sha256.Sum256(buf[:])
	return binary.BigEndian.Uint64(sum[0:8]), binary.BigEndian.Uint64(sum[8:16]) | 1
}

func (f *bloomFilter) add(h Hash) {
	if f == nil || f.m == 0 || f.k == 0 {
		return
	}
	x, step := bloomHashes(h)
	for i := 0; i < f.k; i++ {
		pos := x % f.m
		f.bits[pos>>3] |= 1 << (pos & 7)
		x += step
	}
}

func (f *bloomFilter) maybe(h Hash) bool {
	if f == nil || f.m == 0 || f.k == 0 {
		return false
	}
	x, step := bloomHashes(h)
	for i := 0; i < f.k; i++ {
		pos := x % f.m
		if f.bits[pos>>3]&(1<<(pos&7)) == 0 {
			return false
		}
		x += step
	}
	return true
}
