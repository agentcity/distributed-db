package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

func cliHash(prefix uint16, tail uint32) edgeset.Hash {
	var h edgeset.Hash
	binary.BigEndian.PutUint16(h[0:2], prefix)
	binary.BigEndian.PutUint32(h[28:32], tail)
	return h
}

func cliHashes() []edgeset.Hash {
	return []edgeset.Hash{
		cliHash(0x1111, 1),
		cliHash(0x1111, 2),
		cliHash(0x2222, 1),
	}
}

func buildCLIStore(t *testing.T, hashes []edgeset.Hash, key []byte) string {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := edgeset.Build(hashes, edgeset.BuildOptions{
		SegmentDir: dir,
		SigningKey: key,
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dir
}

func loadCLICache(cacheDir string, verifier edgeset.Verifier) (*edgeset.Store, *edgeset.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "CURRENT"))
	if err != nil {
		return nil, nil, err
	}
	generation := strings.TrimSpace(string(raw))
	return edgeset.LoadStoreVerified(filepath.Join(cacheDir, generation), verifier)
}

func invoke(t *testing.T, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestBuildAndVerify(t *testing.T) {
	key := []byte("cli-key")
	keyHex := hex.EncodeToString(key)
	hashes := cliHashes()
	input := filepath.Join(t.TempDir(), "hashes.txt")
	content := strings.Join([]string{
		"# test hashes",
		"",
		hashes[0].String(),
		hashes[1].String() + "   # inline comment",
		hashes[1].String(), // duplicate
		hashes[2].String(),
	}, "\n") + "\n"
	if err := os.WriteFile(input, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "store")

	code, stdout, stderr := invoke(t, []string{
		"build", "-in", input, "-out", outDir, "-key", keyHex,
	}, "")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "built 3 hashes") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "signed") {
		t.Fatalf("stdout = %q, want signed manifest", stdout)
	}

	store, manifest, err := edgeset.LoadStore(outDir, key)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if store.TotalCount() != len(hashes) {
		t.Fatalf("total = %d, want %d", store.TotalCount(), len(hashes))
	}
	if len(manifest.Signature) != 32 {
		t.Fatalf("signature length = %d", len(manifest.Signature))
	}
}

func TestBuildFromStdinAndPrefixBytes(t *testing.T) {
	hashes := cliHashes()
	var in strings.Builder
	for _, h := range hashes {
		in.WriteString(h.String())
		in.WriteByte('\n')
	}
	outDir := filepath.Join(t.TempDir(), "store")
	code, _, stderr := invoke(t, []string{
		"build", "-in", "-", "-out", outDir, "-prefix-bytes", "1",
	}, in.String())
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	store, manifest, err := edgeset.LoadStore(outDir, nil)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if store.TotalCount() != len(hashes) {
		t.Fatalf("total = %d", store.TotalCount())
	}
	if manifest.PrefixBytes != 1 {
		t.Fatalf("prefix bytes = %d, want 1", manifest.PrefixBytes)
	}
}

func TestBuildErrors(t *testing.T) {
	valid := cliHashes()[0].String()
	cases := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantStderr string
	}{
		{name: "no flags", args: []string{"build"}, wantCode: 2, wantStderr: "-in and -out"},
		{name: "invalid hash", args: []string{"build", "-in", "-", "-out", filepath.Join(t.TempDir(), "s")}, stdin: valid + "\nnot-a-hash\n", wantCode: 1, wantStderr: "line 2"},
		{name: "bad key", args: []string{"build", "-in", "-", "-out", filepath.Join(t.TempDir(), "s"), "-key", "xyz"}, stdin: valid + "\n", wantCode: 2, wantStderr: "invalid hex key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := invoke(t, tc.args, tc.stdin)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %s)", code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.wantStderr)
			}
		})
	}
}

func TestUsageAndUnknownCommand(t *testing.T) {
	code, _, stderr := invoke(t, nil, "")
	if code != 2 || !strings.Contains(stderr, "usage:") {
		t.Fatalf("no args = %d, %q", code, stderr)
	}
	code, _, stderr = invoke(t, []string{"frobnicate"}, "")
	if code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("unknown = %d, %q", code, stderr)
	}
	code, stdout, _ := invoke(t, []string{"help"}, "")
	if code != 0 || !strings.Contains(stdout, "edgehash lookup") {
		t.Fatalf("help = %d, %q", code, stdout)
	}
}

func TestLookupAndSyncCLI(t *testing.T) {
	key := []byte("cli-server-key")
	keyHex := hex.EncodeToString(key)
	hashes := cliHashes()
	storeDir := buildCLIStore(t, hashes, key)
	server, err := edgehttp.NewServer(storeDir, key)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	code, stdout, stderr := invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", hashes[0].String(), "-key", keyHex,
	}, "")
	if code != 0 {
		t.Fatalf("lookup present exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"present: true", "proof: verified", "prefix: 1111"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("lookup stdout = %q, want %q", stdout, want)
		}
	}

	code, stdout, stderr = invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", cliHash(0x1111, 99).String(), "-key", keyHex,
	}, "")
	if code != 0 || !strings.Contains(stdout, "present: false") {
		t.Fatalf("lookup absent = %d, %q, %s", code, stdout, stderr)
	}

	code, _, stderr = invoke(t, []string{
		"lookup", "-server", ts.URL, "-hash", "bad",
	}, "")
	if code != 2 || !strings.Contains(stderr, "hash must be") {
		t.Fatalf("lookup bad hash = %d, %q", code, stderr)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	code, stdout, stderr = invoke(t, []string{
		"sync", "-server", ts.URL, "-dir", cacheDir, "-key", keyHex,
	}, "")
	if code != 0 {
		t.Fatalf("sync exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "synced 2/2 segments") {
		t.Fatalf("sync stdout = %q", stdout)
	}
	cached, manifest, err := loadCLICache(cacheDir, edgeset.NewHMACVerifier(key))
	if err != nil {
		t.Fatalf("load cache generation: %v", err)
	}
	if cached.TotalCount() != len(hashes) || len(manifest.Segments) != 2 {
		t.Fatalf("cache total/segments = %d/%d", cached.TotalCount(), len(manifest.Segments))
	}

	filteredDir := filepath.Join(t.TempDir(), "filtered")
	code, stdout, stderr = invoke(t, []string{
		"sync", "-server", ts.URL, "-dir", filteredDir, "-key", keyHex, "-prefix", "2222",
	}, "")
	if code != 0 {
		t.Fatalf("filtered sync exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "synced 1/2 segments") {
		t.Fatalf("filtered sync stdout = %q", stdout)
	}
	filteredStore, filteredManifest, err := loadCLICache(filteredDir, nil)
	if err != nil {
		t.Fatalf("load filtered cache generation: %v", err)
	}
	if filteredStore.TotalCount() != 1 || len(filteredManifest.Segments) != 1 {
		t.Fatalf("filtered total/segments = %d/%d", filteredStore.TotalCount(), len(filteredManifest.Segments))
	}
}

func TestSyncCLIRejectsWrongKey(t *testing.T) {
	storeDir := buildCLIStore(t, cliHashes(), []byte("right"))
	server, err := edgehttp.NewServer(storeDir, []byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	code, _, stderr := invoke(t, []string{
		"sync", "-server", ts.URL, "-dir", t.TempDir(), "-key", hex.EncodeToString([]byte("wrong")),
	}, "")
	if code != 1 || !strings.Contains(stderr, "signature") {
		t.Fatalf("sync wrong key = %d, %q", code, stderr)
	}
}

func TestServeLifecycle(t *testing.T) {
	storeDir := buildCLIStore(t, cliHashes(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request immediate graceful shutdown

	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		var out, errOut bytes.Buffer
		code := run(ctx, []string{"serve", "-dir", storeDir, "-addr", "127.0.0.1:0"},
			strings.NewReader(""), &out, &errOut)
		done <- result{code: code, stdout: out.String(), stderr: errOut.String()}
	}()

	select {
	case got := <-done:
		if got.code != 0 {
			t.Fatalf("serve exit = %d, stderr = %s", got.code, got.stderr)
		}
		if !strings.Contains(got.stdout, "serving") || !strings.Contains(got.stdout, "shutting down") {
			t.Fatalf("serve stdout = %q", got.stdout)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestServeBadStore(t *testing.T) {
	code, _, stderr := invoke(t, []string{
		"serve", "-dir", filepath.Join(t.TempDir(), "missing"), "-addr", "127.0.0.1:0",
	}, "")
	if code != 1 || !strings.Contains(stderr, "load store") {
		t.Fatalf("serve missing store = %d, %q", code, stderr)
	}
}

func TestParsePrefixList(t *testing.T) {
	prefixes, err := parsePrefixList("1111, 2222,")
	if err != nil {
		t.Fatalf("parsePrefixList: %v", err)
	}
	if len(prefixes) != 2 || prefixes[0] != 0x1111 || prefixes[1] != 0x2222 {
		t.Fatalf("prefixes = %v", prefixes)
	}
	for _, bad := range []string{"1", "zzzz", "12345"} {
		if _, err := parsePrefixList(bad); err == nil {
			t.Errorf("parsePrefixList(%q) succeeded", bad)
		}
	}
}
