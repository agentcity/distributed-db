package metaserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"distributed-db/internal/audit"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/rbac"
)

// metaAdminMethods is the set of MetaService RPCs that mutate cluster
// metadata or orchestrate destructive data movement. They require an admin
// credential (bearer token or verified mTLS identity) when admin auth is
// configured.
//
// Read-only RPCs (AllocTS, GetRange, ListRanges, Leader) are intentionally
// absent: data-plane clients call them on every transaction and they must keep
// working without an admin credential. RaftService methods are also untouched;
// they run on the same listener in replicated mode but are authenticated by
// the Raft transport (TLS/mTLS) rather than by this policy.
//
// Role policy lives in internal/rbac:
//
//   - SetRanges replaces the whole range map, so it is configuration
//     (PermissionConfig) and admin-only.
//   - SplitRange and MergeRanges are cluster operations (PermissionClusterAdmin)
//     available to admin and operator; read-only is denied.
//
// Scope limit: this interceptor only gates inbound MetaService calls. The
// scheduler's outbound KVService admin calls (SplitRange, MergeRanges,
// AddReplica, AddLearner, PromoteReplica, RemoveVoter, RemoveReplica) are
// served by the separate kvserver listener and are not covered here. Those
// RPCs need the KV admin credential in the fan-out. AddReplica and RemoveReplica
// on *Server are in-process orchestration methods, not gRPC handlers, so they
// are unreachable from the network.
var metaAdminMethods = rbac.MetaAdminMethods()

// adminAuthUnaryInterceptor is the legacy single-token constructor retained for
// direct unit tests. It configures one bearer token with the admin role and no
// audit sink, matching the historical Config.AdminToken behaviour.
// Server.Start uses newMetaAdminInterceptor with the policy built from Config.
func adminAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	if token == "" {
		return metaAdminPassThrough
	}
	policy, err := rbac.NewPolicy(rbac.Config{
		Tokens: map[string]string{token: string(rbac.RoleAdmin)},
	})
	if err != nil {
		// The token itself is valid; a failure here would mean rbac is broken.
		// Deny protected calls rather than silently disabling auth.
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if _, ok := metaAdminMethods[info.FullMethod]; !ok {
				return handler(ctx, req)
			}
			return nil, status.Error(codes.Internal, "metaserver: invalid admin auth configuration")
		}
	}
	return newMetaAdminInterceptor(policy, nil)
}

// newMetaAdminInterceptor enforces authentication, RBAC and audit on
// metaAdminMethods. Every protected attempt is audited, including denials and
// handler/application failures. Tokens never appear in audit records: the
// record carries the role (or mTLS CN), method, range and outcome only.
func newMetaAdminInterceptor(policy *rbac.Policy, auditLog *audit.Logger) grpc.UnaryServerInterceptor {
	if policy == nil || !policy.Enabled() {
		return metaAdminPassThrough
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := metaAdminMethods[info.FullMethod]; !ok {
			return handler(ctx, req)
		}
		permission, ok := rbac.MetaPermission(info.FullMethod)
		if !ok {
			_ = auditLog.Record(metaAuditEvent(rbac.Identity{}, info.FullMethod, req,
				audit.ResultDenied, audit.ErrorClassPermissionDenied))
			return nil, status.Error(codes.PermissionDenied, "metaserver: admin method has no RBAC classification")
		}

		identity, err := policy.Authenticate(ctx)
		if err != nil {
			_ = auditLog.Record(metaAuditEvent(rbac.Identity{}, info.FullMethod, req,
				audit.ResultDenied, audit.ErrorClassUnauthenticated))
			return nil, metaAdminAuthError(err)
		}
		if err := policy.Authorize(identity.Role, permission); err != nil {
			_ = auditLog.Record(metaAuditEvent(identity, info.FullMethod, req,
				audit.ResultDenied, audit.ErrorClassPermissionDenied))
			return nil, status.Errorf(codes.PermissionDenied,
				"metaserver: role %q may not perform %q", identity.Role, permission)
		}

		resp, err := handler(ctx, req)
		result, class := audit.Outcome(resp, err)
		_ = auditLog.Record(metaAuditEvent(identity, info.FullMethod, req, result, class))
		return resp, err
	}
}

// metaAdminAuthError maps an RBAC authentication error to the historical gRPC
// messages, so existing clients and tests see the same codes. It never echoes
// the presented credential.
func metaAdminAuthError(err error) error {
	if errors.Is(err, rbac.ErrInvalidToken) {
		return status.Error(codes.Unauthenticated, "metaserver: invalid admin token")
	}
	return status.Error(codes.Unauthenticated, "metaserver: admin token required")
}

func metaAuditEvent(identity rbac.Identity, method string, req any, result, class string) audit.Event {
	rangeLabel := audit.RequestRange(req)
	if rangeLabel == "" && method == pbv2.MetaService_SetRanges_FullMethodName {
		rangeLabel = "cluster"
	}
	return audit.Event{
		Principal:  identity.Principal,
		Role:       string(identity.Role),
		Method:     method,
		Range:      rangeLabel,
		Result:     result,
		ErrorClass: class,
		Auth:       string(identity.Source),
	}
}

func metaAdminPassThrough(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(ctx, req)
}

// buildMetaAdminPolicy merges the legacy AdminToken (always admin) with the
// token/role and mTLS CN/role maps, validates the configuration and returns a
// disabled policy when no credential is configured.
//
// Fail-fast rules mirror kvserver: unknown roles are rejected; AdminMTLSRoles
// requires TLS.Server.ClientCAFile (mTLS); an audit log path requires at least
// one credential.
func buildMetaAdminPolicy(cfg Config) (*rbac.Policy, error) {
	tokens := make(map[string]string, len(cfg.AdminTokenRoles)+1)
	for token, role := range cfg.AdminTokenRoles {
		tokens[token] = role
	}
	if cfg.AdminToken != "" {
		if role, ok := tokens[cfg.AdminToken]; ok {
			parsed, valid := rbac.ParseRole(role)
			if !valid || parsed != rbac.RoleAdmin {
				return nil, errors.New("metaserver: AdminToken is also listed in AdminTokenRoles with a non-admin role")
			}
		}
		tokens[cfg.AdminToken] = string(rbac.RoleAdmin)
	}
	if len(cfg.AdminMTLSRoles) > 0 && cfg.TLS.Server.ClientCAFile == "" {
		return nil, errors.New("metaserver: AdminMTLSRoles requires TLS.Server.ClientCAFile (mTLS)")
	}
	policy, err := rbac.NewPolicy(rbac.Config{Tokens: tokens, CommonNames: cfg.AdminMTLSRoles})
	if err != nil {
		return nil, fmt.Errorf("metaserver: admin RBAC: %w", err)
	}
	if cfg.AuditLogPath != "" && !policy.Enabled() {
		return nil, errors.New("metaserver: AuditLogPath requires at least one admin token or mTLS role")
	}
	return policy, nil
}

// openMetaAdminAudit opens the append-only JSONL audit file when configured. A
// nil logger means audit is disabled.
func openMetaAdminAudit(cfg Config) (*audit.Logger, error) {
	if cfg.AuditLogPath == "" {
		return nil, nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	l, err := audit.Open(cfg.AuditLogPath, audit.Options{
		Sync: cfg.AuditLogSync,
		OnError: func(err error) {
			logger.Error("admin audit write failed", "error", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("metaserver: admin audit: %w", err)
	}
	return l, nil
}

// bearerToken is retained for callers and tests that need the raw Bearer
// parsing helper; it never logs the returned bytes.
func bearerToken(ctx context.Context) ([]byte, bool) {
	return rbac.BearerToken(ctx)
}
