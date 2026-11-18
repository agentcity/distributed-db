package v2client

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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/version"
)

// handshakeMetaServer is a minimal MetaService used to inspect the metadata the
// client attaches and to answer the read-only/admin calls the test makes.
type handshakeMetaServer struct {
	pbv2.UnimplementedMetaServiceServer

	mu  sync.Mutex
	mds map[string][]metadata.MD
}

func newHandshakeMetaServer() *handshakeMetaServer {
	return &handshakeMetaServer{mds: make(map[string][]metadata.MD)}
}

func (s *handshakeMetaServer) record(method string, ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.mds[method] = append(s.mds[method], md.Copy())
	s.mu.Unlock()
}

func (s *handshakeMetaServer) recorded(method string) []metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metadata.MD, len(s.mds[method]))
	for i, md := range s.mds[method] {
		out[i] = md.Copy()
	}
	return out
}

func (s *handshakeMetaServer) ListRanges(ctx context.Context, _ *pbv2.ListRangesRequest) (*pbv2.ListRangesResponse, error) {
	s.record("ListRanges", ctx)
	return &pbv2.ListRangesResponse{}, nil
}

func (s *handshakeMetaServer) SetRanges(ctx context.Context, _ *pbv2.SetRangesRequest) (*pbv2.SetRangesResponse, error) {
	s.record("SetRanges", ctx)
	return &pbv2.SetRangesResponse{Success: true}, nil
}

func (s *handshakeMetaServer) AllocTS(ctx context.Context, _ *pbv2.AllocTSRequest) (*pbv2.AllocTSResponse, error) {
	s.record("AllocTS", ctx)
	return &pbv2.AllocTSResponse{Start: 1, Count: 1}, nil
}

// handshakeKVNode answers Status, which ProbeProtocol uses to force a KV
// handshake without running a transaction.
type handshakeKVNode struct {
	pbv2.UnimplementedKVServiceServer

	mu  sync.Mutex
	mds []metadata.MD
}

func (s *handshakeKVNode) Status(ctx context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.mds = append(s.mds, md.Copy())
	s.mu.Unlock()
	return &pbv2.StatusResponse{Id: "kv1", Role: "leader", Leader: "kv1"}, nil
}

func (s *handshakeKVNode) metadata() []metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metadata.MD, len(s.mds))
	for i, md := range s.mds {
		out[i] = md.Copy()
	}
	return out
}

type handshakeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *handshakeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *handshakeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func handshakeLogger() (*slog.Logger, *handshakeBuffer) {
	var buf handshakeBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func discardHandshakeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startHandshakeServer(t *testing.T, sh *version.ServerHandshake, register func(*grpc.Server)) string {
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
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func startHandshakeMeta(t *testing.T, sh *version.ServerHandshake) (string, *handshakeMetaServer) {
	t.Helper()
	srv := newHandshakeMetaServer()
	addr := startHandshakeServer(t, sh, func(s *grpc.Server) {
		pbv2.RegisterMetaServiceServer(s, srv)
	})
	return addr, srv
}

func startHandshakeKV(t *testing.T, sh *version.ServerHandshake) (string, *handshakeKVNode) {
	t.Helper()
	srv := &handshakeKVNode{}
	addr := startHandshakeServer(t, sh, func(s *grpc.Server) {
		pbv2.RegisterKVServiceServer(s, srv)
	})
	return addr, srv
}

func TestProtocolHandshakeSameVersion(t *testing.T) {
	metaReg := version.NewRegistry()
	kvReg := version.NewRegistry()
	metaAddr, metaSrv := startHandshakeMeta(t, version.NewServerHandshakeWithVersion(metaReg, version.Current, version.CurrentFeatures, discardHandshakeLogger()))
	kvAddr, kvSrv := startHandshakeKV(t, version.NewServerHandshakeWithVersion(kvReg, version.Current, version.CurrentFeatures, discardHandshakeLogger()))

	c, err := New(metaAddr, []string{kvAddr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if _, err := c.ProbeProtocol(ctx, kvAddr); err != nil {
		t.Fatalf("ProbeProtocol: %v", err)
	}

	for _, tc := range []struct{ name, addr string }{{"meta", metaAddr}, {"kv", kvAddr}} {
		rec, ok := c.ProtocolInfo(tc.addr)
		if !ok {
			t.Fatalf("%s: no protocol record for %q (records: %+v)", tc.name, tc.addr, c.ProtocolRecords())
		}
		if rec.PeerVersion != version.Current || !rec.Compatible || rec.Legacy {
			t.Fatalf("%s record = %+v, want compatible current", tc.name, rec)
		}
	}
	if got, ok := c.NegotiatedVersion(); !ok || got != version.Current {
		t.Fatalf("NegotiatedVersion() = %s, %t; want %s", got, ok, version.Current)
	}
	if got := c.ProtocolSummary(); !strings.Contains(got, "local="+version.Current.String()) ||
		!strings.Contains(got, "peer="+version.Current.String()) ||
		!strings.Contains(got, metaAddr) || !strings.Contains(got, kvAddr) {
		t.Fatalf("ProtocolSummary() = %q", got)
	}

	// Every meta call carries the handshake metadata and never adds the admin
	// authorization header on a read-only call.
	mds := metaSrv.recorded("ListRanges")
	if len(mds) != 1 {
		t.Fatalf("ListRanges metadata records = %d, want 1", len(mds))
	}
	if got := mds[0].Get(version.VersionMetadataKey); len(got) != 1 || got[0] != version.Current.String() {
		t.Fatalf("ListRanges version metadata = %v", got)
	}
	if got := mds[0].Get("authorization"); len(got) != 0 {
		t.Fatalf("read-only ListRanges carried authorization metadata: %v", got)
	}

	// The server also recorded the client's version.
	for _, tc := range []struct {
		name string
		reg  *version.Registry
	}{{"meta", metaReg}, {"kv", kvReg}} {
		recs := tc.reg.Snapshot()
		if len(recs) != 1 || recs[0].PeerVersion != version.Current || !recs[0].Compatible {
			t.Fatalf("%s server registry = %+v", tc.name, recs)
		}
	}
	if got := kvSrv.metadata(); len(got) != 1 || len(got[0].Get(version.VersionMetadataKey)) != 1 {
		t.Fatalf("KV Status metadata = %+v", got)
	}
}

func TestProtocolHandshakeOlderMinorServer(t *testing.T) {
	metaReg := version.NewRegistry()
	metaAddr, _ := startHandshakeMeta(t, version.NewServerHandshakeWithVersion(
		metaReg, version.Version{Major: 2, Minor: 0, Patch: 0}, version.CurrentFeatures, discardHandshakeLogger()))

	c, err := NewMetaOnly(metaAddr)
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("older N-1 server was rejected: %v", err)
	}
	rec, ok := c.ProtocolInfo(metaAddr)
	if !ok {
		t.Fatalf("no protocol record (records: %+v)", c.ProtocolRecords())
	}
	if rec.PeerVersion != (version.Version{Major: 2, Minor: 0, Patch: 0}) ||
		!rec.Compatible || rec.Level != version.LevelOlderMinor {
		t.Fatalf("record = %+v, want compatible older minor", rec)
	}
	if len(rec.Warnings) == 0 || !strings.Contains(strings.Join(rec.Warnings, "; "), "older") {
		t.Fatalf("warnings = %v, want an older-minor warning", rec.Warnings)
	}
	if got, ok := c.NegotiatedVersion(); !ok || got.Minor != 0 {
		t.Fatalf("NegotiatedVersion() = %s, %t; want 2.0.x", got, ok)
	}
}

func TestProtocolHandshakeNewerMinorAndUnknownFeatures(t *testing.T) {
	const unknown version.Feature = 1 << 41
	metaReg := version.NewRegistry()
	metaAddr, _ := startHandshakeMeta(t, version.NewServerHandshakeWithVersion(
		metaReg, version.Version{Major: 2, Minor: 2, Patch: 0}, version.CurrentFeatures|unknown, discardHandshakeLogger()))

	c, err := NewMetaOnly(metaAddr)
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("newer minor server was rejected: %v", err)
	}
	rec, ok := c.ProtocolInfo(metaAddr)
	if !ok {
		t.Fatalf("no protocol record (records: %+v)", c.ProtocolRecords())
	}
	if rec.PeerVersion.Minor != 2 || !rec.Compatible || rec.Level != version.LevelNewerMinor {
		t.Fatalf("record = %+v, want compatible newer minor", rec)
	}
	if rec.UnknownFeatures != unknown {
		t.Fatalf("UnknownFeatures = %x, want %x", rec.UnknownFeatures, unknown)
	}
	if rec.CommonFeatures != version.CurrentFeatures {
		t.Fatalf("CommonFeatures = %x, want %x", rec.CommonFeatures, version.CurrentFeatures)
	}
}

func TestProtocolHandshakeMajorMismatch(t *testing.T) {
	metaReg := version.NewRegistry()
	metaAddr, _ := startHandshakeMeta(t, version.NewServerHandshakeWithVersion(
		metaReg, version.Version{Major: 3, Minor: 0, Patch: 0}, version.CurrentFeatures, discardHandshakeLogger()))

	c, err := NewMetaOnly(metaAddr)
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.ListRanges(ctx)
	if err == nil {
		t.Fatal("major mismatch was accepted")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %s, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "major") {
		t.Fatalf("error = %q, want a major-version reason", err)
	}
	recs := metaReg.Snapshot()
	if len(recs) != 1 || recs[0].Level != version.LevelMajorMismatch || recs[0].Compatible {
		t.Fatalf("server registry = %+v, want rejected major mismatch", recs)
	}
	if _, ok := c.NegotiatedVersion(); ok {
		t.Fatal("NegotiatedVersion reported a compatible version after a major mismatch")
	}
}

func TestProtocolHandshakeLegacyServer(t *testing.T) {
	metaAddr, _ := startHandshakeMeta(t, nil) // no interceptor: a pre-handshake server
	c, err := NewMetaOnly(metaAddr)
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("legacy server was rejected: %v", err)
	}
	rec, ok := c.ProtocolInfo(metaAddr)
	if !ok {
		t.Fatalf("no protocol record (records: %+v)", c.ProtocolRecords())
	}
	if !rec.Legacy || rec.PeerVersion != version.LegacyVersion || !rec.Compatible || rec.Level != version.LevelLegacy {
		t.Fatalf("record = %+v, want compatible legacy peer", rec)
	}
	if len(rec.Warnings) == 0 || !strings.Contains(strings.Join(rec.Warnings, "; "), "legacy") {
		t.Fatalf("warnings = %v, want a legacy warning", rec.Warnings)
	}
	if got, ok := c.NegotiatedVersion(); !ok || got != version.LegacyVersion {
		t.Fatalf("NegotiatedVersion() = %s, %t; want legacy %s", got, ok, version.LegacyVersion)
	}
	if got := c.ProtocolSummary(); !strings.Contains(got, "legacy=true") {
		t.Fatalf("ProtocolSummary() = %q, want legacy=true", got)
	}
}

func TestProtocolHandshakeDoesNotLeakAdminToken(t *testing.T) {
	const token = "s3cret-handshake-token"
	metaReg := version.NewRegistry()
	logger, logs := handshakeLogger()
	metaAddr, metaSrv := startHandshakeMeta(t, version.NewServerHandshakeWithVersion(
		metaReg, version.Current, version.CurrentFeatures, logger))

	c, err := NewMetaOnlyWithOptions(metaAddr, Options{AdminToken: token})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SetRanges(ctx, []RangeInfo{{ID: 1, Replicas: []string{"kv1"}}}); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}

	setMD := metaSrv.recorded("SetRanges")
	if len(setMD) != 1 {
		t.Fatalf("SetRanges metadata records = %d, want 1", len(setMD))
	}
	if got := setMD[0].Get("authorization"); len(got) != 1 || got[0] != "Bearer "+token {
		t.Fatalf("SetRanges authorization = %v, want the configured token", got)
	}
	if got := setMD[0].Get(version.VersionMetadataKey); len(got) != 1 {
		t.Fatalf("SetRanges version metadata = %v, want exactly one handshake value", got)
	}
	if got := setMD[0].Get(version.FeaturesMetadataKey); len(got) != 1 {
		t.Fatalf("SetRanges feature metadata = %v, want exactly one handshake value", got)
	}
	listMD := metaSrv.recorded("ListRanges")
	if len(listMD) != 1 {
		t.Fatalf("ListRanges metadata records = %d, want 1", len(listMD))
	}
	if got := listMD[0].Get("authorization"); len(got) != 0 {
		t.Fatalf("read-only ListRanges carried authorization metadata: %v", got)
	}

	// The handshake must not copy credentials into logs, records or summary
	// output, even though the admin call legitimately carries the token.
	if strings.Contains(logs.String(), token) {
		t.Fatalf("server log leaked the token: %s", logs.String())
	}
	for _, rec := range c.ProtocolRecords() {
		if strings.Contains(rec.String(), token) {
			t.Fatalf("client protocol record leaked the token: %s", rec)
		}
	}
	for _, rec := range metaReg.Snapshot() {
		if strings.Contains(rec.String(), token) {
			t.Fatalf("server protocol record leaked the token: %s", rec)
		}
	}
	if strings.Contains(c.ProtocolSummary(), token) {
		t.Fatalf("ProtocolSummary leaked the token: %s", c.ProtocolSummary())
	}
}

func TestProtocolInfoOnDirectTestClientIsEmpty(t *testing.T) {
	c := newTestClient(newFakeMeta("meta1"))
	if _, ok := c.ProtocolInfo("meta1"); ok {
		t.Fatal("direct test client unexpectedly has a protocol record")
	}
	if got := c.ProtocolRecords(); got != nil {
		t.Fatalf("ProtocolRecords() = %+v, want nil", got)
	}
	if got := c.ProtocolSummary(); !strings.Contains(got, "no peer handshake") {
		t.Fatalf("ProtocolSummary() = %q", got)
	}
	if _, err := c.ProbeProtocol(context.Background(), "kv1"); err == nil {
		t.Fatal("ProbeProtocol on a handshake-less client returned nil error")
	}
}
