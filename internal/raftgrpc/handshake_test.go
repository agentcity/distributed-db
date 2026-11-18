package raftgrpc

import (
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

	"distributed-db/internal/model"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/version"
)

type handshakeRaftServer struct {
	pbv2.UnimplementedRaftServiceServer

	mu  sync.Mutex
	mds []metadata.MD
}

func (s *handshakeRaftServer) AppendEntries(ctx context.Context, req *pbv2.AppendEntriesRequest) (*pbv2.AppendEntriesResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.mds = append(s.mds, md.Copy())
	s.mu.Unlock()
	return &pbv2.AppendEntriesResponse{
		Term:       req.GetTerm(),
		Success:    true,
		MatchIndex: uint64(len(req.GetEntries())),
	}, nil
}

func (s *handshakeRaftServer) metadata() []metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metadata.MD, len(s.mds))
	for i, md := range s.mds {
		out[i] = md.Copy()
	}
	return out
}

func discardRaftHandshakeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startHandshakeRaftServer(t *testing.T, sh *version.ServerHandshake) (string, *handshakeRaftServer) {
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
	rs := &handshakeRaftServer{}
	pbv2.RegisterRaftServiceServer(srv, rs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), rs
}

func appendEntries(t *testing.T, tr *Transport, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := tr.AppendEntries(ctx, addr, &raft.AppendEntriesRequest{
		Term:     7,
		LeaderID: "leader-1",
		Entries:  []model.Entry{{Term: 7, Index: 1, Key: "k", Value: []byte("v")}},
	})
	return err
}

func TestTransportHandshakeSameVersion(t *testing.T) {
	serverReg := version.NewRegistry()
	addr, srv := startHandshakeRaftServer(t, version.NewServerHandshakeWithVersion(serverReg, version.Current, version.CurrentFeatures, discardRaftHandshakeLogger()))

	tr := New()
	t.Cleanup(tr.Close)
	if err := appendEntries(t, tr, addr); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}

	rec, ok := tr.PeerProtocol(addr)
	if !ok {
		t.Fatalf("no peer protocol record for %q (records: %+v)", addr, tr.ProtocolRecords())
	}
	if rec.PeerVersion != version.Current || !rec.Compatible || rec.Legacy || rec.Level != version.LevelCompatible {
		t.Fatalf("client record = %+v, want compatible current peer", rec)
	}
	if got, ok := tr.NegotiatedVersion(); !ok || got != version.Current {
		t.Fatalf("NegotiatedVersion() = %s, %t; want %s", got, ok, version.Current)
	}

	recs := serverReg.Snapshot()
	if len(recs) != 1 || recs[0].PeerVersion != version.Current || !recs[0].Compatible {
		t.Fatalf("server registry = %+v", recs)
	}
	mds := srv.metadata()
	if len(mds) != 1 {
		t.Fatalf("server saw %d calls, want 1", len(mds))
	}
	if got := mds[0].Get(version.VersionMetadataKey); len(got) != 1 || got[0] != version.Current.String() {
		t.Fatalf("request version metadata = %v, want %s", got, version.Current)
	}
	if got := mds[0].Get("authorization"); len(got) != 0 {
		t.Fatalf("Raft handshake sent authorization metadata: %v", got)
	}
}

func TestTransportHandshakeOlderMinorServer(t *testing.T) {
	serverReg := version.NewRegistry()
	old := version.Version{Major: 2, Minor: 0, Patch: 0}
	addr, _ := startHandshakeRaftServer(t, version.NewServerHandshakeWithVersion(serverReg, old, version.CurrentFeatures, discardRaftHandshakeLogger()))

	tr := New()
	t.Cleanup(tr.Close)
	if err := appendEntries(t, tr, addr); err != nil {
		t.Fatalf("older N-1 Raft peer was rejected: %v", err)
	}
	rec, ok := tr.PeerProtocol(addr)
	if !ok {
		t.Fatal("no peer protocol record")
	}
	if rec.PeerVersion != old || !rec.Compatible || rec.Level != version.LevelOlderMinor {
		t.Fatalf("record = %+v, want compatible older peer", rec)
	}
	if len(rec.Warnings) == 0 || !strings.Contains(strings.Join(rec.Warnings, "; "), "older") {
		t.Fatalf("warnings = %v, want an older-minor warning", rec.Warnings)
	}
}

func TestTransportHandshakeNewerMinorAndUnknownFeatures(t *testing.T) {
	const unknown version.Feature = 1 << 42
	serverReg := version.NewRegistry()
	newer := version.Version{Major: 2, Minor: 2, Patch: 0}
	addr, _ := startHandshakeRaftServer(t, version.NewServerHandshakeWithVersion(serverReg, newer, version.CurrentFeatures|unknown, discardRaftHandshakeLogger()))

	tr := New()
	t.Cleanup(tr.Close)
	if err := appendEntries(t, tr, addr); err != nil {
		t.Fatalf("newer Raft peer was rejected: %v", err)
	}
	rec, ok := tr.PeerProtocol(addr)
	if !ok {
		t.Fatal("no peer protocol record")
	}
	if rec.PeerVersion != newer || !rec.Compatible || rec.Level != version.LevelNewerMinor {
		t.Fatalf("record = %+v, want compatible newer peer", rec)
	}
	if rec.UnknownFeatures != unknown {
		t.Fatalf("UnknownFeatures = %x, want %x", rec.UnknownFeatures, unknown)
	}
}

func TestTransportHandshakeMajorMismatchRejected(t *testing.T) {
	serverReg := version.NewRegistry()
	addr, _ := startHandshakeRaftServer(t, version.NewServerHandshakeWithVersion(
		serverReg, version.Version{Major: 3, Minor: 0, Patch: 0}, version.CurrentFeatures, discardRaftHandshakeLogger()))

	tr := New()
	t.Cleanup(tr.Close)
	err := appendEntries(t, tr, addr)
	if err == nil {
		t.Fatal("major mismatch was accepted")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %s, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "major") {
		t.Fatalf("error = %q, want a major-version reason", err)
	}
	recs := serverReg.Snapshot()
	if len(recs) != 1 || recs[0].Level != version.LevelMajorMismatch || recs[0].Compatible {
		t.Fatalf("server registry = %+v, want rejected major mismatch", recs)
	}
}

func TestTransportHandshakeLegacyServerAccepted(t *testing.T) {
	addr, _ := startHandshakeRaftServer(t, nil)
	tr := New()
	t.Cleanup(tr.Close)
	if err := appendEntries(t, tr, addr); err != nil {
		t.Fatalf("legacy Raft server was rejected: %v", err)
	}
	rec, ok := tr.PeerProtocol(addr)
	if !ok {
		t.Fatal("no peer protocol record")
	}
	if !rec.Legacy || rec.PeerVersion != version.LegacyVersion || !rec.Compatible || rec.Level != version.LevelLegacy {
		t.Fatalf("record = %+v, want compatible legacy peer", rec)
	}
	if got, ok := tr.NegotiatedVersion(); !ok || got != version.LegacyVersion {
		t.Fatalf("NegotiatedVersion() = %s, %t; want legacy %s", got, ok, version.LegacyVersion)
	}
}
