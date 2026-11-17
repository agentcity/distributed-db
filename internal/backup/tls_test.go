package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	"distributed-db/internal/v2client"
)

// tlsMetaFixture is a standalone meta service that speaks TLS (and requires a
// client certificate when mTLS is set) and enforces the admin token.
type tlsMetaFixture struct {
	addr string
	srv  *metaserver.Server
	// clientTLS verifies the server without presenting a client certificate.
	clientTLS *grpctls.ClientOptions
	// mtlsClientTLS additionally presents the generated client certificate.
	mtlsClientTLS *grpctls.ClientOptions
}

func startTLSAdminMeta(t *testing.T, token string, requireClientCert bool) *tlsMetaFixture {
	t.Helper()
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	serverTLS := grpctls.ServerOptions{CertFile: certs.ServerCert, KeyFile: certs.ServerKey}
	if requireClientCert {
		serverTLS.ClientCAFile = certs.CAFile
	}
	srv, err := metaserver.New(metaserver.Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
		}},
		Logger:     testLogger(),
		AdminToken: token,
		TLS:        grpctls.Options{Server: serverTLS},
	})
	if err != nil {
		t.Fatalf("new TLS meta server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start TLS meta server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	clientTLS := &grpctls.ClientOptions{CAFile: certs.CAFile, ServerName: "localhost"}
	mtlsTLS := *clientTLS
	mtlsTLS.CertFile = certs.ClientCert
	mtlsTLS.KeyFile = certs.ClientKey
	return &tlsMetaFixture{
		addr: srv.Addr(), srv: srv,
		clientTLS: clientTLS, mtlsClientTLS: &mtlsTLS,
	}
}

func tlsTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOpenMetaWithOptionsValidatesTLS(t *testing.T) {
	if _, err := OpenMetaWithOptions(nil, MetaOptions{}); err == nil {
		t.Fatal("OpenMetaWithOptions with no seeds succeeded")
	}

	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	if _, err := OpenMetaWithOptions([]string{"127.0.0.1:1"}, MetaOptions{
		TLS: &grpctls.ClientOptions{CertFile: certs.ClientCert},
	}); err == nil || !strings.Contains(err.Error(), "CertFile and KeyFile") {
		t.Fatalf("certificate without a key: err = %v, want a CertFile/KeyFile error", err)
	}

	missingCA := filepath.Join(t.TempDir(), "missing-ca.pem")
	const token = "meta-validation-token"
	if _, err := OpenMetaWithOptions([]string{"127.0.0.1:1"}, MetaOptions{
		TLS: &grpctls.ClientOptions{CAFile: missingCA}, AdminToken: token,
	}); err == nil {
		t.Fatal("missing CA file was accepted")
	} else if strings.Contains(err.Error(), token) {
		t.Fatalf("TLS validation error leaked the admin token: %v", err)
	}

	// Valid TLS material with an unreachable address must not dial yet.
	client, err := OpenMetaWithOptions([]string{" 127.0.0.1:1 ", "127.0.0.1:1"}, MetaOptions{
		TLS: &grpctls.ClientOptions{CAFile: certs.CAFile, ServerName: "localhost"},
	})
	if err != nil {
		t.Fatalf("OpenMetaWithOptions with valid TLS: %v", err)
	}
	if len(client.seeds) != 1 || client.seeds[0] != "127.0.0.1:1" {
		t.Fatalf("seeds = %v, want one deduplicated address", client.seeds)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestMetaClientTLSAdminToken(t *testing.T) {
	const token = "meta-admin-token-sentinel"
	fx := startTLSAdminMeta(t, token, false)
	ctx := tlsTestContext(t)

	authorized, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS, AdminToken: token})
	if err != nil {
		t.Fatalf("open authorized meta client: %v", err)
	}
	defer func() { _ = authorized.Close() }()

	// Read-only calls work without sending the token.
	ranges, err := authorized.ListRanges(ctx)
	if err != nil {
		t.Fatalf("ListRanges over TLS: %v", err)
	}
	if len(ranges) != 1 || ranges[0].ID != 1 {
		t.Fatalf("initial ranges = %+v, want range 1", ranges)
	}
	desired := []Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{"127.0.0.1:7001"}, Version: 1,
	}}
	if err := authorized.SetRanges(ctx, desired); err != nil {
		t.Fatalf("SetRanges with TLS and token: %v", err)
	}

	noToken, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS})
	if err != nil {
		t.Fatalf("open tokenless meta client: %v", err)
	}
	defer func() { _ = noToken.Close() }()
	err = noToken.SetRanges(ctx, desired)
	if err == nil {
		t.Fatal("SetRanges without a token succeeded against a protected meta")
	}
	if !strings.Contains(err.Error(), "admin token required") {
		t.Fatalf("missing token error = %v, want a clean admin-token-required error", err)
	}

	wrong, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS, AdminToken: "wrong-" + token})
	if err != nil {
		t.Fatalf("open wrong-token meta client: %v", err)
	}
	defer func() { _ = wrong.Close() }()
	err = wrong.SetRanges(ctx, desired)
	if err == nil {
		t.Fatal("SetRanges with a wrong token succeeded")
	}
	if !strings.Contains(err.Error(), "invalid admin token") {
		t.Fatalf("wrong token error = %v, want a clean invalid-admin-token error", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("wrong token error leaked the token: %v", err)
	}
}

func TestMetaClientRejectsMutualTLSWithoutCertificate(t *testing.T) {
	const token = "mtls-admin-token"
	fx := startTLSAdminMeta(t, token, true)
	ctx := tlsTestContext(t)

	authorized, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.mtlsClientTLS, AdminToken: token})
	if err != nil {
		t.Fatalf("open mTLS meta client: %v", err)
	}
	defer func() { _ = authorized.Close() }()
	if _, err := authorized.ListRanges(ctx); err != nil {
		t.Fatalf("ListRanges over mTLS: %v", err)
	}

	noCert, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS})
	if err != nil {
		t.Fatalf("open no-cert meta client: %v", err)
	}
	defer func() { _ = noCert.Close() }()
	if _, err := noCert.ListRanges(ctx); err == nil {
		t.Fatal("ListRanges without a client certificate succeeded against an mTLS meta")
	}
}

func TestMetaClientHasNoPlaintextFallback(t *testing.T) {
	fx := startTLSAdminMeta(t, "", false)
	plain, err := OpenMeta([]string{fx.addr})
	if err != nil {
		t.Fatalf("open plaintext meta client: %v", err)
	}
	defer func() { _ = plain.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := plain.ListRanges(ctx); err == nil {
		t.Fatal("plaintext ListRanges succeeded against a TLS meta; there must be no plaintext fallback")
	}
}

// writeEmptyTLSBackup writes a valid manifest with no chunks. The manifest
// ranges describe a two-range layout so EnsureRanges has to call SetRanges.
func writeEmptyTLSBackup(t *testing.T) string {
	t.Helper()
	m := Manifest{
		Format:        FormatName,
		FormatVersion: FormatVersion,
		CreatedAt:     time.Now().UTC(),
		SnapshotTS:    42,
		ChunkKeys:     DefaultChunkKeys,
		ChunkBytes:    DefaultChunkBytes,
		Ranges: []Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"127.0.0.1:7001"}, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{"127.0.0.1:7001"}, Version: 1},
		},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func TestRestoreSetRangesCarriesTLSAndToken(t *testing.T) {
	const token = "restore-admin-token-sentinel"
	fx := startTLSAdminMeta(t, token, false)
	ctx := tlsTestContext(t)
	dir := writeEmptyTLSBackup(t)

	metaClient, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS, AdminToken: token})
	if err != nil {
		t.Fatalf("open meta client: %v", err)
	}
	defer func() { _ = metaClient.Close() }()
	// Restore only needs the v2 client to write chunks; the backup has none,
	// so a meta-only TLS client is enough to prove the constructor wiring.
	nodeClient, err := v2client.NewMetaOnlyWithOptions(fx.addr, v2client.Options{
		TLS: fx.clientTLS, AdminToken: token,
	})
	if err != nil {
		t.Fatalf("open v2 meta-only client: %v", err)
	}
	defer func() { _ = nodeClient.Close() }()

	report, err := Restore(ctx, RestoreConfig{
		Client: nodeClient, Meta: metaClient, InDir: dir, EnsureRanges: true,
	})
	if err != nil {
		t.Fatalf("restore with TLS and token: %v", err)
	}
	if report.Ranges != 2 || report.Chunks != 0 || report.Keys != 0 {
		t.Fatalf("restore report = %+v, want 2 ranges and no chunks", report)
	}

	ranges, err := metaClient.ListRanges(ctx)
	if err != nil {
		t.Fatalf("list ranges after restore: %v", err)
	}
	if len(ranges) != 2 || string(ranges[0].End) != "m" || string(ranges[1].Start) != "m" {
		t.Fatalf("target ranges after restore = %+v, want the two-range backup layout", ranges)
	}
}

func TestRestoreWithoutTokenFailsCleanly(t *testing.T) {
	const token = "restore-denied-token-sentinel"
	fx := startTLSAdminMeta(t, token, false)
	ctx := tlsTestContext(t)
	dir := writeEmptyTLSBackup(t)

	nodeClient, err := v2client.NewMetaOnlyWithOptions(fx.addr, v2client.Options{TLS: fx.clientTLS})
	if err != nil {
		t.Fatalf("open v2 meta-only client: %v", err)
	}
	defer func() { _ = nodeClient.Close() }()

	tests := []struct {
		name      string
		token     string
		wantError string
	}{
		{name: "missing token", wantError: "admin token required"},
		{name: "wrong token", token: "wrong-" + token, wantError: "invalid admin token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			metaClient, err := OpenMetaWithOptions([]string{fx.addr}, MetaOptions{TLS: fx.clientTLS, AdminToken: tc.token})
			if err != nil {
				t.Fatalf("open meta client: %v", err)
			}
			defer func() { _ = metaClient.Close() }()

			_, err = Restore(ctx, RestoreConfig{
				Client: nodeClient, Meta: metaClient, InDir: dir, EnsureRanges: true,
			})
			if err == nil {
				t.Fatal("restore against a protected meta succeeded without valid credentials")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("restore error = %v, want substring %q", err, tc.wantError)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("restore error leaked the admin token: %v", err)
			}

			// The failed SetRanges must not have changed the target map.
			ranges, listErr := metaClient.ListRanges(ctx)
			if listErr != nil {
				t.Fatalf("list ranges after failed restore: %v", listErr)
			}
			if len(ranges) != 1 {
				t.Fatalf("target ranges after failed restore = %+v, want the original single range", ranges)
			}
		})
	}
}
