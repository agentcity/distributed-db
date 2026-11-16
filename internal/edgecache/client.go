package edgecache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"distributed-db/internal/edgeset"
)

// lookupResponse mirrors the server's GET /v1/lookup/{hash} response.
type lookupResponse struct {
	Present bool           `json:"present"`
	Proof   *edgeset.Proof `json:"proof"`
	Root    string         `json:"root"`
	Prefix  string         `json:"prefix"`
}

// RemoteLookup asks a server directly for h, without materializing a cache. It
// verifies the manifest signature (when key is non-empty) and any returned
// Merkle proof against the manifest root. It is used by the lookup CLI and is
// suitable for one-off checks; long-lived clients should use New/Sync.
func RemoteLookup(ctx context.Context, serverURL string, key []byte, h edgeset.Hash, opts Options) (LookupResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return LookupResult{}, err
	}
	client, err := newHTTPClient(opts)
	if err != nil {
		return LookupResult{}, err
	}
	_, manifest, err := fetchManifest(ctx, baseURL, client, key)
	if err != nil {
		return LookupResult{}, err
	}
	prefix := prefixFor(h, manifest.PrefixBytes)
	meta, ok := segmentMetaMap(manifest)[prefix]
	if !ok {
		return LookupResult{Prefix: prefix}, nil
	}
	return lookupRemote(ctx, baseURL, client, h, prefix, meta)
}

func (c *Cache) lookupRemote(ctx context.Context, h edgeset.Hash, prefix uint16, meta edgeset.SegmentMeta) (LookupResult, error) {
	return lookupRemote(ctx, c.baseURL, c.client, h, prefix, meta)
}

func lookupRemote(ctx context.Context, baseURL string, client *http.Client, h edgeset.Hash, prefix uint16, meta edgeset.SegmentMeta) (LookupResult, error) {
	requestURL := baseURL + "/v1/lookup/" + h.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return LookupResult{}, fmt.Errorf("edgecache: build lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "edgehash-cache/1")

	resp, err := client.Do(req)
	if err != nil {
		return LookupResult{}, fmt.Errorf("edgecache: remote lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// A 404 is treated as "not present" for compatibility with servers
		// that answer absent hashes with 404 instead of present=false.
		return LookupResult{Prefix: prefix}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return LookupResult{}, responseError("remote lookup", requestURL, resp)
	}
	raw, err := readLimited(resp.Body, maxLookupBytes)
	if err != nil {
		return LookupResult{}, fmt.Errorf("edgecache: read lookup response: %w", err)
	}
	var wire lookupResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return LookupResult{}, fmt.Errorf("edgecache: decode lookup response: %w", err)
	}
	result := LookupResult{Prefix: prefix}
	if !wire.Present {
		return result, nil
	}
	if wire.Proof == nil {
		return LookupResult{}, fmt.Errorf("%w: server returned present without a proof", ErrInvalidProof)
	}
	if wire.Proof.Hash != h {
		return LookupResult{}, fmt.Errorf("%w: proof is for a different hash", ErrInvalidProof)
	}
	if wire.Prefix != "" {
		want := fmt.Sprintf("%04x", prefix)
		if !strings.EqualFold(wire.Prefix, want) {
			return LookupResult{}, fmt.Errorf("%w: response prefix %q, want %q", ErrInvalidProof, wire.Prefix, want)
		}
	}
	if wire.Root != "" {
		root, err := edgeset.ParseHash(wire.Root)
		if err != nil {
			return LookupResult{}, fmt.Errorf("%w: invalid response root", ErrInvalidProof)
		}
		if root != meta.Root {
			return LookupResult{}, ErrStaleManifest
		}
	}
	if !edgeset.VerifyProof(meta.Root, h, wire.Proof) {
		return LookupResult{}, ErrInvalidProof
	}
	result.Present = true
	result.Proof = wire.Proof
	result.Root = meta.Root
	return result, nil
}

func fetchManifest(ctx context.Context, baseURL string, client *http.Client, key []byte) ([]byte, *edgeset.Manifest, error) {
	requestURL := baseURL + "/v1/manifest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("edgecache: build manifest request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "edgehash-cache/1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("edgecache: fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, responseError("fetch manifest", requestURL, resp)
	}
	raw, err := readLimited(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("edgecache: read manifest: %w", err)
	}
	manifest, err := decodeManifest(raw, key)
	if err != nil {
		return nil, nil, err
	}
	return raw, manifest, nil
}

func (c *Cache) downloadSegment(ctx context.Context, meta edgeset.SegmentMeta, dst string) error {
	requestURL := fmt.Sprintf("%s/v1/segment/%04x", c.baseURL, meta.Prefix)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("edgecache: build segment request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "edgehash-cache/1")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("edgecache: fetch segment %04x: %w", meta.Prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(fmt.Sprintf("fetch segment %04x", meta.Prefix), requestURL, resp)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != meta.Size {
		return fmt.Errorf("edgecache: segment %04x content length %d, want %d", meta.Prefix, resp.ContentLength, meta.Size)
	}

	file, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("edgecache: create staged segment %04x: %w", meta.Prefix, err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(dst)
		}
	}()

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(resp.Body, meta.Size+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		return fmt.Errorf("edgecache: download segment %04x: %w", meta.Prefix, copyErr)
	case syncErr != nil:
		return fmt.Errorf("edgecache: sync segment %04x: %w", meta.Prefix, syncErr)
	case closeErr != nil:
		return fmt.Errorf("edgecache: close segment %04x: %w", meta.Prefix, closeErr)
	case written != meta.Size:
		return fmt.Errorf("edgecache: segment %04x has %d bytes, want %d", meta.Prefix, written, meta.Size)
	}
	var sum edgeset.Hash
	copy(sum[:], hasher.Sum(nil))
	if sum != meta.SHA256 {
		return fmt.Errorf("edgecache: segment %04x checksum mismatch", meta.Prefix)
	}
	if err := os.Chmod(dst, 0o644); err != nil {
		return fmt.Errorf("edgecache: chmod segment %04x: %w", meta.Prefix, err)
	}
	remove = false
	return nil
}

func decodeManifest(raw []byte, key []byte) (*edgeset.Manifest, error) {
	var manifest edgeset.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrInvalidManifest, err)
	}
	if err := edgeset.VerifyManifest(&manifest, key); err != nil {
		return nil, fmt.Errorf("edgecache: verify manifest: %w", err)
	}
	if err := validateManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateManifest(manifest *edgeset.Manifest) error {
	if manifest == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalidManifest)
	}
	if manifest.Version != edgeset.ManifestVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidManifest, manifest.Version)
	}
	if manifest.PrefixBytes < 1 || manifest.PrefixBytes > 4 {
		return fmt.Errorf("%w: prefix bytes %d outside 1..4", ErrInvalidManifest, manifest.PrefixBytes)
	}
	if len(manifest.Signature) != 0 && len(manifest.Signature) != sha256.Size {
		return fmt.Errorf("%w: signature length %d, want 0 or %d", ErrInvalidManifest, len(manifest.Signature), sha256.Size)
	}

	maxSegments := 1 << 8
	if manifest.PrefixBytes >= 2 {
		maxSegments = 1 << 16
	}
	if len(manifest.Segments) > maxSegments {
		return fmt.Errorf("%w: %d segments exceed prefix capacity %d", ErrInvalidManifest, len(manifest.Segments), maxSegments)
	}

	seen := make(map[uint16]struct{}, len(manifest.Segments))
	previous := -1
	for i := range manifest.Segments {
		meta := &manifest.Segments[i]
		if int(meta.Prefix) <= previous {
			return fmt.Errorf("%w: segments not strictly sorted at %04x", ErrInvalidManifest, meta.Prefix)
		}
		previous = int(meta.Prefix)
		if _, duplicate := seen[meta.Prefix]; duplicate {
			return fmt.Errorf("%w: duplicate segment %04x", ErrInvalidManifest, meta.Prefix)
		}
		seen[meta.Prefix] = struct{}{}
		if manifest.PrefixBytes == 1 && meta.Prefix > 0xff {
			return fmt.Errorf("%w: segment %04x does not fit one-byte prefixes", ErrInvalidManifest, meta.Prefix)
		}
		if meta.Count <= 0 || meta.BloomBits <= 0 || meta.BloomK <= 0 {
			return fmt.Errorf("%w: segment %04x has invalid counts", ErrInvalidManifest, meta.Prefix)
		}
		if meta.Size <= 0 || meta.Size > maxSegmentBytes {
			return fmt.Errorf("%w: segment %04x has invalid size %d", ErrInvalidManifest, meta.Prefix, meta.Size)
		}
		wantFile := fmt.Sprintf("segments/%04x.seg", meta.Prefix)
		if meta.File != wantFile {
			return fmt.Errorf("%w: segment %04x has file %q, want %q", ErrInvalidManifest, meta.Prefix, meta.File, wantFile)
		}
	}
	return nil
}

func marshalLocalManifest(manifest *edgeset.Manifest) ([]byte, error) {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("edgecache: encode local manifest: %w", err)
	}
	return append(body, '\n'), nil
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("edgecache: empty server URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("edgecache: parse server URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("edgecache: server URL must use http or https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("edgecache: server URL has no host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("edgecache: server URL must not contain a query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func newHTTPClient(opts Options) (*http.Client, error) {
	if opts.Timeout < 0 {
		return nil, fmt.Errorf("edgecache: negative HTTP timeout %v", opts.Timeout)
	}
	var client *http.Client
	if opts.HTTPClient != nil {
		clone := *opts.HTTPClient
		client = &clone
	} else {
		client = &http.Client{}
	}
	switch {
	case opts.Timeout > 0:
		client.Timeout = opts.Timeout
	case client.Timeout == 0:
		client.Timeout = defaultHTTPTimeout
	}
	return client, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return raw, nil
}

func responseError(operation, requestURL string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Errorf("edgecache: %s %s: status %d", operation, requestURL, resp.StatusCode)
	}
	return fmt.Errorf("edgecache: %s %s: status %d: %s", operation, requestURL, resp.StatusCode, text)
}

func prefixFor(h edgeset.Hash, prefixBytes int) uint16 {
	if prefixBytes <= 1 {
		return uint16(h[0])
	}
	return uint16(h[0])<<8 | uint16(h[1])
}

// writeFileAtomic writes data to a temporary file in the destination directory
// and renames it into place, so readers see either the old or the new file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("edgecache: create temporary file for %s: %w", filepath.Base(path), err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("edgecache: write %s: %w", filepath.Base(path), err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("edgecache: sync %s: %w", filepath.Base(path), err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("edgecache: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(tempPath, perm); err != nil {
		return fmt.Errorf("edgecache: chmod %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("edgecache: install %s: %w", filepath.Base(path), err)
	}
	cleanup = false
	return nil
}
