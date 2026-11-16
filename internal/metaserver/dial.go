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
func (s *Server) dial(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, grpc.WithTransportCredentials(s.transportCredentials()))
}
