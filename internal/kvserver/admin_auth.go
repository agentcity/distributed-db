package kvserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"distributed-db/internal/audit"
	"distributed-db/internal/rbac"
)

// kvAdminMethods is the set of KVService RPCs that change cluster topology or
// range metadata. They require an admin credential (bearer token or verified
// mTLS identity) when admin auth is configured:
//
//   - SplitRange / MergeRanges: destructive data movement.
//   - AddReplica / RemoveReplica: create or stop a local Raft group.
//   - AddLearner / PromoteReplica / RemoveVoter: change the Raft voter set.
//   - TransferLeader: moves leadership between replicas.
//   - UpdateRangeEpoch: accepts a version published by the meta scheduler.
//
// Everything else is intentionally absent. Data-plane RPCs (Get, Scan,
// Prewrite, Commit, CommitSingle, Rollback, CheckTxnStatus, ResolveLock) run on
// every transaction and must keep working without an admin credential.
// Observability RPCs (Status, RangeStats, Membership) are read-only. RaftService
// methods share the listener but are the peer transport: they are authenticated
// by TLS/mTLS and must not require an admin credential, or elections and
// replication would fail.
//
// Role policy lives in internal/rbac: every method above requires
// cluster_admin, which admin and operator grant and read-only denies. The set
// is derived from the RBAC classification table so the two cannot drift.
var kvAdminMethods = rbac.KVAdminMethods()

const (
	adminTokenMetadataKey = "authorization"
	adminTokenScheme      = "Bearer"
)

// adminAuthUnaryInterceptor is the legacy single-token constructor retained for
// direct unit tests. It configures one bearer token with the admin role and no
// audit sink, matching the historical Config.AdminToken behaviour. Server.Start
// uses newKVAdminInterceptor with the policy built from Config instead.
func adminAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	if token == "" {
		return kvAdminPassThrough
	}
	policy, err := rbac.NewPolicy(rbac.Config{
		Tokens: map[string]string{token: string(rbac.RoleAdmin)},
	})
	if err != nil {
		// The token itself is valid; a failure here would mean rbac is broken.
		// Deny protected calls rather than silently disabling auth.
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if _, ok := kvAdminMethods[info.FullMethod]; !ok {
				return handler(ctx, req)
			}
			return nil, status.Error(codes.Internal, "kvserver: invalid admin auth configuration")
		}
	}
	return newKVAdminInterceptor(policy, nil, false)
}

// newKVAdminInterceptor enforces authentication, RBAC and audit on
// kvAdminMethods. Authentication accepts a configured Bearer token or a
// role-mapped Common Name from a verified mTLS client certificate. Data-plane,
// read-only and Raft RPCs pass through untouched.
//
// Every protected attempt is audited, including denials and handler errors.
// The token itself is never logged: audit records carry the role (or mTLS CN)
// and the method/range/outcome only.
//
// auditRequired turns audit failures into a codes.Unavailable rejection
// (fail-closed). The default false keeps audit best-effort, so an RPC is never
// failed merely because the audit sink is unavailable.
func newKVAdminInterceptor(policy *rbac.Policy, auditLog *audit.Logger, auditRequired ...bool) grpc.UnaryServerInterceptor {
	if policy == nil || !policy.Enabled() {
		return kvAdminPassThrough
	}
	// Variadic keeps the historical two-argument construction (best-effort
	// audit) source-compatible for tests and callers; New always passes the
	// configured value.
	required := len(auditRequired) > 0 && auditRequired[0]
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := kvAdminMethods[info.FullMethod]; !ok {
			return handler(ctx, req)
		}
		// Fail closed before the handler runs if the sink is already broken:
		// an unauditable protected mutation must not execute.
		if err := kvAuditPreflight(auditLog, required); err != nil {
			return nil, err
		}
		permission, ok := rbac.KVPermission(info.FullMethod)
		if !ok {
			// Fail closed if a method was added to the protected set without a
			// permission classification.
			if fail := kvAuditFailure(required, auditLog.Record(audit.Event{
				Method:     info.FullMethod,
				Range:      audit.RequestRange(req),
				Result:     audit.ResultDenied,
				ErrorClass: audit.ErrorClassPermissionDenied,
			})); fail != nil {
				return nil, fail
			}
			return nil, status.Error(codes.PermissionDenied, "kvserver: admin method has no RBAC classification")
		}

		identity, err := policy.Authenticate(ctx)
		if err != nil {
			if fail := kvAuditFailure(required, auditLog.Record(audit.Event{
				Method:     info.FullMethod,
				Range:      audit.RequestRange(req),
				Result:     audit.ResultDenied,
				ErrorClass: audit.ErrorClassUnauthenticated,
			})); fail != nil {
				return nil, fail
			}
			return nil, kvAdminAuthError(err)
		}
		if err := policy.Authorize(identity.Role, permission); err != nil {
			if fail := kvAuditFailure(required, auditLog.Record(kvAuditEvent(identity, info.FullMethod, req,
				audit.ResultDenied, audit.ErrorClassPermissionDenied))); fail != nil {
				return nil, fail
			}
			return nil, status.Errorf(codes.PermissionDenied,
				"kvserver: role %q may not perform %q", identity.Role, permission)
		}

		resp, err := handler(ctx, req)
		result, class := audit.Outcome(resp, err)
		if fail := kvAuditFailure(required, auditLog.Record(
			kvAuditEvent(identity, info.FullMethod, req, result, class))); fail != nil {
			return nil, fail
		}
		return resp, err
	}
}

// kvAuditPreflight reports an audit sink that cannot accept a record before a
// protected handler runs. With audit disabled or best-effort, it is a no-op.
func kvAuditPreflight(auditLog *audit.Logger, required bool) error {
	if !required {
		return nil
	}
	if err := auditLog.Healthy(); err != nil {
		return status.Error(codes.Unavailable, "kvserver: admin audit unavailable")
	}
	return nil
}

// kvAuditFailure converts a failed audit write into the fail-closed rejection.
// The error text is fixed so a filesystem path or log detail never reaches the
// client; the detailed error goes to the server logger via audit.OnError.
func kvAuditFailure(required bool, err error) error {
	if !required || err == nil {
		return nil
	}
	return status.Error(codes.Unavailable, "kvserver: admin audit unavailable")
}

// kvAdminAuthError maps an RBAC authentication error to the historical gRPC
// messages, so existing clients and tests see the same codes. It never echoes
// the presented credential.
func kvAdminAuthError(err error) error {
	if errors.Is(err, rbac.ErrInvalidToken) {
		return status.Error(codes.Unauthenticated, "kvserver: invalid admin token")
	}
	return status.Error(codes.Unauthenticated, "kvserver: admin token required")
}

func kvAuditEvent(identity rbac.Identity, method string, req any, result, class string) audit.Event {
	return audit.Event{
		Principal:  identity.Principal,
		Role:       string(identity.Role),
		Method:     method,
		Range:      audit.RequestRange(req),
		Result:     result,
		ErrorClass: class,
		Auth:       string(identity.Source),
	}
}

func kvAdminPassThrough(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(ctx, req)
}

// buildKVAdminPolicy merges the legacy AdminToken (always admin) with the
// token/role and mTLS CN/role maps, validates the configuration and returns a
// disabled policy when no credential is configured.
//
// Fail-fast rules:
//   - a role value that is not admin/operator/read-only is an error;
//   - AdminMTLSRoles requires TLS.Server.ClientCAFile (mTLS), because a CN is
//     only trustworthy when the transport verified the client certificate;
//   - an audit log path requires at least one credential, otherwise the
//     operator would believe audit is active while auth is not;
//   - AuditRequired requires AuditLogPath, and rotation options must be
//     coherent (non-negative; MaxBackups only with MaxBytes).
func buildKVAdminPolicy(cfg Config) (*rbac.Policy, error) {
	tokens := make(map[string]string, len(cfg.AdminTokenRoles)+1)
	for token, role := range cfg.AdminTokenRoles {
		tokens[token] = role
	}
	if cfg.AdminToken != "" {
		if role, ok := tokens[cfg.AdminToken]; ok {
			parsed, valid := rbac.ParseRole(role)
			if !valid || parsed != rbac.RoleAdmin {
				return nil, errors.New("kvserver: AdminToken is also listed in AdminTokenRoles with a non-admin role")
			}
		}
		tokens[cfg.AdminToken] = string(rbac.RoleAdmin)
	}
	if len(cfg.AdminMTLSRoles) > 0 && cfg.TLS.Server.ClientCAFile == "" {
		return nil, errors.New("kvserver: AdminMTLSRoles requires TLS.Server.ClientCAFile (mTLS)")
	}
	policy, err := rbac.NewPolicy(rbac.Config{Tokens: tokens, CommonNames: cfg.AdminMTLSRoles})
	if err != nil {
		return nil, fmt.Errorf("kvserver: admin RBAC: %w", err)
	}
	if cfg.AuditLogPath != "" && !policy.Enabled() {
		return nil, errors.New("kvserver: AuditLogPath requires at least one admin token or mTLS role")
	}
	if cfg.AuditLogMaxBytes < 0 {
		return nil, errors.New("kvserver: AuditLogMaxBytes must not be negative")
	}
	if cfg.AuditLogMaxBackups < 0 {
		return nil, errors.New("kvserver: AuditLogMaxBackups must not be negative")
	}
	if cfg.AuditLogMaxBackups > 0 && cfg.AuditLogMaxBytes == 0 {
		return nil, errors.New("kvserver: AuditLogMaxBackups requires AuditLogMaxBytes")
	}
	if cfg.AuditRequired && cfg.AuditLogPath == "" {
		return nil, errors.New("kvserver: AuditRequired requires AuditLogPath")
	}
	return policy, nil
}

// openKVAdminAudit opens the append-only JSONL audit file when configured. A nil
// logger means audit is disabled.
func openKVAdminAudit(cfg Config) (*audit.Logger, error) {
	if cfg.AuditLogPath == "" {
		return nil, nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	l, err := audit.Open(cfg.AuditLogPath, audit.Options{
		Sync:       cfg.AuditLogSync,
		MaxBytes:   cfg.AuditLogMaxBytes,
		MaxBackups: cfg.AuditLogMaxBackups,
		OnError: func(err error) {
			logger.Error("admin audit write failed", "error", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("kvserver: admin audit: %w", err)
	}
	return l, nil
}

// adminBearerToken is retained for callers and tests that need the raw Bearer
// parsing helper; it never logs the returned bytes.
func adminBearerToken(ctx context.Context) ([]byte, bool) {
	return rbac.BearerToken(ctx)
}

// WithAdminToken returns a child context carrying token as an
// "authorization: Bearer <token>" outgoing credential. An empty token leaves
// ctx unchanged.
func WithAdminToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, adminTokenMetadataKey, adminTokenScheme+" "+token)
}

// AdminTokenDialOption returns a gRPC dial option that attaches token to
// outbound KVService admin RPCs made through the resulting ClientConn. Only the
// methods in kvAdminMethods receive the credential; data-plane, read-only and
// Raft RPCs are untouched. An empty token adds a pass-through interceptor.
func AdminTokenDialOption(token string) grpc.DialOption {
	return grpc.WithChainUnaryInterceptor(adminTokenClientInterceptor(token))
}

// adminTokenClientInterceptor is the client-side counterpart of the admin auth
// interceptor. It only decorates outbound contexts for KV admin methods, so a
// single ClientConn can serve authenticated admin calls and unauthenticated
// data-plane calls.
func adminTokenClientInterceptor(token string) grpc.UnaryClientInterceptor {
	if token == "" {
		return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := kvAdminMethods[method]; ok {
			ctx = WithAdminToken(ctx, token)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
