// Package rbac defines the control-plane roles and permissions used by the
// KV and meta admin gRPC interceptors.
//
// This is an MVP authorization layer for admin RPCs only. Data-plane RPCs
// (Get/Scan/Prewrite/Commit/..., AllocTS/GetRange/ListRanges/Leader) are not
// authenticated or authorized here and keep their historical behaviour. There
// is no per-user database grant, tenant isolation or resource-level policy.
//
// Roles are deliberately coarse:
//
//   - admin: every control-plane permission, including cluster configuration.
//   - operator: cluster operations (split/merge/replica/leader mutations) but
//     not configuration/RBAC changes.
//   - read-only: no admin permission. It exists so an operator can be issued a
//     credential that cannot mutate the cluster; data-plane reads do not
//     require this credential at all.
//
// Unknown roles are denied. A role is resolved from either a bearer token
// mapped to a role (Config.Tokens) or the Common Name of a verified mTLS
// client certificate (Config.CommonNames). A plain role header is never
// trusted: only verified transport identity or a configured secret is used.
package rbac

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	pbv2 "distributed-db/internal/pb/v2"
)

// Role is a control-plane role name. Only the canonical constants below are
// valid; configuration parsing accepts a few aliases, but an unmapped or
// misspelled role is rejected rather than silently treated as a role.
type Role string

const (
	// RoleUnknown is the zero value: no authenticated role. It has no
	// permissions and is denied by Authorize.
	RoleUnknown Role = ""
	// RoleAdmin may perform every control-plane operation.
	RoleAdmin Role = "admin"
	// RoleOperator may perform cluster operations but not configuration or
	// RBAC changes.
	RoleOperator Role = "operator"
	// RoleReadOnly may not perform any admin operation. It is useful to issue
	// a credential that is explicitly allowed to read but never to mutate.
	RoleReadOnly Role = "read-only"
)

// ParseRole normalizes a configured role string. It accepts the canonical
// names plus common spelling variants; an unknown value returns
// (RoleUnknown, false) so callers can fail closed.
func ParseRole(raw string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "admin":
		return RoleAdmin, true
	case "operator":
		return RoleOperator, true
	case "read-only", "readonly", "read_only", "ro":
		return RoleReadOnly, true
	default:
		return RoleUnknown, false
	}
}

// Valid reports whether r is one of the canonical role constants. It does not
// normalize aliases: configuration must be parsed through ParseRole, and a
// role value that reached a decision point unnormalized is treated as unknown.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleReadOnly:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (r Role) String() string { return string(r) }

// Permission is one capability required by an admin RPC.
type Permission string

const (
	// PermissionClusterAdmin covers topology and replication changes such as
	// SplitRange, MergeRanges, replica/voter changes, leader transfer and range
	// epoch publication.
	PermissionClusterAdmin Permission = "cluster_admin"
	// PermissionConfig covers cluster-wide configuration and RBAC/credential
	// changes. In the current RPC surface only MetaService.SetRanges is
	// classified here (it replaces the whole range map). Future RBAC,
	// credential-rotation or config-reload RPCs must be added to this
	// permission and are admin-only by construction.
	PermissionConfig Permission = "config"
)

// Allows reports whether r grants p. Unknown roles grant nothing.
func (r Role) Allows(p Permission) bool {
	switch r {
	case RoleAdmin:
		// The admin role is intentionally all-powerful for this MVP.
		return p == PermissionClusterAdmin || p == PermissionConfig
	case RoleOperator:
		return p == PermissionClusterAdmin
	case RoleReadOnly:
		return false
	default:
		return false
	}
}

// kvMethodPermissions is the KVService admin classification. Every method here
// requires a credential and a role allowed for the listed permission.
//
// Data-plane and read-only RPCs are deliberately absent; see the kvserver
// admin_auth.go package documentation for the full classification rationale.
var kvMethodPermissions = map[string]Permission{
	pbv2.KVService_SplitRange_FullMethodName:       PermissionClusterAdmin,
	pbv2.KVService_MergeRanges_FullMethodName:      PermissionClusterAdmin,
	pbv2.KVService_AddReplica_FullMethodName:       PermissionClusterAdmin,
	pbv2.KVService_AddLearner_FullMethodName:       PermissionClusterAdmin,
	pbv2.KVService_PromoteReplica_FullMethodName:   PermissionClusterAdmin,
	pbv2.KVService_RemoveVoter_FullMethodName:      PermissionClusterAdmin,
	pbv2.KVService_RemoveReplica_FullMethodName:    PermissionClusterAdmin,
	pbv2.KVService_TransferLeader_FullMethodName:   PermissionClusterAdmin,
	pbv2.KVService_UpdateRangeEpoch_FullMethodName: PermissionClusterAdmin,
}

// metaMethodPermissions is the MetaService admin classification.
//
// SetRanges replaces the complete cluster range map, which is configuration
// rather than a routine cluster operation, so it is admin-only. SplitRange and
// MergeRanges are operational and therefore available to the operator role.
var metaMethodPermissions = map[string]Permission{
	pbv2.MetaService_SetRanges_FullMethodName:   PermissionConfig,
	pbv2.MetaService_SplitRange_FullMethodName:  PermissionClusterAdmin,
	pbv2.MetaService_MergeRanges_FullMethodName: PermissionClusterAdmin,
}

// KVPermission returns the permission required by a KVService method. The
// second result is false when the method is not an admin RPC.
func KVPermission(method string) (Permission, bool) {
	p, ok := kvMethodPermissions[method]
	return p, ok
}

// MetaPermission returns the permission required by a MetaService method. The
// second result is false when the method is not an admin RPC.
func MetaPermission(method string) (Permission, bool) {
	p, ok := metaMethodPermissions[method]
	return p, ok
}

// PermissionForMethod returns the permission required by either KVService or
// MetaService method. The second result is false for unprotected RPCs.
func PermissionForMethod(method string) (Permission, bool) {
	if p, ok := KVPermission(method); ok {
		return p, true
	}
	return MetaPermission(method)
}

// KVAdminMethods returns a copy of the KVService admin method set.
func KVAdminMethods() map[string]struct{} {
	return methodSet(kvMethodPermissions)
}

// MetaAdminMethods returns a copy of the MetaService admin method set.
func MetaAdminMethods() map[string]struct{} {
	return methodSet(metaMethodPermissions)
}

func methodSet(permissions map[string]Permission) map[string]struct{} {
	set := make(map[string]struct{}, len(permissions))
	for method := range permissions {
		set[method] = struct{}{}
	}
	return set
}

// Config configures a Policy. Both maps are optional; an empty Config yields a
// disabled policy (Enabled reports false) so an unconfigured server keeps its
// historical behaviour.
type Config struct {
	// Tokens maps a bearer token to a role name. Tokens are stored only as
	// SHA-256 digests, and they are never included in errors, logs or audit
	// records.
	Tokens map[string]string
	// CommonNames maps the Common Name of a *verified* mTLS client certificate
	// to a role name. Entries only match when the transport authenticated the
	// peer with a certificate chain; a plaintext client cannot spoof a CN.
	CommonNames map[string]string
}

// Policy resolves a request identity and authorizes it against a permission.
// It is immutable after NewPolicy and safe for concurrent use.
type Policy struct {
	tokens      map[[sha256.Size]byte]Role
	commonNames map[string]Role
}

// NewPolicy validates and digests the configured token and mTLS CN maps.
// Configuration errors never echo a token value.
func NewPolicy(cfg Config) (*Policy, error) {
	p := &Policy{
		tokens:      make(map[[sha256.Size]byte]Role, len(cfg.Tokens)),
		commonNames: make(map[string]Role, len(cfg.CommonNames)),
	}
	for token, rawRole := range cfg.Tokens {
		if token == "" {
			return nil, errors.New("rbac: empty bearer token in token role map")
		}
		role, ok := ParseRole(rawRole)
		if !ok {
			return nil, fmt.Errorf("rbac: unknown role %q in token role map", rawRole)
		}
		p.tokens[sha256.Sum256([]byte(token))] = role
	}
	for cn, rawRole := range cfg.CommonNames {
		if strings.TrimSpace(cn) == "" {
			return nil, errors.New("rbac: empty mTLS common name in role map")
		}
		role, ok := ParseRole(rawRole)
		if !ok {
			return nil, fmt.Errorf("rbac: unknown role %q for mTLS common name %q", rawRole, cn)
		}
		p.commonNames[cn] = role
	}
	return p, nil
}

// Enabled reports whether any credential source is configured. A disabled
// policy must not be applied: callers keep the historical no-auth behaviour.
func (p *Policy) Enabled() bool {
	return p != nil && (len(p.tokens) > 0 || len(p.commonNames) > 0)
}

// Source identifies which credential established an Identity. It is included
// in audit records but is not secret.
type Source string

const (
	SourceToken Source = "token"
	SourceMTLS  Source = "mtls"
)

// Identity is the authenticated principal for one admin request.
type Identity struct {
	// Principal is the mTLS certificate Common Name when the identity came
	// from mTLS. For a token it is the role name: this MVP has no per-token
	// user name, so the token path is role-level, not per-user.
	Principal string
	Role      Role
	Source    Source
}

var (
	// ErrNoCredentials means no configured credential was presented. Callers
	// map it to Unauthenticated.
	ErrNoCredentials = errors.New("rbac: no admin credential presented")
	// ErrInvalidToken means a bearer token was presented but is not
	// configured. Callers map it to Unauthenticated.
	ErrInvalidToken = errors.New("rbac: invalid admin token")
	// ErrForbidden means the identity was authenticated but its role does not
	// grant the required permission. Callers map it to PermissionDenied.
	ErrForbidden = errors.New("rbac: forbidden")
)

// Authenticate resolves the request identity from, in order:
//
//  1. an "authorization: Bearer <token>" credential mapped in Config.Tokens;
//  2. the Common Name of a verified mTLS client certificate mapped in
//     Config.CommonNames.
//
// A well-formed Bearer token is authoritative: if it is presented and not
// configured, Authenticate returns ErrInvalidToken even when a valid mTLS
// identity is also available, so a bad token is never silently ignored. An
// unconfigured mTLS CN yields ErrNoCredentials. Errors never contain the
// presented token.
func (p *Policy) Authenticate(ctx context.Context) (Identity, error) {
	if !p.Enabled() {
		return Identity{}, ErrNoCredentials
	}
	if token, ok := BearerToken(ctx); ok {
		role, known := p.tokens[sha256.Sum256(token)]
		if !known {
			return Identity{}, ErrInvalidToken
		}
		return Identity{Principal: string(role), Role: role, Source: SourceToken}, nil
	}
	if cn, ok := mtlsCommonName(ctx); ok {
		if role, known := p.commonNames[cn]; known {
			return Identity{Principal: cn, Role: role, Source: SourceMTLS}, nil
		}
	}
	return Identity{}, ErrNoCredentials
}

// Authorize checks role against permission. An unknown role is denied by
// default; an authenticated operator is denied configuration/RBAC changes, and
// a read-only role is denied every admin permission.
func (p *Policy) Authorize(role Role, permission Permission) error {
	if !role.Valid() {
		return fmt.Errorf("%w: unknown role %q", ErrForbidden, string(role))
	}
	if !role.Allows(permission) {
		return fmt.Errorf("%w: role %q lacks permission %q", ErrForbidden, role, permission)
	}
	return nil
}

// BearerToken extracts a token from an "authorization: Bearer <token>"
// incoming metadata entry. The scheme is case-insensitive per RFC 6750 and the
// token is returned verbatim. Multiple authorization entries are accepted when
// any carries a well-formed non-empty Bearer credential. Callers must never log
// the returned bytes.
func BearerToken(ctx context.Context) ([]byte, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false
	}
	for _, value := range md.Get("authorization") {
		scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			continue
		}
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		return []byte(token), true
	}
	return nil, false
}

// mtlsCommonName returns the Common Name of a verified client certificate from
// the gRPC peer information. It requires a non-empty VerifiedChains, which
// gRPC populates only after the TLS handshake verified the client certificate
// against the configured client CA.
func mtlsCommonName(ctx context.Context) (string, bool) {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr == nil || pr.AuthInfo == nil {
		return "", false
	}
	var info credentials.TLSInfo
	switch ai := pr.AuthInfo.(type) {
	case credentials.TLSInfo:
		info = ai
	case *credentials.TLSInfo:
		if ai == nil {
			return "", false
		}
		info = *ai
	default:
		return "", false
	}
	if len(info.State.VerifiedChains) == 0 {
		return "", false
	}
	chain := info.State.VerifiedChains[0]
	if len(chain) == 0 || chain[0] == nil {
		return "", false
	}
	cn := strings.TrimSpace(chain[0].Subject.CommonName)
	if cn == "" {
		return "", false
	}
	return cn, true
}
