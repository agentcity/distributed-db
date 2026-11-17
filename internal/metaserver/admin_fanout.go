package metaserver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"distributed-db/internal/rbac"
)

// kvAdminMethodNames is the set of KVService RPCs that mutate Raft membership,
// range metadata or the local data layout. The metaserver fan-out must present
// the admin bearer token on exactly these calls; data-plane and read-only RPCs
// must never receive it.
//
// The set comes from the shared RBAC classification table, so it cannot drift
// from the kvserver-side inbound interceptor. The generated service constants
// remain referenced by the tests.
var kvAdminMethodNames = rbac.KVAdminMethods()

const (
	adminTokenMetadataKey = "authorization"
	adminTokenScheme      = "Bearer"
)

// adminTokenUnaryClientInterceptor attaches token as an
// "authorization: Bearer <token>" outgoing credential for KV admin methods
// only. Data-plane, read-only and RaftService RPCs pass through untouched, so
// one shared dial helper can serve both authenticated admin fan-out and
// unauthenticated data traffic.
//
// An empty token installs a pass-through interceptor: development clusters and
// tests that disable the KV check keep working. The interceptor never logs or
// returns the token, so a rejected call cannot leak it through gRPC errors.
//
// The fan-out credential is Config.AdminToken. Config.AdminTokenRoles is the
// inbound RBAC mapping and is deliberately not consulted here: a deployment
// that configures only role tokens must still set AdminToken to a credential
// the KV nodes accept if it wants metaserver-driven split/merge/membership.
func adminTokenUnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	if token == "" {
		return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := kvAdminMethodNames[method]; ok {
			ctx = metadata.AppendToOutgoingContext(ctx,
				adminTokenMetadataKey, adminTokenScheme+" "+token)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
