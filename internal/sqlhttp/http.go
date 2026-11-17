// Package sqlhttp exposes the P0 SQL engine over HTTP.
//
// # Security model
//
// The zero Config is the historical development mode: plaintext HTTP with no
// authentication. It is suitable only for local development and tests. A
// network-exposed deployment should set TLS (TLSConfig or TLSCertFile plus
// TLSKeyFile) and AuthToken.
//
// AuthToken enables a single Bearer-token scheme. When it is non-empty, every
// endpoint except the liveness endpoints GET /health and GET /healthz requires
//
//	Authorization: Bearer <token>
//
// The comparison runs in constant time. A missing or incorrect token gets 401
// with a WWW-Authenticate challenge, and the token is never included in a
// response, an error, or a log line. /metrics and /debug/slo are protected by
// the same token; configure the Prometheus scrape job to send it. The liveness
// endpoints stay public so container and load-balancer probes keep working.
//
// Config.Validate rejects AuthToken without TLS, and NewServer applies that
// policy before serving. NewWithConfig is the lower-level constructor: it
// enforces authentication but deliberately does not validate the listener, so
// callers that terminate TLS in a trusted reverse proxy can use it. NewServer
// is the recommended entry point for a directly exposed listener.
package sqlhttp

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
)

// maxSQLRequestBody bounds the POST /v1/sql request body. Requests larger than
// this are rejected by http.MaxBytesReader before the engine sees them.
const maxSQLRequestBody = 1 << 20

// Config configures a Handler. Engine is required. Metrics, when non-nil, is
// exposed as Prometheus text at /metrics; SLO, when non-nil, backs /debug/slo.
//
// The zero value of the security fields is the development mode: plaintext
// HTTP without authentication. Set TLSConfig or TLSCertFile/TLSKeyFile to
// serve TLS, and AuthToken to require Bearer authentication. See the package
// documentation for the endpoint policy.
type Config struct {
	Engine  *sqlengine.Engine
	Metrics *metrics.Registry
	SLO     *metrics.SLO

	// TLSConfig optionally enables TLS. NewServer clones it before use (the
	// caller's config is never mutated) and raises MinVersion to at least TLS
	// 1.2. It must provide a server certificate, either directly in
	// Certificates, through GetCertificate/GetConfigForClient, or together
	// with TLSCertFile/TLSKeyFile below.
	TLSConfig *tls.Config

	// TLSCertFile and TLSKeyFile are a PEM server certificate/key pair used
	// for the TLS listener. Both must be set together. NewServer loads and
	// validates the pair at startup, so an unreadable or mismatched pair fails
	// fast. The pair may be combined with TLSConfig; the certificate is then
	// appended to the cloned config.
	TLSCertFile string
	TLSKeyFile  string

	// AuthToken, when non-empty, requires "Authorization: Bearer <token>" on
	// /v1/sql, /metrics, and /debug/slo. The comparison is constant time. Auth
	// requires TLS: Config.Validate and NewServer reject a token without a TLS
	// listener. /health and /healthz stay public.
	AuthToken string
}

// TLSEnabled reports whether a TLS listener is configured through either
// TLSConfig or a certificate/key pair.
func (c Config) TLSEnabled() bool {
	return c.TLSConfig != nil || c.TLSCertFile != "" || c.TLSKeyFile != ""
}

// AuthEnabled reports whether Bearer authentication is configured.
func (c Config) AuthEnabled() bool {
	return c.AuthToken != ""
}

// Validate checks the structural configuration before any filesystem or
// network work. It rejects a partial certificate pair, a TLS config without a
// certificate, a TLS version range that excludes TLS 1.2, and Bearer
// authentication without TLS. It does not read certificate files; NewServer
// loads and validates them.
func (c Config) Validate() error {
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("sqlhttp: TLS certificate and key files must be set together")
	}
	if c.TLSConfig != nil {
		if c.TLSConfig.MaxVersion != 0 && c.TLSConfig.MaxVersion < tls.VersionTLS12 {
			return fmt.Errorf("sqlhttp: TLS MaxVersion %#x is below TLS 1.2", c.TLSConfig.MaxVersion)
		}
		if c.TLSCertFile == "" &&
			len(c.TLSConfig.Certificates) == 0 &&
			c.TLSConfig.GetCertificate == nil &&
			c.TLSConfig.GetConfigForClient == nil {
			return errors.New("sqlhttp: TLSConfig does not provide a server certificate")
		}
	}
	if c.AuthEnabled() && !c.TLSEnabled() {
		return errors.New("sqlhttp: Bearer authentication requires TLS; refusing to accept tokens over plaintext")
	}
	return nil
}

// buildTLSConfig clones and normalizes the caller's TLS configuration and
// loads the certificate pair, if any. A nil result means plaintext HTTP.
func (c Config) buildTLSConfig() (*tls.Config, error) {
	var cfg *tls.Config
	if c.TLSConfig != nil {
		cfg = c.TLSConfig.Clone()
	}
	if c.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("sqlhttp: load TLS certificate/key pair: %w", err)
		}
		if cfg == nil {
			cfg = &tls.Config{}
		}
		cfg.Certificates = append(cfg.Certificates, cert)
	}
	if cfg == nil {
		return nil, nil
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		// Do not pin MaxVersion: TLS 1.3 is negotiated whenever both peers
		// support it, while TLS 1.2 keeps older HTTP clients working.
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.MaxVersion != 0 && cfg.MaxVersion < cfg.MinVersion {
		return nil, fmt.Errorf("sqlhttp: TLS MaxVersion %#x is below MinVersion %#x", cfg.MaxVersion, cfg.MinVersion)
	}
	if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetConfigForClient == nil {
		return nil, errors.New("sqlhttp: TLS configuration does not provide a server certificate")
	}
	return cfg, nil
}

// NewServer validates cfg, loads its TLS material, and returns an *http.Server
// ready for Serve/ListenAndServe (plaintext) or ServeTLS/ListenAndServeTLS
// (TLS). The caller owns the listener and the server lifecycle.
//
// NewServer is the production entry point: it rejects misconfigurations,
// including Bearer authentication without TLS, before the listener starts.
// Use srv.TLSConfig != nil to decide which listen method to call.
func NewServer(cfg Config, addr string) (*http.Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsConfig, err := cfg.buildTLSConfig()
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           NewWithConfig(cfg),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}, nil
}

// Handler serves POST /v1/sql, GET /health, GET /healthz, GET /metrics, and
// GET /debug/slo. When authentication is configured, all endpoints except
// /health and /healthz require a Bearer token.
type Handler struct {
	engine         *sqlengine.Engine
	metricsHandler http.Handler

	authEnabled   bool
	authTokenHash [sha256.Size]byte
}

// New returns an http.Handler backed by engine without metrics endpoints
// configured. /metrics still responds, but with no samples.
func New(engine *sqlengine.Engine) *Handler {
	return NewWithConfig(Config{Engine: engine})
}

// NewWithConfig returns an http.Handler backed by cfg.Engine that also
// exposes the configured metrics registry and SLO.
//
// NewWithConfig enforces cfg.AuthToken but does not validate the listener's
// TLS configuration (it does not own a listener). Use NewServer for a directly
// exposed server, where TLS and authentication policy are validated together.
func NewWithConfig(cfg Config) *Handler {
	h := &Handler{
		engine:         cfg.Engine,
		metricsHandler: metrics.NewHTTPHandler(cfg.Metrics, cfg.SLO),
	}
	if cfg.AuthToken != "" {
		h.authEnabled = true
		h.authTokenHash = sha256.Sum256([]byte(cfg.AuthToken))
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.authEnabled && !publicEndpoint(r) && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sqlhttp"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/metrics",
		r.Method == http.MethodGet && r.URL.Path == "/debug/slo",
		r.Method == http.MethodGet && r.URL.Path == "/healthz":
		h.metricsHandler.ServeHTTP(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sql":
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSQLRequestBody)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if req.SQL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sql is required"})
			return
		}
		res, err := h.engine.Execute(r.Context(), req.SQL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// publicEndpoint reports whether a path is exempt from authentication. The
// liveness probes are intentionally public so orchestrators and load balancers
// can check the process without holding the API token.
func publicEndpoint(r *http.Request) bool {
	return r.URL.Path == "/health" || r.URL.Path == "/healthz"
}

// authorized checks the Bearer token in constant time. Tokens are hashed to a
// fixed size before comparison so the length of the configured secret does not
// affect the comparison time.
func (h *Handler) authorized(r *http.Request) bool {
	token, ok := bearerToken(r.Header.Values("Authorization"))
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], h.authTokenHash[:]) == 1
}

// bearerToken extracts the token from exactly one "Bearer <token>"
// Authorization header value. Multiple Authorization headers are rejected to
// avoid proxy/backend disagreement about which credential to trust.
func bearerToken(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
