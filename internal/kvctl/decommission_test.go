package kvctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
	"distributed-db/internal/v2client"
)

func TestDecommissionCommand(t *testing.T) {
	f := newFake()
	f.decommissionResult = DecommissionResult{
		RangeID: 1, Target: "n3", Leader: "n1",
		Voters: []string{"n1", "n2"}, Learners: []string{}, Transferred: true,
	}
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2,n3",
		"decommission", "-range", "1", "-target", "n3")
	if run.code != 0 || run.errOut != "" {
		t.Fatalf("decommission = code %d out %q err %q", run.code, run.out, run.errOut)
	}
	if len(f.decommissionCalls) != 1 {
		t.Fatalf("Decommission calls = %d, want 1", len(f.decommissionCalls))
	}
	got := f.decommissionCalls[0]
	if got.RangeID != 1 || got.Target != "n3" ||
		strings.Join(got.Replicas, ",") != "n1,n2,n3" {
		t.Fatalf("options = %+v", got)
	}
	for _, want := range []string{"decommissioned n3", "leader=n1", "voters=n1,n2", "leadership_transferred=true"} {
		if !strings.Contains(run.out, want) {
			t.Fatalf("output %q does not contain %q", run.out, want)
		}
	}
	if strings.Contains(run.out, "learner_promoted=true") {
		t.Fatalf("output reported a promotion for a voter target: %q", run.out)
	}
}

func TestDecommissionJSONAndAlreadyRemoved(t *testing.T) {
	f := newFake()
	f.decommissionResult = DecommissionResult{
		RangeID: 2, Target: "n2", Leader: "n1",
		Voters: []string{"n1"}, AlreadyRemoved: true,
	}
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2", "-format", "json",
		"decommission", "-range", "2", "-target", "n2")
	if run.code != 0 {
		t.Fatalf("decommission = code %d err %q", run.code, run.errOut)
	}
	var got decommissionJSON
	if err := json.Unmarshal([]byte(run.out), &got); err != nil {
		t.Fatalf("json: %v (%q)", err, run.out)
	}
	if got.Status != "ok" || got.RangeID != 2 || got.Target != "n2" ||
		!got.AlreadyRemoved || len(got.Voters) != 1 || got.Voters[0] != "n1" {
		t.Fatalf("json = %#v", got)
	}
	if got.Learners == nil {
		t.Fatal("learners encoded as null, want []")
	}

	// The table form of the idempotent path is explicit but still a success.
	run = runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2",
		"decommission", "-range", "2", "-target", "n2")
	if run.code != 0 || !strings.Contains(run.out, "already decommissioned") {
		t.Fatalf("idempotent output = code %d out %q err %q", run.code, run.out, run.errOut)
	}
}

func TestDecommissionQuiet(t *testing.T) {
	f := newFake()
	f.decommissionResult = DecommissionResult{RangeID: 1, Target: "n3", Voters: []string{"n1", "n2"}}
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2,n3", "-quiet",
		"decommission", "-range", "1", "-target", "n3")
	if run.code != 0 || run.out != "" {
		t.Fatalf("quiet decommission = code %d out %q err %q", run.code, run.out, run.errOut)
	}
}

func TestDecommissionValidation(t *testing.T) {
	f := newFake()
	cases := [][]string{
		{"decommission"},
		{"decommission", "-target", "n3"},
		{"decommission", "-range", "0", "-target", "n3"},
		{"decommission", "-range", "1"},
		{"decommission", "-range", "1", "-target", "n3", "extra"},
		{"-format", "raw", "decommission", "-range", "1", "-target", "n3"},
		{"decommission", "-range", "1", "-target", "n3", "-step-timeout", "-1s"},
		{"decommission", "-range", "1", "-target", "n3", "-catch-up-timeout", "-1s"},
		{"decommission", "-range", "1", "-target", "n3", "-poll-interval", "-1s"},
	}
	for _, args := range cases {
		run := runCLIArgs(t, f, "", append([]string{"-meta", "m1", "-nodes", "n1,n2,n3"}, args...)...)
		if run.code != 2 {
			t.Errorf("args %v = code %d, want 2 (out %q err %q)", args, run.code, run.out, run.errOut)
		}
	}
	if len(f.decommissionCalls) != 0 {
		t.Fatalf("invalid invocations reached the backend: %+v", f.decommissionCalls)
	}

	// decommission talks to KV nodes; -meta alone is a usage error and must
	// not open a backend.
	run := runCLIArgs(t, f, "", "-meta", "m1", "decommission", "-range", "1", "-target", "n3")
	if run.code != 2 || run.opens != 0 || !strings.Contains(run.errOut, "-nodes") {
		t.Fatalf("no-nodes decommission = code %d opens %d err %q", run.code, run.opens, run.errOut)
	}
}

func TestDecommissionReplicasDefaultAndTargetAdded(t *testing.T) {
	f := newFake()
	f.decommissionResult = DecommissionResult{RangeID: 1, Target: "n4", Voters: []string{"n1", "n2", "n3"}}
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2,n3", "-timeout", "45s",
		"decommission", "-range", "1", "-target", "n4")
	if run.code != 0 {
		t.Fatalf("decommission = code %d err %q", run.code, run.errOut)
	}
	if len(f.decommissionCalls) != 1 {
		t.Fatalf("calls = %d", len(f.decommissionCalls))
	}
	got := f.decommissionCalls[0]
	want := []string{"n1", "n2", "n3", "n4"}
	if strings.Join(got.Replicas, ",") != strings.Join(want, ",") {
		t.Fatalf("replicas = %v, want %v", got.Replicas, want)
	}
	if run.cfg.Timeout != 45*time.Second {
		t.Fatalf("explicit timeout = %v, want 45s", run.cfg.Timeout)
	}

	// The command gets a longer default than interactive data commands.
	f2 := newFake()
	f2.decommissionResult = DecommissionResult{RangeID: 1, Target: "n3", Voters: []string{"n1", "n2"}}
	run = runCLIArgs(t, f2, "", "-meta", "m1", "-nodes", "n1,n2,n3",
		"decommission", "-range", "1", "-target", "n3")
	if run.code != 0 {
		t.Fatalf("decommission = code %d err %q", run.code, run.errOut)
	}
	if run.cfg.Timeout != 2*time.Minute {
		t.Fatalf("default decommission timeout = %v, want 2m", run.cfg.Timeout)
	}
}

func TestDecommissionUsesReplicasFlag(t *testing.T) {
	f := newFake()
	f.decommissionResult = DecommissionResult{RangeID: 1, Target: "r3", Voters: []string{"r1", "r2"}}
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2",
		"decommission", "-range", "1", "-target", "r3", "-replicas", "r1,r2,r3",
		"-catch-up-timeout", "5s", "-step-timeout", "6s", "-poll-interval", "7ms")
	if run.code != 0 {
		t.Fatalf("decommission = code %d err %q", run.code, run.errOut)
	}
	got := f.decommissionCalls[0]
	if strings.Join(got.Replicas, ",") != "r1,r2,r3" {
		t.Fatalf("replicas = %v", got.Replicas)
	}
	if got.CatchUpTimeout != 5*time.Second || got.StepTimeout != 6*time.Second || got.PollInterval != 7*time.Millisecond {
		t.Fatalf("timeouts = %+v", got)
	}
}

func TestDecommissionErrorDoesNotLeakAdminToken(t *testing.T) {
	const token = "decommission-super-secret"
	f := newFake()
	f.decommissionErr = errors.New("rpc error: code = Unauthenticated desc = invalid admin token")
	run := runCLIArgs(t, f, "", "-meta", "m1", "-nodes", "n1,n2,n3", "-admin-token", token,
		"decommission", "-range", "1", "-target", "n3")
	if run.code != 1 {
		t.Fatalf("decommission = code %d, want 1 (err %q)", run.code, run.errOut)
	}
	if !strings.Contains(run.errOut, "Unauthenticated") {
		t.Fatalf("error output = %q, want the RPC error", run.errOut)
	}
	if strings.Contains(run.out, token) || strings.Contains(run.errOut, token) {
		t.Fatalf("admin token leaked: out=%q err=%q", run.out, run.errOut)
	}
}

func TestDecommissionRejectedByShell(t *testing.T) {
	f := newFake()
	run := runCLIArgs(t, f, "decommission -range 1 -target n3\nquit\n", "-nodes", "n1,n2,n3", "shell")
	if run.code != 0 {
		t.Fatalf("shell = code %d err %q", run.code, run.errOut)
	}
	if !strings.Contains(run.errOut, "not available in the shell") {
		t.Fatalf("shell err = %q", run.errOut)
	}
	if len(f.decommissionCalls) != 0 {
		t.Fatalf("shell reached decommission backend: %+v", f.decommissionCalls)
	}
}

// --- real in-process adapter integration ----------------------------------

type decommissionCluster struct {
	metaAddr string
	kvAddrs  []string
	kv       []*kvserver.Server
}

// startDecommissionCluster starts three real single-range KV nodes and a
// standalone meta server. The same token is used for both listeners unless a
// test overrides one side.
func startDecommissionCluster(t *testing.T, kvToken, metaToken string) *decommissionCluster {
	t.Helper()
	addrs := []string{
		decommissionFreeAddr(t), decommissionFreeAddr(t), decommissionFreeAddr(t),
	}
	cluster := &decommissionCluster{kvAddrs: addrs}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	for i, addr := range addrs {
		peers := make([]string, 0, len(addrs)-1)
		for j, peer := range addrs {
			if i != j {
				peers = append(peers, peer)
			}
		}
		srv, err := kvserver.New(kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(),
			Ranges: []kvserver.RangeConfig{{ID: 1, Peers: peers, Epoch: 1}},
			Peers:  nil,
			// A zero lifecycle block would start maintenance loops; disable
			// them so the test only measures the decommission path.
			SnapshotInterval:  -1,
			GCInterval:        -1,
			DiskCheckInterval: -1,
			AdminToken:        kvToken,
			Logger:            logger,
		})
		if err != nil {
			t.Fatalf("new kv %s: %v", addr, err)
		}
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start kv %s: %v", addr, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Stop(ctx)
		})
		cluster.kv = append(cluster.kv, srv)
	}

	ms, err := metaserver.New(metaserver.Config{
		Addr:       decommissionFreeAddr(t),
		Ranges:     []meta.Range{{ID: 1, Replicas: addrs, Version: 1}},
		AdminToken: metaToken,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}
	if err := ms.Start(context.Background()); err != nil {
		t.Fatalf("start meta: %v", err)
	}
	t.Cleanup(func() { _ = ms.Stop() })
	cluster.metaAddr = ms.Addr()

	waitDecommissionLeader(t, cluster.kv, 1)
	return cluster
}

func decommissionFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitDecommissionLeader(t *testing.T, servers []*kvserver.Server, rangeID uint64) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, srv := range servers {
			g, err := srv.GroupForID(rangeID)
			if err == nil && g.Status().Role == raft.Leader {
				return g.Status().ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("range %d did not elect a leader", rangeID)
	return ""
}

func runDecommissionCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	r := NewRunner(strings.NewReader(""), &out, &errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code := r.Run(ctx, args)
	return code, out.String(), errOut.String()
}

func readClusterRanges(t *testing.T, metaAddr, token string) []v2client.RangeInfo {
	t.Helper()
	c, err := v2client.NewMetaOnlyWithOptions(metaAddr, v2client.Options{AdminToken: token})
	if err != nil {
		t.Fatalf("meta client: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ranges, err := c.ListRanges(ctx)
	if err != nil {
		t.Fatalf("ListRanges: %v", err)
	}
	return ranges
}

// TestDecommissionAdapterRealCluster runs the full CLI -> v2client adapter ->
// Decommissioner path against real in-process Raft groups and a real meta
// server. Both listeners require the admin token, so a successful removal and
// range-map publish also proves the credential is sent on every KV admin RPC
// and on SetRanges.
func TestDecommissionAdapterRealCluster(t *testing.T) {
	const token = "decommission-e2e-admin-token"
	cluster := startDecommissionCluster(t, token, token)
	leader := waitDecommissionLeader(t, cluster.kv, 1)
	nodes := strings.Join(cluster.kvAddrs, ",")

	code, out, errOut := runDecommissionCLI(t,
		"-meta", cluster.metaAddr, "-nodes", nodes, "-admin-token", token,
		"-timeout", "60s", "decommission", "-range", "1", "-target", leader)
	if code != 0 {
		t.Fatalf("decommission = code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(out, "decommissioned "+leader) {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, token) || strings.Contains(errOut, token) {
		t.Fatalf("admin token leaked: out=%q err=%q", out, errOut)
	}

	ranges := readClusterRanges(t, cluster.metaAddr, token)
	if len(ranges) != 1 {
		t.Fatalf("ranges = %+v", ranges)
	}
	got := ranges[0]
	if len(got.Replicas) != 2 || containsString(got.Replicas, leader) {
		t.Fatalf("meta replicas = %v, want 2 without %s", got.Replicas, leader)
	}
	if got.Version != 2 {
		t.Fatalf("meta range version = %d, want 2", got.Version)
	}

	// A retry must converge without bumping the map again.
	code, out, errOut = runDecommissionCLI(t,
		"-meta", cluster.metaAddr, "-nodes", nodes, "-admin-token", token,
		"-timeout", "60s", "decommission", "-range", "1", "-target", leader)
	if code != 0 || !strings.Contains(out, "already decommissioned") {
		t.Fatalf("retry = code %d out %q err %q", code, out, errOut)
	}
	ranges = readClusterRanges(t, cluster.metaAddr, token)
	if len(ranges) != 1 || ranges[0].Version != 2 || len(ranges[0].Replicas) != 2 {
		t.Fatalf("retry changed the map: %+v", ranges)
	}
}

// TestDecommissionWrongKVTokenRejected proves the KV admin fan-out is actually
// authenticated: with the right token it succeeds (previous test), with a wrong
// one RemoveVoter is rejected and the range map is untouched.
func TestDecommissionWrongKVTokenRejected(t *testing.T) {
	const token = "decommission-correct-token"
	cluster := startDecommissionCluster(t, token, "")
	leader := waitDecommissionLeader(t, cluster.kv, 1)
	target := ""
	for _, addr := range cluster.kvAddrs {
		if addr != leader {
			target = addr
			break
		}
	}
	nodes := strings.Join(cluster.kvAddrs, ",")

	code, _, errOut := runDecommissionCLI(t,
		"-meta", cluster.metaAddr, "-nodes", nodes, "-admin-token", "wrong-token",
		"-timeout", "30s", "decommission", "-range", "1", "-target", target)
	if code != 1 {
		t.Fatalf("decommission with a wrong token = code %d err %q, want 1", code, errOut)
	}
	if !strings.Contains(strings.ToLower(errOut), "unauthenticated") {
		t.Fatalf("error output = %q, want Unauthenticated", errOut)
	}
	if strings.Contains(errOut, token) || strings.Contains(errOut, "wrong-token") {
		t.Fatalf("error leaked a token: %q", errOut)
	}
	ranges := readClusterRanges(t, cluster.metaAddr, "")
	if len(ranges) != 1 || len(ranges[0].Replicas) != 3 {
		t.Fatalf("range map changed after a rejected Raft change: %+v", ranges)
	}
}

// TestDecommissionMetaTokenRejectedThenRetry isolates the meta SetRanges
// credential: KV admin calls need no token, so the Raft removal succeeds but
// the map publish fails. The retry (same target now absent from Raft) must
// converge with the correct meta token, exercising the orchestrator's
// idempotent crash-recovery path.
func TestDecommissionMetaTokenRejectedThenRetry(t *testing.T) {
	const metaToken = "decommission-meta-only-token"
	cluster := startDecommissionCluster(t, "", metaToken)
	leader := waitDecommissionLeader(t, cluster.kv, 1)
	target := ""
	for _, addr := range cluster.kvAddrs {
		if addr != leader {
			target = addr
			break
		}
	}
	nodes := strings.Join(cluster.kvAddrs, ",")

	code, _, errOut := runDecommissionCLI(t,
		"-meta", cluster.metaAddr, "-nodes", nodes,
		"-timeout", "30s", "decommission", "-range", "1", "-target", target)
	if code != 1 {
		t.Fatalf("decommission without meta token = code %d err %q, want 1", code, errOut)
	}
	if !strings.Contains(strings.ToLower(errOut), "unauthenticated") {
		t.Fatalf("error output = %q, want Unauthenticated for SetRanges", errOut)
	}
	if strings.Contains(errOut, metaToken) {
		t.Fatalf("error leaked the meta token: %q", errOut)
	}

	// The Raft membership change already happened; the map is still stale.
	ranges := readClusterRanges(t, cluster.metaAddr, metaToken)
	if len(ranges) != 1 || len(ranges[0].Replicas) != 3 {
		t.Fatalf("range map should be stale after the rejected publish: %+v", ranges)
	}

	code, out, errOut := runDecommissionCLI(t,
		"-meta", cluster.metaAddr, "-nodes", nodes, "-admin-token", metaToken,
		"-timeout", "30s", "decommission", "-range", "1", "-target", target)
	if code != 0 {
		t.Fatalf("retry = code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(out, "already decommissioned") {
		t.Fatalf("retry output = %q, want the idempotent path", out)
	}
	ranges = readClusterRanges(t, cluster.metaAddr, metaToken)
	if len(ranges) != 1 || len(ranges[0].Replicas) != 2 || containsString(ranges[0].Replicas, target) {
		t.Fatalf("retry did not publish the removal: %+v", ranges)
	}
	if ranges[0].Version != 2 {
		t.Fatalf("retry version = %d, want 2", ranges[0].Version)
	}
}

// --- adapter unit tests ----------------------------------------------------

type adapterFakeKV struct {
	pbv2.UnimplementedKVServiceServer

	token string

	mu       sync.Mutex
	calls    map[string]int
	auth     map[string][]string
	status   *pbv2.StatusResponse
	transfer *pbv2.TransferLeaderResponse
	promote  *pbv2.MembershipResponse
	remove   *pbv2.MembershipResponse
}

func (f *adapterFakeKV) begin(ctx context.Context, method string, requireAuth bool) error {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := append([]string(nil), md.Get("authorization")...)
	f.mu.Lock()
	f.calls[method]++
	f.auth[method] = vals
	f.mu.Unlock()
	if !requireAuth || f.token == "" {
		return nil
	}
	if len(vals) != 1 || vals[0] != "Bearer "+f.token {
		return status.Error(codes.Unauthenticated, "test kv: admin token required")
	}
	return nil
}

func (f *adapterFakeKV) got(method string) (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method], append([]string(nil), f.auth[method]...)
}

func (f *adapterFakeKV) Status(ctx context.Context, _ *pbv2.StatusRequest) (*pbv2.StatusResponse, error) {
	if err := f.begin(ctx, pbv2.KVService_Status_FullMethodName, false); err != nil {
		return nil, err
	}
	if f.status != nil {
		return f.status, nil
	}
	return &pbv2.StatusResponse{}, nil
}

func (f *adapterFakeKV) TransferLeader(ctx context.Context, _ *pbv2.TransferLeaderRequest) (*pbv2.TransferLeaderResponse, error) {
	if err := f.begin(ctx, pbv2.KVService_TransferLeader_FullMethodName, true); err != nil {
		return nil, err
	}
	if f.transfer != nil {
		return f.transfer, nil
	}
	return &pbv2.TransferLeaderResponse{Success: true}, nil
}

func (f *adapterFakeKV) PromoteReplica(ctx context.Context, _ *pbv2.PromoteReplicaRequest) (*pbv2.MembershipResponse, error) {
	if err := f.begin(ctx, pbv2.KVService_PromoteReplica_FullMethodName, true); err != nil {
		return nil, err
	}
	if f.promote != nil {
		return f.promote, nil
	}
	return &pbv2.MembershipResponse{Success: true}, nil
}

func (f *adapterFakeKV) RemoveVoter(ctx context.Context, _ *pbv2.RemoveVoterRequest) (*pbv2.MembershipResponse, error) {
	if err := f.begin(ctx, pbv2.KVService_RemoveVoter_FullMethodName, true); err != nil {
		return nil, err
	}
	if f.remove != nil {
		return f.remove, nil
	}
	return &pbv2.MembershipResponse{Success: true}, nil
}

func startAdapterFakeKV(t *testing.T, token string) (string, *adapterFakeKV) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &adapterFakeKV{
		token: token,
		calls: make(map[string]int),
		auth:  make(map[string][]string),
	}
	srv := grpc.NewServer()
	pbv2.RegisterKVServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fake
}

func TestRPCDecommissionControlMapsRPCsAndToken(t *testing.T) {
	const token = "adapter-token"
	addr, fake := startAdapterFakeKV(t, token)
	fake.status = &pbv2.StatusResponse{
		Id: "kv-a", Role: "leader", Leader: "kv-a", Term: 4,
		CommitIndex: 20, LastApplied: 19, Epoch: 7,
		Voters: []string{"kv-a", "kv-b"}, Learners: []string{"kv-c"}, Learner: false,
	}

	dialer, err := newKVAdminDialer(Config{})
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	defer dialer.Close()
	ctrl := &rpcDecommissionControl{dial: dialer.Dial, token: token}
	ctx := context.Background()

	st, err := ctrl.Status(ctx, 1, addr)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ID != "kv-a" || st.Role != raft.Leader || st.Leader != "kv-a" || st.Term != 4 ||
		st.CommitIndex != 20 || st.LastApplied != 19 || st.LastIndex != 20 ||
		st.ConfigEpoch != 7 || st.Learner ||
		strings.Join(st.Voters, ",") != "kv-a,kv-b" || strings.Join(st.Learners, ",") != "kv-c" {
		t.Fatalf("Status mapping = %+v", st)
	}
	if _, auth := fake.got(pbv2.KVService_Status_FullMethodName); len(auth) != 0 {
		t.Fatalf("Status sent authorization %v, want none", auth)
	}

	for name, call := range map[string]func() error{
		"transfer": func() error { return ctrl.TransferLeadership(ctx, 1, addr, "kv-b") },
		"promote":  func() error { return ctrl.PromoteLearner(ctx, 1, addr, "kv-c") },
		"remove":   func() error { return ctrl.RemoveVoter(ctx, 1, addr, "kv-b") },
	} {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, method := range []string{
		pbv2.KVService_TransferLeader_FullMethodName,
		pbv2.KVService_PromoteReplica_FullMethodName,
		pbv2.KVService_RemoveVoter_FullMethodName,
	} {
		n, auth := fake.got(method)
		if n != 1 {
			t.Fatalf("%s calls = %d, want 1", method, n)
		}
		if len(auth) != 1 || auth[0] != "Bearer "+token {
			t.Fatalf("%s authorization = %v, want the Bearer token", method, auth)
		}
	}
}

func TestRPCDecommissionControlRejectsWrongToken(t *testing.T) {
	const token = "adapter-correct-token"
	addr, _ := startAdapterFakeKV(t, token)
	dialer, err := newKVAdminDialer(Config{})
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	defer dialer.Close()
	ctrl := &rpcDecommissionControl{dial: dialer.Dial, token: "wrong-token"}
	err = ctrl.RemoveVoter(context.Background(), 1, addr, "kv-b")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("RemoveVoter with wrong token = %v, want Unauthenticated", err)
	}
	if strings.Contains(err.Error(), "wrong-token") || strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked a token: %v", err)
	}
}

func TestDecommissionRPCErrorSentinel(t *testing.T) {
	cases := []struct {
		msg  string
		want error
	}{
		{"not leader", raft.ErrNotLeader},
		{"unknown transfer target", raft.ErrUnknownTarget},
		{"target not caught up", raft.ErrTargetNotCaughtUp},
		{"not a voter", raft.ErrNotVoter},
		{"not a learner", raft.ErrNotLearner},
		{"already a voter", raft.ErrAlreadyVoter},
		{"cannot remove the last voter", raft.ErrUnsafeConfigChange},
		{"internal error", nil},
	}
	for _, tc := range cases {
		got := decommissionRPCErrorSentinel(tc.msg)
		if tc.want == nil {
			if got != nil {
				t.Fatalf("%q => %v, want nil", tc.msg, got)
			}
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Fatalf("%q => %v, want %v", tc.msg, got, tc.want)
		}
	}
}
