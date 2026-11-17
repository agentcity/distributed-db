package kvserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
)

// kvAdminMethods is the set of KVService RPCs that change cluster topology or
// range metadata and therefore require the admin bearer token when
// Config.AdminToken is set:
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
// by TLS/mTLS and must not require this token, or elections and replication
// would fail.
//
// Membership is deliberately not protected: it only reports the current
// voter/learner sets and does not mutate them.
//
// Limitations: this is a single shared bearer credential with all-or-nothing
// admin authority. It provides no per-operator identity, no RBAC scopes and no
// audit trail; those remain follow-ups. The token is independent of transport
// TLS but TLS is still required to protect it in transit.
//
// Wiring gap (not fixed here; metaserver is out of scope): metaserver's
// outbound KV admin fan-out (SplitRange, MergeRanges, AddReplica, AddLearner,
// PromoteReplica, RemoveVoter, RemoveReplica, TransferLeader and
// UpdateRangeEpoch) currently dials KV nodes without an admin bearer token. A
// metaserver that fans out to a KV node with AdminToken set will therefore get
// Unauthenticated from every admin RPC until it is wired. Once a metaserver
// config carries the same token, its dial helper can add AdminTokenDialOption
// to the ClientConn, or each call site can wrap its context with
// WithAdminToken. Both helpers below are exported for exactly that purpose.
var kvAdminMethods = map[string]struct{}{
	pbv2.KVService_SplitRange_FullMethodName:       {},
	pbv2.KVService_MergeRanges_FullMethodName:      {},
	pbv2.KVService_AddReplica_FullMethodName:       {},
	pbv2.KVService_AddLearner_FullMethodName:       {},
	pbv2.KVService_PromoteReplica_FullMethodName:   {},
	pbv2.KVService_RemoveVoter_FullMethodName:      {},
	pbv2.KVService_RemoveReplica_FullMethodName:    {},
	pbv2.KVService_TransferLeader_FullMethodName:   {},
	pbv2.KVService_UpdateRangeEpoch_FullMethodName: {},
}

const (
	adminTokenMetadataKey = "authorization"
	adminTokenScheme      = "Bearer"
)

// adminAuthUnaryInterceptor enforces the admin bearer token on kvAdminMethods.
// An empty token disables the check entirely (development default) so existing
// deployments and tests are unaffected; production clusters must set
// Config.AdminToken.
//
// The interceptor hashes both the configured and the presented token with
// SHA-256 and compares the fixed-size digests with subtle.ConstantTimeCompare,
// so timing does not depend on the token length or on the first differing byte.
// Rejections never echo the presented or expected token, and neither token is
// logged.
func adminAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	if token == "" {
		return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	}
	expected := sha256.Sum256([]byte(token))
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := kvAdminMethods[info.FullMethod]; !ok {
			return handler(ctx, req)
		}
		presented, ok := adminBearerToken(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "kvserver: admin token required")
		}
		got := sha256.Sum256(presented)
		if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			return nil, status.Error(codes.Unauthenticated, "kvserver: invalid admin token")
		}
		return handler(ctx, req)
	}
}

// adminBearerToken extracts the token from an "authorization: Bearer <token>"
// incoming metadata entry. The scheme is case-insensitive per RFC 6750 and the
// token itself is compared verbatim. Multiple authorization entries are
// accepted when any of them carries a well-formed Bearer credential.
func adminBearerToken(ctx context.Context) ([]byte, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false
	}
	for _, value := range md.Get(adminTokenMetadataKey) {
		scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
		if !ok || !strings.EqualFold(scheme, adminTokenScheme) {
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

// WithAdminToken returns a child context carrying token as an
// "authorization: Bearer <token>" outgoing credential. It is the per-call
// escape hatch for callers that cannot add a dial option (for example a future
// metaserver fan-out that receives a token at request time). An empty token
// leaves ctx unchanged.
func WithAdminToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, adminTokenMetadataKey, adminTokenScheme+" "+token)
}

// AdminTokenDialOption returns a gRPC dial option that attaches token to
// outbound KVService admin RPCs made through the resulting ClientConn. Only the
// methods in kvAdminMethods receive the credential; data-plane, read-only and
// Raft RPCs are untouched. It exists so a future metaserver fan-out can enable
// admin auth in its dial helper with one option. An empty token adds a
// pass-through interceptor.
func AdminTokenDialOption(token string) grpc.DialOption {
	return grpc.WithChainUnaryInterceptor(adminTokenClientInterceptor(token))
}

// adminTokenClientInterceptor is the client-side counterpart of
// adminAuthUnaryInterceptor. It only decorates outbound contexts for KV admin
// methods, so a single ClientConn can serve authenticated admin calls and
// unauthenticated data-plane calls.
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
