package edgehttp_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"distributed-db/internal/edgehttp"
	"distributed-db/internal/edgeset"
)

func buildOptionsStore(t *testing.T, hashes []edgeset.Hash, opts edgeset.BuildOptions) (string, *edgeset.Manifest) {
	t.Helper()
	dir := t.TempDir()
	opts.SegmentDir = dir
	_, manifest, err := edgeset.Build(hashes, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dir, manifest
}

func newOptionsServer(t *testing.T, dir string, opts edgehttp.Options) *edgehttp.Server {
	t.Helper()
	srv, err := edgehttp.NewServerWithOptions(dir, opts)
	if err != nil {
		t.Fatalf("NewServerWithOptions: %v", err)
	}
	return srv
}

func doRequest(t *testing.T, client *http.Client, method, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, body
}

func TestTLSServerFromOptions(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	})
	if srv.TLSConfig() == nil {
		t.Fatal("TLSConfig() = nil, want configured clone")
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	resp, body := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/manifest", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var manifest edgeset.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if manifest.Version != edgeset.ManifestVersion {
		t.Fatalf("version = %d", manifest.Version)
	}

}

func TestAuthToken(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{AuthToken: "s3cret"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, _ := doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/manifest", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want Bearer challenge", got)
	}

	for _, tc := range []struct {
		auth string
		want int
	}{
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer s3cret", http.StatusOK},
		{"bearer s3cret", http.StatusOK},
	} {
		resp, body := doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/manifest",
			map[string]string{"Authorization": tc.auth})
		if resp.StatusCode != tc.want {
			t.Errorf("Authorization %q status = %d, want %d (body %s)", tc.auth, resp.StatusCode, tc.want, body)
		}
	}
}

func TestHealthzAuthExemption(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)

	exempt := newOptionsServer(t, dir, edgehttp.Options{AuthToken: "token"})
	exemptTS := httptest.NewServer(exempt.Handler())
	t.Cleanup(exemptTS.Close)
	resp, _ := doRequest(t, http.DefaultClient, http.MethodGet, exemptTS.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exempt healthz status = %d, want 200", resp.StatusCode)
	}

	protected := newOptionsServer(t, dir, edgehttp.Options{AuthToken: "token", ProtectHealthz: true})
	protectedTS := httptest.NewServer(protected.Handler())
	t.Cleanup(protectedTS.Close)
	resp, _ = doRequest(t, http.DefaultClient, http.MethodGet, protectedTS.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected healthz status = %d, want 401", resp.StatusCode)
	}
	resp, _ = doRequest(t, http.DefaultClient, http.MethodGet, protectedTS.URL+"/healthz",
		map[string]string{"Authorization": "Bearer token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("protected healthz with token status = %d, want 200", resp.StatusCode)
	}
}

func TestRateLimitPerIP(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{RateLimit: 1, RateBurst: 1})

	request := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	first := request("192.0.2.1:1234")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	second := request("192.0.2.1:9999")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", second.Code)
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 response has no Retry-After")
	}
	other := request("192.0.2.2:1234")
	if other.Code != http.StatusOK {
		t.Fatalf("other IP status = %d, want 200 (per-IP buckets)", other.Code)
	}
}

func TestSegmentRangeRequests(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, data := get(t, ts.URL+"/v1/segment/1111")
	size := int64(len(data))
	if meta := manifest.Segments[0]; size != meta.Size {
		t.Fatalf("segment size = %d, want %d", size, meta.Size)
	}

	tests := []struct {
		name       string
		rangeValue string
		wantStatus int
		wantBody   []byte
		wantCR     string
	}{
		{
			name:       "prefix",
			rangeValue: "bytes=0-9",
			wantStatus: http.StatusPartialContent,
			wantBody:   data[:10],
			wantCR:     "bytes 0-9/" + itoa(size),
		},
		{
			name:       "suffix",
			rangeValue: "bytes=-8",
			wantStatus: http.StatusPartialContent,
			wantBody:   data[size-8:],
			wantCR:     "bytes " + itoa(size-8) + "-" + itoa(size-1) + "/" + itoa(size),
		},
		{
			name:       "open ended",
			rangeValue: "bytes=" + itoa(size-5) + "-",
			wantStatus: http.StatusPartialContent,
			wantBody:   data[size-5:],
			wantCR:     "bytes " + itoa(size-5) + "-" + itoa(size-1) + "/" + itoa(size),
		},
		{
			name:       "unsatisfiable",
			rangeValue: "bytes=" + itoa(size) + "-",
			wantStatus: http.StatusRequestedRangeNotSatisfiable,
			wantCR:     "bytes */" + itoa(size),
		},
		{
			name:       "malformed",
			rangeValue: "bytes=potato",
			wantStatus: http.StatusRequestedRangeNotSatisfiable,
			wantCR:     "bytes */" + itoa(size),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/segment/1111",
				map[string]string{"Range": tc.rangeValue})
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.wantStatus, body)
			}
			if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges = %q, want bytes", got)
			}
			if tc.wantBody != nil && !bytes.Equal(body, tc.wantBody) {
				t.Fatalf("range body mismatch: got %d bytes, want %d", len(body), len(tc.wantBody))
			}
			if got := resp.Header.Get("Content-Range"); got != tc.wantCR {
				t.Fatalf("Content-Range = %q, want %q", got, tc.wantCR)
			}
		})
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestLookupAbsenceProof(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	absent := testHash(0x1111, 99)
	resp, body := get(t, ts.URL+"/v1/lookup/"+absent.String()+"?absence=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Present      bool                  `json:"present"`
		Proof        *edgeset.Proof        `json:"proof"`
		AbsenceProof *edgeset.AbsenceProof `json:"absence_proof"`
		Root         string                `json:"root"`
		Prefix       string                `json:"prefix"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Present {
		t.Fatal("absent lookup reported present")
	}
	if got.Proof != nil {
		t.Fatal("absent lookup returned an inclusion proof")
	}
	if got.AbsenceProof == nil {
		t.Fatalf("absent lookup returned no absence_proof: %s", body)
	}
	root, err := edgeset.ParseHash(got.Root)
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	meta, ok := manifestSegment(manifest, 0x1111)
	if !ok {
		t.Fatal("segment 1111 missing from manifest")
	}
	if root != meta.Root {
		t.Fatalf("root = %s, want %s", root, meta.Root)
	}
	if !edgeset.VerifyAbsence(root, absent, got.AbsenceProof) {
		t.Fatal("server absence proof does not verify")
	}

	// Without the query parameter the historical response shape is preserved.
	resp, body = get(t, ts.URL+"/v1/lookup/"+absent.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain status = %d", resp.StatusCode)
	}
	var plain struct {
		Present      bool                  `json:"present"`
		AbsenceProof *edgeset.AbsenceProof `json:"absence_proof"`
		Root         string                `json:"root"`
	}
	if err := json.Unmarshal(body, &plain); err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	if plain.AbsenceProof != nil || plain.Root != "" {
		t.Fatalf("plain absent lookup = %+v, want no proof and empty root", plain)
	}
}

func TestCompressedSegmentServedVerbatim(t *testing.T) {
	dir, manifest := buildOptionsStore(t, testHashes(), edgeset.BuildOptions{Compression: "zstd"})
	meta := manifest.Segments[0]
	if meta.Compression != edgeset.CompressionZstd {
		t.Fatalf("compression = %q, want zstd", meta.Compression)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(meta.File)))
	if err != nil {
		t.Fatal(err)
	}
	srv := newOptionsServer(t, dir, edgehttp.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, body := get(t, ts.URL+"/v1/segment/1111")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding = %q, want empty (verbatim stored bytes)", resp.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(body, onDisk) {
		t.Fatal("server did not serve the stored compressed bytes verbatim")
	}
	if edgeset.HashOf(body) != meta.SHA256 {
		t.Fatal("served compressed segment checksum mismatch")
	}

	// The same bytes must still be readable/decodable locally by edgeset.
	store, _, err := edgeset.LoadStore(dir, nil)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if present, _, err := store.Contains(testHashes()[0]); err != nil || !present {
		t.Fatalf("Contains compressed store = %v, %v", present, err)
	}
}

func TestMethodAndNotFound(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, _ := doRequest(t, http.DefaultClient, http.MethodPost, ts.URL+"/v1/manifest", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q, want GET", allow)
	}
	resp, _ = doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/segment/abcd", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing segment status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doRequest(t, http.DefaultClient, http.MethodGet, ts.URL+"/v1/segment/zzzz", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid segment status = %d, want 400", resp.StatusCode)
	}
}

func TestMaxSegmentBytes(t *testing.T) {
	dir, manifest := buildTestStore(t, testHashes(), nil)
	meta := manifest.Segments[0]
	srv := newOptionsServer(t, dir, edgehttp.Options{MaxSegmentBytes: meta.Size - 1})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, body := get(t, ts.URL+"/v1/segment/1111")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", resp.StatusCode, body)
	}
}

func TestEd25519Server(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := buildOptionsStore(t, testHashes(), edgeset.BuildOptions{Ed25519PrivateKey: priv})
	srv := newOptionsServer(t, dir, edgehttp.Options{Verifier: edgeset.NewEd25519Verifier(pub)})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, body := get(t, ts.URL+"/v1/manifest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d", resp.StatusCode)
	}
	var manifest edgeset.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SignatureAlg != edgeset.SignatureAlgEd25519 {
		t.Fatalf("signature_alg = %q", manifest.SignatureAlg)
	}
	if err := edgeset.VerifyManifestWithPublicKey(&manifest, pub); err != nil {
		t.Fatalf("VerifyManifestWithPublicKey: %v", err)
	}

	resp, body = get(t, ts.URL+"/v1/lookup/"+testHashes()[0].String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d", resp.StatusCode)
	}
	var lookup struct {
		Present bool `json:"present"`
	}
	if err := json.Unmarshal(body, &lookup); err != nil || !lookup.Present {
		t.Fatalf("lookup = %+v, %v", lookup, err)
	}

	// An HMAC verifier must reject the Ed25519 manifest instead of downgrading.
	if _, _, err := edgeset.LoadStoreVerified(dir, edgeset.NewHMACVerifier([]byte("k"))); err == nil {
		t.Fatal("LoadStoreVerified accepted Ed25519 store with an HMAC verifier")
	}
}

func TestManifestCacheControlAndETag(t *testing.T) {
	dir, _ := buildTestStore(t, testHashes(), nil)
	srv := newOptionsServer(t, dir, edgehttp.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, _ := get(t, ts.URL+"/v1/manifest")
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("manifest Cache-Control = %q", got)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("manifest has no ETag")
	}
	resp, _ = get(t, ts.URL+"/v1/segment/1111")
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("segment Cache-Control = %q", got)
	}
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("segment ETag = %q, want strong quoted ETag", etag)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
}
