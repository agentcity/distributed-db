package edgeset

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestSignature(t *testing.T) {
	key := []byte("manifest-signing-key")
	hashes := randomHashes(200, 30)
	dir := t.TempDir()
	_, manifest := mustBuild(t, hashes, BuildOptions{SegmentDir: dir, SigningKey: key})

	if len(manifest.Signature) != sha256.Size {
		t.Fatalf("signature length = %d, want %d", len(manifest.Signature), sha256.Size)
	}
	if err := VerifyManifest(manifest, key); err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if err := VerifyManifest(manifest, nil); err != nil {
		t.Fatalf("VerifyManifest with no key must skip verification: %v", err)
	}
	if err := VerifyManifest(manifest, []byte("wrong-key")); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("wrong key error = %v, want ErrManifestSignature", err)
	}
	if err := VerifyManifest(manifest, nil); err != nil {
		t.Fatalf("VerifyManifest changed the manifest: %v", err)
	}

	tampers := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "version", mutate: func(m *Manifest) { m.Version++ }},
		{name: "prefix bytes", mutate: func(m *Manifest) { m.PrefixBytes++ }},
		{name: "segment count", mutate: func(m *Manifest) { m.Segments[0].Count++ }},
		{name: "segment root", mutate: func(m *Manifest) { m.Segments[0].Root[0] ^= 1 }},
		{name: "segment file", mutate: func(m *Manifest) { m.Segments[0].File += ".bak" }},
		{name: "segment checksum", mutate: func(m *Manifest) { m.Segments[0].SHA256[0] ^= 1 }},
		{name: "signature", mutate: func(m *Manifest) { m.Signature[0] ^= 0xff }},
		{name: "truncated signature", mutate: func(m *Manifest) { m.Signature = m.Signature[:8] }},
	}
	if len(manifest.Segments) > 1 {
		tampers = append(tampers, struct {
			name   string
			mutate func(*Manifest)
		}{
			name: "reordered segments",
			mutate: func(m *Manifest) {
				m.Segments[0], m.Segments[1] = m.Segments[1], m.Segments[0]
			},
		})
	}
	for _, tc := range tampers {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneManifest(manifest)
			tc.mutate(tampered)
			if err := VerifyManifest(tampered, key); err == nil {
				t.Fatalf("VerifyManifest accepted tampered %s", tc.name)
			}
		})
	}

	// Correct key loads; wrong key and a key against an unsigned file fail
	// before segment verification.
	if _, _, err := LoadStore(dir, key); err != nil {
		t.Fatalf("LoadStore with correct key: %v", err)
	}
	if _, _, err := LoadStore(dir, []byte("wrong-key")); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStore with wrong key error = %v", err)
	}
	if _, _, err := LoadStore(dir, nil); err != nil {
		t.Fatalf("LoadStore with no key must skip signature verification: %v", err)
	}

	unsignedStore, unsigned := mustBuild(t, hashes, BuildOptions{})
	if len(unsigned.Signature) != 0 {
		t.Fatalf("unsigned manifest has a %d-byte signature", len(unsigned.Signature))
	}
	if err := VerifyManifest(unsigned, nil); err != nil {
		t.Fatalf("VerifyManifest(unsigned, nil): %v", err)
	}
	if err := VerifyManifest(unsigned, key); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("unsigned manifest with key error = %v", err)
	}
	if _, _, err := LoadStore(unsignedStore.Dir(), nil); err != nil {
		t.Fatalf("LoadStore(unsigned): %v", err)
	}
	if _, _, err := LoadStore(unsignedStore.Dir(), key); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStore(unsigned, key) error = %v", err)
	}

	// A manifest edited on disk must be rejected by LoadStore even when the
	// individual segment files are intact.
	manifestPath := filepath.Join(dir, manifestFileName)
	originalJSON, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Manifest
	if err := json.Unmarshal(originalJSON, &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk.Segments[0].Count++
	tamperedJSON, err := json.MarshalIndent(&onDisk, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, tamperedJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, key); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("LoadStore after manifest edit error = %v", err)
	}
	if err := os.WriteFile(manifestPath, originalJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(dir, key); err != nil {
		t.Fatalf("LoadStore after restoring manifest: %v", err)
	}
}

func TestLoadStoreRejectsInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	hashes := randomHashes(100, 31)
	_, manifest := mustBuild(t, hashes, BuildOptions{SegmentDir: dir})
	if len(manifest.Segments) < 2 {
		t.Fatalf("test needs at least two segments, got %d", len(manifest.Segments))
	}

	manifestPath := filepath.Join(dir, manifestFileName)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "version", mutate: func(m *Manifest) { m.Version = 999 }},
		{name: "prefix bytes zero", mutate: func(m *Manifest) { m.PrefixBytes = 0 }},
		{name: "prefix bytes too large", mutate: func(m *Manifest) { m.PrefixBytes = 5 }},
		{name: "unsorted segments", mutate: func(m *Manifest) {
			m.Segments[0], m.Segments[1] = m.Segments[1], m.Segments[0]
		}},
		{name: "duplicate segment", mutate: func(m *Manifest) { m.Segments = append(m.Segments, m.Segments[0]) }},
		{name: "zero count", mutate: func(m *Manifest) { m.Segments[0].Count = 0 }},
		{name: "zero bloom bits", mutate: func(m *Manifest) { m.Segments[0].BloomBits = 0 }},
		{name: "zero bloom k", mutate: func(m *Manifest) { m.Segments[0].BloomK = 0 }},
		{name: "zero size", mutate: func(m *Manifest) { m.Segments[0].Size = 0 }},
		{name: "path traversal", mutate: func(m *Manifest) { m.Segments[0].File = "../escape.seg" }},
		{name: "non-canonical file name", mutate: func(m *Manifest) {
			m.Segments[0].File = "segments/not-the-prefix.seg"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edited := cloneManifest(manifest)
			tc.mutate(edited)
			editedJSON, err := json.MarshalIndent(edited, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, editedJSON, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadStore(dir, nil); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("LoadStore error = %v, want ErrInvalidManifest", err)
			}
			if err := os.WriteFile(manifestPath, original, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}

	if _, _, err := LoadStore(dir, nil); err != nil {
		t.Fatalf("LoadStore with restored manifest: %v", err)
	}
}

func TestManifestCanonicalJSONStable(t *testing.T) {
	key := []byte("canonical-key")
	_, manifest := mustBuild(t, randomHashes(20, 32), BuildOptions{SigningKey: key})

	payload, err := canonicalManifest(manifest)
	if err != nil {
		t.Fatalf("canonicalManifest: %v", err)
	}
	again, err := canonicalManifest(cloneManifest(manifest))
	if err != nil {
		t.Fatalf("canonicalManifest: %v", err)
	}
	if !bytes.Equal(payload, again) {
		t.Fatal("canonical manifest encoding is not stable")
	}

	// Signature is explicitly outside the canonical payload.
	changedSignature := cloneManifest(manifest)
	changedSignature.Signature[0] ^= 0xff
	payload2, err := canonicalManifest(changedSignature)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, payload2) {
		t.Fatal("canonical manifest includes Signature")
	}

	var decoded Manifest
	if err := json.Unmarshal(mustMarshal(t, manifest), &decoded); err != nil {
		t.Fatalf("manifest JSON round trip: %v", err)
	}
	if err := VerifyManifest(&decoded, key); err != nil {
		t.Fatalf("round-tripped manifest signature: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}
