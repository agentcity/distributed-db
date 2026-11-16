package edgeset

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	segmentMagic       = "EDGSEG01"
	segmentFormat      = uint32(1)
	segmentHeaderSize  = 8 + 4 + 2 + 2 + 4 + 8 + 4 + 8 // 40 bytes
	segmentHashSize    = 32
	segmentReservedOff = 14
)

// On-disk layout (all integers big-endian):
//
//	offset  size  field
//	0       8     magic "EDGSEG01"
//	8       4     format version (1)
//	12      2     segment prefix
//	14      2     reserved (0)
//	16      4     hash count
//	20      8     Bloom bit count m
//	28      4     Bloom probe count k
//	32      8     Bloom bitset byte length
//	40      ...   Bloom bitset ((m+7)/8 bytes)
//	...     ...   sorted hashes, 32 bytes each
//
// The Merkle root is not duplicated in the file: it lives in the manifest and
// is recomputed from the hashes when a segment is decoded. The manifest's
// SHA256 covers the complete file, including the header.

type segment struct {
	meta   SegmentMeta
	hashes []Hash
	bloom  *bloomFilter
	levels [][]Hash
	data   []byte
}

func encodeSegment(prefix uint16, hashes []Hash, bloom *bloomFilter) []byte {
	bloomBytes := len(bloom.bits)
	data := make([]byte, segmentHeaderSize+bloomBytes+len(hashes)*segmentHashSize)

	copy(data[0:8], segmentMagic)
	binary.BigEndian.PutUint32(data[8:12], segmentFormat)
	binary.BigEndian.PutUint16(data[12:14], prefix)
	binary.BigEndian.PutUint16(data[segmentReservedOff:segmentReservedOff+2], 0)
	binary.BigEndian.PutUint32(data[16:20], uint32(len(hashes)))
	binary.BigEndian.PutUint64(data[20:28], bloom.m)
	binary.BigEndian.PutUint32(data[28:32], uint32(bloom.k))
	binary.BigEndian.PutUint64(data[32:40], uint64(bloomBytes))
	copy(data[40:40+bloomBytes], bloom.bits)

	offset := segmentHeaderSize + bloomBytes
	for _, h := range hashes {
		copy(data[offset:offset+segmentHashSize], h[:])
		offset += segmentHashSize
	}
	return data
}

// verifySegmentBytes checks the manifest size and SHA-256 before parsing.
// Checksum verification is deliberately separate from parsing so a tampered
// file is rejected even if the malformed bytes happen to form a valid header.
func verifySegmentBytes(data []byte, meta SegmentMeta) error {
	if int64(len(data)) != meta.Size {
		return fmt.Errorf("%w: prefix %04x size %d, manifest says %d", ErrCorruptSegment, meta.Prefix, len(data), meta.Size)
	}
	sum := sha256.Sum256(data)
	if sum != meta.SHA256 {
		return fmt.Errorf("%w: prefix %04x SHA-256 mismatch", ErrCorruptSegment, meta.Prefix)
	}
	return nil
}

// decodeSegment parses and fully validates a segment against its manifest
// metadata: header fields, Bloom parameters, exact length, strictly increasing
// hashes, routing prefix and Merkle root.
func decodeSegment(data []byte, meta SegmentMeta, prefixBytes int) (*segment, error) {
	if len(data) < segmentHeaderSize {
		return nil, fmt.Errorf("%w: prefix %04x: file too short (%d bytes)", ErrCorruptSegment, meta.Prefix, len(data))
	}
	if string(data[0:8]) != segmentMagic {
		return nil, fmt.Errorf("%w: prefix %04x: bad magic", ErrCorruptSegment, meta.Prefix)
	}
	if got := binary.BigEndian.Uint32(data[8:12]); got != segmentFormat {
		return nil, fmt.Errorf("%w: prefix %04x: unsupported format %d", ErrCorruptSegment, meta.Prefix, got)
	}
	if got := binary.BigEndian.Uint16(data[12:14]); got != meta.Prefix {
		return nil, fmt.Errorf("%w: prefix %04x: header prefix %04x", ErrCorruptSegment, meta.Prefix, got)
	}
	if got := binary.BigEndian.Uint16(data[segmentReservedOff : segmentReservedOff+2]); got != 0 {
		return nil, fmt.Errorf("%w: prefix %04x: reserved field %04x", ErrCorruptSegment, meta.Prefix, got)
	}

	count := binary.BigEndian.Uint32(data[16:20])
	bloomBits := binary.BigEndian.Uint64(data[20:28])
	bloomK := binary.BigEndian.Uint32(data[28:32])
	bloomBytes := binary.BigEndian.Uint64(data[32:40])

	if int64(count) != int64(meta.Count) {
		return nil, fmt.Errorf("%w: prefix %04x: header count %d, manifest says %d", ErrCorruptSegment, meta.Prefix, count, meta.Count)
	}
	if bloomBits != uint64(meta.BloomBits) || int(bloomK) != meta.BloomK {
		return nil, fmt.Errorf("%w: prefix %04x: Bloom parameters do not match manifest", ErrCorruptSegment, meta.Prefix)
	}
	if bloomK == 0 || bloomK > maxBloomK || bloomBits == 0 {
		return nil, fmt.Errorf("%w: prefix %04x: invalid Bloom parameters", ErrCorruptSegment, meta.Prefix)
	}
	wantBloomBytes := (bloomBits + 7) / 8
	if bloomBytes != wantBloomBytes {
		return nil, fmt.Errorf("%w: prefix %04x: Bloom byte length %d, want %d", ErrCorruptSegment, meta.Prefix, bloomBytes, wantBloomBytes)
	}

	wantSize := uint64(segmentHeaderSize) + bloomBytes + uint64(count)*segmentHashSize
	if wantSize != uint64(len(data)) {
		return nil, fmt.Errorf("%w: prefix %04x: file size %d, header implies %d", ErrCorruptSegment, meta.Prefix, len(data), wantSize)
	}

	bloomStart := segmentHeaderSize
	bloomEnd := bloomStart + int(bloomBytes)
	hashStart := bloomEnd
	hashes := make([]Hash, int(count))
	var previous Hash
	for i := range hashes {
		offset := hashStart + i*segmentHashSize
		copy(hashes[i][:], data[offset:offset+segmentHashSize])
		if i > 0 && !hashLess(previous, hashes[i]) {
			return nil, fmt.Errorf("%w: prefix %04x: hashes not strictly sorted at position %d", ErrCorruptSegment, meta.Prefix, i)
		}
		if got := prefixOf(hashes[i], prefixBytes); got != meta.Prefix {
			return nil, fmt.Errorf("%w: prefix %04x: hash %s routes to %04x", ErrCorruptSegment, meta.Prefix, hashes[i], got)
		}
		previous = hashes[i]
	}

	levels := merkleLevels(hashes)
	if len(levels) == 0 {
		return nil, fmt.Errorf("%w: prefix %04x: no hashes", ErrCorruptSegment, meta.Prefix)
	}
	if root := levels[len(levels)-1][0]; root != meta.Root {
		return nil, fmt.Errorf("%w: prefix %04x: Merkle root mismatch", ErrCorruptSegment, meta.Prefix)
	}

	bloom := &bloomFilter{
		bits: data[bloomStart:bloomEnd],
		m:    bloomBits,
		k:    int(bloomK),
	}
	for _, h := range hashes {
		if !bloom.maybe(h) {
			return nil, fmt.Errorf("%w: prefix %04x: Bloom filter has a false negative for %s", ErrCorruptSegment, meta.Prefix, h)
		}
	}

	return &segment{
		meta:   meta,
		hashes: hashes,
		bloom:  bloom,
		levels: levels,
		data:   data,
	}, nil
}

// readSegmentFile reads, checksums and parses one segment. Every cache miss
// goes through this function, so a segment edited after LoadStore is rejected
// before it can answer a lookup.
func readSegmentFile(path string, meta SegmentMeta, prefixBytes int) (*segment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: prefix %04x: %w", ErrCorruptSegment, meta.Prefix, err)
	}
	if err := verifySegmentBytes(data, meta); err != nil {
		return nil, err
	}
	return decodeSegment(data, meta, prefixBytes)
}
