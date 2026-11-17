package metaserver

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// transportCredentials returns the credentials used by every outbound KV and
// meta dial. peerCreds is nil only when TLS is disabled, in which case the
// plaintext development default applies. A TLS-enabled server always carries
// non-nil peerCreds, so there is no per-dial plaintext fallback.
func (s *Server) transportCredentials() credentials.TransportCredentials {
	if s.peerCreds != nil {
		return s.peerCreds
	}
	return insecure.NewCredentials()
}

// dial opens one short-lived client connection to addr using the server's
// shared transport security.
//
// Every outbound KV dial also carries the server's configured admin token on
// KV admin methods (split/merge, membership changes, leader transfer and epoch
// propagation). The token is attached by a client interceptor rather than per
// call site, so scheduler, membership and leader-rebalance fan-out cannot
// diverge. Status and other read-only/data RPCs stay unauthenticated. An empty
// AdminToken installs a pass-through interceptor, preserving the development
// default.
func (s *Server) dial(addr string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(s.transportCredentials()),
		grpc.WithChainUnaryInterceptor(adminTokenUnaryClientInterceptor(s.cfg.AdminToken)),
	}
	return grpc.NewClient(addr, opts...)
}
