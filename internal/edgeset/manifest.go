package edgeset

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const (
	// ManifestVersion is the only manifest schema understood by this package.
	ManifestVersion uint64 = 1

	manifestFileName = "manifest.json"
)

// canonicalManifest encodes every Manifest field except Signature. Encoding a
// plain struct (rather than a map) makes the byte stream deterministic: fields
// keep declaration order, slices keep their order and Hash values are hex text.
func canonicalManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	type canonical struct {
		Version     uint64        `json:"version"`
		PrefixBytes int           `json:"prefix_bytes"`
		Segments    []SegmentMeta `json:"segments"`
	}
	return json.Marshal(canonical{
		Version:     m.Version,
		PrefixBytes: m.PrefixBytes,
		Segments:    m.Segments,
	})
}

// signManifest computes and stores the HMAC of the canonical manifest JSON.
func signManifest(m *Manifest, key []byte) error {
	payload, err := canonicalManifest(m)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	m.Signature = mac.Sum(nil)
	return nil
}

// VerifyManifest verifies the HMAC-SHA256 signature of m with key.
//
// An empty key means "do not verify": unsigned manifests and callers that only
// want structural validation pass nil. When a key is supplied the signature
// must be present and match. Verification covers every field except Signature.
func VerifyManifest(m *Manifest, key []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if len(key) == 0 {
		return nil
	}
	if len(m.Signature) != sha256.Size {
		return fmt.Errorf("%w: got %d-byte signature, want %d", ErrManifestSignature, len(m.Signature), sha256.Size)
	}
	payload, err := canonicalManifest(m)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), m.Signature) {
		return ErrManifestSignature
	}
	return nil
}

func validateManifest(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if m.Version != ManifestVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidManifest, m.Version)
	}
	if m.PrefixBytes < minPrefixBytes || m.PrefixBytes > maxPrefixBytes {
		return fmt.Errorf("%w: prefix bytes %d outside %d..%d", ErrInvalidManifest, m.PrefixBytes, minPrefixBytes, maxPrefixBytes)
	}

	var previous uint16
	for i := range m.Segments {
		meta := &m.Segments[i]
		if i > 0 && meta.Prefix <= previous {
			return fmt.Errorf("%w: segments not strictly sorted at %04x", ErrInvalidManifest, meta.Prefix)
		}
		previous = meta.Prefix

		if m.PrefixBytes == 1 && meta.Prefix > 0xff {
			return fmt.Errorf("%w: segment %04x does not fit one-byte prefixes", ErrInvalidManifest, meta.Prefix)
		}
		if meta.Count <= 0 {
			return fmt.Errorf("%w: segment %04x has count %d", ErrInvalidManifest, meta.Prefix, meta.Count)
		}
		if meta.BloomBits <= 0 || meta.BloomBits > maxBloomBits {
			return fmt.Errorf("%w: segment %04x has BloomBits %d", ErrInvalidManifest, meta.Prefix, meta.BloomBits)
		}
		if meta.BloomK <= 0 || meta.BloomK > maxBloomK {
			return fmt.Errorf("%w: segment %04x has BloomK %d", ErrInvalidManifest, meta.Prefix, meta.BloomK)
		}
		if meta.Size <= 0 {
			return fmt.Errorf("%w: segment %04x has size %d", ErrInvalidManifest, meta.Prefix, meta.Size)
		}
		wantFile := segmentFileFor(meta.Prefix)
		if meta.File != wantFile {
			return fmt.Errorf("%w: segment %04x has file %q, want %q", ErrInvalidManifest, meta.Prefix, meta.File, wantFile)
		}
	}
	return nil
}

// cloneManifest deep-copies m so a Store never aliases caller-owned slices.
func cloneManifest(m *Manifest) *Manifest {
	if m == nil {
		return nil
	}
	out := &Manifest{
		Version:     m.Version,
		PrefixBytes: m.PrefixBytes,
	}
	if m.Segments != nil {
		out.Segments = append([]SegmentMeta{}, m.Segments...)
	}
	if m.Signature != nil {
		out.Signature = append([]byte(nil), m.Signature...)
	}
	return out
}
