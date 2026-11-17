package rbac

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	pbv2 "distributed-db/internal/pb/v2"
)

func TestParseRole(t *testing.T) {
	cases := []struct {
		raw  string
		want Role
		ok   bool
	}{
		{raw: "admin", want: RoleAdmin, ok: true},
		{raw: " ADMIN ", want: RoleAdmin, ok: true},
		{raw: "operator", want: RoleOperator, ok: true},
		{raw: "read-only", want: RoleReadOnly, ok: true},
		{raw: "readonly", want: RoleReadOnly, ok: true},
		{raw: "read_only", want: RoleReadOnly, ok: true},
		{raw: "RO", want: RoleReadOnly, ok: true},
		{raw: "root"},
		{raw: "superuser"},
		{raw: ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := ParseRole(tc.raw)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseRole(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRolePermissionMatrix(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleAdmin, PermissionClusterAdmin, true},
		{RoleAdmin, PermissionConfig, true},
		{RoleOperator, PermissionClusterAdmin, true},
		{RoleOperator, PermissionConfig, false},
		{RoleReadOnly, PermissionClusterAdmin, false},
		{RoleReadOnly, PermissionConfig, false},
		{RoleUnknown, PermissionClusterAdmin, false},
		{RoleUnknown, PermissionConfig, false},
		{Role("superuser"), PermissionConfig, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+string(tc.perm), func(t *testing.T) {
			if got := tc.role.Allows(tc.perm); got != tc.want {
				t.Fatalf("%q.Allows(%q) = %v, want %v", tc.role, tc.perm, got, tc.want)
			}
		})
	}
}

func TestAuthorizeDeniesUnknownRoleByDefault(t *testing.T) {
	p, err := NewPolicy(Config{Tokens: map[string]string{"token": "admin"}})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	for _, role := range []Role{RoleUnknown, Role("root"), Role("Admin")} {
		if err := p.Authorize(role, PermissionConfig); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Authorize(%q, config) = %v, want ErrForbidden", role, err)
		}
	}
	if err := p.Authorize(RoleOperator, PermissionConfig); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator config authorization = %v, want ErrForbidden", err)
	}
	if err := p.Authorize(RoleReadOnly, PermissionClusterAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only cluster authorization = %v, want ErrForbidden", err)
	}
	if err := p.Authorize(RoleOperator, PermissionClusterAdmin); err != nil {
		t.Fatalf("operator cluster authorization: %v", err)
	}
}

func TestPermissionForMethod(t *testing.T) {
	kvMethods := []string{
		pbv2.KVService_SplitRange_FullMethodName,
		pbv2.KVService_MergeRanges_FullMethodName,
		pbv2.KVService_AddReplica_FullMethodName,
		pbv2.KVService_AddLearner_FullMethodName,
		pbv2.KVService_PromoteReplica_FullMethodName,
		pbv2.KVService_RemoveVoter_FullMethodName,
		pbv2.KVService_RemoveReplica_FullMethodName,
		pbv2.KVService_TransferLeader_FullMethodName,
		pbv2.KVService_UpdateRangeEpoch_FullMethodName,
	}
	for _, method := range kvMethods {
		p, ok := PermissionForMethod(method)
		if !ok || p != PermissionClusterAdmin {
			t.Fatalf("%s = (%q, %v), want (cluster_admin, true)", method, p, ok)
		}
	}
	if p, ok := MetaPermission(pbv2.MetaService_SetRanges_FullMethodName); !ok || p != PermissionConfig {
		t.Fatalf("SetRanges = (%q, %v), want (config, true)", p, ok)
	}
	for _, method := range []string{
		pbv2.MetaService_SplitRange_FullMethodName,
		pbv2.MetaService_MergeRanges_FullMethodName,
	} {
		if p, ok := MetaPermission(method); !ok || p != PermissionClusterAdmin {
			t.Fatalf("%s = (%q, %v), want (cluster_admin, true)", method, p, ok)
		}
	}

	// Data-plane and observability RPCs must remain unclassified.
	for _, method := range []string{
		pbv2.KVService_Get_FullMethodName,
		pbv2.KVService_Scan_FullMethodName,
		pbv2.KVService_Prewrite_FullMethodName,
		pbv2.KVService_Commit_FullMethodName,
		pbv2.KVService_CommitSingle_FullMethodName,
		pbv2.KVService_Rollback_FullMethodName,
		pbv2.KVService_CheckTxnStatus_FullMethodName,
		pbv2.KVService_ResolveLock_FullMethodName,
		pbv2.KVService_Status_FullMethodName,
		pbv2.KVService_RangeStats_FullMethodName,
		pbv2.KVService_Membership_FullMethodName,
		pbv2.MetaService_AllocTS_FullMethodName,
		pbv2.MetaService_GetRange_FullMethodName,
		pbv2.MetaService_ListRanges_FullMethodName,
		pbv2.MetaService_Leader_FullMethodName,
		pbv2.RaftService_RequestVote_FullMethodName,
		pbv2.RaftService_AppendEntries_FullMethodName,
		"/unknown.Service/Method",
	} {
		if p, ok := PermissionForMethod(method); ok {
			t.Fatalf("%s was classified as %q; data/read RPCs must not require RBAC", method, p)
		}
	}
}

func TestAdminMethodSetsCoverEveryClassifiedMethod(t *testing.T) {
	kv := KVAdminMethods()
	if len(kv) != len(kvMethodPermissions) {
		t.Fatalf("KVAdminMethods size = %d, want %d", len(kv), len(kvMethodPermissions))
	}
	for method := range kvMethodPermissions {
		if _, ok := kv[method]; !ok {
			t.Fatalf("KVAdminMethods is missing %s", method)
		}
	}
	meta := MetaAdminMethods()
	if len(meta) != len(metaMethodPermissions) {
		t.Fatalf("MetaAdminMethods size = %d, want %d", len(meta), len(metaMethodPermissions))
	}
	for method := range metaMethodPermissions {
		if _, ok := meta[method]; !ok {
			t.Fatalf("MetaAdminMethods is missing %s", method)
		}
	}

	// The sets are copies: mutating them must not affect the policy tables.
	delete(kv, pbv2.KVService_SplitRange_FullMethodName)
	if _, ok := KVPermission(pbv2.KVService_SplitRange_FullMethodName); !ok {
		t.Fatal("KVAdminMethods returned a shared map")
	}
}

func TestNewPolicyValidationDoesNotLeakTokens(t *testing.T) {
	const token = "super-secret-operator-token"
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "unknown token role",
			cfg:  Config{Tokens: map[string]string{token: "root"}},
			want: "unknown role",
		},
		{
			name: "unknown cn role",
			cfg:  Config{CommonNames: map[string]string{"client.test": "root"}},
			want: "unknown role",
		},
		{
			name: "empty token",
			cfg:  Config{Tokens: map[string]string{"": "admin"}},
			want: "empty bearer token",
		},
		{
			name: "empty cn",
			cfg:  Config{CommonNames: map[string]string{" ": "admin"}},
			want: "empty mTLS common name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPolicy(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewPolicy error = %v, want substring %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("NewPolicy error leaked the token: %v", err)
			}
		})
	}
}

func TestPolicyDisabled(t *testing.T) {
	p, err := NewPolicy(Config{})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if p.Enabled() {
		t.Fatal("empty Config produced an enabled policy")
	}
	if _, err := p.Authenticate(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("disabled Authenticate = %v, want ErrNoCredentials", err)
	}
	var nilPolicy *Policy
	if nilPolicy.Enabled() {
		t.Fatal("nil policy reported Enabled")
	}
	if _, err := nilPolicy.Authenticate(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("nil Authenticate = %v, want ErrNoCredentials", err)
	}
}

func tokenCtx(ctx context.Context, token string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}

func TestAuthenticateTokenRoles(t *testing.T) {
	const (
		adminToken    = "admin-token-value"
		operatorToken = "operator-token-value"
		readOnlyToken = "read-only-token-value"
		unknownToken  = "unknown-token-value"
	)
	p, err := NewPolicy(Config{Tokens: map[string]string{
		adminToken:    "admin",
		operatorToken: "operator",
		readOnlyToken: "read-only",
	}})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	cases := []struct {
		token     string
		wantRole  Role
		wantClass Source
	}{
		{adminToken, RoleAdmin, SourceToken},
		{operatorToken, RoleOperator, SourceToken},
		{readOnlyToken, RoleReadOnly, SourceToken},
	}
	for _, tc := range cases {
		id, err := p.Authenticate(tokenCtx(context.Background(), tc.token))
		if err != nil {
			t.Fatalf("Authenticate(%q): %v", tc.wantRole, err)
		}
		if id.Role != tc.wantRole || id.Source != tc.wantClass || id.Principal != string(tc.wantRole) {
			t.Fatalf("identity = %+v, want role %q source %q principal %q",
				id, tc.wantRole, tc.wantClass, tc.wantRole)
		}
	}

	if _, err := p.Authenticate(tokenCtx(context.Background(), unknownToken)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown token = %v, want ErrInvalidToken", err)
	}
	if _, err := p.Authenticate(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no credentials = %v, want ErrNoCredentials", err)
	}
	// Wrong schemes and raw tokens are not a presented Bearer credential.
	for _, value := range []string{adminToken, "Basic " + adminToken, "Bearer "} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
		_, err := p.Authenticate(ctx)
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("authorization %q = %v, want ErrNoCredentials", value, err)
		}
		if strings.Contains(err.Error(), adminToken) {
			t.Fatalf("error leaked the token: %v", err)
		}
	}
}

func verifiedPeerCtx(cn string, verified bool) context.Context {
	var chains [][]*x509.Certificate
	if verified {
		chains = [][]*x509.Certificate{{{
			Subject: pkix.Name{CommonName: cn},
		}}}
	}
	info := credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: chains}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: info})
}

func TestAuthenticateMTLSCommonName(t *testing.T) {
	p, err := NewPolicy(Config{CommonNames: map[string]string{
		"ops-client":  "operator",
		"root-client": "admin",
	}})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	id, err := p.Authenticate(verifiedPeerCtx("ops-client", true))
	if err != nil {
		t.Fatalf("Authenticate(ops-client): %v", err)
	}
	if id.Role != RoleOperator || id.Source != SourceMTLS || id.Principal != "ops-client" {
		t.Fatalf("identity = %+v, want operator/mtls/ops-client", id)
	}

	if _, err := p.Authenticate(verifiedPeerCtx("unmapped-client", true)); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("unmapped CN = %v, want ErrNoCredentials", err)
	}
	// A certificate with no verified chain (e.g. a plaintext peer or an
	// unauthenticated TLS client) must not be accepted from its CN.
	if _, err := p.Authenticate(verifiedPeerCtx("ops-client", false)); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("unverified CN = %v, want ErrNoCredentials", err)
	}
}

func TestTokenTakesPrecedenceOverMTLS(t *testing.T) {
	p, err := NewPolicy(Config{
		Tokens:      map[string]string{"ops-token": "operator"},
		CommonNames: map[string]string{"root-client": "admin"},
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	ctx := verifiedPeerCtx("root-client", true)
	id, err := p.Authenticate(tokenCtx(ctx, "ops-token"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Role != RoleOperator || id.Source != SourceToken {
		t.Fatalf("identity = %+v, want the token's operator role", id)
	}

	// An invalid Bearer token is rejected even when a valid mTLS identity is
	// also present; it must not silently fall back.
	if _, err := p.Authenticate(tokenCtx(ctx, "bad-token")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad token over mTLS = %v, want ErrInvalidToken", err)
	}

	// With no Bearer credential, the mTLS CN is used.
	id, err = p.Authenticate(ctx)
	if err != nil {
		t.Fatalf("Authenticate(mTLS): %v", err)
	}
	if id.Role != RoleAdmin || id.Source != SourceMTLS {
		t.Fatalf("identity = %+v, want the mTLS admin role", id)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	base := context.Background()
	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "bearer", value: "Bearer s3cret", want: "s3cret", ok: true},
		{name: "lowercase scheme", value: "bearer s3cret", want: "s3cret", ok: true},
		{name: "extra spaces", value: "Bearer   s3cret", want: "s3cret", ok: true},
		{name: "basic", value: "Basic s3cret"},
		{name: "raw token", value: "s3cret"},
		{name: "empty bearer", value: "Bearer   "},
		{name: "empty", value: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(base, metadata.Pairs("authorization", tc.value))
			got, ok := BearerToken(ctx)
			if ok != tc.ok || string(got) != tc.want {
				t.Fatalf("BearerToken(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}
