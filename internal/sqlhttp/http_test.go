package sqlhttp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/metrics"
	"distributed-db/internal/sqlengine"
)

type nopClient struct{}

func (nopClient) Begin(context.Context) (sqlengine.Txn, error) { return nopTxn{}, nil }

type nopTxn struct{}

func (nopTxn) Get(context.Context, []byte) ([]byte, error)                       { return nil, sqlengine.ErrNotFound }
func (nopTxn) Set([]byte, []byte)                                                {}
func (nopTxn) Delete([]byte)                                                     {}
func (nopTxn) Scan(context.Context, []byte, []byte, int) ([]sqlengine.KV, error) { return nil, nil }
func (nopTxn) Commit(context.Context) error                                      { return nil }
func (nopTxn) Rollback(context.Context) error                                    { return nil }

func TestHandlerBasics(t *testing.T) {
	h := New(sqlengine.New(nopClient{}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader([]byte("{")))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	body, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM missing"})
	resp, err = http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing table status = %v, %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
}

func newMetricsTestServer(t *testing.T) (*httptest.Server, *metrics.Registry, *sqlengine.SQLMetrics) {
	t.Helper()
	engine := sqlengine.New(nopClient{})
	reg := metrics.NewRegistry()
	sqlm := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine.SetMetrics(sqlm)
	h := NewWithConfig(Config{Engine: engine, Metrics: reg, SLO: sqlm.SLO})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, reg, sqlm
}

func postSQL(t *testing.T, srv *httptest.Server, sql string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(srv.URL+"/v1/sql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/sql: %v", err)
	}
	return resp
}

func TestMetricsEndpointsAfterSQL(t *testing.T) {
	srv, _, sqlm := newMetricsTestServer(t)

	resp := postSQL(t, srv, "CREATE TABLE metrics_test (id INT PRIMARY KEY)")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CREATE status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = postSQL(t, srv, "SELECT * FROM missing WHERE id = 424242")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("failing SELECT status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if got := sqlm.SLO.Requests.Value(); got != 2 {
		t.Fatalf("SQL SLO requests = %d, want 2", got)
	}
	if got := sqlm.SLO.Errors.Value(); got != 1 {
		t.Fatalf("SQL SLO errors = %d, want 1", got)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"sql_requests_total 2",
		"sql_errors_total 1",
		`sql_statement_requests_total{kind="DDL"} 1`,
		`sql_statement_errors_total{kind="SELECT"} 1`,
		`sql_statement_request_duration_seconds_count{kind="SELECT"} 1`,
		"# TYPE sql_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q:\n%s", want, body)
		}
	}
	for _, leaked := range []string{"424242", "metrics_test", "missing"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("/metrics leaked SQL text %q:\n%s", leaked, body)
		}
	}

	resp, err = http.Get(srv.URL + "/debug/slo")
	if err != nil {
		t.Fatalf("GET /debug/slo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/debug/slo status = %d, want 200", resp.StatusCode)
	}
	var report metrics.Report
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode /debug/slo: %v", err)
	}
	if report.Requests != 2 || report.Errors != 1 || report.ErrorRate != 0.5 {
		t.Fatalf("report = %+v, want requests=2 errors=1 rate=0.5", report)
	}

	resp, err = http.Get(srv.URL + "/debug/slo?format=text")
	if err != nil {
		t.Fatalf("GET /debug/slo?format=text: %v", err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "requests: 2") {
		t.Fatalf("/debug/slo text = %q", raw)
	}
}

func TestMetricsEndpointsWithoutConfiguration(t *testing.T) {
	srv := httptest.NewServer(New(sqlengine.New(nopClient{})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/debug/slo")
	if err != nil {
		t.Fatalf("GET /debug/slo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/debug/slo status = %d, want 404", resp.StatusCode)
	}
}

// testCertificate is a generated CA and server certificate for TLS tests.
type testCertificate struct {
	certFile string
	keyFile  string
	pool     *x509.CertPool
}

// newTestCertificate generates a CA and a server certificate valid for
// localhost and 127.0.0.1, writes the server pair to temporary files, and
// returns a pool that trusts the CA.
func newTestCertificate(t *testing.T) testCertificate {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sqlhttp-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "sqlhttp-test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o644); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return testCertificate{certFile: certFile, keyFile: keyFile, pool: pool}
}

// startTLSServer starts srv on a loopback listener and returns its base URL.
func startTLSServer(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-done:
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("ServeTLS: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("TLS server did not stop")
		}
	})
	return "https://" + ln.Addr().String()
}

// tlsClient returns an HTTP client that trusts the generated test CA.
func tlsClient(cert testCertificate) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    cert.pool,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
		},
	}}
}

func doJSONRequest(t *testing.T, client *http.Client, method, url, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestTLSHandshakeWithGeneratedCertificate(t *testing.T) {
	cert := newTestCertificate(t)
	srv, err := NewServer(Config{
		Engine:      sqlengine.New(nopClient{}),
		TLSCertFile: cert.certFile,
		TLSKeyFile:  cert.keyFile,
	}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	baseURL := startTLSServer(t, srv)

	conn, err := tls.Dial("tcp", strings.TrimPrefix(baseURL, "https://"), &tls.Config{
		RootCAs:    cert.pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("TLS handshake did not complete")
	}
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("negotiated TLS version %#x, want >= TLS 1.2", state.Version)
	}

	resp, err := tlsClient(cert).Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz over TLS status = %d, want 200", resp.StatusCode)
	}
}

func TestBearerAuthOverTLS(t *testing.T) {
	cert := newTestCertificate(t)
	reg := metrics.NewRegistry()
	sqlm := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine := sqlengine.New(nopClient{})
	engine.SetMetrics(sqlm)
	srv, err := NewServer(Config{
		Engine:      engine,
		Metrics:     reg,
		SLO:         sqlm.SLO,
		TLSCertFile: cert.certFile,
		TLSKeyFile:  cert.keyFile,
		AuthToken:   "s3cret-token",
	}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	baseURL := startTLSServer(t, srv)
	client := tlsClient(cert)

	// Liveness endpoints stay public.
	for _, path := range []string{"/health", "/healthz"} {
		resp := doJSONRequest(t, client, http.MethodGet, baseURL+path, "", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s without token status = %d, want 200", path, resp.StatusCode)
		}
	}

	// SQL endpoint: missing, wrong, and correct token.
	resp := doJSONRequest(t, client, http.MethodPost, baseURL+"/v1/sql", "",
		`{"sql":"SELECT * FROM missing"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}

	resp = doJSONRequest(t, client, http.MethodPost, baseURL+"/v1/sql", "wrong-token",
		`{"sql":"SELECT * FROM missing"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}

	resp = doJSONRequest(t, client, http.MethodPost, baseURL+"/v1/sql", "s3cret-token",
		`{"sql":"CREATE TABLE auth_test (id INT PRIMARY KEY)"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct token status = %d, want 200", resp.StatusCode)
	}

	// Unauthorized requests must not reach the engine: only the authorized
	// CREATE above is counted.
	if got := sqlm.SLO.Requests.Value(); got != 1 {
		t.Fatalf("SQL requests = %d, want 1 (unauthorized requests must not execute)", got)
	}

	// Metrics and SLO endpoints use the same token.
	for _, path := range []string{"/metrics", "/debug/slo"} {
		resp = doJSONRequest(t, client, http.MethodGet, baseURL+path, "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token status = %d, want 401", path, resp.StatusCode)
		}
		resp = doJSONRequest(t, client, http.MethodGet, baseURL+path, "s3cret-token", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s with token status = %d, want 200", path, resp.StatusCode)
		}
	}

	// Invalid and oversized bodies are still rejected after authentication.
	resp = doJSONRequest(t, client, http.MethodPost, baseURL+"/v1/sql", "s3cret-token", "{")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d, want 400", resp.StatusCode)
	}
	oversized, err := json.Marshal(map[string]string{"sql": strings.Repeat("a", maxSQLRequestBody+1)})
	if err != nil {
		t.Fatalf("marshal oversized body: %v", err)
	}
	resp = doJSONRequest(t, client, http.MethodPost, baseURL+"/v1/sql", "s3cret-token", string(oversized))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", resp.StatusCode)
	}
}

func TestBearerAuthHandlerPolicyWithoutTLS(t *testing.T) {
	// NewWithConfig is usable behind a TLS-terminating proxy. It enforces the
	// token but leaves listener validation to NewServer.
	reg := metrics.NewRegistry()
	sqlm := sqlengine.NewSQLMetrics(reg, metrics.SLOConfig{Prefix: "sql"})
	engine := sqlengine.New(nopClient{})
	engine.SetMetrics(sqlm)
	h := NewWithConfig(Config{
		Engine:    engine,
		Metrics:   reg,
		SLO:       sqlm.SLO,
		AuthToken: "tok",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := doJSONRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	resp = doJSONRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/health", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", resp.StatusCode)
	}
	resp = doJSONRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/metrics", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/metrics without token status = %d, want 401", resp.StatusCode)
	}
	resp = doJSONRequest(t, http.DefaultClient, http.MethodGet, srv.URL+"/metrics", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics with token status = %d, want 200", resp.StatusCode)
	}

	// A correct token with a lowercase scheme is accepted (RFC 7235 schemes
	// are case-insensitive).
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lowercase scheme status = %d, want 200", resp.StatusCode)
	}

	// More than one Authorization header is ambiguous and rejected.
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("Authorization", "Bearer tok")
	req.Header.Add("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateRejectsUnsafeConfiguration(t *testing.T) {
	engine := sqlengine.New(nopClient{})
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "auth without TLS",
			cfg:  Config{Engine: engine, AuthToken: "secret"},
			want: "requires TLS",
		},
		{
			name: "certificate without key",
			cfg:  Config{Engine: engine, TLSCertFile: "server.crt"},
			want: "must be set together",
		},
		{
			name: "key without certificate",
			cfg:  Config{Engine: engine, TLSKeyFile: "server.key"},
			want: "must be set together",
		},
		{
			name: "TLSConfig without certificate",
			cfg:  Config{Engine: engine, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
			want: "does not provide a server certificate",
		},
		{
			name: "TLS below 1.2",
			cfg:  Config{Engine: engine, TLSConfig: &tls.Config{MaxVersion: tls.VersionTLS11}},
			want: "below TLS 1.2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want %q", err, tt.want)
			}
			if _, err := NewServer(tt.cfg, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewServer error = %v, want %q", err, tt.want)
			}
			// Configuration errors must never echo the auth token.
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked the auth token: %v", err)
			}
		})
	}
}

func TestNewServerLoadsCertificatePairAndClonesTLSConfig(t *testing.T) {
	cert := newTestCertificate(t)
	base := &tls.Config{MinVersion: tls.VersionTLS10}
	srv, err := NewServer(Config{
		Engine:      sqlengine.New(nopClient{}),
		TLSConfig:   base,
		TLSCertFile: cert.certFile,
		TLSKeyFile:  cert.keyFile,
		AuthToken:   "tok",
	}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", srv.TLSConfig.MinVersion)
	}
	if len(srv.TLSConfig.Certificates) != 1 {
		t.Fatalf("len(Certificates) = %d, want 1", len(srv.TLSConfig.Certificates))
	}
	if base.MinVersion != tls.VersionTLS10 {
		t.Fatalf("caller TLSConfig was mutated: MinVersion = %#x", base.MinVersion)
	}
}

func TestNewServerRejectsMissingCertificateFiles(t *testing.T) {
	_, err := NewServer(Config{
		Engine:      sqlengine.New(nopClient{}),
		TLSCertFile: filepath.Join(t.TempDir(), "missing.crt"),
		TLSKeyFile:  filepath.Join(t.TempDir(), "missing.key"),
		AuthToken:   "tok",
	}, "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "load TLS certificate/key pair") {
		t.Fatalf("NewServer error = %v, want a certificate load error", err)
	}
}

func TestTLSConfigOptionServesTLS(t *testing.T) {
	cert := newTestCertificate(t)
	pair, err := tls.LoadX509KeyPair(cert.certFile, cert.keyFile)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	srv, err := NewServer(Config{
		Engine:    sqlengine.New(nopClient{}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
		AuthToken: "tok",
	}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.TLSConfig == nil {
		t.Fatal("TLSConfig is nil, want TLS from Config.TLSConfig")
	}
	baseURL := startTLSServer(t, srv)
	resp := doJSONRequest(t, tlsClient(cert), http.MethodGet, baseURL+"/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	resp = doJSONRequest(t, tlsClient(cert), http.MethodGet, baseURL+"/metrics", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/metrics without token status = %d, want 401", resp.StatusCode)
	}
}
