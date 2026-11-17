package v2client

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pbv2 "distributed-db/internal/pb/v2"
)

// authCaptureMeta records the outgoing "authorization" metadata of every meta
// call it sees and delegates to an in-memory meta service.
type authCaptureMeta struct {
	metaAPI
	mu   sync.Mutex
	auth map[string][]string
}

func newAuthCaptureMeta(base metaAPI) *authCaptureMeta {
	return &authCaptureMeta{metaAPI: base, auth: make(map[string][]string)}
}

func (m *authCaptureMeta) record(method string, ctx context.Context) {
	md, _ := metadata.FromOutgoingContext(ctx)
	vals := append([]string(nil), md.Get("authorization")...)
	m.mu.Lock()
	m.auth[method] = vals
	m.mu.Unlock()
}

func (m *authCaptureMeta) got(method string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.auth[method]...)
}

func (m *authCaptureMeta) AllocTS(ctx context.Context, in *pbv2.AllocTSRequest, opts ...grpc.CallOption) (*pbv2.AllocTSResponse, error) {
	m.record("AllocTS", ctx)
	return m.metaAPI.AllocTS(ctx, in, opts...)
}

func (m *authCaptureMeta) GetRange(ctx context.Context, in *pbv2.GetRangeRequest, opts ...grpc.CallOption) (*pbv2.RangeInfo, error) {
	m.record("GetRange", ctx)
	return m.metaAPI.GetRange(ctx, in, opts...)
}

func (m *authCaptureMeta) Leader(ctx context.Context, in *pbv2.LeaderRequest, opts ...grpc.CallOption) (*pbv2.LeaderResponse, error) {
	m.record("Leader", ctx)
	return m.metaAPI.Leader(ctx, in, opts...)
}

func (m *authCaptureMeta) ListRanges(ctx context.Context, in *pbv2.ListRangesRequest, opts ...grpc.CallOption) (*pbv2.ListRangesResponse, error) {
	m.record("ListRanges", ctx)
	return m.metaAPI.ListRanges(ctx, in, opts...)
}

func (m *authCaptureMeta) SetRanges(ctx context.Context, in *pbv2.SetRangesRequest, opts ...grpc.CallOption) (*pbv2.SetRangesResponse, error) {
	m.record("SetRanges", ctx)
	return m.metaAPI.SetRanges(ctx, in, opts...)
}

func (m *authCaptureMeta) SplitRange(ctx context.Context, in *pbv2.MetaSplitRangeRequest, opts ...grpc.CallOption) (*pbv2.MetaSplitRangeResponse, error) {
	m.record("SplitRange", ctx)
	return m.metaAPI.SplitRange(ctx, in, opts...)
}

func (m *authCaptureMeta) MergeRanges(ctx context.Context, in *pbv2.MetaMergeRangesRequest, opts ...grpc.CallOption) (*pbv2.MetaMergeRangesResponse, error) {
	m.record("MergeRanges", ctx)
	return m.metaAPI.MergeRanges(ctx, in, opts...)
}

func TestAdminCallsSendBearerToken(t *testing.T) {
	captured := newAuthCaptureMeta(newFakeMeta("meta1"))
	c := newTestClient(captured)
	c.adminToken = "s3cret"
	ctx := context.Background()

	if err := c.SetRanges(ctx, []RangeInfo{{ID: 1, Replicas: []string{"kv1"}}}); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if _, err := c.SplitRange(ctx, 1, []byte("m")); err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if err := c.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("MergeRanges: %v", err)
	}

	for _, method := range []string{"SetRanges", "SplitRange", "MergeRanges"} {
		got := captured.got(method)
		if len(got) != 1 || got[0] != "Bearer s3cret" {
			t.Fatalf("%s authorization = %q, want [Bearer s3cret]", method, got)
		}
	}
}

func TestAdminCallsWithoutTokenSendNoAuthorization(t *testing.T) {
	captured := newAuthCaptureMeta(newFakeMeta("meta1"))
	c := newTestClient(captured)
	ctx := context.Background()

	if err := c.SetRanges(ctx, []RangeInfo{{ID: 1, Replicas: []string{"kv1"}}}); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if _, err := c.SplitRange(ctx, 1, []byte("m")); err != nil {
		t.Fatalf("SplitRange: %v", err)
	}
	if err := c.MergeRanges(ctx, 1, 2); err != nil {
		t.Fatalf("MergeRanges: %v", err)
	}

	for _, method := range []string{"SetRanges", "SplitRange", "MergeRanges"} {
		if got := captured.got(method); len(got) != 0 {
			t.Fatalf("%s authorization = %q, want none", method, got)
		}
	}
}

func TestReadOnlyMetaCallsDoNotSendAdminToken(t *testing.T) {
	captured := newAuthCaptureMeta(newFakeMeta("meta1"))
	c := newTestClient(captured)
	c.adminToken = "s3cret"
	ctx := context.Background()

	if _, err := c.ListRanges(ctx); err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	if _, _, err := c.MetaLeader(ctx); err != nil {
		t.Fatalf("MetaLeader: %v", err)
	}
	if _, err := c.AllocTS(ctx); err != nil {
		t.Fatalf("AllocTS: %v", err)
	}
	if _, err := c.rangeFor(ctx, []byte("k")); err != nil {
		t.Fatalf("rangeFor: %v", err)
	}

	for _, method := range []string{"ListRanges", "Leader", "AllocTS", "GetRange"} {
		if got := captured.got(method); len(got) != 0 {
			t.Fatalf("%s authorization = %q, want none (read-only RPC)", method, got)
		}
	}
}

// dataAuthKV records the incoming outgoing metadata of KV data calls.
type dataAuthKV struct {
	pbv2.KVServiceClient
	mu   sync.Mutex
	auth []string
}

func (k *dataAuthKV) record(ctx context.Context) {
	md, _ := metadata.FromOutgoingContext(ctx)
	// Record one entry per call, using the empty string when the call carried
	// no authorization metadata.
	vals := strings.Join(md.Get("authorization"), ",")
	k.mu.Lock()
	k.auth = append(k.auth, vals)
	k.mu.Unlock()
}

func (k *dataAuthKV) Get(ctx context.Context, _ *pbv2.GetRequest, _ ...grpc.CallOption) (*pbv2.GetResponse, error) {
	k.record(ctx)
	return &pbv2.GetResponse{Found: true, Value: []byte("v"), Leader: "kv1"}, nil
}

func (k *dataAuthKV) Status(ctx context.Context, _ *pbv2.StatusRequest, _ ...grpc.CallOption) (*pbv2.StatusResponse, error) {
	k.record(ctx)
	return &pbv2.StatusResponse{Leader: "kv1", Role: "leader"}, nil
}

func (k *dataAuthKV) auths() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.auth...)
}

func TestDataCallsDoNotSendAdminToken(t *testing.T) {
	c := newTestClient(newFakeMeta("meta1"))
	c.adminToken = "s3cret"
	kv := &dataAuthKV{}
	c.clients["kv1"] = kv
	c.leaders[1] = leaderEntry{addr: "kv1", until: time.Now().Add(time.Hour)}

	txn := &Txn{c: c, startTS: 1}
	if _, err := txn.Get(context.Background(), []byte("k")); err != nil {
		t.Fatalf("Txn.Get: %v", err)
	}
	auths := kv.auths()
	if len(auths) == 0 {
		t.Fatal("no KV data calls recorded")
	}
	for _, got := range auths {
		if got != "" {
			t.Fatalf("KV data call authorization = %q, want none", got)
		}
	}
}

func TestAdminTokenOptionPropagates(t *testing.T) {
	c, err := NewMetaOnlyWithOptions("127.0.0.1:1", Options{AdminToken: "s3cret"})
	if err != nil {
		t.Fatalf("NewMetaOnlyWithOptions: %v", err)
	}
	defer c.Close()
	if c.adminToken != "s3cret" {
		t.Fatalf("adminToken = %q, want s3cret", c.adminToken)
	}

	c2, err := NewWithOptions("127.0.0.1:1", []string{"127.0.0.1:2"}, Options{AdminToken: "other"})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c2.Close()
	if c2.adminToken != "other" {
		t.Fatalf("adminToken = %q, want other", c2.adminToken)
	}
}

func TestAdminTokenNotInSetRangesError(t *testing.T) {
	const token = "super-secret-admin-token"
	fake := newFakeMeta("meta1")
	fake.setSetRanges(func(*pbv2.SetRangesRequest) (*pbv2.SetRangesResponse, error) {
		return &pbv2.SetRangesResponse{Error: "meta: unauthenticated"}, nil
	})
	c := newTestClient(fake)
	c.adminToken = token
	err := c.SetRanges(context.Background(), []RangeInfo{{ID: 1, Replicas: []string{"kv1"}}})
	if err == nil {
		t.Fatal("SetRanges unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("admin token leaked into error: %v", err)
	}
}
