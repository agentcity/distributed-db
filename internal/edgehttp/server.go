// Package edgehttp exposes an immutable edgeset Store over HTTP.
//
// A Server is constructed with NewServer, which loads and fully verifies the
// store (manifest HMAC, segment sizes, checksums, headers and Merkle roots)
// before any request is served. A tampered store therefore fails at startup
// instead of serving unverified bytes.
//
// Endpoints:
//
//	GET /healthz                 liveness probe
//	GET /v1/manifest             manifest JSON, including its signature
//	GET /v1/manifest/version     schema version JSON
//	GET /v1/segment/{prefix}     raw verified segment bytes (4 hex characters)
//	GET /v1/lookup/{hash}        exact lookup, proof on success
//
// The server is read-only and safe for concurrent use. It does not implement
// authentication, rate limiting or TLS; put it behind a gateway when the
// network is untrusted.
package edgehttp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"distributed-db/internal/edgeset"
)

// maxErrorBodyBytes bounds the error text returned from a failed lookup.
const maxErrorBodyBytes = 512

// Server is an http.Handler backed by one immutable edgeset Store.
type Server struct {
	dir          string
	store        *edgeset.Store
	manifest     *edgeset.Manifest
	manifestJSON []byte
	manifestETag string
	handler      http.Handler
}

// New is an alias for NewServer.
func New(dir string, key []byte) (*Server, error) {
	return NewServer(dir, key)
}

// NewServer loads dir with the optional HMAC key and returns a ready handler.
// It returns an error for a missing or structurally invalid manifest, a bad
// signature, or any segment that does not match its manifest metadata.
func NewServer(dir string, key []byte) (*Server, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("edgehttp: empty store directory")
	}
	store, manifest, err := edgeset.LoadStore(dir, key)
	if err != nil {
		return nil, fmt.Errorf("edgehttp: load store: %w", err)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("edgehttp: encode manifest: %w", err)
	}
	body = append(body, '\n')
	sum := sha256.Sum256(body)

	s := &Server{
		dir:          filepath.Clean(dir),
		store:        store,
		manifest:     manifest,
		manifestJSON: body,
		manifestETag: quoteETag(hex.EncodeToString(sum[:])),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/manifest", s.handleManifest)
	mux.HandleFunc("GET /v1/manifest/version", s.handleVersion)
	mux.HandleFunc("GET /v1/segment/{prefix}", s.handleSegment)
	mux.HandleFunc("GET /v1/lookup/{hash}", s.handleLookup)
	s.handler = mux
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler returns the immutable handler built by NewServer.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Dir returns the store directory backing the server.
func (s *Server) Dir() string {
	return s.dir
}

// Store returns the verified store. Store itself is immutable and safe for
// concurrent use.
func (s *Server) Store() *edgeset.Store {
	return s.store
}

// Manifest returns a deep copy of the served manifest.
func (s *Server) Manifest() *edgeset.Manifest {
	if s == nil || s.manifest == nil {
		return nil
	}
	out := &edgeset.Manifest{
		Version:     s.manifest.Version,
		PrefixBytes: s.manifest.PrefixBytes,
		Segments:    append([]edgeset.SegmentMeta(nil), s.manifest.Segments...),
	}
	if s.manifest.Signature != nil {
		out.Signature = append([]byte(nil), s.manifest.Signature...)
	}
	return out
}

// lookupResponse is the wire form of GET /v1/lookup/{hash}. Root and Proof are
// only populated when the hash is present. Prefix is always the routing prefix
// of the queried hash.
type lookupResponse struct {
	Present bool           `json:"present"`
	Proof   *edgeset.Proof `json:"proof,omitempty"`
	Root    string         `json:"root"`
	Prefix  string         `json:"prefix"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", s.manifestETag)
	if etagMatches(r.Header.Get("If-None-Match"), s.manifestETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(s.manifestJSON)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.manifestJSON)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Version uint64 `json:"version"`
	}{Version: s.manifest.Version})
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	prefix, err := parsePrefix(r.PathValue("prefix"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta, ok := s.store.SegmentMeta(prefix)
	if !ok {
		writeError(w, http.StatusNotFound, "segment not found")
		return
	}

	etag := quoteETag(meta.SHA256.String())
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Segment verifies the bytes against the manifest checksum again. A file
	// modified after startup is therefore rejected rather than served.
	data, err := s.store.Segment(prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "segment unavailable")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	h, err := edgeset.ParseHash(r.PathValue("hash"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid hash: 64 hex characters required")
		return
	}
	present, proof, err := s.store.Contains(h)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	prefix := s.store.PrefixFor(h)
	resp := lookupResponse{
		Present: present,
		Proof:   proof,
		Prefix:  fmt.Sprintf("%04x", prefix),
	}
	if present {
		if meta, ok := s.store.SegmentMeta(prefix); ok {
			resp.Root = meta.Root.String()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parsePrefix accepts exactly four hexadecimal characters, the on-disk segment
// id used by edgeset.
func parsePrefix(s string) (uint16, error) {
	if len(s) != 4 {
		return 0, fmt.Errorf("invalid prefix: exactly 4 hex characters required")
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid prefix: exactly 4 hex characters required")
	}
	return uint16(v), nil
}

func quoteETag(value string) string {
	return `"` + value + `"`
}

// etagMatches implements the If-None-Match weak comparison used for immutable
// segment bytes and the manifest.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.HasPrefix(candidate, "W/") && candidate[2:] == etag {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	if len(msg) > maxErrorBodyBytes {
		msg = msg[:maxErrorBodyBytes]
	}
	writeJSON(w, code, map[string]string{"error": msg})
}
