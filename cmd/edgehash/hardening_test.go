package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

func writePKCS8PrivateKey(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ed25519.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePublicKeyPEM(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ed25519.pub.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildEd25519CompressionAndLookup(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPath := writePKCS8PrivateKey(t, priv)
	publicKeyPath := writePublicKeyPEM(t, pub)

	hashes := cliHashes()
	input := filepath.Join(t.TempDir(), "hashes.txt")
	var lines strings.Builder
	for _, h := range hashes {
		lines.WriteString(h.String())
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(input, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(t.TempDir(), "store")
	code, stdout, stderr := invoke(t, []string{
		"build", "-in", input, "-out", storeDir,
		"-ed25519-key", privateKeyPath, "-compression", "zstd",
	}, "")
	if code != 0 {
		t.Fatalf("build exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "signed") || !strings.Contains(stdout, "built 3 hashes") {
		t.Fatalf("build stdout = %q", stdout)
	}

	store, manifest, err := edgeset.LoadStoreVerified(storeDir, edgeset.NewEd25519Verifier(pub))
	if err != nil {
		t.Fatalf("LoadStoreVerified: %v", err)
	}
	if store.TotalCount() != len(hashes) {
		t.Fatalf("total = %d, want %d", store.TotalCount(), len(hashes))
	}
	if manifest.SignatureAlg != edgeset.SignatureAlgEd25519 {
		t.Fatalf("signature_alg = %q", manifest.SignatureAlg)
	}
	for _, meta := range manifest.Segments {
		if meta.Compression != edgeset.CompressionZstd {
			t.Fatalf("segment %04x compression = %q", meta.Prefix, meta.Compression)
		}
	}

	server, err := edgehttp.NewServerWithOptions(storeDir, edgehttp.Options{
		Verifier: edgeset.NewEd25519Verifier(pub),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	code, stdout, stderr = invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", hashes[0].String(),
		"-verify-ed25519", publicKeyPath,
	}, "")
	if code != 0 {
		t.Fatalf("lookup exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "present: true") || !strings.Contains(stdout, "proof: verified") {
		t.Fatalf("lookup stdout = %q", stdout)
	}

	code, stdout, stderr = invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", cliHash(0x1111, 99).String(),
		"-verify-ed25519", publicKeyPath,
	}, "")
	if code != 0 {
		t.Fatalf("absent lookup exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "present: false") || !strings.Contains(stdout, "absence_proof: verified") {
		t.Fatalf("absent lookup stdout = %q", stdout)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	code, stdout, stderr = invoke(t, []string{
		"sync", "-server", ts.URL, "-dir", cacheDir,
		"-verify-ed25519", publicKeyPath,
		"-max-segments", "1",
	}, "")
	if code != 0 {
		t.Fatalf("sync exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "synced") || !strings.Contains(stdout, "evictions=1") {
		t.Fatalf("sync stdout = %q", stdout)
	}
	cached, _, err := loadCLICache(cacheDir, nil)
	if err != nil {
		t.Fatalf("load cache generation: %v", err)
	}
	if cached.SegmentCount() != 1 {
		t.Fatalf("cached segments = %d, want 1", cached.SegmentCount())
	}
}

func TestBuildRawEd25519Key(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privateKeyPath := filepath.Join(dir, "ed25519.raw")
	publicKeyPath := filepath.Join(dir, "ed25519.pub.raw")
	if err := os.WriteFile(privateKeyPath, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicKeyPath, pub, 0o644); err != nil {
		t.Fatal(err)
	}

	hashes := cliHashes()
	var in strings.Builder
	for _, h := range hashes {
		in.WriteString(h.String())
		in.WriteByte('\n')
	}
	outDir := filepath.Join(t.TempDir(), "raw-store")
	code, _, stderr := invoke(t, []string{
		"build", "-in", "-", "-out", outDir, "-ed25519-key", privateKeyPath,
	}, in.String())
	if code != 0 {
		t.Fatalf("build exit = %d, stderr = %s", code, stderr)
	}
	if _, _, err := edgeset.LoadStoreVerified(outDir, edgeset.NewEd25519Verifier(pub)); err != nil {
		t.Fatalf("LoadStoreVerified: %v", err)
	}

	// The raw public key path must work for lookup against a test server too.
	store, _, err := edgeset.LoadStoreVerified(outDir, edgeset.NewEd25519Verifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	_ = store
	server, err := edgehttp.NewServerWithOptions(outDir, edgehttp.Options{
		Verifier: edgeset.NewEd25519Verifier(pub),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	code, stdout, stderr := invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", hashes[0].String(),
		"-verify-ed25519", publicKeyPath,
	}, "")
	if code != 0 || !strings.Contains(stdout, "present: true") {
		t.Fatalf("lookup = %d, %q, %s", code, stdout, stderr)
	}
}

func TestCompressionFlagValidation(t *testing.T) {
	code, _, stderr := invoke(t, []string{
		"build", "-in", "-", "-out", filepath.Join(t.TempDir(), "s"), "-compression", "bogus",
	}, cliHashes()[0].String()+"\n")
	if code != 2 || !strings.Contains(stderr, "compression") {
		t.Fatalf("build bad compression = %d, %q", code, stderr)
	}
}

func TestEd25519FlagConflicts(t *testing.T) {
	dir := buildCLIStore(t, cliHashes(), []byte("key"))
	code, _, stderr := invoke(t, []string{
		"serve", "-dir", dir, "-key", "00", "-verify-ed25519", "pub.pem",
	}, "")
	if code != 2 || !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("serve conflicting verifiers = %d, %q", code, stderr)
	}
	code, _, stderr = invoke(t, []string{
		"build", "-in", "-", "-out", filepath.Join(t.TempDir(), "s"), "-key", "00", "-ed25519-key", "priv.pem",
	}, cliHashes()[0].String()+"\n")
	if code != 2 || !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("build conflicting keys = %d, %q", code, stderr)
	}
}
