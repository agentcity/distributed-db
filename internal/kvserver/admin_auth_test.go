package kvserver

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"distributed-db/internal/grpctls"
	"distributed-db/internal/grpctls/testutil"
	pbv2 "distributed-db/internal/pb/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	kvAuthTestToken      = "kv-auth-test-admin-token"
	kvAuthTestWrongToken = "kv-auth-test-wrong-token"
)

// kvAuthSyncBuffer is a concurrency-safe log sink; the gRPC server may log
// while the test reads the buffer.
type kvAuthSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *kvAuthSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *kvAuthSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func kvAuthTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func kvAuthWithBearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func kvAuthRequireCode(t *testing.T, err error, want codes.Code, wantMsg string) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("status code = %v, want %v (err = %v)", status.Code(err), want, err)
	}
	if wantMsg != "" && !strings.Contains(status.Convert(err).Message(), wantMsg) {
		t.Fatalf("error message = %q, want substring %q", status.Convert(err).Message(), wantMsg)
	}
}

// kvAuthStartServer starts a quiet single-node, single-range server with the
// given admin token. It returns the server and its bound address.
func kvAuthStartServer(t *testing.T, token string, logger *slog.Logger) (*Server, string) {
	t.Helper()
	addr := testFreeAddr(t)
	if logger == nil {
		logger = testLogger()
	}
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		Ranges:             []RangeConfig{{ID: 1}},
		AdminToken:         token,
		ElectionTimeoutMin: 40 * time.Millisecond,
		ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             logger,
		SnapshotInterval:   -1,
		GCInterval:         -1,
		DiskCheckInterval:  -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	waitRangeLeader(t, srv, 1)
	return srv, srv.Addr()
}

func kvAuthDial(t *testing.T, addr string) pbv2.KVServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pbv2.NewKVServiceClient(cc)
}

// kvAuthCall is one protected RPC invocation. The request is deliberately
// minimal/invalid: with a valid token it must reach the handler (which then
// answers with an application error or a success) instead of being stopped by
// the auth interceptor.
type kvAuthCall struct {
	name string
	call func(context.Context, pbv2.KVServiceClient) error
}

func kvAuthProtectedCalls() []kvAuthCall {
	return []kvAuthCall{
		{name: "SplitRange", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.SplitRange(ctx, &pbv2.SplitRangeRequest{})
			return err
		}},
		{name: "MergeRanges", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.MergeRanges(ctx, &pbv2.MergeRangesRequest{})
			return err
		}},
		{name: "AddReplica", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.AddReplica(ctx, &pbv2.AddReplicaRequest{})
			return err
		}},
		{name: "AddLearner", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.AddLearner(ctx, &pbv2.AddLearnerRequest{})
			return err
		}},
		{name: "PromoteReplica", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.PromoteReplica(ctx, &pbv2.PromoteReplicaRequest{})
			return err
		}},
		{name: "RemoveVoter", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.RemoveVoter(ctx, &pbv2.RemoveVoterRequest{})
			return err
		}},
		{name: "RemoveReplica", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.RemoveReplica(ctx, &pbv2.RemoveReplicaRequest{})
			return err
		}},
		{name: "TransferLeader", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.TransferLeader(ctx, &pbv2.TransferLeaderRequest{})
			return err
		}},
		{name: "UpdateRangeEpoch", call: func(ctx context.Context, c pbv2.KVServiceClient) error {
			_, err := c.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{})
			return err
		}},
	}
}

func TestKVAdminLocalRejectsMissingToken(t *testing.T) {
	_, addr := kvAuthStartServer(t, kvAuthTestToken, nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	for _, tc := range kvAuthProtectedCalls() {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(ctx, client)
			kvAuthRequireCode(t, err, codes.Unauthenticated, "admin token required")
		})
	}
}

func TestKVAdminLocalRejectsWrongToken(t *testing.T) {
	_, addr := kvAuthStartServer(t, kvAuthTestToken, nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	contexts := []struct {
		name    string
		ctx     context.Context
		wantMsg string
	}{
		{name: "wrong bearer", ctx: kvAuthWithBearer(ctx, kvAuthTestWrongToken), wantMsg: "invalid admin token"},
		// A raw token or another scheme is not a well-formed Bearer
		// credential, so it is reported as missing rather than compared.
		{name: "raw token", ctx: metadata.AppendToOutgoingContext(ctx, "authorization", kvAuthTestToken), wantMsg: "admin token required"},
		{name: "basic scheme", ctx: metadata.AppendToOutgoingContext(ctx, "authorization", "Basic "+kvAuthTestToken), wantMsg: "admin token required"},
	}
	for _, auth := range contexts {
		for _, tc := range kvAuthProtectedCalls() {
			t.Run(auth.name+"/"+tc.name, func(t *testing.T) {
				err := tc.call(auth.ctx, client)
				kvAuthRequireCode(t, err, codes.Unauthenticated, auth.wantMsg)
				if strings.Contains(err.Error(), kvAuthTestToken) || strings.Contains(err.Error(), kvAuthTestWrongToken) {
					t.Fatalf("error leaked the admin token: %v", err)
				}
			})
		}
	}
}

func TestKVAdminLocalAcceptsCorrectToken(t *testing.T) {
	srv, addr := kvAuthStartServer(t, kvAuthTestToken, nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	if _, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 9}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("UpdateRangeEpoch without token error = %v, want Unauthenticated", err)
	}

	for _, tc := range kvAuthProtectedCalls() {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(kvAuthWithBearer(ctx, kvAuthTestToken), client); err != nil {
				t.Fatalf("%s with the correct token was rejected: %v", tc.name, err)
			}
		})
	}

	// The token must actually authorize the operation, not merely reach a
	// handler that rejects everything.
	resp, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvAuthTestToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 9})
	if err != nil || !resp.GetSuccess() || resp.GetEpoch() != 9 {
		t.Fatalf("UpdateRangeEpoch with token = %v %+v, want success epoch 9", err, resp)
	}
	if got := srv.Ranges()[0].Epoch; got != 9 {
		t.Fatalf("range epoch = %d, want 9", got)
	}
}

func TestKVAdminLocalDisabledWithEmptyToken(t *testing.T) {
	_, addr := kvAuthStartServer(t, "", nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	for _, tc := range kvAuthProtectedCalls() {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(ctx, client); err != nil {
				t.Fatalf("%s with empty AdminToken error = %v, want handler-level result", tc.name, err)
			}
		})
	}
	resp, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 3})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("UpdateRangeEpoch with empty AdminToken = %v %+v, want success", err, resp)
	}
}

// TestKVAdminLocalReadAndDataRPCsUnaffected is the data-plane regression: a
// token-enabled node must still serve transactional reads/writes and the
// read-only observability RPCs without an authorization header.
func TestKVAdminLocalReadAndDataRPCsUnaffected(t *testing.T) {
	_, addr := kvAuthStartServer(t, kvAuthTestToken, nil)
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	putStatsKV(t, client, "data-key", "data-value", 100)

	get, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("data-key"), Ts: 1 << 62})
	if err != nil || !get.GetFound() || string(get.GetValue()) != "data-value" {
		t.Fatalf("Get without token = %v %+v, want the committed value", err, get)
	}
	scan, err := client.Scan(ctx, &pbv2.ScanRequest{Start: []byte("a"), End: []byte("z"), Ts: 1 << 62})
	if err != nil || len(scan.GetKvs()) != 1 || string(scan.GetKvs()[0].GetKey()) != "data-key" {
		t.Fatalf("Scan without token = %v %+v, want data-key", err, scan)
	}
	statusResp, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: 1})
	if err != nil || statusResp.GetRangeId() != 1 {
		t.Fatalf("Status without token = %v %+v", err, statusResp)
	}
	stats, err := client.RangeStats(ctx, &pbv2.RangeStatsRequest{RangeId: 1})
	if err != nil || !stats.GetSuccess() {
		t.Fatalf("RangeStats without token = %v %+v", err, stats)
	}
	membership, err := client.Membership(ctx, &pbv2.MembershipRequest{RangeId: 1})
	if err != nil || !membership.GetSuccess() {
		t.Fatalf("Membership without token = %v %+v", err, membership)
	}
}

func TestKVAdminLocalTokenNotLogged(t *testing.T) {
	logs := &kvAuthSyncBuffer{}
	srv, addr := kvAuthStartServer(t, kvAuthTestToken,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	client := kvAuthDial(t, addr)
	ctx := kvAuthTestCtx(t)

	if _, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); err == nil {
		t.Fatal("UpdateRangeEpoch without token unexpectedly succeeded")
	}
	if _, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvAuthTestWrongToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); err == nil {
		t.Fatal("UpdateRangeEpoch with a wrong token unexpectedly succeeded")
	}
	if _, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvAuthTestToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); err != nil {
		t.Fatalf("UpdateRangeEpoch with token: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	logged := logs.String()
	if strings.Contains(logged, kvAuthTestToken) || strings.Contains(logged, kvAuthTestWrongToken) {
		t.Fatalf("admin token appeared in logs:\n%s", logged)
	}
	if !strings.Contains(logged, "admin_auth=true") {
		t.Fatalf("startup log did not report admin_auth=true:\n%s", logged)
	}
}

// TestKVAdminLocalTLSListenerEnforces verifies that token enforcement is
// independent of transport TLS: a TLS listener still rejects unauthenticated
// admin calls, accepts the Bearer credential and keeps read-only calls open.
func TestKVAdminLocalTLSListenerEnforces(t *testing.T) {
	certs, err := testutil.Generate(t.TempDir())
	if err != nil {
		t.Fatalf("generate certs: %v", err)
	}
	addr := testFreeAddr(t)
	srv, err := New(Config{
		ID:                 addr,
		RaftAddr:           addr,
		DataDir:            t.TempDir(),
		Ranges:             []RangeConfig{{ID: 1}},
		AdminToken:         kvAuthTestToken,
		ElectionTimeoutMin: 40 * time.Millisecond,
		ElectionTimeoutMax: 80 * time.Millisecond,
		HeartbeatInterval:  10 * time.Millisecond,
		CacheSize:          8 << 20,
		MemTableSize:       4 << 20,
		Logger:             testLogger(),
		TLS:                tlsTestOptions(t, certs, false),
		SnapshotInterval:   -1,
		GCInterval:         -1,
		DiskCheckInterval:  -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		_ = srv.Stop(context.Background())
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	waitRangeLeader(t, srv, 1)

	creds, err := grpctls.ClientCredentials(grpctls.ClientOptions{
		CAFile:     certs.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	cc, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial TLS: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := pbv2.NewKVServiceClient(cc)
	ctx := kvAuthTestCtx(t)

	if _, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("TLS UpdateRangeEpoch without token error = %v, want Unauthenticated", err)
	}
	resp, err := client.UpdateRangeEpoch(kvAuthWithBearer(ctx, kvAuthTestToken),
		&pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("TLS UpdateRangeEpoch with token = %v %+v, want success", err, resp)
	}
	if _, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: 1}); err != nil {
		t.Fatalf("TLS Status without token: %v", err)
	}
}

// TestKVAdminLocalMethodSet pins the protected set so a future RPC addition cannot
// silently become admin-only or, worse, an admin RPC slip through unprotected.
func TestKVAdminLocalMethodSet(t *testing.T) {
	want := map[string]struct{}{
		pbv2.KVService_SplitRange_FullMethodName:       {},
		pbv2.KVService_MergeRanges_FullMethodName:      {},
		pbv2.KVService_AddReplica_FullMethodName:       {},
		pbv2.KVService_AddLearner_FullMethodName:       {},
		pbv2.KVService_PromoteReplica_FullMethodName:   {},
		pbv2.KVService_RemoveVoter_FullMethodName:      {},
		pbv2.KVService_RemoveReplica_FullMethodName:    {},
		pbv2.KVService_TransferLeader_FullMethodName:   {},
		pbv2.KVService_UpdateRangeEpoch_FullMethodName: {},
	}
	if len(kvAdminMethods) != len(want) {
		t.Fatalf("protected method count = %d, want %d: %v", len(kvAdminMethods), len(want), kvAdminMethods)
	}
	for method := range want {
		if _, ok := kvAdminMethods[method]; !ok {
			t.Fatalf("%s is not protected", method)
		}
	}
	for _, method := range []string{
		pbv2.KVService_Get_FullMethodName,
		pbv2.KVService_Scan_FullMethodName,
		pbv2.KVService_Prewrite_FullMethodName,
		pbv2.KVService_Commit_FullMethodName,
		pbv2.KVService_CommitSingle_FullMethodName,
		pbv2.KVService_Rollback_FullMethodName,
		pbv2.KVService_CheckTxnStatus_FullMethodName,
		pbv2.KVService_ResolveLock_FullMethodName,
		pbv2.KVService_RangeStats_FullMethodName,
		pbv2.KVService_Status_FullMethodName,
		pbv2.KVService_Membership_FullMethodName,
		pbv2.RaftService_RequestVote_FullMethodName,
		pbv2.RaftService_AppendEntries_FullMethodName,
		pbv2.RaftService_InstallSnapshot_FullMethodName,
		pbv2.RaftService_InstallSnapshotChunk_FullMethodName,
		pbv2.RaftService_TimeoutNow_FullMethodName,
	} {
		if _, ok := kvAdminMethods[method]; ok {
			t.Fatalf("%s must not require the admin token", method)
		}
	}
}

// TestKVAdminLocalInterceptorClassification exercises the interceptor directly,
// including Raft and data-plane methods that are hard to call over a live
// single-node server.
func TestKVAdminLocalInterceptorClassification(t *testing.T) {
	interceptor := adminAuthUnaryInterceptor(kvAuthTestToken)
	handlerCalls := 0
	handler := func(context.Context, any) (any, error) {
		handlerCalls++
		return "ok", nil
	}
	noAuthCtx := context.Background()
	// The interceptor reads incoming metadata on the server side; gRPC moves
	// outgoing metadata into incoming metadata in transit. In a direct unit
	// call that translation does not happen, so build incoming metadata.
	authCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+kvAuthTestToken))

	for method := range kvAdminMethods {
		handlerCalls = 0
		_, err := interceptor(noAuthCtx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		kvAuthRequireCode(t, err, codes.Unauthenticated, "")
		if handlerCalls != 0 {
			t.Fatalf("%s: handler ran without a token", method)
		}

		handlerCalls = 0
		resp, err := interceptor(authCtx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		if err != nil || resp != "ok" || handlerCalls != 1 {
			t.Fatalf("%s with token = (%v, %v), handler calls %d; want ok", method, resp, err, handlerCalls)
		}
	}

	unprotected := []string{
		pbv2.KVService_Get_FullMethodName,
		pbv2.KVService_Scan_FullMethodName,
		pbv2.KVService_Prewrite_FullMethodName,
		pbv2.KVService_Commit_FullMethodName,
		pbv2.KVService_CommitSingle_FullMethodName,
		pbv2.KVService_Rollback_FullMethodName,
		pbv2.KVService_CheckTxnStatus_FullMethodName,
		pbv2.KVService_ResolveLock_FullMethodName,
		pbv2.KVService_RangeStats_FullMethodName,
		pbv2.KVService_Status_FullMethodName,
		pbv2.KVService_Membership_FullMethodName,
		pbv2.RaftService_RequestVote_FullMethodName,
		pbv2.RaftService_AppendEntries_FullMethodName,
		pbv2.RaftService_InstallSnapshot_FullMethodName,
		pbv2.RaftService_InstallSnapshotChunk_FullMethodName,
		pbv2.RaftService_TimeoutNow_FullMethodName,
	}
	for _, method := range unprotected {
		handlerCalls = 0
		resp, err := interceptor(noAuthCtx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		if err != nil || resp != "ok" || handlerCalls != 1 {
			t.Fatalf("%s without token = (%v, %v), handler calls %d; want ok", method, resp, err, handlerCalls)
		}
	}
}

func TestKVAdminLocalBearerParsing(t *testing.T) {
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
			got, ok := adminBearerToken(ctx)
			if ok != tc.ok || string(got) != tc.want {
				t.Fatalf("adminBearerToken(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestKVAdminLocalWithAdminTokenContext(t *testing.T) {
	ctx := WithAdminToken(context.Background(), kvAuthTestToken)
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("WithAdminToken did not attach outgoing metadata")
	}
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer "+kvAuthTestToken {
		t.Fatalf("authorization metadata = %v", got)
	}
	emptyCtx := WithAdminToken(context.Background(), "")
	if md, ok := metadata.FromOutgoingContext(emptyCtx); ok && len(md.Get("authorization")) != 0 {
		t.Fatalf("WithAdminToken with an empty token attached metadata: %v", md)
	}
}

func TestKVAdminLocalClientInterceptor(t *testing.T) {
	interceptor := adminTokenClientInterceptor(kvAuthTestToken)
	var got []string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		got = md.Get("authorization")
		return nil
	}
	invoke := func(method string) {
		t.Helper()
		got = nil
		if err := interceptor(context.Background(), method, nil, nil, nil, invoker); err != nil {
			t.Fatalf("interceptor(%s): %v", method, err)
		}
	}

	invoke(pbv2.KVService_UpdateRangeEpoch_FullMethodName)
	if len(got) != 1 || got[0] != "Bearer "+kvAuthTestToken {
		t.Fatalf("admin method authorization metadata = %v", got)
	}
	invoke(pbv2.KVService_Get_FullMethodName)
	if len(got) != 0 {
		t.Fatalf("data method received authorization metadata: %v", got)
	}
}

// TestKVAdminLocalDialOption exercises the outbound helper end to end: a
// ClientConn built with AdminTokenDialOption can call a protected RPC without
// per-call context decoration.
func TestKVAdminLocalDialOption(t *testing.T) {
	_, addr := kvAuthStartServer(t, kvAuthTestToken, nil)
	cc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		AdminTokenDialOption(kvAuthTestToken))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := pbv2.NewKVServiceClient(cc)
	ctx := kvAuthTestCtx(t)

	resp, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 4})
	if err != nil || !resp.GetSuccess() {
		t.Fatalf("UpdateRangeEpoch through AdminTokenDialOption = %v %+v, want success", err, resp)
	}

	// The same connection still serves unauthenticated data-plane traffic.
	putStatsKV(t, client, "dial-option-key", "value", 200)
	get, err := client.Get(ctx, &pbv2.GetRequest{Key: []byte("dial-option-key"), Ts: 1 << 62})
	if err != nil || !get.GetFound() {
		t.Fatalf("Get through the same connection = %v %+v", err, get)
	}
}
