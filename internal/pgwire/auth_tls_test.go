package pgwire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/sqlengine"
)

// testCert is a self-signed certificate for 127.0.0.1/localhost plus the
// trust pool a client should use to verify the server.
type testCert struct {
	certPEM     []byte
	keyPEM      []byte
	certificate tls.Certificate
	pool        *x509.CertPool
}

func newTestCert(t *testing.T) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pgwire test server"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse key pair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append certificate to pool")
	}
	return testCert{certPEM: certPEM, keyPEM: keyPEM, certificate: certificate, pool: pool}
}

func writeCertFiles(t *testing.T, cert testCert) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, cert.certPEM, 0o644); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, cert.keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startServerWithConfig(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	srv, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		srv.Stop()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, addr
}

// assertQueryRoundTrip runs a small DDL/DML/SELECT cycle to prove that the
// authenticated and/or encrypted connection is fully usable.
func assertQueryRoundTrip(t *testing.T, tc *testConn) {
	t.Helper()
	msgs := tc.query("CREATE TABLE tls_probe (id INT PRIMARY KEY)")
	tc.assertNoError(msgs)
	assertCommandTag(t, msgs, "OK")

	msgs = tc.query("INSERT INTO tls_probe VALUES (1)")
	tc.assertNoError(msgs)
	assertCommandTag(t, msgs, "INSERT 0 1")

	msgs = tc.query("SELECT id FROM tls_probe")
	tc.assertNoError(msgs)
	assertCommandTag(t, msgs, "SELECT 1")
	row := parseDataRow(t, msgOfType(msgs, msgDataRow))
	if len(row) != 1 || row[0] != "1" {
		t.Fatalf("SELECT id row = %v, want [1]", row)
	}
}

func dialTLS(t *testing.T, addr string, roots *x509.CertPool) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS dial %s: %v", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("SetDeadline: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTLSHandshakeWithGeneratedCert(t *testing.T) {
	cert := newTestCert(t)
	srv, addr := startServerWithConfig(t, Config{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.certificate}},
	})
	tlsCfg := srv.TLSConfig()
	if tlsCfg == nil {
		t.Fatal("TLSConfig() = nil, want configured TLS")
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", tlsCfg.MinVersion)
	}
	conn := dialTLS(t, addr, cert.pool)
	state := conn.ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("TLS handshake did not complete")
	}
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("negotiated TLS version %#x, want >= TLS 1.2", state.Version)
	}
	t.Logf("negotiated TLS version %s, cipher %s",
		tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))

	tc := &testConn{t: t, conn: conn}
	tc.startup(map[string]string{"user": "alice", "database": "testdb"})
	assertQueryRoundTrip(t, tc)
}

func TestTLSCertificateFiles(t *testing.T) {
	cert := newTestCert(t)
	certFile, keyFile := writeCertFiles(t, cert)
	_, addr := startServerWithConfig(t, Config{
		TLSCertFile: certFile,
		TLSKeyFile:  keyFile,
	})
	conn := dialTLS(t, addr, cert.pool)
	tc := &testConn{t: t, conn: conn}
	tc.startup(map[string]string{"user": "alice"})
	assertQueryRoundTrip(t, tc)
}

func TestZeroConfigPlainNoAuthStillWorks(t *testing.T) {
	srv, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{})
	if err != nil {
		t.Fatalf("NewWithConfig(zero): %v", err)
	}
	if srv.TLSConfig() != nil {
		t.Fatal("zero Config unexpectedly enabled TLS")
	}
	if srv.AuthEnabled() {
		t.Fatal("zero Config unexpectedly enabled auth")
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	c := dialTestConn(t, addr)
	c.startup(map[string]string{"user": "alice", "database": "testdb"})
	assertQueryRoundTrip(t, c)
}

func TestPasswordAuthCorrectAndWrong(t *testing.T) {
	cert := newTestCert(t)
	_, addr := startServerWithConfig(t, Config{
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{cert.certificate}},
		AuthUser:     "alice",
		AuthPassword: "correct horse",
	})

	t.Run("correct password", func(t *testing.T) {
		conn := dialTLS(t, addr, cert.pool)
		statuses, err := startupConnAuthed(conn, map[string]string{"user": "alice", "database": "testdb"}, "correct horse")
		if err != nil {
			t.Fatalf("startup with correct password: %v", err)
		}
		if statuses["server_version"] != "14.0" {
			t.Fatalf("server_version = %q, want 14.0", statuses["server_version"])
		}
		tc := &testConn{t: t, conn: conn}
		assertQueryRoundTrip(t, tc)
	})

	t.Run("wrong password", func(t *testing.T) {
		conn := dialTLS(t, addr, cert.pool)
		_, err := startupConnAuthed(conn, map[string]string{"user": "alice"}, "wrong")
		if err == nil {
			t.Fatal("startup with wrong password succeeded")
		}
		var se *startupError
		if !errors.As(err, &se) {
			t.Fatalf("startup error = %v, want *startupError", err)
		}
		if se.code != "28P01" {
			t.Fatalf("SQLSTATE = %q, want 28P01", se.code)
		}
		if !strings.Contains(se.message, "password authentication failed") {
			t.Fatalf("error message = %q, want authentication failure", se.message)
		}
		if strings.Contains(se.message, "wrong") {
			t.Fatalf("error message leaked the password: %q", se.message)
		}
	})

	t.Run("wrong user", func(t *testing.T) {
		conn := dialTLS(t, addr, cert.pool)
		_, err := startupConnAuthed(conn, map[string]string{"user": "bob"}, "correct horse")
		var se *startupError
		if !errors.As(err, &se) || se.code != "28P01" {
			t.Fatalf("wrong-user startup error = %v, want 28P01", err)
		}
	})
}

func TestPasswordAuthCallback(t *testing.T) {
	cert := newTestCert(t)
	cfg := Config{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.certificate}},
		PasswordAuth: func(user, password string) bool {
			return user == "svc" && password == "token"
		},
	}
	_, addr := startServerWithConfig(t, cfg)

	conn := dialTLS(t, addr, cert.pool)
	if _, err := startupConnAuthed(conn, map[string]string{"user": "svc"}, "token"); err != nil {
		t.Fatalf("callback accepted credentials rejected: %v", err)
	}

	conn = dialTLS(t, addr, cert.pool)
	_, err := startupConnAuthed(conn, map[string]string{"user": "svc"}, "bad")
	var se *startupError
	if !errors.As(err, &se) || se.code != "28P01" {
		t.Fatalf("callback rejection = %v, want 28P01", err)
	}
}

func TestAuthWithoutTLSRejected(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "built-in credentials", cfg: Config{AuthUser: "alice", AuthPassword: "secret"}},
		{name: "custom verifier", cfg: Config{PasswordAuth: func(string, string) bool { return true }}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires TLS") {
				t.Fatalf("Config.Validate() = %v, want requires-TLS error", err)
			}
			_, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), "requires TLS") {
				t.Fatalf("NewWithConfig() = %v, want requires-TLS error", err)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	cert := newTestCert(t)
	certFile, keyFile := writeCertFiles(t, cert)

	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "cert without key",
			cfg:     Config{TLSCertFile: certFile},
			wantErr: "set together",
		},
		{
			name:    "key without cert",
			cfg:     Config{TLSKeyFile: keyFile},
			wantErr: "set together",
		},
		{
			name:    "both TLS styles",
			cfg:     Config{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.certificate}}, TLSCertFile: certFile, TLSKeyFile: keyFile},
			wantErr: "mutually exclusive",
		},
		{
			name:    "user without password",
			cfg:     Config{AuthUser: "alice"},
			wantErr: "set together",
		},
		{
			name:    "password without user",
			cfg:     Config{AuthPassword: "secret"},
			wantErr: "set together",
		},
		{
			name:    "mixed auth styles",
			cfg:     Config{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.certificate}}, AuthUser: "alice", AuthPassword: "secret", PasswordAuth: func(string, string) bool { return true }},
			wantErr: "mutually exclusive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTLSConfigNormalization(t *testing.T) {
	cert := newTestCert(t)
	base := &tls.Config{
		Certificates: []tls.Certificate{cert.certificate},
		MinVersion:   tls.VersionTLS10,
	}
	srv, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{TLSConfig: base})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	got := srv.TLSConfig()
	if got.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}
	if got.MaxVersion != 0 {
		t.Fatalf("MaxVersion = %#x, want 0 (allow TLS 1.3)", got.MaxVersion)
	}
	if base.MinVersion != tls.VersionTLS10 {
		t.Fatalf("caller's TLSConfig was mutated: MinVersion = %#x", base.MinVersion)
	}

	_, err = NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert.certificate},
			MaxVersion:   tls.VersionTLS11,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "below MinVersion") {
		t.Fatalf("MaxVersion below TLS 1.2: err = %v", err)
	}

	_, err = NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert.certificate},
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS12,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "below MinVersion") {
		t.Fatalf("MaxVersion below MinVersion: err = %v", err)
	}

	_, err = NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{
		TLSConfig: &tls.Config{},
	})
	if err == nil || !strings.Contains(err.Error(), "does not provide a server certificate") {
		t.Fatalf("empty TLSConfig: err = %v", err)
	}
}

func TestStartupValidatesCertificatePair(t *testing.T) {
	cert := newTestCert(t)
	certFile, keyFile := writeCertFiles(t, cert)
	other := newTestCert(t)
	_, otherKeyFile := writeCertFiles(t, other)

	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "missing key", cfg: Config{TLSCertFile: certFile}},
		{name: "mismatched pair", cfg: Config{TLSCertFile: certFile, TLSKeyFile: otherKeyFile}},
		{name: "missing cert file", cfg: Config{TLSCertFile: filepath.Join(t.TempDir(), "missing.crt"), TLSKeyFile: keyFile}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), tc.cfg)
			if err == nil {
				t.Fatal("NewWithConfig succeeded, want certificate error")
			}
		})
	}

	t.Run("bad PEM", func(t *testing.T) {
		dir := t.TempDir()
		badCert := filepath.Join(dir, "bad.crt")
		badKey := filepath.Join(dir, "bad.key")
		if err := os.WriteFile(badCert, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(badKey, []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewWithConfig(sqlengine.New(newFakeClient()), newTestLogger(), Config{
			TLSCertFile: badCert,
			TLSKeyFile:  badKey,
		})
		if err == nil || !strings.Contains(err.Error(), "load TLS certificate/key pair") {
			t.Fatalf("bad PEM: err = %v, want load error", err)
		}
	})
}

func TestPasswordIsNotLogged(t *testing.T) {
	cert := newTestCert(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := NewWithConfig(sqlengine.New(newFakeClient()), logger, Config{
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{cert.certificate}},
		AuthUser:     "alice",
		AuthPassword: "super-secret-password",
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer srv.Stop()
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := dialTLS(t, addr, cert.pool)
	if _, err := startupConnAuthed(conn, map[string]string{"user": "alice"}, "super-secret-password-wrong"); err == nil {
		t.Fatal("wrong password was accepted")
	}
	srv.Stop()
	if strings.Contains(logs.String(), "super-secret-password") {
		t.Fatalf("server logs leaked the password:\n%s", logs.String())
	}
}

func TestTLSCertificateFilesWithPasswordAuth(t *testing.T) {
	cert := newTestCert(t)
	certFile, keyFile := writeCertFiles(t, cert)
	_, addr := startServerWithConfig(t, Config{
		TLSCertFile:  certFile,
		TLSKeyFile:   keyFile,
		AuthUser:     "alice",
		AuthPassword: "secret",
	})
	conn := dialTLS(t, addr, cert.pool)
	if _, err := startupConnAuthed(conn, map[string]string{"user": "alice"}, "secret"); err != nil {
		t.Fatalf("startup with file-based TLS and password: %v", err)
	}
}
