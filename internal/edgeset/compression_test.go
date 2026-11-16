package edgeset

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func repetitiveHash(prefix uint16, tail uint32) Hash {
	var h Hash
	for i := 0; i < len(h)-4; i++ {
		h[i] = byte(i % 8)
	}
	binary.BigEndian.PutUint16(h[0:2], prefix)
	binary.BigEndian.PutUint32(h[28:32], tail)
	return h
}

func repetitiveHashes(n int, prefix uint16) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = repetitiveHash(prefix, uint32(i))
	}
	return out
}

func rawPayloadSize(meta SegmentMeta) uint64 {
	return uint64((meta.BloomBits+7)/8) + uint64(meta.Count)*segmentHashSize
}

func segmentHeaderBytes(t *testing.T, store *Store, prefix uint16) []byte {
	t.Helper()
	data, err := store.Segment(prefix)
	if err != nil {
		t.Fatalf("Segment(%04x): %v", prefix, err)
	}
	return data
}

func TestCompressionModesRoundTrip(t *testing.T) {
	key := []byte("compression-signing-key")
	hashes := repetitiveHashes(3000, 0x6666)

	for _, tc := range []struct {
		mode string
		want string
		code uint8
	}{
		{mode: CompressionNone, want: CompressionNone, code: segmentCompressionNone},
		{mode: CompressionZstd, want: CompressionZstd, code: segmentCompressionZstd},
		{mode: " ZSTD ", want: CompressionZstd, code: segmentCompressionZstd},
	} {
		t.Run(tc.want, func(t *testing.T) {
			dir := t.TempDir()
			store, manifest := mustBuild(t, hashes, BuildOptions{
				SegmentDir:  dir,
				SigningKey:  key,
				Compression: tc.mode,
			})
			if len(manifest.Segments) != 1 {
				t.Fatalf("segments = %d, want 1", len(manifest.Segments))
			}
			meta := manifest.Segments[0]
			if meta.Compression != tc.want {
				t.Fatalf("meta.Compression = %q, want %q", meta.Compression, tc.want)
			}
			rawSize := rawPayloadSize(meta)
			data := segmentHeaderBytes(t, store, meta.Prefix)

			if string(data[0:8]) != segmentMagic {
				t.Fatalf("bad magic %q", data[0:8])
			}
			if got := binary.BigEndian.Uint32(data[8:12]); got != segmentFormatV2 {
				t.Fatalf("format = %d, want %d", got, segmentFormatV2)
			}
			if got := data[segmentCompressionOff]; got != tc.code {
				t.Fatalf("compression code = %d, want %d", got, tc.code)
			}
			if got := binary.BigEndian.Uint64(data[segmentRawSizeOff : segmentRawSizeOff+8]); got != rawSize {
				t.Fatalf("raw size = %d, want %d", got, rawSize)
			}
			if uint64(len(data)) != uint64(meta.Size) {
				t.Fatalf("stored length = %d, meta.Size = %d", len(data), meta.Size)
			}
			if tc.code == segmentCompressionNone && uint64(meta.Size) != uint64(segmentHeaderV2Size)+rawSize {
				t.Fatalf("uncompressed size = %d, want %d", meta.Size, uint64(segmentHeaderV2Size)+rawSize)
			}
			if sum := sha256.Sum256(data); sum != meta.SHA256 {
				t.Fatal("stored bytes do not match manifest SHA-256")
			}

			loaded, loadedManifest := mustLoad(t, dir, key)
			if err := VerifyManifest(loadedManifest, key); err != nil {
				t.Fatalf("reloaded manifest signature: %v", err)
			}
			if loadedManifest.Segments[0].Compression != tc.want {
				t.Fatalf("reloaded compression = %q", loadedManifest.Segments[0].Compression)
			}
			for _, h := range hashes {
				if ok, proof, err := loaded.Contains(h); err != nil || !ok || !VerifyProof(meta.Root, h, proof) {
					t.Fatalf("Contains(%s) = %v, %v", h, ok, err)
				}
			}
			absent := repetitiveHash(0x6666, 1<<20)
			proof, err := loaded.ProveAbsent(absent)
			if err != nil {
				t.Fatalf("ProveAbsent on %s segment: %v", tc.want, err)
			}
			if !VerifyAbsence(meta.Root, absent, proof) {
				t.Fatalf("absence proof on %s segment does not verify", tc.want)
			}
		})
	}
}

func TestCompressionAutoDefault(t *testing.T) {
	tiny, tinyManifest := mustBuild(t, []Hash{repetitiveHash(0x1111, 0)}, BuildOptions{})
	if len(tinyManifest.Segments) != 1 {
		t.Fatalf("tiny segments = %d", len(tinyManifest.Segments))
	}
	if got := tinyManifest.Segments[0].Compression; got != CompressionNone {
		t.Fatalf("tiny auto compression = %q, want %q", got, CompressionNone)
	}
	if data := segmentHeaderBytes(t, tiny, tinyManifest.Segments[0].Prefix); data[segmentCompressionOff] != segmentCompressionNone {
		t.Fatalf("tiny auto compression code = %d", data[segmentCompressionOff])
	}

	large, largeManifest := mustBuild(t, repetitiveHashes(5000, 0x2222), BuildOptions{})
	if len(largeManifest.Segments) != 1 {
		t.Fatalf("large segments = %d", len(largeManifest.Segments))
	}
	if got := largeManifest.Segments[0].Compression; got != CompressionZstd {
		t.Fatalf("large auto compression = %q, want %q", got, CompressionZstd)
	}
	if data := segmentHeaderBytes(t, large, largeManifest.Segments[0].Prefix); data[segmentCompressionOff] != segmentCompressionZstd {
		t.Fatalf("large auto compression code = %d", data[segmentCompressionOff])
	}

	t.Run("invalid option", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := Build(nil, BuildOptions{SegmentDir: dir, Compression: "gzip"}); !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("Build error = %v, want ErrInvalidOption", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("invalid compression left %d destination entries", len(entries))
		}
	})
}

func TestCompressionRatioOnRepetitiveHashes(t *testing.T) {
	hashes := repetitiveHashes(20_000, 0x4444)

	noneStore, noneManifest := mustBuild(t, hashes, BuildOptions{Compression: CompressionNone})
	zstdStore, zstdManifest := mustBuild(t, hashes, BuildOptions{Compression: CompressionZstd})
	if len(noneManifest.Segments) != 1 || len(zstdManifest.Segments) != 1 {
		t.Fatalf("segment counts = %d/%d, want 1/1", len(noneManifest.Segments), len(zstdManifest.Segments))
	}
	if noneStore.TotalCount() != len(hashes) || zstdStore.TotalCount() != len(hashes) {
		t.Fatalf("counts = %d/%d, want %d", noneStore.TotalCount(), zstdStore.TotalCount(), len(hashes))
	}
	noneSize := noneManifest.Segments[0].Size
	zstdSize := zstdManifest.Segments[0].Size
	if zstdSize*2 >= noneSize {
		t.Fatalf("zstd size %d is not less than half of uncompressed size %d", zstdSize, noneSize)
	}
	t.Logf("compression ratio = %.3f (%d -> %d bytes)", float64(zstdSize)/float64(noneSize), noneSize, zstdSize)

	// Both encodings decode to the same membership answers.
	for _, h := range []Hash{hashes[0], hashes[len(hashes)/2], hashes[len(hashes)-1]} {
		for _, store := range []*Store{noneStore, zstdStore} {
			if ok, proof, err := store.Contains(h); err != nil || !ok || !VerifyProof(segmentRoot(t, store, h), h, proof) {
				t.Fatalf("Contains(%s) = %v, %v", h, ok, err)
			}
		}
	}
}

func TestCompressionTinySegment(t *testing.T) {
	// Forced zstd on a tiny payload exercises the decoder's minimum window
	// (a zstd frame can use a 1 KiB window for a much smaller payload).
	h := repetitiveHash(0x6161, 7)
	dir := t.TempDir()
	store, manifest := mustBuild(t, []Hash{h}, BuildOptions{
		SegmentDir:  dir,
		Compression: CompressionZstd,
	})
	meta := manifest.Segments[0]
	if meta.Compression != CompressionZstd {
		t.Fatalf("compression = %q, want zstd", meta.Compression)
	}
	loaded, _ := mustLoad(t, dir, nil)
	if ok, proof, err := loaded.Contains(h); err != nil || !ok || !VerifyProof(meta.Root, h, proof) {
		t.Fatalf("Contains(tiny zstd) = %v, %v", ok, err)
	}
	absent := repetitiveHash(0x6161, 8)
	proof, err := loaded.ProveAbsent(absent)
	if err != nil || !VerifyAbsence(meta.Root, absent, proof) {
		t.Fatalf("ProveAbsent(tiny zstd) = %v, %v", proof, err)
	}
	if data := segmentHeaderBytes(t, store, meta.Prefix); data[segmentCompressionOff] != segmentCompressionZstd {
		t.Fatalf("compression code = %d", data[segmentCompressionOff])
	}
}

func TestCompressionDeterministic(t *testing.T) {
	hashes := repetitiveHashes(5000, 0x6262)
	firstStore, first := mustBuild(t, hashes, BuildOptions{Compression: CompressionZstd})
	secondStore, second := mustBuild(t, hashes, BuildOptions{Compression: CompressionZstd})
	if len(first.Segments) != 1 || len(second.Segments) != 1 {
		t.Fatalf("segment counts = %d/%d", len(first.Segments), len(second.Segments))
	}
	if first.Segments[0].SHA256 != second.Segments[0].SHA256 || first.Segments[0].Size != second.Segments[0].Size {
		t.Fatal("zstd builds with identical inputs produced different segment metadata")
	}
	a, err := firstStore.Segment(first.Segments[0].Prefix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := secondStore.Segment(second.Segments[0].Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("zstd builds with identical inputs produced different segment bytes")
	}
}

func TestLegacyV1StoreCompatibility(t *testing.T) {
	key := []byte("legacy-v1-hmac-key")
	hashes := forcedPrefixHashes(80, 0x7777, 91) // all one segment
	sort.Slice(hashes, func(i, j int) bool { return hashLess(hashes[i], hashes[j]) })
	prefix := uint16(0x7777)

	bloom, err := newBloomFilter(len(hashes), defaultFalsePositiveRate)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hashes {
		bloom.add(h)
	}
	levels := merkleLevels(hashes)
	root := levels[len(levels)-1][0]
	data := encodeSegmentV1(prefix, hashes, bloom)

	meta := SegmentMeta{
		Prefix:    prefix,
		Count:     len(hashes),
		BloomBits: int(bloom.m),
		BloomK:    bloom.k,
		Root:      root,
		File:      segmentFileFor(prefix),
		Size:      int64(len(data)),
		SHA256:    sha256.Sum256(data),
	}
	manifest := &Manifest{
		Version:     1,
		PrefixBytes: 2,
		Segments:    []SegmentMeta{meta},
	}
	// Sign exactly like the pre-Ed25519/pre-compression implementation: the
	// canonical JSON has no signature_alg and no compression field.
	type legacyCanonical struct {
		Version     uint64        `json:"version"`
		PrefixBytes int           `json:"prefix_bytes"`
		Segments    []SegmentMeta `json:"segments"`
	}
	payload, err := json.Marshal(legacyCanonical{
		Version:     manifest.Version,
		PrefixBytes: manifest.PrefixBytes,
		Segments:    manifest.Segments,
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	manifest.Signature = mac.Sum(nil)

	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("signature_alg")) || bytes.Contains(body, []byte("compression")) {
		t.Fatalf("legacy fixture unexpectedly contains new fields: %s", body)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(meta.File)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), body, 0o644); err != nil {
		t.Fatal(err)
	}

	store, loadedManifest, err := LoadStore(dir, key)
	if err != nil {
		t.Fatalf("LoadStore(v1): %v", err)
	}
	if loadedManifest.SignatureAlg != "" {
		t.Fatalf("legacy SignatureAlg = %q, want empty", loadedManifest.SignatureAlg)
	}
	if loadedManifest.Segments[0].Compression != "" {
		t.Fatalf("legacy Compression = %q, want empty", loadedManifest.Segments[0].Compression)
	}
	if err := VerifyManifest(loadedManifest, key); err != nil {
		t.Fatalf("VerifyManifest(v1): %v", err)
	}
	if _, _, err := LoadStoreVerified(dir, HMACVerifier{Key: key}); err != nil {
		t.Fatalf("LoadStoreVerified(v1, HMACVerifier): %v", err)
	}
	for _, h := range hashes {
		if ok, proof, err := store.Contains(h); err != nil || !ok || !VerifyProof(root, h, proof) {
			t.Fatalf("Contains(%s) = %v, %v", h, ok, err)
		}
	}
	absent := prefixedHash(prefix, 0xff)
	if ok, _, err := store.Contains(absent); err != nil || ok {
		t.Fatalf("Contains(absent) = %v, %v", ok, err)
	}
	proof, err := store.ProveAbsent(absent)
	if err != nil {
		t.Fatalf("ProveAbsent(v1): %v", err)
	}
	if !VerifyAbsence(root, absent, proof) {
		t.Fatal("v1 absence proof does not verify")
	}
	served, err := store.Segment(prefix)
	if err != nil || !bytes.Equal(served, data) {
		t.Fatalf("Segment(v1) = %d bytes, %v", len(served), err)
	}
	if _, _, err := LoadStore(dir, []byte("wrong-key")); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStore(v1, wrong key) error = %v, want ErrManifestSignature", err)
	}
	// An Ed25519-only verifier must reject the legacy HMAC manifest.
	if _, _, err := LoadStoreVerified(dir, Ed25519Verifier{PublicKey: make([]byte, 32)}); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStoreVerified(Ed25519, v1 HMAC) error = %v, want ErrManifestSignature", err)
	}
}

func TestCorruptCompressedSegmentRejected(t *testing.T) {
	hashes := repetitiveHashes(4000, 0x5555)
	dir := t.TempDir()
	store, manifest := mustBuild(t, hashes, BuildOptions{
		SegmentDir:  dir,
		Compression: CompressionZstd,
	})
	if len(manifest.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(manifest.Segments))
	}
	originalMeta := manifest.Segments[0]
	path := filepath.Join(dir, filepath.FromSlash(originalMeta.File))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if original[segmentCompressionOff] != segmentCompressionZstd {
		t.Fatalf("test fixture compression code = %d", original[segmentCompressionOff])
	}
	if _, _, err := LoadStore(dir, nil); err != nil {
		t.Fatalf("baseline LoadStore: %v", err)
	}

	writeFixture := func(t *testing.T, data []byte, meta SegmentMeta) {
		t.Helper()
		meta.Size = int64(len(data))
		meta.SHA256 = sha256.Sum256(data)
		manifest.Segments[0] = meta
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, manifestFileName), append(body, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(t *testing.T) {
		t.Helper()
		writeFixture(t, original, originalMeta)
		if _, _, err := LoadStore(dir, nil); err != nil {
			t.Fatalf("restored store does not load: %v", err)
		}
	}

	// Truncating the compressed payload while keeping the manifest checksum
	// valid must fail in the zstd decoder, not in the checksum check.
	truncated := append([]byte(nil), original[:len(original)-8]...)
	writeFixture(t, truncated, originalMeta)
	if _, _, err := LoadStore(dir, nil); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("truncated compressed segment error = %v, want ErrCorruptSegment", err)
	}
	restore(t)

	// An unknown compression code is rejected before decoding.
	badCode := append([]byte(nil), original...)
	badCode[segmentCompressionOff] = 99
	writeFixture(t, badCode, originalMeta)
	if _, _, err := LoadStore(dir, nil); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("unknown compression code error = %v, want ErrCorruptSegment", err)
	}
	restore(t)

	// A raw-size mismatch is rejected before decompression.
	badRaw := append([]byte(nil), original...)
	raw := binary.BigEndian.Uint64(badRaw[segmentRawSizeOff : segmentRawSizeOff+8])
	binary.BigEndian.PutUint64(badRaw[segmentRawSizeOff:segmentRawSizeOff+8], raw+1)
	writeFixture(t, badRaw, originalMeta)
	if _, _, err := LoadStore(dir, nil); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("raw-size mismatch error = %v, want ErrCorruptSegment", err)
	}
	restore(t)

	// A changed payload with the old manifest checksum is rejected by SHA-256
	// before decode.
	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0xff
	writeFixture(t, tampered, originalMeta) // writeFixture updates SHA, so undo it below.
	manifest.Segments[0] = originalMeta
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, nil); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("checksum mismatch error = %v, want ErrCorruptSegment", err)
	}
	restore(t)

	// The restored fixture still answers lookups.
	if ok, _, err := store.Contains(hashes[0]); err != nil || !ok {
		t.Fatalf("original store lookup after fixture writes = %v, %v", ok, err)
	}
}

func TestCompressionPrefixRoutingAndRangeAccess(t *testing.T) {
	smallHashes := forcedPrefixHashes(4, 0x4242, 92)
	largeHashes := repetitiveHashes(8000, 0x4141)
	all := append(append([]Hash(nil), smallHashes...), largeHashes...)
	store, manifest := mustBuild(t, all, BuildOptions{})

	if len(manifest.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(manifest.Segments))
	}
	byPrefix := make(map[uint16]SegmentMeta, len(manifest.Segments))
	for _, meta := range manifest.Segments {
		byPrefix[meta.Prefix] = meta
	}
	small, ok := byPrefix[0x4242]
	if !ok {
		t.Fatal("missing 4242 segment")
	}
	large, ok := byPrefix[0x4141]
	if !ok {
		t.Fatal("missing 4141 segment")
	}
	if small.Compression != CompressionNone {
		t.Fatalf("small segment compression = %q, want none", small.Compression)
	}
	if large.Compression != CompressionZstd {
		t.Fatalf("large segment compression = %q, want zstd", large.Compression)
	}

	for _, tc := range []struct {
		prefix uint16
		hashes []Hash
		code   uint8
		absent Hash
	}{
		{prefix: 0x4242, hashes: smallHashes, code: segmentCompressionNone, absent: prefixedHash(0x4242, 0xfe)},
		{prefix: 0x4141, hashes: largeHashes, code: segmentCompressionZstd, absent: repetitiveHash(0x4141, 1<<20)},
	} {
		meta := byPrefix[tc.prefix]
		for _, h := range tc.hashes {
			if store.PrefixFor(h) != tc.prefix {
				t.Fatalf("PrefixFor(%s) = %04x, want %04x", h, store.PrefixFor(h), tc.prefix)
			}
			if ok, proof, err := store.Contains(h); err != nil || !ok || !VerifyProof(meta.Root, h, proof) {
				t.Fatalf("Contains(%s) = %v, %v", h, ok, err)
			}
		}
		proof, err := store.ProveAbsent(tc.absent)
		if err != nil {
			t.Fatalf("ProveAbsent(%s): %v", tc.absent, err)
		}
		if !VerifyAbsence(meta.Root, tc.absent, proof) {
			t.Fatalf("absence proof for prefix %04x does not verify", tc.prefix)
		}

		data, err := store.Segment(tc.prefix)
		if err != nil {
			t.Fatalf("Segment(%04x): %v", tc.prefix, err)
		}
		if data[segmentCompressionOff] != tc.code {
			t.Fatalf("Segment(%04x) compression code = %d, want %d", tc.prefix, data[segmentCompressionOff], tc.code)
		}
		path, err := store.SegmentPath(tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		ranges := [][2]int{{0, 8}, {segmentHeaderV2Size, segmentHeaderV2Size + 32}, {len(data) - 16, len(data)}}
		for _, r := range ranges {
			buf := make([]byte, r[1]-r[0])
			if _, err := file.ReadAt(buf, int64(r[0])); err != nil {
				t.Fatalf("ReadAt(%04x, %d:%d): %v", tc.prefix, r[0], r[1], err)
			}
			if !bytes.Equal(buf, data[r[0]:r[1]]) {
				t.Fatalf("range %d:%d of segment %04x does not match in-memory bytes", r[0], r[1], tc.prefix)
			}
		}
	}

	missing := prefixedHash(0x9999, 1)
	if proof, err := store.ProveAbsent(missing); !errors.Is(err, ErrSegmentNotFound) || proof != nil {
		t.Fatalf("ProveAbsent(missing prefix) = %v, %v", proof, err)
	}
	reloaded, _ := mustLoad(t, store.Dir(), nil)
	if ok, _, err := reloaded.Contains(largeHashes[123]); err != nil || !ok {
		t.Fatalf("reloaded Contains = %v, %v", ok, err)
	}
}
