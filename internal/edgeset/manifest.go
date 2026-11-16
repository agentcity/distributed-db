package edgeset

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const (
	// ManifestVersion is the only manifest schema understood by this package.
	// Optional fields added by newer builds are omitted when empty, so manifests
	// written by older versions keep their canonical bytes and signatures.
	ManifestVersion uint64 = 1

	// SignatureAlgHMACSHA256 identifies HMAC-SHA256 manifest signatures. An
	// empty SignatureAlg is also treated as legacy HMAC-SHA256.
	SignatureAlgHMACSHA256 = "hmac-sha256"

	// SignatureAlgEd25519 identifies Ed25519 manifest signatures.
	SignatureAlgEd25519 = "ed25519"

	manifestFileName = "manifest.json"
)

// canonicalManifest encodes every manifest field covered by the signature.
//
// Encoding a plain struct (rather than a map) makes the byte stream
// deterministic: fields keep declaration order, slices keep their order and
// Hash values are hex text. signature_alg is omitted when empty so legacy
// HMAC-SHA256 manifests produce exactly the byte stream older versions signed.
func canonicalManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	type canonical struct {
		Version      uint64        `json:"version"`
		PrefixBytes  int           `json:"prefix_bytes"`
		Segments     []SegmentMeta `json:"segments"`
		SignatureAlg string        `json:"signature_alg,omitempty"`
	}
	return json.Marshal(canonical{
		Version:      m.Version,
		PrefixBytes:  m.PrefixBytes,
		Segments:     m.Segments,
		SignatureAlg: m.SignatureAlg,
	})
}

// signManifest computes and stores the legacy HMAC-SHA256 signature.
//
// The algorithm field is cleared first: HMAC manifests retain the historical
// empty signature_alg and therefore stay byte-compatible with older builds and
// verifiers. Callers that explicitly want signature_alg "hmac-sha256" must
// construct the signature themselves.
func signManifest(m *Manifest, key []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if len(key) == 0 {
		return fmt.Errorf("%w: empty HMAC signing key", ErrInvalidOption)
	}
	m.SignatureAlg = ""
	payload, err := canonicalManifest(m)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	m.Signature = mac.Sum(nil)
	return nil
}

// signManifestEd25519 sets SignatureAlg and signs the canonical manifest with
// priv. The manifest's existing signature fields are replaced.
func signManifestEd25519(m *Manifest, priv ed25519.PrivateKey) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: ed25519 private key must be %d bytes, got %d", ErrInvalidOption, ed25519.PrivateKeySize, len(priv))
	}
	m.SignatureAlg = SignatureAlgEd25519
	payload, err := canonicalManifest(m)
	if err != nil {
		return err
	}
	m.Signature = ed25519.Sign(priv, payload)
	return nil
}

// VerifyManifest verifies a legacy HMAC-SHA256 manifest signature with key.
//
// An empty key means "do not verify": unsigned manifests and callers that only
// want structural validation pass nil. When a key is supplied the signature
// must be present and match. VerifyManifest accepts SignatureAlg empty or
// "hmac-sha256"; an Ed25519-signed manifest must be checked with
// VerifyManifestWithPublicKey. Verification covers every field except
// Signature.
func VerifyManifest(m *Manifest, key []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if len(key) == 0 {
		return nil
	}
	switch m.SignatureAlg {
	case "", SignatureAlgHMACSHA256:
		// Handled below.
	case SignatureAlgEd25519:
		return fmt.Errorf("%w: manifest is ed25519-signed; use an ed25519 public key", ErrManifestSignature)
	default:
		return fmt.Errorf("%w: unsupported signature algorithm %q", ErrManifestSignature, m.SignatureAlg)
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

// VerifyManifestWithPublicKey verifies an Ed25519 manifest signature.
//
// The manifest must declare SignatureAlg == "ed25519"; legacy HMAC-SHA256
// manifests are rejected so an attacker cannot downgrade the algorithm. The
// public key must be a crypto/ed25519.PublicKey (32 bytes).
func VerifyManifestWithPublicKey(m *Manifest, pub ed25519.PublicKey) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: invalid ed25519 public key length %d", ErrManifestSignature, len(pub))
	}
	if m.SignatureAlg != SignatureAlgEd25519 {
		return fmt.Errorf("%w: manifest signature algorithm %q is not ed25519", ErrManifestSignature, m.SignatureAlg)
	}
	if len(m.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: got %d-byte ed25519 signature, want %d", ErrManifestSignature, len(m.Signature), ed25519.SignatureSize)
	}
	payload, err := canonicalManifest(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, payload, m.Signature) {
		return ErrManifestSignature
	}
	return nil
}

// Verifier authenticates a manifest signature.
//
// LoadStoreVerified accepts any Verifier. Implementations must reject
// signature algorithms they do not support; this is what prevents a manifest
// from being downgraded from Ed25519 to a weaker algorithm.
type Verifier interface {
	VerifyManifest(m *Manifest) error
}

// HMACVerifier verifies legacy HMAC-SHA256 manifests. An empty Key disables
// signature verification, matching LoadStore and VerifyManifest.
type HMACVerifier struct {
	Key []byte
}

// VerifyManifest implements Verifier.
func (v HMACVerifier) VerifyManifest(m *Manifest) error {
	return VerifyManifest(m, v.Key)
}

// Ed25519Verifier verifies Ed25519 manifests with a public key.
type Ed25519Verifier struct {
	PublicKey ed25519.PublicKey
}

// VerifyManifest implements Verifier.
func (v Ed25519Verifier) VerifyManifest(m *Manifest) error {
	return VerifyManifestWithPublicKey(m, v.PublicKey)
}

// NewHMACVerifier returns a Verifier for legacy HMAC-SHA256 manifests. The key
// is copied so later caller mutations do not affect verification.
func NewHMACVerifier(key []byte) Verifier {
	return HMACVerifier{Key: append([]byte(nil), key...)}
}

// NewEd25519Verifier returns a Verifier for Ed25519 manifests. The public key
// is copied so later caller mutations do not affect verification.
func NewEd25519Verifier(pub ed25519.PublicKey) Verifier {
	return Ed25519Verifier{PublicKey: append(ed25519.PublicKey(nil), pub...)}
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
	switch m.SignatureAlg {
	case "", SignatureAlgHMACSHA256, SignatureAlgEd25519:
	default:
		return fmt.Errorf("%w: unsupported signature algorithm %q", ErrInvalidManifest, m.SignatureAlg)
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
		switch meta.Compression {
		case "", CompressionNone, CompressionZstd:
		default:
			return fmt.Errorf("%w: segment %04x has unknown compression %q", ErrInvalidManifest, meta.Prefix, meta.Compression)
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
		Version:      m.Version,
		PrefixBytes:  m.PrefixBytes,
		SignatureAlg: m.SignatureAlg,
	}
	if m.Segments != nil {
		out.Segments = append([]SegmentMeta{}, m.Segments...)
	}
	if m.Signature != nil {
		out.Signature = append([]byte(nil), m.Signature...)
	}
	return out
}
