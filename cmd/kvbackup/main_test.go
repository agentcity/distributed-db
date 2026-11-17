package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-db/internal/backup"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
)

func writeEmptyBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := backup.Manifest{
		Format:        backup.FormatName,
		FormatVersion: backup.FormatVersion,
		CreatedAt:     time.Now().UTC(),
		SnapshotTS:    77,
		ChunkKeys:     backup.DefaultChunkKeys,
		ChunkBytes:    backup.DefaultChunkBytes,
		Ranges:        []backup.Range{{ID: 1, Version: 1}},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func TestRunVerify(t *testing.T) {
	dir := writeEmptyBackup(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"verify", "-in", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("verify exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verify ok") || !strings.Contains(stdout.String(), "snapshot_ts=77") {
		t.Fatalf("verify output = %q", stdout.String())
	}

	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), []byte("{bad"), 0o644); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"verify", "-in", dir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("corrupt verify exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid manifest") {
		t.Fatalf("corrupt verify stderr = %q", stderr.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"unknown"},
		{"backup"},
		{"restore", "-in", "x"},
		{"verify"},
	}
	for i, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("case %d args %v: exit = %d, want 2", i, args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "kvbackup backup") {
		t.Fatalf("help output = %q", stdout.String())
	}
}

// parseClientFlags parses args with the shared TLS/admin flag set. It avoids
// run() so flag precedence can be tested without dialing a cluster.
func parseClientFlags(t *testing.T, args ...string) *clientFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var sink bytes.Buffer
	fs.SetOutput(&sink)
	f := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	f.resolve(fs)
	return f
}

func TestClientFlagsAdminTokenEnvPrecedence(t *testing.T) {
	t.Setenv(adminTokenEnv, "env-token")
	if got := parseClientFlags(t).adminToken; got != "env-token" {
		t.Fatalf("env token = %q, want env-token", got)
	}
	if got := parseClientFlags(t, "-admin-token", "flag-token").adminToken; got != "flag-token" {
		t.Fatalf("flag token = %q, want flag-token (flag must win over env)", got)
	}
	// An explicit empty flag deliberately disables the env fallback.
	if got := parseClientFlags(t, "-admin-token", "").adminToken; got != "" {
		t.Fatalf("explicit empty token = %q, want empty", got)
	}
	t.Setenv(adminTokenEnv, "")
	if got := parseClientFlags(t).adminToken; got != "" {
		t.Fatalf("unset env token = %q, want empty", got)
	}
}

func TestClientFlagsValidate(t *testing.T) {
	cases := []struct {
		name    string
		flags   clientFlags
		wantErr string
	}{
		{name: "plaintext token is rejected", flags: clientFlags{adminToken: "secret"}, wantErr: "without TLS"},
		{name: "plaintext token with explicit override", flags: clientFlags{adminToken: "secret", allowPlaintextToken: true}},
		{name: "token with CA", flags: clientFlags{adminToken: "secret", tlsCA: "ca.pem"}},
		{name: "token with server name", flags: clientFlags{adminToken: "secret", tlsServerName: "localhost"}},
		{name: "token with client certificate", flags: clientFlags{adminToken: "secret", tlsCert: "c.pem", tlsKey: "k.pem"}},
		{name: "cert without key", flags: clientFlags{tlsCert: "c.pem"}, wantErr: "together"},
		{name: "key without cert", flags: clientFlags{tlsKey: "k.pem"}, wantErr: "together"},
		{name: "plaintext without token", flags: clientFlags{}},
	}
	for _, tc := range cases {
		err := tc.flags.validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: validate = %v, want nil", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: validate = %v, want error containing %q", tc.name, err, tc.wantErr)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: error leaked the admin token: %v", tc.name, err)
		}
	}
}

func TestClientFlagsTLSOptions(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	flags := &clientFlags{
		tlsCA: certs.CAFile, tlsCert: certs.ClientCert,
		tlsKey: certs.ClientKey, tlsServerName: "localhost",
	}
	opts, err := flags.clientOptions()
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}
	if opts == nil {
		t.Fatal("clientOptions = nil, want TLS options")
	}
	if opts.CAFile != certs.CAFile || opts.CertFile != certs.ClientCert ||
		opts.KeyFile != certs.ClientKey || opts.ServerName != "localhost" {
		t.Fatalf("clientOptions = %+v, want the flag values", *opts)
	}

	plain, err := (&clientFlags{}).clientOptions()
	if err != nil || plain != nil {
		t.Fatalf("plaintext clientOptions = (%v, %v), want (nil, nil)", plain, err)
	}
	partial := &clientFlags{tlsCert: certs.ClientCert}
	if _, err := partial.clientOptions(); err == nil {
		t.Fatal("certificate without a key was accepted")
	}
}

func TestTokenWithoutTLSRejectedByCommands(t *testing.T) {
	const token = "s3cret-admin-token"
	dir := t.TempDir()
	cases := [][]string{
		{"backup", "-meta", "127.0.0.1:1", "-nodes", "127.0.0.1:2", "-out", dir, "-admin-token", token},
		{"restore", "-meta", "127.0.0.1:1", "-nodes", "127.0.0.1:2", "-in", dir, "-admin-token", token},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("%v exit = %d, want 2 (stderr = %s)", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "without TLS") {
			t.Fatalf("%v stderr = %q, want a plaintext-token rejection", args, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), token) {
			t.Fatalf("%v leaked the admin token in command output", args)
		}
	}

	// The env fallback must be validated exactly like the flag.
	t.Setenv(adminTokenEnv, token)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"backup", "-meta", "127.0.0.1:1", "-nodes", "127.0.0.1:2", "-out", dir}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "without TLS") {
		t.Fatalf("env token: exit = %d, stderr = %q; want exit 2 with a plaintext-token rejection", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("env token leaked into command output")
	}
}

func TestHelpDoesNotLeakAdminTokenEnv(t *testing.T) {
	const token = "env-only-secret-token"
	t.Setenv(adminTokenEnv, token)
	for _, args := range [][]string{{"help"}, {"backup", "-h"}, {"restore", "-h"}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%v exit = %d, want 0", args, code)
		}
		if strings.Contains(stdout.String()+stderr.String(), token) {
			t.Fatalf("%v printed the env admin token", args)
		}
	}
}

func TestOpenClientsBuildsTLS(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	tlsOpts, err := grpctls.NewClientOptions(certs.CAFile, certs.ClientCert, certs.ClientKey, "localhost")
	if err != nil {
		t.Fatalf("client TLS options: %v", err)
	}
	// No RPC is made, so the addresses need not be reachable: both
	// constructors validate TLS material and create lazy gRPC connections.
	client, meta, err := openClients("127.0.0.1:1", "127.0.0.1:2", tlsOpts, "admin-token")
	if err != nil {
		t.Fatalf("openClients: %v", err)
	}
	if client == nil || meta == nil {
		t.Fatalf("openClients = (%v, %v), want two clients", client, meta)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close v2 client: %v", err)
	}
	if err := meta.Close(); err != nil {
		t.Fatalf("close meta client: %v", err)
	}
}

func TestOpenClientsErrorDoesNotLeakAdminToken(t *testing.T) {
	const token = "leak-check-admin-token"
	missingCA := filepath.Join(t.TempDir(), "missing-ca.pem")
	_, _, err := openClients("127.0.0.1:1", "127.0.0.1:2", &grpctls.ClientOptions{CAFile: missingCA}, token)
	if err == nil {
		t.Fatal("openClients with a missing CA succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("openClients error leaked the admin token: %v", err)
	}
}

// writeEmptySplitBackup writes a valid two-range manifest with no chunks, so a
// restore against the default one-range target must call meta SetRanges.
func writeEmptySplitBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := backup.Manifest{
		Format:        backup.FormatName,
		FormatVersion: backup.FormatVersion,
		CreatedAt:     time.Now().UTC(),
		SnapshotTS:    99,
		ChunkKeys:     backup.DefaultChunkKeys,
		ChunkBytes:    backup.DefaultChunkBytes,
		Ranges: []backup.Range{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"127.0.0.1:7001"}, Version: 1},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{"127.0.0.1:7001"}, Version: 1},
		},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// startTLSAdminMetaForCLI starts a TLS meta service with the given admin token.
func startTLSAdminMetaForCLI(t *testing.T, token string) (string, *testutil.Certs) {
	t.Helper()
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	srv, err := metaserver.New(metaserver.Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
		}},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminToken: token,
		TLS: grpctls.Options{Server: grpctls.ServerOptions{
			CertFile: certs.ServerCert, KeyFile: certs.ServerKey,
		}},
	})
	if err != nil {
		t.Fatalf("new meta server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start meta server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv.Addr(), certs
}

func TestRestoreCLIWithTLSAndAdminToken(t *testing.T) {
	const token = "cli-admin-token-sentinel"
	cases := []struct {
		name       string
		tokenFlag  string
		envToken   string
		wantCode   int
		wantStderr string
	}{
		{name: "flag token", tokenFlag: token, wantCode: 0},
		{name: "env token", envToken: token, wantCode: 0},
		{name: "missing token fails cleanly", wantCode: 1, wantStderr: "admin token required"},
		{name: "wrong token fails cleanly", tokenFlag: "wrong-" + token, wantCode: 1, wantStderr: "invalid admin token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, certs := startTLSAdminMetaForCLI(t, token)
			dir := writeEmptySplitBackup(t)
			if tc.envToken != "" {
				t.Setenv(adminTokenEnv, tc.envToken)
			}
			args := []string{
				"restore", "-meta", addr, "-nodes", "127.0.0.1:1", "-in", dir, "-ensure-ranges",
				"-tls-ca", certs.CAFile, "-tls-server-name", "localhost",
			}
			if tc.tokenFlag != "" {
				args = append(args, "-admin-token", tc.tokenFlag)
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("restore exit = %d, want %d (stdout = %s, stderr = %s)", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("restore stderr = %q, want substring %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantCode == 0 && !strings.Contains(stdout.String(), "restore complete") {
				t.Fatalf("restore stdout = %q, want a completion line", stdout.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), token) {
				t.Fatal("admin token leaked into command output")
			}
		})
	}
}
