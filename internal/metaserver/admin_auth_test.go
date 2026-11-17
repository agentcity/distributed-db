package metaserver

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	"distributed-db/internal/meta"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/v2client"
)

const testAdminToken = "test-admin-token"

// syncBuffer is a concurrency-safe log sink. The gRPC server may log while a
// test reads the buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// startAuthTestMeta starts a standalone meta server with one range and the
// given admin token. It returns the server, its address and the captured log.
func startAuthTestMeta(t *testing.T, token string) (*Server, string, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	s, err := New(Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
		}},
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		AdminToken: token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s, s.Addr(), logs
}

func rawMetaClient(t *testing.T, addr string) pbv2.MetaServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewMetaServiceClient(cc)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func withBearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func requireCode(t *testing.T, err error, want codes.Code, wantMsg string) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("status code = %v, want %v (err = %v)", status.Code(err), want, err)
	}
	if wantMsg != "" && !strings.Contains(status.Convert(err).Message(), wantMsg) {
		t.Fatalf("error message = %q, want substring %q", status.Convert(err).Message(), wantMsg)
	}
}

func setRangesRequest(ids ...uint64) *pbv2.SetRangesRequest {
	req := &pbv2.SetRangesRequest{}
	for _, id := range ids {
		req.Ranges = append(req.Ranges, &pbv2.RangeInfo{Id: id, Replicas: []string{"127.0.0.1:7001"}})
	}
	return req
}

func TestMetaAdminAuthRejectsMissingToken(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	_, err := cl.SetRanges(ctx, setRangesRequest(2))
	requireCode(t, err, codes.Unauthenticated, "admin token required")

	rows, err := cl.ListRanges(ctx, &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if len(rows.GetRanges()) != 1 || rows.GetRanges()[0].GetId() != 1 {
		t.Fatalf("range map changed after rejected SetRanges: %+v", rows.GetRanges())
	}
}

func TestMetaAdminAuthRejectsWrongToken(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	cases := []context.Context{
		withBearer(ctx, "wrong-token"),
		// A raw token without the Bearer scheme is not accepted.
		metadata.AppendToOutgoingContext(ctx, "authorization", testAdminToken),
		metadata.AppendToOutgoingContext(ctx, "authorization", "Basic "+testAdminToken),
	}
	for i, authCtx := range cases {
		_, err := cl.SetRanges(authCtx, setRangesRequest(2))
		requireCode(t, err, codes.Unauthenticated, "")
		if strings.Contains(err.Error(), testAdminToken) {
			t.Fatalf("case %d leaked the admin token in the error: %v", i, err)
		}
	}
}

func TestMetaAdminAuthAcceptsCorrectToken(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	resp, err := cl.SetRanges(withBearer(ctx, testAdminToken), setRangesRequest(10))
	if err != nil {
		t.Fatalf("SetRanges with token: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("SetRanges with token = %+v, want success", resp)
	}
	rows, err := cl.ListRanges(ctx, &pbv2.ListRangesRequest{})
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if len(rows.GetRanges()) != 1 || rows.GetRanges()[0].GetId() != 10 {
		t.Fatalf("range map = %+v, want the authenticated update", rows.GetRanges())
	}
}

func TestMetaAdminAuthProtectsSplitAndMerge(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	if _, err := cl.SplitRange(ctx, &pbv2.MetaSplitRangeRequest{RangeId: 99, SplitKey: []byte("m")}); err == nil {
		t.Fatal("SplitRange without a token succeeded")
	} else {
		requireCode(t, err, codes.Unauthenticated, "")
	}
	if _, err := cl.MergeRanges(ctx, &pbv2.MetaMergeRangesRequest{LeftRangeId: 99, RightRangeId: 100}); err == nil {
		t.Fatal("MergeRanges without a token succeeded")
	} else {
		requireCode(t, err, codes.Unauthenticated, "")
	}

	// With the token the request reaches the handler; unknown ranges are
	// reported in the response body, without contacting any KV node.
	resp, err := cl.SplitRange(withBearer(ctx, testAdminToken), &pbv2.MetaSplitRangeRequest{RangeId: 99, SplitKey: []byte("m")})
	if err != nil {
		t.Fatalf("SplitRange with token: %v", err)
	}
	if resp.GetSuccess() || !strings.Contains(resp.GetError(), "range not found") {
		t.Fatalf("SplitRange with token = %+v, want handler-level range-not-found", resp)
	}
	mresp, err := cl.MergeRanges(withBearer(ctx, testAdminToken), &pbv2.MetaMergeRangesRequest{LeftRangeId: 99, RightRangeId: 100})
	if err != nil {
		t.Fatalf("MergeRanges with token: %v", err)
	}
	if mresp.GetSuccess() || !strings.Contains(mresp.GetError(), "range not found") {
		t.Fatalf("MergeRanges with token = %+v, want handler-level range-not-found", mresp)
	}
}

func TestMetaReadOnlyRPCsDoNotRequireToken(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	if _, err := cl.ListRanges(ctx, &pbv2.ListRangesRequest{}); err != nil {
		t.Fatalf("ListRanges without token: %v", err)
	}
	info, err := cl.GetRange(ctx, &pbv2.GetRangeRequest{Key: []byte("k")})
	if err != nil {
		t.Fatalf("GetRange without token: %v", err)
	}
	if info.GetId() != 1 {
		t.Fatalf("GetRange = %+v, want range 1", info)
	}
	if _, err := cl.Leader(ctx, &pbv2.LeaderRequest{}); err != nil {
		t.Fatalf("Leader without token: %v", err)
	}
	ts, err := cl.AllocTS(ctx, &pbv2.AllocTSRequest{Count: 1})
	if err != nil {
		t.Fatalf("AllocTS without token: %v", err)
	}
	if ts.GetStart() == 0 {
		t.Fatalf("AllocTS returned zero timestamp: %+v", ts)
	}
}

func TestMetaAdminAuthDisabledWithEmptyToken(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, "")
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	resp, err := cl.SetRanges(ctx, setRangesRequest(3))
	if err != nil {
		t.Fatalf("SetRanges without token and empty AdminToken: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("SetRanges = %+v, want success", resp)
	}
}

func TestMetaAdminTokenNotLogged(t *testing.T) {
	s, addr, logs := startAuthTestMeta(t, testAdminToken)
	cl := rawMetaClient(t, addr)
	ctx := testCtx(t)

	if _, err := cl.SetRanges(ctx, setRangesRequest(2)); err == nil {
		t.Fatal("SetRanges without token unexpectedly succeeded")
	}
	if _, err := cl.SetRanges(withBearer(ctx, "wrong-token"), setRangesRequest(2)); err == nil {
		t.Fatal("SetRanges with a wrong token unexpectedly succeeded")
	}
	if _, err := cl.SetRanges(withBearer(ctx, testAdminToken), setRangesRequest(2)); err != nil {
		t.Fatalf("SetRanges with token: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	logged := logs.String()
	if strings.Contains(logged, testAdminToken) || strings.Contains(logged, "wrong-token") {
		t.Fatalf("admin token appeared in logs:\n%s", logged)
	}
	if !strings.Contains(logged, "admin_auth=true") {
		t.Fatalf("startup log did not report admin_auth=true:\n%s", logged)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	base := context.Background()
	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "bearer", value: "Bearer s3cret", want: "s3cret", ok: true},
		{name: "lowercase scheme", value: "bearer s3cret", want: "s3cret", ok: true},
		{name: "extra spaces", value: "Bearer   s3cret", want: "s3cret", ok: true},
		{name: "basic", value: "Basic s3cret"},
		{name: "raw token", value: "s3cret"},
		{name: "empty bearer", value: "Bearer   "},
		{name: "empty", value: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(base, metadata.Pairs("authorization", tc.value))
			got, ok := bearerToken(ctx)
			if ok != tc.ok || string(got) != tc.want {
				t.Fatalf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestMetaAdminAuthWithV2Client exercises the client/server contract: the
// client attaches the token only on admin calls, and read-only calls still work
// without one.
func TestMetaAdminAuthWithV2Client(t *testing.T) {
	_, addr, _ := startAuthTestMeta(t, testAdminToken)
	ctx := testCtx(t)

	noToken, err := v2client.NewMetaOnly(addr)
	if err != nil {
		t.Fatalf("NewMetaOnly: %v", err)
	}
	t.Cleanup(func() { _ = noToken.Close() })
	withToken, err := v2client.NewMetaOnlyWithOptions(addr, v2client.Options{AdminToken: testAdminToken})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = withToken.Close() })

	err = noToken.SetRanges(ctx, []v2client.RangeInfo{{ID: 7, Replicas: []string{"127.0.0.1:7001"}}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("client without token: SetRanges error = %v, want Unauthenticated", err)
	}
	if _, err := noToken.SplitRange(ctx, 1, []byte("m")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("client without token: SplitRange error = %v, want Unauthenticated", err)
	}
	rows, err := noToken.ListRanges(ctx)
	if err != nil {
		t.Fatalf("client without token: ListRanges: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("ListRanges without token = %+v, want the initial map", rows)
	}

	if err := withToken.SetRanges(ctx, []v2client.RangeInfo{{ID: 7, Replicas: []string{"127.0.0.1:7001"}}}); err != nil {
		t.Fatalf("client with token: SetRanges: %v", err)
	}
	rows, err = noToken.ListRanges(ctx)
	if err != nil {
		t.Fatalf("ListRanges after authenticated SetRanges: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 7 {
		t.Fatalf("ListRanges after authenticated SetRanges = %+v, want range 7", rows)
	}
}

// TestTLSMetaServerEnforcesAdminToken verifies that the token check is
// independent of the transport: a TLS listener still rejects unauthenticated
// admin calls and accepts the Bearer credential.
func TestTLSMetaServerEnforcesAdminToken(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	s, err := New(Config{
		Addr: "127.0.0.1:0",
		Ranges: []meta.Range{{
			ID: 1, Replicas: []string{"127.0.0.1:7001"}, Leader: "127.0.0.1:7001", Version: 1,
		}},
		Logger:     tlsTestLogger(),
		TLS:        metaTLSOptions(certs, false),
		AdminToken: testAdminToken,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	clientTLS := &grpctls.ClientOptions{CAFile: certs.CAFile, ServerName: "localhost"}
	noToken, err := v2client.NewMetaOnlyWithOptions(s.Addr(), v2client.Options{TLS: clientTLS})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = noToken.Close() })
	withToken, err := v2client.NewMetaOnlyWithOptions(s.Addr(), v2client.Options{
		TLS: clientTLS, AdminToken: testAdminToken,
	})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = withToken.Close() })

	ctx := testCtx(t)
	if err := noToken.SetRanges(ctx, []v2client.RangeInfo{{ID: 2, Replicas: []string{"127.0.0.1:7001"}}}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("TLS client without token: SetRanges error = %v, want Unauthenticated", err)
	}
	if err := withToken.SetRanges(ctx, []v2client.RangeInfo{{ID: 2, Replicas: []string{"127.0.0.1:7001"}}}); err != nil {
		t.Fatalf("TLS client with token: SetRanges: %v", err)
	}
}
