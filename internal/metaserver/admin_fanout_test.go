package metaserver

import (
	"context"
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
)

// fanoutAuthKV is a KVService test double that enforces the admin bearer
// token on the RPCs used by the fan-out test and records the authorization
// metadata it observed per method.
type fanoutAuthKV struct {
	pbv2.UnimplementedKVServiceServer

	token string

	mu   sync.Mutex
	auth map[string][]string
}

func (k *fanoutAuthKV) record(method string, ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	k.mu.Lock()
	k.auth[method] = append([]string(nil), md.Get(adminTokenMetadataKey)...)
	k.mu.Unlock()
}

func (k *fanoutAuthKV) got(method string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.auth[method]...)
}

func (k *fanoutAuthKV) authorize(ctx context.Context) error {
	if k.token == "" {
		return nil
	}
	for _, value := range metadata.ValueFromIncomingContext(ctx, adminTokenMetadataKey) {
		scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
		if ok && strings.EqualFold(scheme, adminTokenScheme) && strings.TrimSpace(token) == k.token {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "test kv: admin token required")
}

func (k *fanoutAuthKV) Status(ctx context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	k.record(pbv2.KVService_Status_FullMethodName, ctx)
	// Status is read-only and must stay reachable without a token.
	return &pbv2.StatusResponse{Id: "kv1", Role: "leader", Leader: "kv1"}, nil
}

func (k *fanoutAuthKV) UpdateRangeEpoch(ctx context.Context, req *pbv2.UpdateRangeEpochRequest) (*pbv2.UpdateRangeEpochResponse, error) {
	k.record(pbv2.KVService_UpdateRangeEpoch_FullMethodName, ctx)
	if err := k.authorize(ctx); err != nil {
		return nil, err
	}
	return &pbv2.UpdateRangeEpochResponse{Success: true, Epoch: req.GetEpoch()}, nil
}

func startFanoutAuthKV(t *testing.T, token string) (string, *fanoutAuthKV) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fanoutAuthKV{token: token, auth: make(map[string][]string)}
	srv := grpc.NewServer()
	pbv2.RegisterKVServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fake
}

// TestAdminFanoutMethodSet pins the outbound credential to exactly the KV
// admin RPCs. A new admin RPC must be added here deliberately; a data-plane
// RPC must never start receiving the token.
func TestAdminFanoutMethodSet(t *testing.T) {
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
	if len(kvAdminMethodNames) != len(want) {
		t.Fatalf("fan-out admin method count = %d, want %d: %v",
			len(kvAdminMethodNames), len(want), kvAdminMethodNames)
	}
	for method := range want {
		if _, ok := kvAdminMethodNames[method]; !ok {
			t.Fatalf("%s is not in the fan-out admin method set", method)
		}
	}

	const token = "fanout-method-set-token"
	interceptor := adminTokenUnaryClientInterceptor(token)
	var invokedAuth []string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		invokedAuth = md.Get(adminTokenMetadataKey)
		return nil
	}
	for method := range want {
		invokedAuth = nil
		if err := interceptor(context.Background(), method, nil, nil, nil, invoker); err != nil {
			t.Fatalf("interceptor(%s): %v", method, err)
		}
		if len(invokedAuth) != 1 || invokedAuth[0] != adminTokenScheme+" "+token {
			t.Fatalf("%s authorization = %v, want [%s %s]",
				method, invokedAuth, adminTokenScheme, token)
		}
	}

	for _, method := range []string{
		pbv2.KVService_Get_FullMethodName,
		pbv2.KVService_Scan_FullMethodName,
		pbv2.KVService_Status_FullMethodName,
		pbv2.KVService_RangeStats_FullMethodName,
		pbv2.KVService_Membership_FullMethodName,
		pbv2.RaftService_RequestVote_FullMethodName,
		pbv2.MetaService_ListRanges_FullMethodName,
	} {
		invokedAuth = nil
		if err := interceptor(context.Background(), method, nil, nil, nil, invoker); err != nil {
			t.Fatalf("interceptor(%s): %v", method, err)
		}
		if len(invokedAuth) != 0 {
			t.Fatalf("%s received authorization metadata %v, want none", method, invokedAuth)
		}
	}
}

// TestAdminFanoutSendsTokenOnAdminCalls exercises the real dial helper: the
// metaserver's ClientConn must authenticate on UpdateRangeEpoch and leave
// Status unauthenticated.
func TestAdminFanoutSendsTokenOnAdminCalls(t *testing.T) {
	const token = "fanout-live-token"
	addr, fake := startFanoutAuthKV(t, token)

	s, err := New(Config{Addr: "127.0.0.1:1", AdminToken: token})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cc, err := s.dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()
	client := pbv2.NewKVServiceClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp, err := client.UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2}); err != nil || !resp.GetSuccess() {
		t.Fatalf("UpdateRangeEpoch with admin token = %v %+v, want success", err, resp)
	}
	if got := fake.got(pbv2.KVService_UpdateRangeEpoch_FullMethodName); len(got) != 1 || got[0] != "Bearer "+token {
		t.Fatalf("UpdateRangeEpoch authorization = %v, want [Bearer token]", got)
	}
	if _, err := client.Status(ctx, &pbv2.StatusRequest{RangeId: 1}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := fake.got(pbv2.KVService_Status_FullMethodName); len(got) != 0 {
		t.Fatalf("Status authorization = %v, want none", got)
	}
}

// TestAdminFanoutRejectedWithoutToken proves the token is what makes the fan-
// out work, not a permissive test seam: an unconfigured metaserver is rejected
// by a token-protected KV node, and the rejection never echoes the token.
func TestAdminFanoutRejectedWithoutToken(t *testing.T) {
	const token = "fanout-secret-token"
	addr, _ := startFanoutAuthKV(t, token)

	s, err := New(Config{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.AdminToken != "" {
		t.Fatalf("test server AdminToken = %q, want empty", s.cfg.AdminToken)
	}
	cc, err := s.dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pbv2.NewKVServiceClient(cc).UpdateRangeEpoch(ctx, &pbv2.UpdateRangeEpochRequest{RangeId: 1, Epoch: 2})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("UpdateRangeEpoch without token error = %v, want Unauthenticated", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the admin token: %v", err)
	}
}
