package edgeset

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func ed25519Keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func TestEd25519ManifestSignVerify(t *testing.T) {
	pub, priv := ed25519Keypair(t)
	hashes := randomHashes(400, 71)
	dir := t.TempDir()
	_, manifest := mustBuild(t, hashes, BuildOptions{
		SegmentDir:        dir,
		Ed25519PrivateKey: priv,
	})

	if manifest.SignatureAlg != SignatureAlgEd25519 {
		t.Fatalf("SignatureAlg = %q, want %q", manifest.SignatureAlg, SignatureAlgEd25519)
	}
	if len(manifest.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature length = %d, want %d", len(manifest.Signature), ed25519.SignatureSize)
	}
	if err := VerifyManifestWithPublicKey(manifest, pub); err != nil {
		t.Fatalf("VerifyManifestWithPublicKey: %v", err)
	}
	if err := VerifyManifest(manifest, nil); err != nil {
		t.Fatalf("VerifyManifest with no key must skip verification: %v", err)
	}
	if err := VerifyManifest(manifest, []byte("hmac-key")); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("VerifyManifest(ed25519 manifest, hmac key) error = %v, want ErrManifestSignature", err)
	}

	loaded, loadedManifest, err := LoadStoreVerified(dir, Ed25519Verifier{PublicKey: pub})
	if err != nil {
		t.Fatalf("LoadStoreVerified: %v", err)
	}
	if loadedManifest.SignatureAlg != SignatureAlgEd25519 {
		t.Fatalf("loaded SignatureAlg = %q", loadedManifest.SignatureAlg)
	}
	for _, h := range hashes {
		if ok, proof, err := loaded.Contains(h); err != nil || !ok || !VerifyProof(segmentRoot(t, loaded, h), h, proof) {
			t.Fatalf("loaded Contains(%s) = %v, %v", h, ok, err)
		}
	}

	if _, _, err := LoadStoreVerified(dir, NewEd25519Verifier(pub)); err != nil {
		t.Fatalf("LoadStoreVerified with NewEd25519Verifier: %v", err)
	}
	// No key means "structurally validate only", matching LoadStore.
	if _, _, err := LoadStore(dir, nil); err != nil {
		t.Fatalf("LoadStore(dir, nil): %v", err)
	}
	// The HMAC-only compatibility entry point must not accept an Ed25519
	// manifest when an HMAC key is supplied.
	if _, _, err := LoadStore(dir, []byte("hmac-key")); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStore(ed25519, hmac key) error = %v, want ErrManifestSignature", err)
	}
	if _, _, err := LoadStoreVerified(dir, HMACVerifier{Key: []byte("hmac-key")}); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStoreVerified(HMAC) error = %v, want ErrManifestSignature", err)
	}

	wrongPub, _ := ed25519Keypair(t)
	if err := VerifyManifestWithPublicKey(manifest, wrongPub); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("wrong public key error = %v, want ErrManifestSignature", err)
	}
	if _, _, err := LoadStoreVerified(dir, Ed25519Verifier{PublicKey: wrongPub}); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStoreVerified with wrong public key error = %v", err)
	}
	if err := VerifyManifestWithPublicKey(manifest, pub[:16]); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("short public key error = %v, want ErrManifestSignature", err)
	}

	tampers := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "version", mutate: func(m *Manifest) { m.Version++ }},
		{name: "prefix bytes", mutate: func(m *Manifest) { m.PrefixBytes++ }},
		{name: "segment count", mutate: func(m *Manifest) { m.Segments[0].Count++ }},
		{name: "segment root", mutate: func(m *Manifest) { m.Segments[0].Root[0] ^= 1 }},
		{name: "segment compression", mutate: func(m *Manifest) {
			if m.Segments[0].Compression == CompressionZstd {
				m.Segments[0].Compression = CompressionNone
			} else {
				m.Segments[0].Compression = CompressionZstd
			}
		}},
		{name: "signature bit", mutate: func(m *Manifest) { m.Signature[0] ^= 0xff }},
		{name: "signature truncation", mutate: func(m *Manifest) { m.Signature = m.Signature[:8] }},
		{name: "algorithm downgrade", mutate: func(m *Manifest) { m.SignatureAlg = "" }},
		{name: "algorithm switch", mutate: func(m *Manifest) { m.SignatureAlg = SignatureAlgHMACSHA256 }},
		{name: "algorithm unknown", mutate: func(m *Manifest) { m.SignatureAlg = "rsa-pss" }},
	}
	for _, tc := range tampers {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneManifest(manifest)
			tc.mutate(tampered)
			if err := VerifyManifestWithPublicKey(tampered, pub); err == nil {
				t.Fatalf("VerifyManifestWithPublicKey accepted tampered %s", tc.name)
			}
			if _, _, err := LoadStoreVerified(dir, Ed25519Verifier{PublicKey: pub}); err != nil {
				t.Fatalf("original store no longer loads: %v", err)
			}
		})
	}
}

func TestManifestAlgorithmUpgradeAndDowngrade(t *testing.T) {
	key := []byte("legacy-hmac-key")
	pub, priv := ed25519Keypair(t)
	_, hmacManifest := mustBuild(t, randomHashes(64, 72), BuildOptions{SigningKey: key})

	// HMAC builds stay on the legacy encoding: no signature_alg field and no
	// change to the canonical bytes used by older versions.
	if hmacManifest.SignatureAlg != "" {
		t.Fatalf("HMAC SignatureAlg = %q, want empty legacy value", hmacManifest.SignatureAlg)
	}
	hmacJSON := mustMarshal(t, hmacManifest)
	if bytes.Contains(hmacJSON, []byte(`"signature_alg"`)) {
		t.Fatalf("legacy HMAC manifest JSON contains signature_alg: %s", hmacJSON)
	}
	if err := VerifyManifest(hmacManifest, key); err != nil {
		t.Fatalf("legacy HMAC manifest does not verify: %v", err)
	}

	// Upgrade: the same manifest content re-signed with Ed25519.
	upgraded := cloneManifest(hmacManifest)
	if err := signManifestEd25519(upgraded, priv); err != nil {
		t.Fatalf("signManifestEd25519: %v", err)
	}
	if upgraded.SignatureAlg != SignatureAlgEd25519 {
		t.Fatalf("upgraded algorithm = %q", upgraded.SignatureAlg)
	}
	if err := VerifyManifestWithPublicKey(upgraded, pub); err != nil {
		t.Fatalf("upgraded manifest does not verify with public key: %v", err)
	}
	if err := VerifyManifest(upgraded, key); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("HMAC verifier accepted an Ed25519 manifest: %v", err)
	}

	// Downgrade: clearing the algorithm without re-signing must fail hard.
	downgraded := cloneManifest(upgraded)
	downgraded.SignatureAlg = ""
	if err := VerifyManifestWithPublicKey(downgraded, pub); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("Ed25519 verifier accepted algorithm downgrade: %v", err)
	}
	// An intentional HMAC downgrade requires a valid HMAC signature, but an
	// Ed25519-only verifier still rejects it.
	if err := signManifest(downgraded, key); err != nil {
		t.Fatalf("signManifest: %v", err)
	}
	if err := VerifyManifest(downgraded, key); err != nil {
		t.Fatalf("intentional HMAC downgrade does not verify: %v", err)
	}
	if err := VerifyManifestWithPublicKey(downgraded, pub); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("Ed25519 verifier accepted HMAC downgrade: %v", err)
	}

	// An explicit "hmac-sha256" algorithm is accepted by the HMAC verifier. Its
	// canonical payload includes the algorithm field.
	explicit := cloneManifest(hmacManifest)
	explicit.SignatureAlg = SignatureAlgHMACSHA256
	explicit.Signature = nil
	payload, err := canonicalManifest(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"signature_alg":"hmac-sha256"`)) {
		t.Fatalf("explicit HMAC canonical payload lacks signature_alg: %s", payload)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	explicit.Signature = mac.Sum(nil)
	if err := VerifyManifest(explicit, key); err != nil {
		t.Fatalf("explicit hmac-sha256 manifest does not verify: %v", err)
	}
	if err := VerifyManifestWithPublicKey(explicit, pub); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("Ed25519 verifier accepted explicit HMAC manifest: %v", err)
	}
}

func TestBuildRejectsMixedOrInvalidSigningKeys(t *testing.T) {
	_, priv := ed25519Keypair(t)

	tests := []struct {
		name string
		opts BuildOptions
	}{
		{
			name: "both keys",
			opts: BuildOptions{SigningKey: []byte("hmac"), Ed25519PrivateKey: priv},
		},
		{
			name: "truncated ed25519 key",
			opts: BuildOptions{Ed25519PrivateKey: priv[:32]},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.SegmentDir = t.TempDir()
			_, _, err := Build(nil, tc.opts)
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Build error = %v, want ErrInvalidOption", err)
			}
			entries, readErr := os.ReadDir(tc.opts.SegmentDir)
			if readErr != nil {
				t.Fatalf("read destination: %v", readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed Build wrote %d destination entries", len(entries))
			}
		})
	}
}

func TestManifestSignatureAlgJSON(t *testing.T) {
	key := []byte("json-alg-key")
	_, hmacManifest := mustBuild(t, randomHashes(8, 73), BuildOptions{SigningKey: key})
	if strings.Contains(string(mustMarshal(t, hmacManifest)), "signature_alg") {
		t.Fatal("HMAC manifest serialized signature_alg")
	}

	pub, priv := ed25519Keypair(t)
	_, edManifest := mustBuild(t, randomHashes(8, 74), BuildOptions{Ed25519PrivateKey: priv})
	body := mustMarshal(t, edManifest)
	if !bytes.Contains(body, []byte(`"signature_alg":"ed25519"`)) {
		t.Fatalf("Ed25519 manifest JSON lacks signature_alg: %s", body)
	}
	var decoded Manifest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestWithPublicKey(&decoded, pub); err != nil {
		t.Fatalf("JSON round-tripped Ed25519 manifest: %v", err)
	}
}
