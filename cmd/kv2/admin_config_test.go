package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"distributed-db/internal/audit"
	"distributed-db/internal/grpctls"
)

func newAdminFlagSet(t *testing.T) (*flag.FlagSet, *adminFlags) {
	t.Helper()
	fs := flag.NewFlagSet("kv2-admin-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, registerAdminFlags(fs)
}

func mtlsTLS() grpctls.Options {
	return grpctls.Options{Server: grpctls.ServerOptions{ClientCAFile: "clients.pem"}}
}

func TestAdminRoleTokenPairsParse(t *testing.T) {
	got, err := parseRoleTokenPairs("admin=s3cret-a,operator=s3cret-b,read-only=s3cret-c,ADMIN=s3cret-d")
	if err != nil {
		t.Fatalf("parseRoleTokenPairs: %v", err)
	}
	want := map[string]string{
		"s3cret-a": "admin",
		"s3cret-b": "operator",
		"s3cret-c": "read-only",
		"s3cret-d": "admin",
	}
	if len(got) != len(want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	for token, role := range want {
		if got[token] != role {
			t.Fatalf("token role = %q, want %q (map %v)", got[token], role, got)
		}
	}
}

func TestAdminRoleTokenPairsRejectInvalidEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "missing equals", raw: "admin"},
		{name: "unknown role", raw: "superuser=secret-token"},
		{name: "empty role", raw: "=secret-token"},
		{name: "empty token", raw: "admin="},
		{name: "conflicting roles", raw: "admin=secret-token,operator=secret-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRoleTokenPairs(tc.raw)
			if err == nil {
				t.Fatalf("parseRoleTokenPairs(%q) succeeded", tc.raw)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("error leaked the token: %v", err)
			}
		})
	}
}

func TestAdminMTLSRolePairsParse(t *testing.T) {
	got, err := parseMTLSRolePairs("client.test=operator,ops.example=admin")
	if err != nil {
		t.Fatalf("parseMTLSRolePairs: %v", err)
	}
	if got["client.test"] != "operator" || got["ops.example"] != "admin" {
		t.Fatalf("mtls roles = %v", got)
	}
	for _, raw := range []string{"client.test", "client.test=bogus", "=admin", "client.test=operator,client.test=admin"} {
		if _, err := parseMTLSRolePairs(raw); err == nil {
			t.Fatalf("parseMTLSRolePairs(%q) succeeded", raw)
		}
	}
}

func TestAdminTokenRolesFileHonorsCommentsAndRejectsConflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles")
	if err := os.WriteFile(path, []byte("# role=token\n\nadmin=file-admin-token\noperator=file-op-token\n"), 0o600); err != nil {
		t.Fatalf("write roles: %v", err)
	}
	got, err := loadRoleTokenFile(path)
	if err != nil {
		t.Fatalf("loadRoleTokenFile: %v", err)
	}
	if got["file-admin-token"] != "admin" || got["file-op-token"] != "operator" {
		t.Fatalf("file roles = %v", got)
	}

	inline, err := parseRoleTokenPairs("admin=file-admin-token")
	if err != nil {
		t.Fatalf("parse inline: %v", err)
	}
	if _, err := mergeTokenRoles(inline, map[string]string{"file-admin-token": "operator"}); err == nil {
		t.Fatal("mergeTokenRoles accepted conflicting roles")
	}
}

func TestAdminFlagsEnvFallback(t *testing.T) {
	t.Setenv(adminTokenEnv, "env-legacy-token")
	t.Setenv(adminTokenRolesEnv, "admin=env-admin-token,operator=env-op-token")
	t.Setenv(adminMTLSRolesEnv, "client.test=read-only")
	t.Setenv(auditLogEnv, "/var/log/kv2/audit.jsonl")
	t.Setenv(auditLogSyncEnv, "true")
	t.Setenv(auditLogMaxBytesEnv, "4096")
	t.Setenv(auditLogMaxBackupsEnv, "2")
	t.Setenv(auditRequiredEnv, "1")

	fs, adm := newAdminFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := adm.resolveEnv(fs); err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	cfg, err := adm.build(mtlsTLS())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.token != "env-legacy-token" {
		t.Fatalf("token = %q", cfg.token)
	}
	if len(cfg.tokenRoles) != 2 || cfg.tokenRoles["env-admin-token"] != "admin" ||
		cfg.tokenRoles["env-op-token"] != "operator" {
		t.Fatalf("token roles = %v", cfg.tokenRoles)
	}
	if cfg.mtlsRoles["client.test"] != "read-only" {
		t.Fatalf("mtls roles = %v", cfg.mtlsRoles)
	}
	if cfg.auditLog != "/var/log/kv2/audit.jsonl" || !cfg.auditSync ||
		cfg.auditMaxBytes != 4096 || cfg.auditMaxBackups != 2 || !cfg.auditRequired {
		t.Fatalf("audit config = %+v", cfg)
	}
}

func TestAdminFlagsExplicitFlagWinsOverEnv(t *testing.T) {
	t.Setenv(adminTokenRolesEnv, "admin=env-secret-token")
	t.Setenv(auditLogEnv, "/env/audit.jsonl")
	t.Setenv(auditLogSyncEnv, "true")

	fs, adm := newAdminFlagSet(t)
	if err := fs.Parse([]string{
		"-admin-token-roles", "operator=flag-secret-token",
		"-audit-log", "",
		"-audit-log-sync=false",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := adm.resolveEnv(fs); err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	cfg, err := adm.build(grpctls.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := cfg.tokenRoles["env-secret-token"]; ok {
		t.Fatal("environment token leaked in despite an explicit -admin-token-roles")
	}
	if cfg.tokenRoles["flag-secret-token"] != "operator" {
		t.Fatalf("flag token role = %q", cfg.tokenRoles["flag-secret-token"])
	}
	if cfg.auditLog != "" || cfg.auditSync {
		t.Fatalf("explicit empty -audit-log / false -audit-log-sync did not win: %+v", cfg)
	}
}

func TestAdminFlagsRejectInvalidEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{name: "sync bool", env: auditLogSyncEnv, val: "sometimes"},
		{name: "max bytes", env: auditLogMaxBytesEnv, val: "huge"},
		{name: "negative max bytes", env: auditLogMaxBytesEnv, val: "-1"},
		{name: "max backups", env: auditLogMaxBackupsEnv, val: "many"},
		{name: "required bool", env: auditRequiredEnv, val: "yes-please"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			fs, adm := newAdminFlagSet(t)
			if err := fs.Parse(nil); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := adm.resolveEnv(fs); err == nil {
				t.Fatalf("resolveEnv accepted %s=%q", tc.env, tc.val)
			}
		})
	}
}

func TestAdminFlagsBuildValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		tls     grpctls.Options
		want    string
		anyTok  string
		wantErr bool
	}{
		{
			name:    "mtls roles need client CA",
			args:    []string{"-admin-mtls-roles", "client.test=operator"},
			tls:     grpctls.Options{},
			want:    "-tls-client-ca",
			wantErr: true,
		},
		{
			name:    "audit log needs credentials",
			args:    []string{"-audit-log", "/tmp/a.jsonl"},
			want:    "-audit-log requires at least one admin credential",
			anyTok:  "",
			wantErr: true,
		},
		{
			name:    "audit required needs log",
			args:    []string{"-admin-token", "secret-token", "-audit-required"},
			want:    "-audit-required requires -audit-log",
			anyTok:  "secret-token",
			wantErr: true,
		},
		{
			name:    "negative max bytes",
			args:    []string{"-admin-token", "secret-token", "-audit-log", "/tmp/a.jsonl", "-audit-log-max-bytes=-1"},
			want:    "must not be negative",
			anyTok:  "secret-token",
			wantErr: true,
		},
		{
			name:    "negative max backups",
			args:    []string{"-admin-token", "secret-token", "-audit-log", "/tmp/a.jsonl", "-audit-log-max-backups=-2"},
			want:    "must not be negative",
			anyTok:  "secret-token",
			wantErr: true,
		},
		{
			name: "valid rotation",
			args: []string{
				"-admin-token", "secret-token",
				"-audit-log", "/tmp/a.jsonl",
				"-audit-log-max-bytes", "1024",
				"-audit-log-max-backups", "3",
				"-audit-required",
			},
			wantErr: false,
		},
		{
			name: "zero bytes normalizes backups",
			args: []string{
				"-admin-token", "secret-token",
				"-audit-log", "/tmp/a.jsonl",
				"-audit-log-max-bytes", "0",
				"-audit-log-max-backups", "4",
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, adm := newAdminFlagSet(t)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			cfg, err := adm.build(tc.tls)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("build error = %v, want substring %q", err, tc.want)
				}
				if tc.anyTok != "" && strings.Contains(err.Error(), tc.anyTok) {
					t.Fatalf("build error leaked a token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if tc.name == "zero bytes normalizes backups" && cfg.auditMaxBackups != 0 {
				t.Fatalf("auditMaxBackups = %d, want 0 when rotation is disabled", cfg.auditMaxBackups)
			}
		})
	}
}

func TestAdminFlagsHelpDoesNotContainSecrets(t *testing.T) {
	const token = "help-must-not-show-this-token"
	t.Setenv(adminTokenEnv, token)
	t.Setenv(adminTokenRolesEnv, "admin="+token)
	fs, _ := newAdminFlagSet(t)
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	if strings.Contains(buf.String(), token) {
		t.Fatalf("flag defaults leaked a token:\n%s", buf.String())
	}
	// The environment variable names may appear in usage, but never values.
	if !strings.Contains(buf.String(), adminTokenRolesEnv) {
		t.Fatalf("help does not document %s:\n%s", adminTokenRolesEnv, buf.String())
	}
}

func TestAdminFlagsMirrorServerDefaults(t *testing.T) {
	fs, _ := newAdminFlagSet(t)
	bytesFlag := fs.Lookup("audit-log-max-bytes")
	backupsFlag := fs.Lookup("audit-log-max-backups")
	if bytesFlag == nil || backupsFlag == nil {
		t.Fatalf("rotation flags are not registered: %v", []string{"audit-log-max-bytes", "audit-log-max-backups"})
	}
	if got := bytesFlag.DefValue; got != "104857600" {
		t.Fatalf("audit-log-max-bytes default = %s, want %d", got, audit.DefaultMaxBytes)
	}
	if got := backupsFlag.DefValue; got != "5" {
		t.Fatalf("audit-log-max-backups default = %s, want %d", got, audit.DefaultMaxBackups)
	}
}

func TestMetaServerConfigWiresAdminAndAudit(t *testing.T) {
	adm := adminConfig{
		token:           "legacy-token",
		tokenRoles:      map[string]string{"role-token": "operator"},
		mtlsRoles:       map[string]string{"client.test": "admin"},
		auditLog:        "/var/log/meta/audit.jsonl",
		auditSync:       true,
		auditMaxBytes:   2048,
		auditMaxBackups: 3,
		auditRequired:   true,
	}
	cfg := metaServerConfig(":9090", "meta-1", "./data/meta", nil, nil, grpctls.Options{}, adm)
	if cfg.AdminToken != adm.token {
		t.Fatalf("AdminToken = %q", cfg.AdminToken)
	}
	if cfg.AdminTokenRoles["role-token"] != "operator" || cfg.AdminMTLSRoles["client.test"] != "admin" {
		t.Fatalf("role maps = %v / %v", cfg.AdminTokenRoles, cfg.AdminMTLSRoles)
	}
	if cfg.AuditLogPath != adm.auditLog || !cfg.AuditLogSync ||
		cfg.AuditLogMaxBytes != adm.auditMaxBytes ||
		cfg.AuditLogMaxBackups != adm.auditMaxBackups || !cfg.AuditRequired {
		t.Fatalf("audit config = %+v", cfg)
	}
}

func TestNodeServerConfigWiresAdminAndAudit(t *testing.T) {
	adm := adminConfig{
		token:           "legacy-token",
		tokenRoles:      map[string]string{"role-token": "operator"},
		mtlsRoles:       map[string]string{"client.test": "admin"},
		auditLog:        "/var/log/kv2/audit.jsonl",
		auditSync:       true,
		auditMaxBytes:   2048,
		auditMaxBackups: 3,
		auditRequired:   true,
	}
	cfg := nodeServerConfig("n1", "127.0.0.1:7001", "./data/n1", "sync",
		[]string{"127.0.0.1:7002"}, nil, grpctls.Options{}, adm)
	if cfg.ID != "n1" || cfg.RaftAddr != "127.0.0.1:7001" || len(cfg.Peers) != 1 {
		t.Fatalf("node identity/peers = %+v", cfg)
	}
	if cfg.AdminToken != adm.token || cfg.AdminTokenRoles["role-token"] != "operator" ||
		cfg.AdminMTLSRoles["client.test"] != "admin" {
		t.Fatalf("admin wiring = token %q roles %v mtls %v", cfg.AdminToken, cfg.AdminTokenRoles, cfg.AdminMTLSRoles)
	}
	if cfg.AuditLogPath != adm.auditLog || !cfg.AuditLogSync ||
		cfg.AuditLogMaxBytes != adm.auditMaxBytes ||
		cfg.AuditLogMaxBackups != adm.auditMaxBackups || !cfg.AuditRequired {
		t.Fatalf("audit wiring = %+v", cfg)
	}
}
