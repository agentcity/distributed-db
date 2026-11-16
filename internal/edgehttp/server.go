// Package edgehttp exposes an immutable edgeset Store over HTTP.
//
// A Server is constructed with NewServer or NewServerWithOptions, which loads
// and fully verifies the store (manifest signature, segment sizes, checksums,
// headers, compression and Merkle roots) before any request is served. A
// tampered store therefore fails at startup instead of serving unverified
// bytes.
//
// Endpoints:
//
//	GET /healthz                 liveness probe
//	GET /v1/manifest             manifest JSON, including its signature
//	GET /v1/manifest/version     schema version JSON
//	GET /v1/segment/{prefix}     raw verified segment bytes (4 hex characters)
//	GET /v1/lookup/{hash}        exact lookup, proof on success
//
// GET /v1/lookup/{hash}?absence=1 additionally returns an absence_proof for a
// hash that is not in the set but whose segment exists. The proof can be
// verified with edgeset.VerifyAbsence against the segment root returned in the
// same response (and that root must match an authenticated manifest).
//
// Segment responses are safe to serve through a CDN: they are immutable,
// cacheable for a year, carry a strong SHA-256 ETag and support single and
// multiple byte ranges.
//
// NewServerWithOptions adds optional Bearer-token authentication, per-IP token
// bucket rate limiting and TLS. /healthz is exempt from auth and rate limiting
// by default; set Options.ProtectHealthz to change that.
package edgehttp

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"distributed-db/internal/edgeset"
)

// maxErrorBodyBytes bounds the error text returned from a failed lookup.
const maxErrorBodyBytes = 512

// defaultReadHeaderTimeout protects against slow-header clients when Options
// does not specify a value.
const defaultReadHeaderTimeout = 10 * time.Second

// Options configures NewServerWithOptions.
//
// The zero value serves an unsigned store over plain HTTP without
// authentication or rate limiting. Callers that need integrity should set
// Verifier (for example edgeset.NewHMACVerifier(key) or
// edgeset.NewEd25519Verifier(pub)).
type Options struct {
	// Verifier authenticates the manifest. A nil Verifier disables signature
	// verification but still validates the manifest structure and every
	// segment. Use edgeset.NewHMACVerifier for legacy HMAC-SHA256 stores and
	// edgeset.NewEd25519Verifier for Ed25519 stores.
	Verifier edgeset.Verifier

	// TLSConfig optionally enables TLS for ServeTLS/StartTLS. It is cloned by
	// the constructor; the caller's config is never mutated.
	TLSConfig *tls.Config

	// TLSCertFile and TLSKeyFile are a PEM certificate/key pair used by
	// ServeTLS/StartTLS and exposed through TLSConfig. Both must be set
	// together. CertFile/KeyFile are accepted as aliases.
	TLSCertFile string
	TLSKeyFile  string
	CertFile    string
	KeyFile     string

	// TLSCert and TLSKey are aliases for TLSCertFile and TLSKeyFile.
	TLSCert string
	TLSKey  string
	// TLSCertPath and TLSKeyPath are additional aliases.
	TLSCertPath string
	TLSKeyPath  string

	// AuthToken, when non-empty, requires "Authorization: Bearer <token>" on
	// every endpoint except an exempt /healthz. Comparison is constant time.
	AuthToken string

	// RateLimit is the sustained request rate per remote IP in requests per
	// second. Zero disables rate limiting. Negative or non-finite values are
	// rejected.
	RateLimit float64

	// RateBurst is the per-IP token bucket burst. Zero defaults to
	// ceil(RateLimit), with a minimum of one. Burst is accepted as an alias.
	RateBurst int
	Burst     int

	// RateLimitPerIP is accepted for compatibility and documentation. Limiting
	// is always performed per remote IP. PerIP is an alias.
	RateLimitPerIP bool
	PerIP          bool

	// ReadHeaderTimeout bounds request header reads. Zero selects ten seconds.
	ReadHeaderTimeout time.Duration

	// MaxSegmentBytes refuses to serve a segment larger than this many stored
	// (possibly compressed) bytes with 413. Zero means unlimited.
	MaxSegmentBytes int64

	// ProtectHealthz makes /healthz require auth and consume rate limit. By
	// default /healthz is exempt. RequireAuthForHealthz and HealthzAuth are
	// aliases.
	ProtectHealthz        bool
	RequireAuthForHealthz bool
	HealthzAuth           bool

	// ExemptHealthz explicitly exempts /healthz. It is redundant with the
	// default but makes the intent visible. HealthzExempt and PublicHealthz are
	// aliases.
	ExemptHealthz bool
	HealthzExempt bool
	PublicHealthz bool
}

// Server is an http.Handler backed by one immutable edgeset Store.
type Server struct {
	dir          string
	store        *edgeset.Store
	manifest     *edgeset.Manifest
	manifestJSON []byte
	manifestETag string
	opts         Options
	handler      http.Handler

	tlsConfig         *tls.Config
	tlsCertFile       string
	tlsKeyFile        string
	readHeaderTimeout time.Duration
	maxSegmentBytes   int64

	authToken     string
	rateLimit     float64
	rateBurst     int
	protectHealth bool

	rateMu        sync.Mutex
	limiters      map[string]*ipLimiter
	lastRateSweep time.Time
}

// ipLimiter is one remote IP's token bucket plus its last-seen time.
type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New is an alias for NewServer.
func New(dir string, key []byte) (*Server, error) {
	return NewServer(dir, key)
}

// NewServer loads dir with the optional HMAC key and returns a ready handler.
// It is equivalent to NewServerWithOptions with a legacy HMAC verifier.
func NewServer(dir string, key []byte) (*Server, error) {
	return NewServerWithOptions(dir, Options{Verifier: edgeset.NewHMACVerifier(key)})
}

// NewServerWithOptions loads dir and returns a fully verified HTTP handler.
//
// It returns an error for a missing or structurally invalid manifest, a bad
// signature, any segment that does not match its manifest metadata, invalid
// options, or an unreadable TLS certificate/key pair.
func NewServerWithOptions(dir string, opts Options) (*Server, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("edgehttp: empty store directory")
	}
	if opts.RateLimit < 0 || math.IsNaN(opts.RateLimit) || math.IsInf(opts.RateLimit, 0) {
		return nil, fmt.Errorf("edgehttp: RateLimit %v must be a finite non-negative number", opts.RateLimit)
	}
	if opts.ReadHeaderTimeout < 0 {
		return nil, fmt.Errorf("edgehttp: ReadHeaderTimeout must not be negative")
	}
	if opts.MaxSegmentBytes < 0 {
		return nil, fmt.Errorf("edgehttp: MaxSegmentBytes must not be negative")
	}

	tlsCertFile, tlsKeyFile, err := resolveTLSPaths(opts)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := buildTLSConfig(opts.TLSConfig, tlsCertFile, tlsKeyFile)
	if err != nil {
		return nil, err
	}

	store, manifest, err := loadStore(dir, opts.Verifier)
	if err != nil {
		return nil, fmt.Errorf("edgehttp: load store: %w", err)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("edgehttp: encode manifest: %w", err)
	}
	body = append(body, '\n')
	sum := sha256.Sum256(body)

	readHeaderTimeout := opts.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}
	rateBurst := opts.RateBurst
	if rateBurst <= 0 {
		rateBurst = opts.Burst
	}
	if opts.RateLimit > 0 && rateBurst <= 0 {
		rateBurst = int(math.Ceil(opts.RateLimit))
		if rateBurst < 1 {
			rateBurst = 1
		}
	}

	s := &Server{
		dir:               filepath.Clean(dir),
		store:             store,
		manifest:          manifest,
		manifestJSON:      body,
		manifestETag:      quoteETag(hex.EncodeToString(sum[:])),
		opts:              opts,
		tlsConfig:         tlsConfig,
		tlsCertFile:       tlsCertFile,
		tlsKeyFile:        tlsKeyFile,
		readHeaderTimeout: readHeaderTimeout,
		maxSegmentBytes:   opts.MaxSegmentBytes,
		authToken:         opts.AuthToken,
		rateLimit:         opts.RateLimit,
		rateBurst:         rateBurst,
		protectHealth:     healthzProtected(opts),
		limiters:          make(map[string]*ipLimiter),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/manifest", s.handleManifest)
	mux.HandleFunc("GET /v1/manifest/version", s.handleVersion)
	mux.HandleFunc("GET /v1/segment/{prefix}", s.handleSegment)
	mux.HandleFunc("GET /v1/lookup/{hash}", s.handleLookup)

	var handler http.Handler = mux
	handler = s.rateLimitMiddleware(handler)
	handler = s.authMiddleware(handler)
	s.handler = handler
	return s, nil
}

// healthzProtected resolves the exemption aliases. The default is exempt so a
// liveness probe keeps working when auth/rate limiting is enabled.
func healthzProtected(opts Options) bool {
	if opts.ExemptHealthz || opts.HealthzExempt || opts.PublicHealthz {
		return false
	}
	return opts.ProtectHealthz || opts.RequireAuthForHealthz || opts.HealthzAuth
}

// loadStore loads with signature verification according to verifier. A nil
// verifier keeps the structural and segment checks but skips signatures.
func loadStore(dir string, verifier edgeset.Verifier) (*edgeset.Store, *edgeset.Manifest, error) {
	return edgeset.LoadStoreVerified(dir, verifier)
}

// resolveTLSPaths merges the public cert/key path aliases.
func resolveTLSPaths(opts Options) (certFile, keyFile string, err error) {
	certFile = firstNonEmpty(opts.TLSCertFile, opts.CertFile, opts.TLSCert, opts.TLSCertPath)
	keyFile = firstNonEmpty(opts.TLSKeyFile, opts.KeyFile, opts.TLSKey, opts.TLSKeyPath)
	if (certFile == "") != (keyFile == "") {
		return "", "", fmt.Errorf("edgehttp: TLS certificate and key paths must be set together")
	}
	return certFile, keyFile, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// buildTLSConfig clones the caller's config and, when cert/key files are
// supplied, loads them so startup fails fast on an unreadable pair.
func buildTLSConfig(base *tls.Config, certFile, keyFile string) (*tls.Config, error) {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	}
	if certFile == "" {
		return cfg, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("edgehttp: load TLS key pair: %w", err)
	}
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg.Certificates = append(cfg.Certificates, cert)
	return cfg, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler returns the immutable middleware-wrapped handler built at startup.
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
	return cloneManifest(s.manifest)
}

// TLSConfig returns a clone of the configured TLS config, or nil when TLS was
// not configured.
func (s *Server) TLSConfig() *tls.Config {
	if s == nil || s.tlsConfig == nil {
		return nil
	}
	return s.tlsConfig.Clone()
}

// HTTPHandlerServer returns an http.Server using this handler and the
// configured TLS and read-header settings. The caller owns its lifecycle.
func (s *Server) HTTPHandlerServer() *http.Server {
	return &http.Server{
		Handler:           s.handler,
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: s.readHeaderTimeout,
		IdleTimeout:       2 * time.Minute,
	}
}

// HTTPServer is an alias for HTTPHandlerServer.
func (s *Server) HTTPServer() *http.Server {
	return s.HTTPHandlerServer()
}

// ServeTLS serves TLS on ln. The optional certFile/keyFile arguments use the
// certificate pair from Options (or Options.TLSConfig) when omitted or empty.
// The call blocks until the server stops.
func (s *Server) ServeTLS(ln net.Listener, files ...string) error {
	certFile, keyFile := "", ""
	if len(files) > 0 {
		certFile = files[0]
	}
	if len(files) > 1 {
		keyFile = files[1]
	}
	if certFile == "" {
		certFile = s.tlsCertFile
	}
	if keyFile == "" {
		keyFile = s.tlsKeyFile
	}
	return s.HTTPHandlerServer().ServeTLS(ln, certFile, keyFile)
}

// StartTLS is ServeTLS using the certificate pair from Options. It is useful
// when TLSConfig already carries certificates.
func (s *Server) StartTLS(ln net.Listener, files ...string) error {
	return s.ServeTLS(ln, files...)
}

// ListenAndServeTLS listens on addr and serves TLS. Omitted certFile/keyFile
// use the Options pair.
func (s *Server) ListenAndServeTLS(addr string, files ...string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeTLS(ln, files...)
}

func cloneManifest(m *edgeset.Manifest) *edgeset.Manifest {
	if m == nil {
		return nil
	}
	out := &edgeset.Manifest{
		Version:      m.Version,
		PrefixBytes:  m.PrefixBytes,
		SignatureAlg: m.SignatureAlg,
		Segments:     append([]edgeset.SegmentMeta(nil), m.Segments...),
	}
	if m.Signature != nil {
		out.Signature = append([]byte(nil), m.Signature...)
	}
	return out
}

// authMiddleware enforces a constant-time Bearer token check.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.authToken == "" {
		return next
	}
	token := []byte(s.authToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.healthzExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(got), token) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="edgehash"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from a "Bearer <token>" Authorization value.
func bearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

// rateLimitMiddleware applies a per-IP token bucket.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	if s.rateLimit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.healthzExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.allow(remoteIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(s.retryAfterSeconds()))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// healthzExempt reports whether the request is the exempt liveness endpoint.
func (s *Server) healthzExempt(r *http.Request) bool {
	return r.URL.Path == "/healthz" && !s.protectHealth
}

func (s *Server) retryAfterSeconds() int {
	if s.rateLimit <= 0 {
		return 1
	}
	seconds := int(math.Ceil(1 / s.rateLimit))
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// allow consumes one token from ip's bucket. Limiters are retained briefly so
// a burst of requests from one client is tracked consistently.
func (s *Server) allow(ip string) bool {
	now := time.Now()

	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.limiters == nil {
		s.limiters = make(map[string]*ipLimiter)
	}
	if now.Sub(s.lastRateSweep) >= time.Minute {
		for key, entry := range s.limiters {
			if now.Sub(entry.lastSeen) > 10*time.Minute {
				delete(s.limiters, key)
			}
		}
		s.lastRateSweep = now
	}
	entry := s.limiters[ip]
	if entry == nil {
		entry = &ipLimiter{limiter: rate.NewLimiter(rate.Limit(s.rateLimit), s.rateBurst)}
		s.limiters[ip] = entry
	}
	entry.lastSeen = now
	return entry.limiter.Allow()
}

func remoteIP(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		return host
	}
	if addr == "" {
		return "unknown"
	}
	return addr
}

// lookupResponse is the wire form of GET /v1/lookup/{hash}.
type lookupResponse struct {
	Present      bool                  `json:"present"`
	Proof        *edgeset.Proof        `json:"proof,omitempty"`
	AbsenceProof *edgeset.AbsenceProof `json:"absence_proof,omitempty"`
	Root         string                `json:"root"`
	Prefix       string                `json:"prefix"`
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
	if s.maxSegmentBytes > 0 && meta.Size > s.maxSegmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "segment exceeds configured size limit")
		return
	}

	// Segment verifies the stored bytes against the manifest checksum again. A
	// file modified after startup is therefore rejected rather than served.
	data, err := s.store.Segment(prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "segment unavailable")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", quoteETag(meta.SHA256.String()))
	h.Set("Accept-Ranges", "bytes")

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && !validByteRangeHeader(rangeHeader) {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid range")
		return
	}

	// ServeContent implements single and multipart ranges, If-Range, 206/416
	// and conditional requests. The ETag is strong and derived from SHA-256.
	// Bytes are always served verbatim (compressed segments are not decoded).
	http.ServeContent(w, r, filepath.Base(meta.File), time.Time{}, bytes.NewReader(data))
}

// validByteRangeHeader performs a conservative syntax check before handing the
// request to http.ServeContent. ServeContent deliberately ignores some
// malformed Range values; the edge API answers those with 416 instead.
func validByteRangeHeader(header string) bool {
	header = strings.TrimSpace(header)
	const prefix = "bytes="
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	for _, spec := range strings.Split(header[len(prefix):], ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return false
		}
		dash := strings.IndexByte(spec, '-')
		if dash < 0 || strings.IndexByte(spec[dash+1:], '-') >= 0 {
			return false
		}
		start, end := spec[:dash], spec[dash+1:]
		if start == "" && end == "" {
			return false
		}
		for _, digits := range []string{start, end} {
			for i := 0; i < len(digits); i++ {
				if digits[i] < '0' || digits[i] > '9' {
					return false
				}
			}
		}
	}
	return true
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
		meta, ok := s.store.SegmentMeta(prefix)
		if !ok || proof == nil || !edgeset.VerifyProof(meta.Root, h, proof) {
			writeError(w, http.StatusInternalServerError, "invalid inclusion proof")
			return
		}
		resp.Root = meta.Root.String()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if wantsAbsence(r) {
		absence, err := s.store.ProveAbsent(h)
		switch {
		case err == nil:
			if !edgeset.VerifyAbsence(absence.Root, h, absence) {
				writeError(w, http.StatusInternalServerError, "invalid absence proof")
				return
			}
			resp.AbsenceProof = absence
			resp.Root = absence.Root.String()
		case errors.Is(err, edgeset.ErrSegmentNotFound):
			// The manifest itself proves there is no covering segment; there is
			// no Merkle proof to build, so the response stays proof-free.
		default:
			writeError(w, http.StatusInternalServerError, "absence proof unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// wantsAbsence reports whether the lookup asked for a non-membership proof.
// "?absence" and "?absence=1" request it; 0/false/no/off disable it.
func wantsAbsence(r *http.Request) bool {
	query := r.URL.Query()
	if !query.Has("absence") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(query.Get("absence"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
