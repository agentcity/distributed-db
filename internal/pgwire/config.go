package pgwire

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"

	"distributed-db/internal/sqlengine"
)

// Config configures optional TLS and password authentication for a Server.
//
// The zero value preserves the historical development behavior: plain TCP
// with no authentication. That mode is intended for local development and
// tests only, not for a network-exposed deployment.
//
// Password authentication always uses PostgreSQL cleartext password
// authentication (AuthenticationCleartextPassword), which sends the password
// over the wire. It therefore requires TLS. Config.Validate rejects an auth
// configuration without TLS, and NewWithConfig enforces the same rule.
type Config struct {
	// TLSConfig enables TLS when non-nil. The server clones it before use and
	// raises MinVersion to at least TLS 1.2. The config must provide a server
	// certificate either directly or through GetCertificate /
	// GetConfigForClient. It is mutually exclusive with the certificate files
	// below.
	TLSConfig *tls.Config

	// TLSCertFile and TLSKeyFile are a PEM server certificate/key pair. Both
	// must be set together. The pair is loaded and validated by NewWithConfig,
	// so an unreadable or mismatched pair fails startup. When set, the server
	// uses TLS 1.2 as the minimum version (TLS 1.3 is negotiated when the
	// client supports it).
	TLSCertFile string
	TLSKeyFile  string

	// AuthUser and AuthPassword enable built-in cleartext password
	// authentication for one user. Both must be set together. Verification is
	// constant-time. Requires TLS.
	AuthUser     string
	AuthPassword string

	// PasswordAuth, when non-nil, replaces AuthUser/AuthPassword with a custom
	// verifier. It must return true only for an accepted user/password pair.
	// The callback should compare secrets in constant time. Requires TLS.
	PasswordAuth func(user, password string) bool
}

// TLSEnabled reports whether either TLSConfig or a certificate file pair is
// configured.
func (c Config) TLSEnabled() bool {
	return c.TLSConfig != nil || c.TLSCertFile != "" || c.TLSKeyFile != ""
}

// AuthEnabled reports whether password authentication is configured.
func (c Config) AuthEnabled() bool {
	return c.PasswordAuth != nil || c.AuthUser != "" || c.AuthPassword != ""
}

// Validate checks the configuration before any network or filesystem work.
// It rejects partial certificate pairs, mixed TLS configuration styles, and
// password authentication without TLS (cleartext passwords must never travel
// over plaintext). It does not read certificate files; NewWithConfig performs
// that check.
func (c Config) Validate() error {
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("pgwire: TLS certificate and key files must be set together")
	}
	if c.TLSConfig != nil && c.TLSCertFile != "" {
		return errors.New("pgwire: TLSConfig and TLSCertFile/TLSKeyFile are mutually exclusive")
	}
	if c.PasswordAuth != nil && (c.AuthUser != "" || c.AuthPassword != "") {
		return errors.New("pgwire: PasswordAuth and AuthUser/AuthPassword are mutually exclusive")
	}
	if (c.AuthUser == "") != (c.AuthPassword == "") {
		return errors.New("pgwire: AuthUser and AuthPassword must be set together")
	}
	if c.AuthEnabled() && !c.TLSEnabled() {
		return errors.New("pgwire: password authentication requires TLS; refusing to send cleartext passwords over plaintext")
	}
	return nil
}

// NewWithConfig returns a server configured with cfg. It validates the
// configuration (including loading and checking a certificate/key file pair)
// before returning, so misconfiguration fails at startup rather than on a
// client connection.
//
// New remains available for the zero-configuration plain-TCP, no-auth mode.
func NewWithConfig(engine *sqlengine.Engine, logger *slog.Logger, cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsConfig, err := cfg.buildTLSConfig()
	if err != nil {
		return nil, err
	}
	auth, err := cfg.buildAuthenticator()
	if err != nil {
		return nil, err
	}

	s := New(engine, logger)
	s.tlsConfig = tlsConfig
	s.auth = auth
	return s, nil
}

// buildTLSConfig clones and normalizes the caller's TLS configuration, or
// loads the certificate pair. Returning nil means plain TCP.
func (c Config) buildTLSConfig() (*tls.Config, error) {
	if c.TLSConfig != nil {
		cfg := c.TLSConfig.Clone()
		if cfg.MinVersion < tls.VersionTLS12 {
			// Do not pin MaxVersion: TLS 1.3 is negotiated whenever both
			// peers support it, and TLS 1.2 remains available for older
			// clients.
			cfg.MinVersion = tls.VersionTLS12
		}
		if cfg.MaxVersion != 0 && cfg.MaxVersion < cfg.MinVersion {
			return nil, fmt.Errorf("pgwire: TLS MaxVersion %#x is below MinVersion %#x", cfg.MaxVersion, cfg.MinVersion)
		}
		if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetConfigForClient == nil {
			return nil, errors.New("pgwire: TLSConfig does not provide a server certificate")
		}
		return cfg, nil
	}
	if c.TLSCertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("pgwire: load TLS certificate/key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// buildAuthenticator returns nil when authentication is disabled.
func (c Config) buildAuthenticator() (*authenticator, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	switch {
	case c.PasswordAuth != nil:
		return &authenticator{check: c.PasswordAuth}, nil
	case c.AuthUser != "":
		return &authenticator{
			userHash: sha256.Sum256([]byte(c.AuthUser)),
			passHash: sha256.Sum256([]byte(c.AuthPassword)),
		}, nil
	default:
		return nil, nil
	}
}

// authenticator verifies a password supplied in a PasswordMessage.
type authenticator struct {
	userHash [sha256.Size]byte
	passHash [sha256.Size]byte
	check    func(user, password string) bool
}

// verify compares credentials in constant time. Both hashes are always
// computed and compared, regardless of whether the user matched.
func (a *authenticator) verify(user, password string) bool {
	if a.check != nil {
		return a.check(user, password)
	}
	userHash := sha256.Sum256([]byte(user))
	passHash := sha256.Sum256([]byte(password))
	userOK := subtle.ConstantTimeCompare(userHash[:], a.userHash[:])
	passOK := subtle.ConstantTimeCompare(passHash[:], a.passHash[:])
	return userOK&passOK == 1
}
