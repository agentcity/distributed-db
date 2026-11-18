package version

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"
)

const (
	// VersionMetadataKey carries the semantic protocol version
	// ("major.minor.patch") on a gRPC request and on the response headers.
	VersionMetadataKey = "x-db-protocol-version"
	// FeaturesMetadataKey carries the decimal feature bitmask. It is advisory;
	// a receiver ignores bits it does not know.
	FeaturesMetadataKey = "x-db-protocol-features"
)

// PeerFromMetadata parses the protocol metadata in md. Missing version metadata
// is not an error: it identifies a pre-handshake peer, reported as Legacy with
// LegacyVersion. Malformed metadata is an error so a server can reject it
// instead of guessing a version.
func PeerFromMetadata(md metadata.MD) (PeerInfo, error) {
	versions := md.Get(VersionMetadataKey)
	if len(versions) == 0 || strings.TrimSpace(versions[0]) == "" {
		return PeerInfo{Version: LegacyVersion, Legacy: true}, nil
	}
	v, err := Parse(versions[0])
	if err != nil {
		return PeerInfo{}, err
	}
	out := PeerInfo{Version: v}
	if features := md.Get(FeaturesMetadataKey); len(features) > 0 {
		f, err := ParseFeatures(features[0])
		if err != nil {
			return PeerInfo{}, err
		}
		out.Features = f
	}
	return out, nil
}

// PeerFromIncomingContext parses protocol metadata from an incoming server
// request.
func PeerFromIncomingContext(ctx context.Context) (PeerInfo, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	return PeerFromMetadata(md)
}

// PeerFromOutgoingContext parses protocol metadata from an outgoing client
// context. It is mainly useful in interceptors and tests.
func PeerFromOutgoingContext(ctx context.Context) (PeerInfo, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	return PeerFromMetadata(md)
}

// appendClientMetadata sets (not appends) the protocol keys in the outgoing
// metadata. Existing metadata such as the admin authorization header is
// preserved untouched; the handshake never adds credentials.
func appendClientMetadata(ctx context.Context, local Version, features Feature) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	md.Set(VersionMetadataKey, local.String())
	md.Set(FeaturesMetadataKey, FormatFeatures(features))
	return metadata.NewOutgoingContext(ctx, md)
}

// handshakeMetadata builds the server response header carrying the local
// version and feature set.
func handshakeMetadata(local Version, features Feature) metadata.MD {
	return metadata.Pairs(
		VersionMetadataKey, local.String(),
		FeaturesMetadataKey, FormatFeatures(features),
	)
}

// checkMetadataLimits rejects metadata values that are absurdly long before
// parsing. It keeps an attacker from forcing expensive version parsing, and
// gives the server a single generic error path.
func checkMetadataLimits(md metadata.MD) error {
	for _, key := range []string{VersionMetadataKey, FeaturesMetadataKey} {
		for _, v := range md.Get(key) {
			if len(v) > 128 {
				return fmt.Errorf("version: protocol metadata %s is too long", key)
			}
		}
	}
	return nil
}
