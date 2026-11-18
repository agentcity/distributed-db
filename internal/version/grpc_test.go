package version

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
)

// handshakeKV is a minimal KVService used only to exercise the interceptors.
type handshakeKV struct {
	pbv2.UnimplementedKVServiceServer

	mu  sync.Mutex
	mds []metadata.MD
}

func (k *handshakeKV) Status(ctx context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	k.mu.Lock()
	k.mds = append(k.mds, md.Copy())
	k.mu.Unlock()
	return &pbv2.StatusResponse{Id: "n1", Role: "leader", Leader: "n1"}, nil
}

func (k *handshakeKV) metadata() []metadata.MD {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]metadata.MD, len(k.mds))
	for i, md := range k.mds {
		out[i] = md.Copy()
	}
	return out
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func bufferLogger() (*slog.Logger, *lockedBuffer) {
	var buf lockedBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func startHandshakeKVServer(t *testing.T, sh *ServerHandshake) (string, *handshakeKV) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var opts []grpc.ServerOption
	if sh != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(sh.Unary()),
			grpc.ChainStreamInterceptor(sh.Stream()))
	}
	srv := grpc.NewServer(opts...)
	kv := &handshakeKV{}
	pbv2.RegisterKVServiceServer(srv, kv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), kv
}

func newHandshakeKVClient(t *testing.T, addr string, ch *ClientHandshake) (pbv2.KVServiceClient, *grpc.ClientConn) {
	t.Helper()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if ch != nil {
		opts = append(opts,
			grpc.WithChainUnaryInterceptor(ch.Unary()),
			grpc.WithChainStreamInterceptor(ch.Stream()))
	}
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc), cc
}

func callStatus(t *testing.T, client pbv2.KVServiceClient, ctx context.Context) error {
	t.Helper()
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	_, err := client.Status(ctx, &pbv2.StatusRequest{})
	return err
}

func onlyServerRecord(t *testing.T, reg *Registry) PeerRecord {
	t.Helper()
	recs := reg.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("server registry has %d records, want 1: %+v", len(recs), recs)
	}
	return recs[0]
}

func onlyClientRecord(t *testing.T, reg *Registry) PeerRecord {
	t.Helper()
	recs := reg.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("client registry has %d records, want 1: %+v", len(recs), recs)
	}
	return recs[0]
}

func TestHandshakeSameVersion(t *testing.T) {
	clientReg := NewRegistry()
	serverReg := NewRegistry()
	addr, kv := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Current, CurrentFeatures, discardLogger()))
	client, cc := newHandshakeKVClient(t, addr, NewClientHandshake(clientReg))

	if err := callStatus(t, client, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}

	crec, ok := clientReg.Lookup(cc.Target())
	if !ok {
		t.Fatalf("no client record for target %q: %+v", cc.Target(), clientReg.Snapshot())
	}
	if crec.PeerVersion != Current || !crec.Compatible || crec.Legacy || crec.Level != LevelCompatible {
		t.Fatalf("client record = %+v, want compatible current peer", crec)
	}
	if crec.CommonFeatures != CurrentFeatures {
		t.Fatalf("client CommonFeatures = %x, want %x", crec.CommonFeatures, CurrentFeatures)
	}

	slrec := onlyServerRecord(t, serverReg)
	if slrec.PeerVersion != Current || !slrec.Compatible || slrec.Legacy || slrec.Level != LevelCompatible {
		t.Fatalf("server record = %+v, want compatible current peer", slrec)
	}
	if slrec.PeerFeatures != CurrentFeatures {
		t.Fatalf("server peer features = %x, want %x", slrec.PeerFeatures, CurrentFeatures)
	}

	md := kv.metadata()
	if len(md) != 1 {
		t.Fatalf("handler saw %d calls, want 1", len(md))
	}
	if got := md[0].Get(VersionMetadataKey); len(got) != 1 || got[0] != Current.String() {
		t.Fatalf("request version metadata = %v, want %s", got, Current)
	}
	if got := md[0].Get(FeaturesMetadataKey); len(got) != 1 || got[0] != FormatFeatures(CurrentFeatures) {
		t.Fatalf("request feature metadata = %v, want %s", got, FormatFeatures(CurrentFeatures))
	}
	if got := md[0].Get("authorization"); len(got) != 0 {
		t.Fatalf("handshake added authorization metadata: %v", got)
	}
}

func TestHandshakeWarnsOnOlderMinorPeer(t *testing.T) {
	clientReg := NewRegistry()
	serverReg := NewRegistry()
	logger, logs := bufferLogger()
	serverVersion := Version{2, 1, 0}
	addr, _ := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, serverVersion, CurrentFeatures, logger))
	client, _ := newHandshakeKVClient(t, addr, NewClientHandshakeWithVersion(clientReg, Version{2, 0, 0}, FeatureTxnAtomic))

	if err := callStatus(t, client, nil); err != nil {
		t.Fatalf("older N-1 client was rejected: %v", err)
	}

	crec := onlyClientRecord(t, clientReg)
	if crec.PeerVersion != serverVersion || !crec.Compatible || crec.Level != LevelNewerMinor {
		t.Fatalf("client record = %+v, want compatible newer server", crec)
	}

	srec := onlyServerRecord(t, serverReg)
	if srec.PeerVersion != (Version{2, 0, 0}) || !srec.Compatible || srec.Legacy || srec.Level != LevelOlderMinor {
		t.Fatalf("server record = %+v, want compatible older peer", srec)
	}
	if len(srec.Warnings) == 0 || !strings.Contains(strings.Join(srec.Warnings, "; "), "older") {
		t.Fatalf("server warnings = %v, want an older-minor warning", srec.Warnings)
	}
	if got := logs.String(); !strings.Contains(got, "older") {
		t.Fatalf("server did not warn about an older minor peer: %s", got)
	}
	if strings.Contains(logs.String(), "authorization") || strings.Contains(logs.String(), "Bearer") {
		t.Fatalf("handshake log leaked credentials: %s", logs.String())
	}
}

func TestHandshakeNewerMinorUnknownFeaturesIgnored(t *testing.T) {
	const unknown Feature = 1 << 40
	clientReg := NewRegistry()
	serverReg := NewRegistry()
	addr, _ := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Version{2, 2, 0}, CurrentFeatures|unknown, discardLogger()))
	client, _ := newHandshakeKVClient(t, addr, NewClientHandshake(clientReg))

	if err := callStatus(t, client, nil); err != nil {
		t.Fatalf("newer minor peer was rejected: %v", err)
	}

	crec := onlyClientRecord(t, clientReg)
	if crec.PeerVersion != (Version{2, 2, 0}) || !crec.Compatible || crec.Level != LevelNewerMinor {
		t.Fatalf("client record = %+v, want compatible newer peer", crec)
	}
	if crec.UnknownFeatures != unknown {
		t.Fatalf("UnknownFeatures = %x, want %x", crec.UnknownFeatures, unknown)
	}
	if crec.CommonFeatures != CurrentFeatures {
		t.Fatalf("CommonFeatures = %x, want %x", crec.CommonFeatures, CurrentFeatures)
	}
	if len(crec.Warnings) == 0 || !strings.Contains(strings.Join(crec.Warnings, "; "), "newer") {
		t.Fatalf("client warnings = %v, want a newer-minor warning", crec.Warnings)
	}
}

func TestHandshakeMajorMismatchRejectedWithoutTokenLeak(t *testing.T) {
	const token = "top-secret-token"
	clientReg := NewRegistry()
	serverReg := NewRegistry()
	logger, logs := bufferLogger()
	addr, _ := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Version{3, 0, 0}, CurrentFeatures, logger))
	client, _ := newHandshakeKVClient(t, addr, NewClientHandshake(clientReg))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

	err := callStatus(t, client, ctx)
	if err == nil {
		t.Fatal("major mismatch was accepted")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %s, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "major") {
		t.Fatalf("error = %q, want a major-version reason", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("server log leaked the token: %s", logs.String())
	}

	srec := onlyServerRecord(t, serverReg)
	if srec.Level != LevelMajorMismatch || srec.Compatible {
		t.Fatalf("server record = %+v, want rejected major mismatch", srec)
	}
	if strings.Contains(srec.String(), token) {
		t.Fatalf("peer record leaked the token: %s", srec)
	}
}

func TestHandshakeLegacyServerAccepted(t *testing.T) {
	clientReg := NewRegistry()
	addr, _ := startHandshakeKVServer(t, nil)
	client, _ := newHandshakeKVClient(t, addr, NewClientHandshake(clientReg))

	if err := callStatus(t, client, nil); err != nil {
		t.Fatalf("legacy server was rejected: %v", err)
	}
	crec := onlyClientRecord(t, clientReg)
	if !crec.Legacy || crec.PeerVersion != LegacyVersion || !crec.Compatible || crec.Level != LevelLegacy {
		t.Fatalf("client record = %+v, want compatible legacy server", crec)
	}
	if len(crec.Warnings) == 0 || !strings.Contains(strings.Join(crec.Warnings, "; "), "legacy") {
		t.Fatalf("client warnings = %v, want a legacy warning", crec.Warnings)
	}
}

func TestHandshakeLegacyClientAccepted(t *testing.T) {
	serverReg := NewRegistry()
	logger, logs := bufferLogger()
	addr, _ := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Current, CurrentFeatures, logger))
	client, _ := newHandshakeKVClient(t, addr, nil) // no handshake metadata

	if err := callStatus(t, client, nil); err != nil {
		t.Fatalf("legacy client was rejected: %v", err)
	}
	srec := onlyServerRecord(t, serverReg)
	if !srec.Legacy || srec.PeerVersion != LegacyVersion || !srec.Compatible || srec.Level != LevelLegacy {
		t.Fatalf("server record = %+v, want compatible legacy client", srec)
	}
	if len(srec.Warnings) == 0 || !strings.Contains(strings.Join(srec.Warnings, "; "), "legacy") {
		t.Fatalf("server warnings = %v, want a legacy warning", srec.Warnings)
	}
	if got := logs.String(); !strings.Contains(got, "legacy") {
		t.Fatalf("server did not warn about a legacy peer: %s", got)
	}
}

func TestHandshakeMalformedMetadataRejectedWithoutEcho(t *testing.T) {
	const marker = "Bearer do-not-echo"
	serverReg := NewRegistry()
	logger, logs := bufferLogger()
	addr, _ := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Current, CurrentFeatures, logger))
	client, _ := newHandshakeKVClient(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, VersionMetadataKey, marker)

	err := callStatus(t, client, ctx)
	if err == nil {
		t.Fatal("malformed metadata was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("status code = %s, want InvalidArgument (%v)", status.Code(err), err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error echoed malformed metadata: %v", err)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("server log echoed malformed metadata: %s", logs.String())
	}
}

func TestHandshakeSuccessfulCallDoesNotLeakToken(t *testing.T) {
	const token = "another-secret"
	serverReg := NewRegistry()
	logger, logs := bufferLogger()
	addr, kv := startHandshakeKVServer(t, NewServerHandshakeWithVersion(serverReg, Current, CurrentFeatures, logger))
	client, _ := newHandshakeKVClient(t, addr, NewClientHandshake(NewRegistry()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	if err := callStatus(t, client, ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}

	// The handshake must preserve, not strip, metadata set by the caller; the
	// handler still authenticates the request. What matters is that it never
	// copies the token into its own logs or peer records.
	md := kv.metadata()
	if got := md[0].Get("authorization"); len(got) != 1 || got[0] != "Bearer "+token {
		t.Fatalf("handler authorization = %v, want the caller token preserved", got)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("handshake log leaked the token: %s", logs.String())
	}
	srec := onlyServerRecord(t, serverReg)
	if strings.Contains(srec.String(), token) {
		t.Fatalf("peer record leaked the token: %s", srec)
	}
}

func TestHandshakeStreamingRPC(t *testing.T) {
	clientReg := NewRegistry()
	serverReg := NewRegistry()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sh := NewServerHandshakeWithVersion(serverReg, Current, CurrentFeatures, discardLogger())
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(sh.Unary()),
		grpc.ChainStreamInterceptor(sh.Stream()))
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ch := NewClientHandshake(clientReg)
	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(ch.Unary()),
		grpc.WithChainStreamInterceptor(ch.Stream()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := healthpb.NewHealthClient(cc).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Watch Recv: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		rec, ok := clientReg.Lookup(cc.Target())
		if ok {
			if rec.PeerVersion != Current || !rec.Compatible {
				t.Fatalf("stream client record = %+v, want compatible current", rec)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream handshake was not recorded: %+v", clientReg.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	srec := onlyServerRecord(t, serverReg)
	if srec.PeerVersion != Current || !srec.Compatible {
		t.Fatalf("stream server record = %+v, want compatible current", srec)
	}
}
