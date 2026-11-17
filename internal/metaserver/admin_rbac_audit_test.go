package metaserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"distributed-db/internal/audit"
	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
)

const (
	metaRBACAdminToken    = "meta-rbac-admin-token"
	metaRBACOperatorToken = "meta-rbac-operator-token"
	metaRBACReadOnlyToken = "meta-rbac-read-only-token"
	metaRBACUnknownToken  = "meta-rbac-unknown-token"
)

func startMetaAdminRBACServer(t *testing.T, mutate func(*Config)) (*Server, string) {
	t.Helper()
	cfg := Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
		}},
		Logger: tlsTestLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		_ = s.Stop()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s, s.Addr()
}

func readMetaAuditEvents(t *testing.T, path string) []audit.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	events := make([]audit.Event, 0, len(lines))
	for i, line := range lines {
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %d is not JSON: %v\n%s", i, err, line)
		}
		events = append(events, e)
	}
	return events
}

func TestMetaAdminRBACRolesAndAudit(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "meta-admin-audit.jsonl")
	s, addr := startMetaAdminRBACServer(t, func(cfg *Config) {
		cfg.AdminTokenRoles = map[string]string{
			metaRBACAdminToken:    "admin",
			metaRBACOperatorToken: "operator",
			metaRBACReadOnlyToken: "read-only",
		}
		cfg.AuditLogPath = auditPath
		cfg.AuditLogSync = true
	})
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	// operator may split/merge, and an application-level failure reaches the
	// audit log as an error.
	split, err := cl.SplitRange(withBearer(ctx, metaRBACOperatorToken),
		&pbv2.MetaSplitRangeRequest{RangeId: 99, SplitKey: []byte("m")})
	if err != nil {
		t.Fatalf("operator SplitRange transport error: %v", err)
	}
	if split.GetSuccess() || split.GetError() == "" {
		t.Fatalf("operator SplitRange = %+v, want handler-level range-not-found", split)
	}

	// operator may not change cluster configuration.
	_, err = cl.SetRanges(withBearer(ctx, metaRBACOperatorToken), setRangesRequest(2))
	requireCode(t, err, codes.PermissionDenied, "may not perform")

	// read-only may not perform any admin operation.
	_, err = cl.SetRanges(withBearer(ctx, metaRBACReadOnlyToken), setRangesRequest(3))
	requireCode(t, err, codes.PermissionDenied, "may not perform")

	// admin may.
	resp, err := cl.SetRanges(withBearer(ctx, metaRBACAdminToken), setRangesRequest(10))
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("admin SetRanges = %v %+v, want success", err, resp)
	}
	rows, err := cl.ListRanges(ctx, &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if len(rows.GetRanges()) != 1 || rows.GetRanges()[0].GetId() != 10 {
		t.Fatalf("range map after admin SetRanges = %+v, want range 10", rows.GetRanges())
	}

	// Unknown or missing credentials are Unauthenticated.
	if _, err := cl.SetRanges(withBearer(ctx, metaRBACUnknownToken), setRangesRequest(11)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown token: error = %v, want Unauthenticated", err)
	}
	if _, err := cl.SetRanges(ctx, setRangesRequest(12)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token: error = %v, want Unauthenticated", err)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readMetaAuditEvents(t, auditPath)
	if len(events) != 6 {
		t.Fatalf("audit event count = %d, want 6: %+v", len(events), events)
	}
	want := []struct {
		role       string
		result     string
		errorClass string
		method     string
		rangeLabel string
		auth       string
	}{
		{"operator", audit.ResultError, audit.ErrorClassApplication, pbv2.MetaService_SplitRange_FullMethodName, "99", "token"},
		{"operator", audit.ResultDenied, audit.ErrorClassPermissionDenied, pbv2.MetaService_SetRanges_FullMethodName, "cluster", "token"},
		{"read-only", audit.ResultDenied, audit.ErrorClassPermissionDenied, pbv2.MetaService_SetRanges_FullMethodName, "cluster", "token"},
		{"admin", audit.ResultSuccess, audit.ErrorClassNone, pbv2.MetaService_SetRanges_FullMethodName, "cluster", "token"},
		{"unknown", audit.ResultDenied, audit.ErrorClassUnauthenticated, pbv2.MetaService_SetRanges_FullMethodName, "cluster", ""},
		{"unknown", audit.ResultDenied, audit.ErrorClassUnauthenticated, pbv2.MetaService_SetRanges_FullMethodName, "cluster", ""},
	}
	for i, w := range want {
		got := events[i]
		if got.Role != w.role || got.Result != w.result || got.ErrorClass != w.errorClass ||
			got.Method != w.method || got.Range != w.rangeLabel || got.Auth != w.auth {
			t.Fatalf("event %d = %+v, want role=%s result=%s class=%s method=%s range=%s auth=%s",
				i, got, w.role, w.result, w.errorClass, w.method, w.rangeLabel, w.auth)
		}
		if got.Time.IsZero() {
			t.Fatalf("event %d has no timestamp", i)
		}
	}

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	for _, token := range []string{metaRBACAdminToken, metaRBACOperatorToken, metaRBACReadOnlyToken, metaRBACUnknownToken} {
		if strings.Contains(string(raw), token) {
			t.Fatalf("audit file leaked token %q:\n%s", token, raw)
		}
	}
}

func TestMetaAdminMTLSCNRoleAndAudit(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certificates: %v", err)
	}
	auditPath := filepath.Join(t.TempDir(), "meta-mtls-audit.jsonl")
	s, addr := startMetaAdminRBACServer(t, func(cfg *Config) {
		cfg.TLS = metaTLSOptions(certs, true)
		cfg.AdminMTLSRoles = map[string]string{"client.test": "operator"}
		cfg.AuditLogPath = auditPath
	})

	creds, err := grpctls.ClientCredentials(grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		CertFile:   certs.ClientCert,
		KeyFile:    certs.ClientKey,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	cl := pbv2.NewMetaServiceClient(cc)
	ctx := testCtx(t)

	// The operator CN reaches the SplitRange handler without a token.
	split, err := cl.SplitRange(ctx, &pbv2.MetaSplitRangeRequest{RangeId: 99, SplitKey: []byte("m")})
	if err != nil {
		t.Fatalf("mTLS operator SplitRange transport error: %v", err)
	}
	if split.GetSuccess() || split.GetError() == "" {
		t.Fatalf("mTLS operator SplitRange = %+v, want handler-level range-not-found", split)
	}

	// The operator CN may not change configuration.
	_, err = cl.SetRanges(ctx, setRangesRequest(7))
	requireCode(t, err, codes.PermissionDenied, "may not perform")

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readMetaAuditEvents(t, auditPath)
	if len(events) != 2 {
		t.Fatalf("audit event count = %d, want 2: %+v", len(events), events)
	}
	if events[0].Role != "operator" || events[0].Principal != "client.test" || events[0].Auth != "mtls" ||
		events[0].Result != audit.ResultError || events[0].ErrorClass != audit.ErrorClassApplication {
		t.Fatalf("mTLS successful-path audit event = %+v", events[0])
	}
	if events[1].Role != "operator" || events[1].Principal != "client.test" ||
		events[1].Result != audit.ResultDenied || events[1].ErrorClass != audit.ErrorClassPermissionDenied {
		t.Fatalf("mTLS denied-path audit event = %+v", events[1])
	}
}

func TestMetaAdminRBACConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "unknown role",
			mutate: func(cfg *Config) {
				cfg.AdminTokenRoles = map[string]string{"secret-token": "superuser"}
			},
			want: "unknown role",
		},
		{
			name: "conflicting legacy token",
			mutate: func(cfg *Config) {
				cfg.AdminToken = "secret-token"
				cfg.AdminTokenRoles = map[string]string{"secret-token": "operator"}
			},
			want: "non-admin role",
		},
		{
			name: "mtls roles without mtls",
			mutate: func(cfg *Config) {
				cfg.AdminMTLSRoles = map[string]string{"client.test": "operator"}
			},
			want: "AdminMTLSRoles requires",
		},
		{
			name: "audit without auth",
			mutate: func(cfg *Config) {
				cfg.AuditLogPath = filepath.Join(t.TempDir(), "audit.jsonl")
			},
			want: "AuditLogPath requires",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Addr: "127.0.0.1:0",
				Ranges: []meta.Range{{
					ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
				}},
				Logger: tlsTestLogger(),
			}
			tc.mutate(&cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want substring %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("New error leaked a token: %v", err)
			}
		})
	}
}

func TestMetaAdminEmptyConfigRemainsPassThrough(t *testing.T) {
	s, addr := startMetaAdminRBACServer(t, nil)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	if s.authz.Enabled() {
		t.Fatal("empty Config produced an enabled RBAC policy")
	}
	if s.auditLog != nil {
		t.Fatal("empty Config produced an audit logger")
	}
	resp, err := cl.SetRanges(ctx, setRangesRequest(3))
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("SetRanges with empty config = %v %+v, want success", err, resp)
	}
}
