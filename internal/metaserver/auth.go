package metaserver

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

// metaAdminMethods is the set of MetaService RPCs that mutate cluster
// metadata or orchestrate destructive data movement. They require the admin
// bearer token when Config.AdminToken is set.
//
// Read-only RPCs (AllocTS, GetRange, ListRanges, Leader) are intentionally
// absent: data-plane clients call them on every transaction and they must keep
// working without an admin credential. RaftService methods are also untouched;
// they run on the same listener in replicated mode but are authenticated by
// the Raft transport (TLS/mTLS) rather than by this token.
//
// Scope limit: this interceptor only gates inbound MetaService calls. The
// scheduler's outbound KVService admin calls (SplitRange, MergeRanges,
// AddReplica, AddLearner, PromoteReplica, RemoveVoter, RemoveReplica) are
// served by the separate kvserver listener and are not covered here. Those
// RPCs need their own auth interceptor in the kvserver package, and this
// server does not send the admin token on the fan-out today. AddReplica and
// RemoveReplica on *Server are in-process orchestration methods, not gRPC
// handlers, so they are unreachable from the network.
var metaAdminMethods = map[string]struct{}{
	pbv2.MetaService_SetRanges_FullMethodName:   {},
	pbv2.MetaService_SplitRange_FullMethodName:  {},
	pbv2.MetaService_MergeRanges_FullMethodName: {},
}

// adminAuthUnaryInterceptor enforces the admin bearer token on the methods in
// metaAdminMethods. An empty token disables the check entirely, which keeps
// development clusters and existing tests working; production deployments
// must set Config.AdminToken.
//
// The token is independent of transport TLS: it protects against unauthenticated
// or accidentally destructive callers even on a plaintext or mTLS listener.
// Rejections never echo the presented or expected token, and no call logs it.
func adminAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	if token == "" {
		return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	}
	expected := sha256.Sum256([]byte(token))
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := metaAdminMethods[info.FullMethod]; !ok {
			return handler(ctx, req)
		}
		presented, ok := bearerToken(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "metaserver: admin token required")
		}
		// Compare fixed-size digests so the comparison time does not depend on
		// the token length or on where the first differing byte is.
		got := sha256.Sum256(presented)
		if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			return nil, status.Error(codes.Unauthenticated, "metaserver: invalid admin token")
		}
		return handler(ctx, req)
	}
}

// bearerToken extracts the token from an "authorization: Bearer <token>"
// incoming metadata entry. The scheme is case-insensitive per RFC 6750; the
// token itself is compared verbatim. Multiple authorization entries are
// accepted if any of them is a well-formed Bearer credential.
func bearerToken(ctx context.Context) ([]byte, bool) {
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
