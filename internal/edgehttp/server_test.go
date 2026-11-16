package edgehttp_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

func testHash(prefix uint16, tail uint32) edgeset.Hash {
	var h edgeset.Hash
	binary.BigEndian.PutUint16(h[0:2], prefix)
	binary.BigEndian.PutUint32(h[28:32], tail)
	return h
}

func testHashes() []edgeset.Hash {
	return []edgeset.Hash{
		testHash(0x1111, 1),
		testHash(0x1111, 2),
		testHash(0x1111, 3),
		testHash(0x2222, 1),
		testHash(0x2222, 2),
	}
}

func buildTestStore(t *testing.T, hashes []edgeset.Hash, key []byte) (string, *edgeset.Manifest) {
	t.Helper()
	dir := t.TempDir()
	_, manifest, err := edgeset.Build(hashes, edgeset.BuildOptions{
		SegmentDir: dir,
		SigningKey: key,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dir, manifest
}

func startServer(t *testing.T, dir string, key []byte) *httptest.Server {
	t.Helper()
	srv, err := edgehttp.NewServer(dir, key)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, body
}

func TestHealthz(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)

	resp, body := get(t, ts.URL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("status = %q", payload.Status)
	}
}

func TestManifest(t *testing.T) {
	key := []byte("test-signing-key")
	hashes := testHashes()
	dir, manifest := buildTestStore(t, hashes, key)
	ts := startServer(t, dir, key)

	resp, body := get(t, ts.URL+"/v1/manifest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}

	var got edgeset.Manifest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if err := edgeset.VerifyManifest(&got, key); err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if len(got.Signature) != 32 {
		t.Fatalf("signature length = %d, want 32", len(got.Signature))
	}
	if !reflect.DeepEqual(got.Segments, manifest.Segments) {
		t.Fatalf("served segments differ from built manifest")
	}
	if got.Version != manifest.Version || got.PrefixBytes != manifest.PrefixBytes {
		t.Fatalf("header fields = %d/%d, want %d/%d",
			got.Version, got.PrefixBytes, manifest.Version, manifest.PrefixBytes)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("manifest has no ETag")
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", resp2.StatusCode)
	}
}

func TestManifestVersion(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)

	resp, body := get(t, ts.URL+"/v1/manifest/version")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Version != manifest.Version {
		t.Fatalf("version = %d, want %d", payload.Version, manifest.Version)
	}
}

func TestSegment(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)
	meta := manifest.Segments[0]

	resp, body := get(t, ts.URL+"/v1/segment/1111")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if int64(len(body)) != meta.Size {
		t.Fatalf("body size = %d, want %d", len(body), meta.Size)
	}
	if sum := edgeset.HashOf(body); sum != meta.SHA256 {
		t.Fatalf("body sha256 = %s, want %s", sum, meta.SHA256)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(meta.File)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, onDisk) {
		t.Fatal("served bytes differ from the segment file")
	}
	if ct := resp.Header.Get("Cache-Control"); ct == "" {
		t.Fatal("segment response has no Cache-Control")
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("segment has no ETag")
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/segment/1111", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", resp2.StatusCode)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/segment/111", http.StatusBadRequest},
		{"/v1/segment/zzzz", http.StatusBadRequest},
		{"/v1/segment/abcd", http.StatusNotFound},
	} {
		resp, _ := get(t, ts.URL+tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s status = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	respPost, err := http.Post(ts.URL+"/v1/segment/1111", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	respPost.Body.Close()
	if respPost.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", respPost.StatusCode)
	}
}

func TestLookupPresent(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)
	h := testHash(0x1111, 2)

	resp, body := get(t, ts.URL+"/v1/lookup/"+h.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var got struct {
		Present bool           `json:"present"`
		Proof   *edgeset.Proof `json:"proof"`
		Root    string         `json:"root"`
		Prefix  string         `json:"prefix"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Present {
		t.Fatal("present = false, want true")
	}
	if got.Prefix != "1111" {
		t.Fatalf("prefix = %q, want 1111", got.Prefix)
	}
	if got.Proof == nil {
		t.Fatal("present lookup has no proof")
	}
	meta, ok := manifestSegment(manifest, 0x1111)
	if !ok {
		t.Fatal("segment 1111 missing from manifest")
	}
	if got.Root != meta.Root.String() {
		t.Fatalf("root = %q, want %q", got.Root, meta.Root.String())
	}
	root, err := edgeset.ParseHash(got.Root)
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	if !edgeset.VerifyProof(root, h, got.Proof) {
		t.Fatal("server proof does not verify")
	}
}

func TestLookupAbsent(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)

	for _, h := range []edgeset.Hash{
		testHash(0x9999, 1),  // no segment with this prefix
		testHash(0x1111, 99), // existing segment, absent hash
	} {
		resp, body := get(t, ts.URL+"/v1/lookup/"+h.String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var got struct {
			Present bool           `json:"present"`
			Proof   *edgeset.Proof `json:"proof"`
			Root    string         `json:"root"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Present {
			t.Fatalf("hash %s present = true, want false", h)
		}
		if got.Proof != nil {
			t.Fatal("absent lookup returned a proof")
		}
		if got.Root != "" {
			t.Fatalf("absent lookup root = %q, want empty", got.Root)
		}
	}
}

func TestLookupBadHash(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)

	for _, hash := range []string{"abc", strings.Repeat("zz", 32)} {
		resp, _ := get(t, ts.URL+"/v1/lookup/"+hash)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("lookup %q status = %d, want 400", hash, resp.StatusCode)
		}
	}
}

func TestTamperedStoreRejectedAtStartup(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	meta := manifest.Segments[0]
	path := filepath.Join(dir, filepath.FromSlash(meta.File))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := edgehttp.NewServer(dir, nil); !errors.Is(err, edgeset.ErrCorruptSegment) {
		t.Fatalf("NewServer error = %v, want ErrCorruptSegment", err)
	}
}

func TestTamperedManifestSignatureRejected(t *testing.T) {
	key := []byte("signing-key")
	dir, manifest := buildTestStore(t, testHashes(), key)
	manifest.Signature[0] ^= 0xff
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := edgehttp.NewServer(dir, key); !errors.Is(err, edgeset.ErrManifestSignature) {
		t.Fatalf("NewServer error = %v, want ErrManifestSignature", err)
	}
}

func TestTamperedSegmentAfterStartup(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	ts := startServer(t, dir, nil)
	meta := manifest.Segments[0]
	path := filepath.Join(dir, filepath.FromSlash(meta.File))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	resp, _ := get(t, ts.URL+"/v1/segment/1111")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for tampered segment", resp.StatusCode)
	}
}

func TestEmptyStore(t *testing.T) {
	dir, manifest := buildTestStore(t, nil, nil)
	if len(manifest.Segments) != 0 {
		t.Fatalf("segments = %d, want 0", len(manifest.Segments))
	}
	ts := startServer(t, dir, nil)

	resp, body := get(t, ts.URL+"/v1/lookup/"+testHash(0, 0).String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d", resp.StatusCode)
	}
	var got struct {
		Present bool `json:"present"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Present {
		t.Fatal("empty store reported a present hash")
	}
	if resp, _ := get(t, ts.URL+"/v1/segment/0000"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("segment status = %d, want 404", resp.StatusCode)
	}
}

func manifestSegment(m *edgeset.Manifest, prefix uint16) (edgeset.SegmentMeta, bool) {
	for _, meta := range m.Segments {
		if meta.Prefix == prefix {
			return meta, true
		}
	}
	return edgeset.SegmentMeta{}, false
}
