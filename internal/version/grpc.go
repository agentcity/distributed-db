package version

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ClientHandshake attaches the local protocol version and feature bits to
// every outgoing RPC and records the version advertised in the response
// headers. The same handshake can be shared by every connection of a client;
// records are keyed by the gRPC target string.
//
// A server that does not implement the handshake returns no response metadata
// and is recorded as a legacy peer. That is not an error: a pre-handshake
// server keeps working during a rolling upgrade.
type ClientHandshake struct {
	registry *Registry
	local    Version
	features Feature
}

// NewClientHandshake returns a handshake for the current build.
func NewClientHandshake(reg *Registry) *ClientHandshake {
	return NewClientHandshakeWithVersion(reg, Current, CurrentFeatures)
}

// NewClientHandshakeWithVersion returns a handshake with an explicit local
// version and feature set. It exists for tests and tools that must emulate a
// different release; production callers use NewClientHandshake.
func NewClientHandshakeWithVersion(reg *Registry, local Version, features Feature) *ClientHandshake {
	if local.IsZero() {
		local = Current
	}
	return &ClientHandshake{registry: reg, local: local, features: features}
}

// Registry returns the peer registry populated by this handshake, or nil.
func (h *ClientHandshake) Registry() *Registry {
	if h == nil {
		return nil
	}
	return h.registry
}

// Local returns the version advertised by this client.
func (h *ClientHandshake) Local() Version {
	if h == nil {
		return Current
	}
	return h.local
}

// Features returns the feature bits advertised by this client.
func (h *ClientHandshake) Features() Feature {
	if h == nil {
		return CurrentFeatures
	}
	return h.features
}

// Unary returns a unary client interceptor that sends the handshake metadata
// and records the server's response headers.
func (h *ClientHandshake) Unary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if h == nil {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx = appendClientMetadata(ctx, h.local, h.features)
		var header metadata.MD
		callOpts := make([]grpc.CallOption, 0, len(opts)+1)
		callOpts = append(callOpts, opts...)
		callOpts = append(callOpts, grpc.Header(&header))
		err := invoker(ctx, method, req, reply, cc, callOpts...)
		_, cerr := h.observe(cc.Target(), header, err == nil)
		if err != nil {
			return err
		}
		return cerr
	}
}

// Stream returns a streaming client interceptor. The response header is
// observed asynchronously because a stream does not necessarily receive it
// before returning from the streamer, and waiting would block the first
// message.
func (h *ClientHandshake) Stream() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if h == nil {
			return streamer(ctx, desc, cc, method, opts...)
		}
		ctx = appendClientMetadata(ctx, h.local, h.features)
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			return nil, err
		}
		go func() {
			md, herr := cs.Header()
			if herr != nil {
				return
			}
			_, _ = h.observe(cc.Target(), md, true)
		}()
		return cs, nil
	}
}

// observe parses a response header and records the result. A successful call
// with no protocol metadata is a legacy server; a failed call with no header
// (for example a connection error) is not recorded at all, so a transient
// failure does not masquerade as a legacy peer.
func (h *ClientHandshake) observe(addr string, md metadata.MD, gotResponse bool) (Report, error) {
	if h == nil {
		return Report{}, nil
	}
	if !gotResponse && len(md.Get(VersionMetadataKey)) == 0 {
		return Report{}, nil
	}
	if err := checkMetadataLimits(md); err != nil {
		return Report{}, nil
	}
	peerInfo, err := PeerFromMetadata(md)
	if err != nil {
		return Report{}, nil
	}
	rep, cerr := Evaluate(h.Local(), h.Features(), peerInfo)
	h.registry.Record(rep.PeerRecord(addr))
	return rep, cerr
}

// ServerHandshake validates the protocol metadata on every incoming RPC,
// records the peer, warns about an older (or legacy) peer and echoes the local
// version in the response headers. A major mismatch or a peer below the N-1
// support window is rejected with FailedPrecondition.
type ServerHandshake struct {
	registry *Registry
	local    Version
	features Feature
	logger   *slog.Logger
}

// NewServerHandshake returns a server handshake for the current build that logs
// warnings through slog.Default.
func NewServerHandshake(reg *Registry) *ServerHandshake {
	return NewServerHandshakeWithVersion(reg, Current, CurrentFeatures, slog.Default())
}

// NewServerHandshakeWithVersion returns a server handshake with an explicit
// local version, feature set and logger. A nil logger uses slog.Default.
func NewServerHandshakeWithVersion(reg *Registry, local Version, features Feature, logger *slog.Logger) *ServerHandshake {
	if local.IsZero() {
		local = Current
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ServerHandshake{registry: reg, local: local, features: features, logger: logger}
}

// Registry returns the peer registry populated by this handshake, or nil.
func (h *ServerHandshake) Registry() *Registry {
	if h == nil {
		return nil
	}
	return h.registry
}

// Local returns the version advertised by this server.
func (h *ServerHandshake) Local() Version {
	if h == nil {
		return Current
	}
	return h.local
}

// Features returns the feature bits advertised by this server.
func (h *ServerHandshake) Features() Feature {
	if h == nil {
		return CurrentFeatures
	}
	return h.features
}

func (h *ServerHandshake) loggerOrDefault() *slog.Logger {
	if h == nil || h.logger == nil {
		return slog.Default()
	}
	return h.logger
}

// Unary returns a unary server interceptor implementing the server side of the
// handshake.
func (h *ServerHandshake) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := h.begin(ctx, func(md metadata.MD) error { return grpc.SetHeader(ctx, md) }); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns a streaming server interceptor implementing the server side of
// the handshake.
func (h *ServerHandshake) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := h.begin(ss.Context(), ss.SetHeader); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func (h *ServerHandshake) begin(ctx context.Context, setHeader func(metadata.MD) error) error {
	if h == nil {
		return nil
	}
	addr := peerAddr(ctx)
	md, _ := metadata.FromIncomingContext(ctx)
	if err := checkMetadataLimits(md); err != nil {
		h.loggerOrDefault().Warn("protocol handshake: oversized peer metadata", "peer", addr)
		return status.Error(codes.InvalidArgument, "version: malformed protocol metadata")
	}
	peerInfo, err := PeerFromMetadata(md)
	if err != nil {
		// Never echo the offending value: it is peer-controlled and could be
		// used for log or error injection.
		h.loggerOrDefault().Warn("protocol handshake: malformed peer metadata", "peer", addr)
		return status.Error(codes.InvalidArgument, "version: malformed protocol metadata")
	}

	rep, cerr := Evaluate(h.Local(), h.Features(), peerInfo)
	h.registry.Record(rep.PeerRecord(addr))

	if !rep.Compatible {
		h.loggerOrDefault().Warn("protocol handshake rejected",
			"peer", addr,
			"peer_version", rep.Peer.Version.String(),
			"local_version", rep.Local.String(),
			"reason", rep.Reason)
	} else if len(rep.Warnings) > 0 {
		h.loggerOrDefault().Warn("protocol handshake warning",
			"peer", addr,
			"peer_version", rep.Peer.Version.String(),
			"local_version", rep.Local.String(),
			"legacy", rep.Peer.Legacy,
			"warnings", strings.Join(rep.Warnings, "; "))
	}

	// Advertise our version even when rejecting so the peer can report a
	// precise reason. A header already sent is not fatal.
	if herr := setHeader(handshakeMetadata(h.Local(), h.Features())); herr != nil {
		h.loggerOrDefault().Debug("protocol handshake: response header not sent", "peer", addr, "error", herr)
	}
	if cerr != nil {
		return status.Error(codes.FailedPrecondition, cerr.Error())
	}
	return nil
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
