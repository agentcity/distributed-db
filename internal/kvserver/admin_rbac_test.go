package kvserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"distributed-db/internal/audit"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/rbac"
)

const (
	kvRBACAdminToken    = "kv-rbac-admin-token"
	kvRBACOperatorToken = "kv-rbac-operator-token"
	kvRBACReadOnlyToken = "kv-rbac-read-only-token"
	kvRBACUnknownToken  = "kv-rbac-unknown-token"
)

// startKVAdminRBACServer starts a single-node server with a base Config and
// applies mutate before New. It is the flexible counterpart of
// kvAuthStartServer for RBAC/audit tests.
func startKVAdminRBACServer(t *testing.T, mutate func(*Config)) (*Server, string) {
	t.Helper()
	addr := testFreeAddr(t)
	cfg := Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		Ranges:             []RangeConfig{{ID: 1}},
		Logger:             testLogger(),
		ElectionTimeoutMin: 40 * time.Millisecond,
		ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		SnapshotInterval:   -1,
		GCInterval:         -1,
		DiskCheckInterval:  -1,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	waitRangeLeader(t, srv, 1)
	return srv, srv.Addr()
}

func readAuditEvents(t *testing.T, path string) []audit.Event {
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

func TestKVAdminRBACRolesAndAudit(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "kv-admin-audit.jsonl")
	srv, addr := startKVAdminRBACServer(t, func(cfg *Config) {
		cfg.AdminTokenRoles = map[string]string{
			kvRBACAdminToken:    "admin",
			kvRBACOperatorToken: "operator",
			kvRBACReadOnlyToken: "read-only",
		}
		cfg.AuditLogPath = auditPath
		cfg.AuditLogSync = true
	})
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	// No credential and an unknown token are both Unauthenticated.
	if _, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: error = %v, want Unauthenticated", err)
	}
	_, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvRBACUnknownToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown token: error = %v, want Unauthenticated", err)
	}
	if err != nil && strings.Contains(err.Error(), kvRBACUnknownToken) {
		t.Fatalf("error leaked the unknown token: %v", err)
	}

	// read-only is authenticated but forbidden.
	initialEpoch := srv.Ranges()[0].Epoch
	_, err = client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvRBACReadOnlyToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 5})
	kvAuthRequireCode(t, err, codes.PermissionDenied, "may not perform")
	if got := srv.Ranges()[0].Epoch; got != initialEpoch {
		t.Fatalf("range epoch = %d after a read-only attempt, want %d", got, initialEpoch)
	}

	// operator may perform cluster operations.
	resp, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvRBACOperatorToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 10})
	if err != nil || !resp.GetSuccess() || resp.GetEpoch() != 10 {
		t.Fatalf("operator UpdateRangeEpoch = %v %+v, want success epoch 10", err, resp)
	}

	// An operator request that reaches the handler and fails at the
	// application layer is audited as an error, not as a denial.
	split, err := client.SplitRange(kvAuthWithBearer(ctx, kvRBACOperatorToken),
		&pbv2.SplitRangeRequest{RangeId: 99, SplitKey: []byte("m"), NewRangeId: 100})
	if err != nil {
		t.Fatalf("operator SplitRange transport error: %v", err)
	}
	if split.GetError() == "" {
		t.Fatalf("operator SplitRange on a missing range = %+v, want an application error", split)
	}

	// admin may perform everything.
	resp, err = client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvRBACAdminToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 11})
	if err != nil || !resp.GetSuccess() || resp.GetEpoch() != 11 {
		t.Fatalf("admin UpdateRangeEpoch = %v %+v, want success epoch 11", err, resp)
	}

	// Data-plane RPCs remain unauthenticated and unaudited.
	if _, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: 1}); err != nil {
		t.Fatalf("Status without a token: %v", err)
	}

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := readAuditEvents(t, auditPath)
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
		{"unknown", audit.ResultDenied, audit.ErrorClassUnauthenticated, pbv2.KVService_UpdateRangeEpoch_FullMethodName, "1", ""},
		{"unknown", audit.ResultDenied, audit.ErrorClassUnauthenticated, pbv2.KVService_UpdateRangeEpoch_FullMethodName, "1", ""},
		{"read-only", audit.ResultDenied, audit.ErrorClassPermissionDenied, pbv2.KVService_UpdateRangeEpoch_FullMethodName, "1", "token"},
		{"operator", audit.ResultSuccess, audit.ErrorClassNone, pbv2.KVService_UpdateRangeEpoch_FullMethodName, "1", "token"},
		{"operator", audit.ResultError, audit.ErrorClassApplication, pbv2.KVService_SplitRange_FullMethodName, "99", "token"},
		{"admin", audit.ResultSuccess, audit.ErrorClassNone, pbv2.KVService_UpdateRangeEpoch_FullMethodName, "1", "token"},
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
	if got := events[4].Principal; got != "operator" {
		t.Fatalf("operator principal = %q, want role-level principal", got)
	}

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	for _, token := range []string{kvRBACAdminToken, kvRBACOperatorToken, kvRBACReadOnlyToken, kvRBACUnknownToken} {
		if strings.Contains(string(raw), token) {
			t.Fatalf("audit file leaked token %q:\n%s", token, raw)
		}
	}
	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit file mode = %o, want 600", perm)
	}
}

func TestKVAuditRecordsHandlerError(t *testing.T) {
	var buf bytes.Buffer
	auditLog := audit.NewWriter(&buf, audit.Options{})
	policy, err := rbac.NewPolicy(rbac.Config{Tokens: map[string]string{"token": "admin"}})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	interceptor := newKVAdminInterceptor(policy, auditLog)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer token"))
	_, err = interceptor(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 3, Epoch: 1},
		&grpc.UnaryServerInfo{FullMethod: pbv2.KVService_UpdateRangeEpoch_FullMethodName},
		func(context.Context, any) (any, error) {
			return nil, status.Error(codes.Unavailable, "storage down")
		})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("handler error = %v, want Unavailable", err)
	}
	var event audit.Event
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &event); err != nil {
		t.Fatalf("audit event is not JSON: %v\n%s", err, buf.String())
	}
	if event.Result != audit.ResultError || event.ErrorClass != "unavailable" ||
		event.Range != "3" || event.Role != "admin" || event.Auth != "token" {
		t.Fatalf("handler-error audit event = %+v", event)
	}
	if strings.Contains(buf.String(), "storage down") {
		t.Fatalf("audit event leaked the handler error message: %s", buf.String())
	}
}

func TestKVAdminRBACConfigValidation(t *testing.T) {
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
				RaftAddr: "127.0.0.1:0",
				DataDir:  t.TempDir(),
				Logger:   testLogger(),
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

func TestKVAdminEmptyConfigRemainsPassThrough(t *testing.T) {
	srv, addr := startKVAdminRBACServer(t, nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	if srv.authz.Enabled() {
		t.Fatal("empty Config produced an enabled RBAC policy")
	}
	if srv.auditLog != nil {
		t.Fatal("empty Config produced an audit logger")
	}
	resp, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 3})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("UpdateRangeEpoch with empty config = %v %+v, want success", err, resp)
	}
}
