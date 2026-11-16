package edgeset

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	segmentMagic = "EDGSEG01"

	// Segment format history:
	//   1 - uncompressed Bloom+hash payload, 40-byte header.
	//   2 - header carries a compression code and the raw (uncompressed)
	//       payload size; the Bloom+hash payload may be zstd-compressed.
	segmentFormatV1 uint32 = 1
	segmentFormatV2 uint32 = 2

	// segmentFormat is the format written by new builds.
	segmentFormat = segmentFormatV2

	segmentHeaderV1Size = 8 + 4 + 2 + 2 + 4 + 8 + 4 + 8 // 40 bytes
	segmentHeaderV2Size = segmentHeaderV1Size + 8       // 48 bytes
	segmentHashSize     = 32

	// segmentHeaderSize is the legacy v1 header size. New code uses the
	// explicit v1/v2 constants; this alias is kept for older package-internal
	// references.
	segmentHeaderSize = segmentHeaderV1Size

	// Header field offsets shared by v1 and v2.
	segmentCountOff     = 16
	segmentBloomBitsOff = 20
	segmentBloomKOff    = 28
	segmentBloomLenOff  = 32

	// v1 has a two-byte reserved field at 14. v2 uses byte 14 for the
	// compression code and byte 15 as reserved.
	segmentV1ReservedOff  = 14
	segmentCompressionOff = 14
	segmentV2ReservedOff  = 15
	segmentRawSizeOff     = segmentHeaderV1Size // 40, v2 only
	segmentReservedOff    = segmentV1ReservedOff
	maxSegmentRawSize     = uint64(1 << 32) // practical allocation cap for a segment
)

// Compression identifiers stored in SegmentMeta.Compression. The empty string
// is reserved for legacy format v1 metadata.
const (
	CompressionNone = "none"
	CompressionZstd = "zstd"
)

// Segment compression codes stored in the v2 header.
const (
	segmentCompressionNone uint8 = 0
	segmentCompressionZstd uint8 = 1
)

// On-disk layout (all integers big-endian).
//
// Format v1 (legacy):
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
//	40      ...   Bloom bitset ((m+7)/8 bytes), then sorted hashes
//
// Format v2:
//
//	offset  size  field
//	0       8     magic "EDGSEG01"
//	8       4     format version (2)
//	12      2     segment prefix
//	14      1     compression (0 none, 1 zstd)
//	15      1     reserved (0)
//	16      4     hash count
//	20      8     Bloom bit count m
//	28      4     Bloom probe count k
//	32      8     Bloom bitset byte length
//	40      8     uncompressed payload size (Bloom bytes + count*32)
//	48      ...   payload: Bloom bitset then sorted hashes, optionally zstd
//
// The payload covered by compression is exactly the Bloom bitset followed by
// the sorted 32-byte hashes. Size and SHA256 in SegmentMeta cover the stored
// file (header and payload). The Merkle root is not duplicated in the file: it
// lives in the manifest and is recomputed from the decoded hashes.

type segment struct {
	meta   SegmentMeta
	hashes []Hash
	bloom  *bloomFilter
	levels [][]Hash
	data   []byte
}

// compressionMode is the parsed BuildOptions.Compression value.
type compressionMode uint8

const (
	compressionAuto compressionMode = iota
	compressionNoneMode
	compressionZstdMode
)

func parseCompression(raw string) (compressionMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return compressionAuto, nil
	case CompressionNone:
		return compressionNoneMode, nil
	case CompressionZstd:
		return compressionZstdMode, nil
	default:
		return 0, fmt.Errorf("%w: Compression %q must be %q, %q or empty", ErrInvalidOption, raw, CompressionNone, CompressionZstd)
	}
}

// compress reports whether a segment with an uncompressed payload of rawSize
// bytes should be zstd-compressed under this mode.
func (m compressionMode) compress(rawSize int) bool {
	switch m {
	case compressionNoneMode:
		return false
	case compressionZstdMode:
		return true
	default:
		return rawSize >= defaultCompressionThreshold
	}
}

// rawSegmentPayload returns the uncompressed Bloom bitset followed by the
// sorted hashes.
func rawSegmentPayload(hashes []Hash, bloom *bloomFilter) []byte {
	bloomBytes := len(bloom.bits)
	raw := make([]byte, bloomBytes+len(hashes)*segmentHashSize)
	copy(raw[:bloomBytes], bloom.bits)
	offset := bloomBytes
	for _, h := range hashes {
		copy(raw[offset:offset+segmentHashSize], h[:])
		offset += segmentHashSize
	}
	return raw
}

func writeCommonHeader(data []byte, format uint32, prefix uint16, hashes []Hash, bloom *bloomFilter) {
	copy(data[0:8], segmentMagic)
	binary.BigEndian.PutUint32(data[8:12], format)
	binary.BigEndian.PutUint16(data[12:14], prefix)
	binary.BigEndian.PutUint32(data[segmentCountOff:segmentCountOff+4], uint32(len(hashes)))
	binary.BigEndian.PutUint64(data[segmentBloomBitsOff:segmentBloomBitsOff+8], bloom.m)
	binary.BigEndian.PutUint32(data[segmentBloomKOff:segmentBloomKOff+4], uint32(bloom.k))
	binary.BigEndian.PutUint64(data[segmentBloomLenOff:segmentBloomLenOff+8], uint64(len(bloom.bits)))
}

// encodeSegmentV1 writes the legacy uncompressed format. It is retained for
// compatibility fixtures and tests; Build always writes format v2.
func encodeSegmentV1(prefix uint16, hashes []Hash, bloom *bloomFilter) []byte {
	raw := rawSegmentPayload(hashes, bloom)
	data := make([]byte, segmentHeaderV1Size+len(raw))
	writeCommonHeader(data, segmentFormatV1, prefix, hashes, bloom)
	binary.BigEndian.PutUint16(data[segmentV1ReservedOff:segmentV1ReservedOff+2], 0)
	copy(data[segmentHeaderV1Size:], raw)
	return data
}

// encodeSegmentV2 writes format v2, optionally compressing the Bloom+hash
// payload. enc must be non-nil when compress is true. It returns the stored
// bytes and the SegmentMeta.Compression identifier for the file.
func encodeSegmentV2(prefix uint16, hashes []Hash, bloom *bloomFilter, compress bool, enc *zstd.Encoder) ([]byte, string, error) {
	raw := rawSegmentPayload(hashes, bloom)
	if uint64(len(raw)) > maxSegmentRawSize {
		return nil, "", fmt.Errorf("%w: segment %04x raw payload %d exceeds %d", ErrInvalidOption, prefix, len(raw), maxSegmentRawSize)
	}

	stored := raw
	compression := CompressionNone
	compressionCode := segmentCompressionNone
	if compress {
		if enc == nil {
			return nil, "", fmt.Errorf("edgeset: zstd encoder is nil")
		}
		stored = enc.EncodeAll(raw, nil)
		compression = CompressionZstd
		compressionCode = segmentCompressionZstd
	}

	data := make([]byte, segmentHeaderV2Size+len(stored))
	writeCommonHeader(data, segmentFormatV2, prefix, hashes, bloom)
	data[segmentCompressionOff] = compressionCode
	data[segmentV2ReservedOff] = 0
	binary.BigEndian.PutUint64(data[segmentRawSizeOff:segmentRawSizeOff+8], uint64(len(raw)))
	copy(data[segmentHeaderV2Size:], stored)
	return data, compression, nil
}

// verifySegmentBytes checks the manifest size and SHA-256 before parsing.
// Checksum verification is deliberately separate from parsing so a tampered
// file is rejected even if the malformed bytes happen to form a valid header,
// and so decompression only ever runs on bytes matching the manifest.
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

func compressionName(code uint8) (string, bool) {
	switch code {
	case segmentCompressionNone:
		return CompressionNone, true
	case segmentCompressionZstd:
		return CompressionZstd, true
	default:
		return "", false
	}
}

// decompressZstd decodes one compressed segment payload, enforcing rawSize.
// The decoder's memory bound leaves room for zstd's frame window (which can be
// larger than a tiny payload) while still preventing a corrupt frame from
// expanding without bound.
func decompressZstd(src []byte, rawSize uint64) ([]byte, error) {
	if rawSize == 0 {
		return nil, fmt.Errorf("zero raw payload size")
	}
	if rawSize > maxSegmentRawSize {
		return nil, fmt.Errorf("raw payload size %d exceeds %d", rawSize, maxSegmentRawSize)
	}
	if rawSize > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("raw payload size %d does not fit in int", rawSize)
	}
	// zstd uses a minimum 1 KiB window and may round a small content size up to
	// the next window size. Allow generous slack on top of the authenticated
	// raw size without permitting unbounded allocation.
	const decoderSlack = uint64(1 << 20)
	memoryLimit := rawSize + decoderSlack
	if memoryLimit < rawSize {
		return nil, fmt.Errorf("raw payload size %d overflows decoder limit", rawSize)
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(memoryLimit),
	)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	dst := make([]byte, 0, int(rawSize))
	out, err := dec.DecodeAll(src, dst)
	if err != nil {
		return nil, err
	}
	if uint64(len(out)) != rawSize {
		return nil, fmt.Errorf("decoded payload size %d, want %d", len(out), rawSize)
	}
	return out, nil
}

func decodeSegmentV1(data []byte, meta SegmentMeta, headerSize int) ([]byte, error) {
	if got := binary.BigEndian.Uint16(data[segmentV1ReservedOff : segmentV1ReservedOff+2]); got != 0 {
		return nil, fmt.Errorf("%w: prefix %04x: reserved field %04x", ErrCorruptSegment, meta.Prefix, got)
	}
	if meta.Compression != "" && meta.Compression != CompressionNone {
		return nil, fmt.Errorf("%w: prefix %04x: v1 segment has compression %q", ErrCorruptSegment, meta.Prefix, meta.Compression)
	}
	return data[headerSize:], nil
}

func decodeSegmentV2(data []byte, meta SegmentMeta, headerSize int) ([]byte, error) {
	code := data[segmentCompressionOff]
	expected, ok := compressionName(code)
	if !ok {
		return nil, fmt.Errorf("%w: prefix %04x: unsupported compression code %d", ErrCorruptSegment, meta.Prefix, code)
	}
	if meta.Compression == "" {
		// Tolerate a missing manifest field for an uncompressed v2 segment, but
		// never silently treat zstd metadata as absent.
		if code != segmentCompressionNone {
			return nil, fmt.Errorf("%w: prefix %04x: header is %s but manifest omits compression", ErrCorruptSegment, meta.Prefix, expected)
		}
	} else if meta.Compression != expected {
		return nil, fmt.Errorf("%w: prefix %04x: header compression %q, manifest says %q", ErrCorruptSegment, meta.Prefix, expected, meta.Compression)
	}
	if got := data[segmentV2ReservedOff]; got != 0 {
		return nil, fmt.Errorf("%w: prefix %04x: reserved field %02x", ErrCorruptSegment, meta.Prefix, got)
	}

	rawSize := binary.BigEndian.Uint64(data[segmentRawSizeOff : segmentRawSizeOff+8])
	if rawSize > maxSegmentRawSize {
		return nil, fmt.Errorf("%w: prefix %04x: raw payload size %d exceeds %d", ErrCorruptSegment, meta.Prefix, rawSize, maxSegmentRawSize)
	}
	stored := data[headerSize:]
	if code == segmentCompressionNone {
		if uint64(len(stored)) != rawSize {
			return nil, fmt.Errorf("%w: prefix %04x: stored payload size %d, header says %d", ErrCorruptSegment, meta.Prefix, len(stored), rawSize)
		}
		return stored, nil
	}
	decoded, err := decompressZstd(stored, rawSize)
	if err != nil {
		return nil, fmt.Errorf("%w: prefix %04x: zstd decode: %v", ErrCorruptSegment, meta.Prefix, err)
	}
	return decoded, nil
}

// decodeSegment parses and fully validates a segment against its manifest
// metadata: header fields, compression, Bloom parameters, exact length,
// strictly increasing hashes, routing prefix and Merkle root.
func decodeSegment(data []byte, meta SegmentMeta, prefixBytes int) (*segment, error) {
	if len(data) < segmentHeaderV1Size {
		return nil, fmt.Errorf("%w: prefix %04x: file too short (%d bytes)", ErrCorruptSegment, meta.Prefix, len(data))
	}
	if string(data[0:8]) != segmentMagic {
		return nil, fmt.Errorf("%w: prefix %04x: bad magic", ErrCorruptSegment, meta.Prefix)
	}
	format := binary.BigEndian.Uint32(data[8:12])
	var headerSize int
	switch format {
	case segmentFormatV1:
		headerSize = segmentHeaderV1Size
	case segmentFormatV2:
		headerSize = segmentHeaderV2Size
	default:
		return nil, fmt.Errorf("%w: prefix %04x: unsupported format %d", ErrCorruptSegment, meta.Prefix, format)
	}
	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: prefix %04x: file too short (%d bytes) for format %d", ErrCorruptSegment, meta.Prefix, len(data), format)
	}
	if got := binary.BigEndian.Uint16(data[12:14]); got != meta.Prefix {
		return nil, fmt.Errorf("%w: prefix %04x: header prefix %04x", ErrCorruptSegment, meta.Prefix, got)
	}

	count := binary.BigEndian.Uint32(data[segmentCountOff : segmentCountOff+4])
	bloomBits := binary.BigEndian.Uint64(data[segmentBloomBitsOff : segmentBloomBitsOff+8])
	bloomK := binary.BigEndian.Uint32(data[segmentBloomKOff : segmentBloomKOff+4])
	bloomBytes := binary.BigEndian.Uint64(data[segmentBloomLenOff : segmentBloomLenOff+8])

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

	// Derive the uncompressed payload size from the authenticated Bloom
	// parameters and hash count, with overflow/allocation bounds checked before
	// any decompression or slice construction.
	if bloomBytes > maxSegmentRawSize {
		return nil, fmt.Errorf("%w: prefix %04x: Bloom payload %d exceeds %d", ErrCorruptSegment, meta.Prefix, bloomBytes, maxSegmentRawSize)
	}
	countBytes := uint64(count) * segmentHashSize
	if countBytes > maxSegmentRawSize-bloomBytes {
		return nil, fmt.Errorf("%w: prefix %04x: payload exceeds %d bytes", ErrCorruptSegment, meta.Prefix, maxSegmentRawSize)
	}
	wantRaw := bloomBytes + countBytes
	if wantRaw == 0 {
		return nil, fmt.Errorf("%w: prefix %04x: empty payload", ErrCorruptSegment, meta.Prefix)
	}

	var (
		payload []byte
		err     error
	)
	switch format {
	case segmentFormatV1:
		payload, err = decodeSegmentV1(data, meta, headerSize)
		if err != nil {
			return nil, err
		}
		if uint64(len(payload)) != wantRaw {
			return nil, fmt.Errorf("%w: prefix %04x: payload size %d, header implies %d", ErrCorruptSegment, meta.Prefix, len(payload), wantRaw)
		}
	case segmentFormatV2:
		rawSize := binary.BigEndian.Uint64(data[segmentRawSizeOff : segmentRawSizeOff+8])
		if rawSize != wantRaw {
			return nil, fmt.Errorf("%w: prefix %04x: raw payload size %d, header implies %d", ErrCorruptSegment, meta.Prefix, rawSize, wantRaw)
		}
		payload, err = decodeSegmentV2(data, meta, headerSize)
		if err != nil {
			return nil, err
		}
		if uint64(len(payload)) != wantRaw {
			return nil, fmt.Errorf("%w: prefix %04x: decoded payload size %d, want %d", ErrCorruptSegment, meta.Prefix, len(payload), wantRaw)
		}
	}

	bloomEnd := int(bloomBytes)
	hashStart := bloomEnd
	hashes := make([]Hash, int(count))
	var previous Hash
	for i := range hashes {
		offset := hashStart + i*segmentHashSize
		copy(hashes[i][:], payload[offset:offset+segmentHashSize])
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
		bits: payload[:bloomEnd],
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
// before it can answer a lookup. Size and SHA-256 are checked before any
// decompression.
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
