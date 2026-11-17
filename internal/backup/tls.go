package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"distributed-db/internal/grpctls"
	pbv2 "distributed-db/internal/pb/v2"
)

// MetaOptions configures the transport and admin authentication of a
// MetaClient.
//
// TLS, when non-nil, secures every meta connection. There is no plaintext
// fallback: an invalid CA, certificate or key makes OpenMetaWithOptions fail,
// and an unverifiable server makes every RPC fail. A nil TLS keeps the
// historical plaintext transport, which is a development default.
//
// AdminToken is sent as "authorization: Bearer <token>" on SetRanges only.
// Read-only calls (ListRanges, Leader) never carry it. The token is never
// logged and never included in an error. A non-empty token without TLS sends
// the credential in clear text; OpenMetaWithOptions permits it for parity with
// v2client, but the kvbackup CLI rejects that combination unless
// -allow-plaintext-token is passed.
type MetaOptions struct {
	TLS        *grpctls.ClientOptions
	AdminToken string
}

// OpenMetaWithOptions creates a MetaClient over the given meta service
// addresses with optional TLS/mTLS and an optional meta admin token. At least
// one non-empty address is required. TLS options are validated before any
// connection is created.
func OpenMetaWithOptions(seeds []string, opts MetaOptions) (*MetaClient, error) {
	creds := credentials.TransportCredentials(insecure.NewCredentials())
	if opts.TLS != nil {
		c, err := grpctls.ClientCredentials(*opts.TLS)
		if err != nil {
			return nil, fmt.Errorf("backup: meta TLS: %w", err)
		}
		creds = c
	}
	return newMetaClient(seeds, creds, opts.AdminToken)
}

// newMetaClient validates, deduplicates and builds a MetaClient. creds must be
// non-nil; a nil value is treated as plaintext so a zero-value MetaClient
// cannot panic.
func newMetaClient(seeds []string, creds credentials.TransportCredentials, adminToken string) (*MetaClient, error) {
	clean := make([]string, 0, len(seeds))
	seen := make(map[string]struct{}, len(seeds))
	for _, addr := range seeds {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		clean = append(clean, addr)
	}
	if len(clean) == 0 {
		return nil, errors.New("backup: at least one meta address is required")
	}
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	return &MetaClient{
		seeds:      clean,
		conns:      make(map[string]*grpc.ClientConn),
		clients:    make(map[string]pbv2.MetaServiceClient),
		creds:      creds,
		adminToken: adminToken,
	}, nil
}

// adminCallContext attaches the configured admin Bearer credential to an
// outgoing SetRanges call. Read-only meta calls deliberately bypass it so the
// token is never sent where it is not required.
func (m *MetaClient) adminCallContext(ctx context.Context) context.Context {
	if m.adminToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+m.adminToken)
}
