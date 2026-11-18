package kvctl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"distributed-db/internal/backup"
	"distributed-db/internal/kvserver"
	"distributed-db/internal/meta"
	"distributed-db/internal/metaserver"
	pbv2 "distributed-db/internal/pb/v2"
	"distributed-db/internal/raft"
)

// TestE2EAdminCommands runs the admin command set against a real 3-node KV
// cluster with a 3-node replicated meta group. It is opt-in because it starts
// real Raft groups and storage engines:
//
//	KVCTL_E2E=1 go test ./internal/kvctl -run TestE2EAdminCommands -v
func TestE2EAdminCommands(t *testing.T) {
	if os.Getenv("KVCTL_E2E") == "" {
		t.Skip("set KVCTL_E2E=1 to run the 3-node admin CLI end-to-end test")
	}
	metaLeader, kvAddrs, kvServers := startE2ECluster(t)
	nodesArg := strings.Join(kvAddrs, ",")
	base := []string{"-meta", metaLeader, "-timeout", "30s"}

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		r := NewRunner(strings.NewReader(""), &out, &errOut)
		code := r.Run(context.Background(), args)
		return code, out.String(), errOut.String()
	}

	for _, kv := range [][2]string{{"a", "1"}, {"m", "2"}, {"z", "3"}} {
		args := append(append([]string{}, base...), "-nodes", nodesArg, "put", kv[0], kv[1])
		if code, out, errOut := run(args...); code != 0 {
			t.Fatalf("put %s = code %d out %q err %q", kv[0], code, out, errOut)
		}
	}

	// ranges and status must work with -meta only.
	code, out, errOut := run(append(append([]string{}, base...), "-format", "json", "ranges")...)
	if code != 0 {
		t.Fatalf("ranges = code %d err %q", code, errOut)
	}
	var ranges []struct {
		ID           uint64   `json:"id"`
		Start        string   `json:"start"`
		End          string   `json:"end"`
		Leader       string   `json:"leader"`
		LeaderKnown  bool     `json:"leader_known"`
		Replicas     []string `json:"replicas"`
		ReplicaCount int      `json:"replica_count"`
		LiveVoters   []string `json:"live_voters"`
		Version      uint64   `json:"version"`
		Epoch        uint64   `json:"epoch"`
		Stale        bool     `json:"stale"`
	}
	if err := json.Unmarshal([]byte(out), &ranges); err != nil {
		t.Fatalf("ranges json: %v (%q)", err, out)
	}
	if len(ranges) != 1 || ranges[0].ID != 1 || len(ranges[0].Replicas) != len(kvAddrs) {
		t.Fatalf("initial ranges = %s", out)
	}
	// The whole point of the health view: a live cluster must not show an
	// empty LEADER or an empty live-voter set.
	if !ranges[0].LeaderKnown || ranges[0].Leader == "" {
		t.Fatalf("initial range has no truthful leader: %s", out)
	}
	if !slices.Contains(kvAddrs, ranges[0].Leader) {
		t.Fatalf("leader %q is not a replica: %s", ranges[0].Leader, out)
	}
	if ranges[0].ReplicaCount != len(kvAddrs) || len(ranges[0].LiveVoters) == 0 ||
		ranges[0].Stale || ranges[0].Epoch == 0 {
		t.Fatalf("initial range health = %s", out)
	}

	code, out, errOut = run(append(append([]string{}, base...), "-format", "json", "status")...)
	if code != 0 {
		t.Fatalf("status = code %d err %q", code, errOut)
	}
	var status struct {
		MetaLeader string         `json:"meta_leader"`
		MetaTerm   uint64         `json:"meta_term"`
		Ranges     int            `json:"ranges"`
		Leaders    map[string]int `json:"leaders"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("status json: %v (%q)", err, out)
	}
	if status.MetaLeader != metaLeader || status.MetaTerm == 0 || status.Ranges != 1 {
		t.Fatalf("status = %s (meta leader %s)", out, metaLeader)
	}
	// Status uses the same health-enriched range list, so a live cluster must
	// report at least one range leader and never an empty key.
	if len(status.Leaders) == 0 {
		t.Fatalf("status reported no live range leaders: %s", out)
	}
	for leader := range status.Leaders {
		if leader == "" || !slices.Contains(kvAddrs, leader) {
			t.Fatalf("status leaders contain an unknown leader: %v", status.Leaders)
		}
	}

	// split, wait for the new group's election, then read across the boundary.
	code, out, errOut = run(append(append([]string{}, base...), "-format", "json", "split", "1", "m")...)
	if code != 0 {
		t.Fatalf("split = code %d err %q", code, errOut)
	}
	var split struct {
		NewRangeID uint64 `json:"new_range_id"`
	}
	if err := json.Unmarshal([]byte(out), &split); err != nil || split.NewRangeID == 0 {
		t.Fatalf("split json = %q err %v", out, err)
	}
	waitE2EGroupLeader(t, kvServers, split.NewRangeID)

	code, out, errOut = run(append(append([]string{}, base...), "-nodes", nodesArg, "get", "m")...)
	if code != 0 || out != "m\t2\n" {
		t.Fatalf("get after split = code %d out %q err %q", code, out, errOut)
	}
	code, out, _ = run(append(append([]string{}, base...), "-format", "json", "ranges")...)
	if code != 0 {
		t.Fatalf("ranges after split = code %d", code)
	}
	ranges = nil
	if err := json.Unmarshal([]byte(out), &ranges); err != nil {
		t.Fatalf("ranges json after split: %v (%q)", err, out)
	}
	if len(ranges) != 2 || ranges[0].ID != 1 || ranges[1].ID != split.NewRangeID ||
		ranges[0].End != "m" || ranges[1].Start != "m" {
		t.Fatalf("ranges after split = %s", out)
	}
	for _, r := range ranges {
		if !r.LeaderKnown || !slices.Contains(kvAddrs, r.Leader) {
			t.Fatalf("range %d has no truthful leader after split: %s", r.ID, out)
		}
	}

	// merge the right range back into the left one.
	rightID := split.NewRangeID
	code, out, errOut = run(append(append([]string{}, base...), "merge", "1", strconv.FormatUint(rightID, 10))...)
	if code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("merge = code %d out %q err %q", code, out, errOut)
	}
	code, out, _ = run(append(append([]string{}, base...), "-format", "json", "ranges")...)
	if code != 0 {
		t.Fatalf("ranges after merge = code %d", code)
	}
	ranges = nil
	if err := json.Unmarshal([]byte(out), &ranges); err != nil {
		t.Fatalf("ranges json after merge: %v (%q)", err, out)
	}
	if len(ranges) != 1 || ranges[0].ID != 1 || ranges[0].End != "" {
		t.Fatalf("ranges after merge = %s", out)
	}
	if !ranges[0].LeaderKnown || !slices.Contains(kvAddrs, ranges[0].Leader) {
		t.Fatalf("merged range has no truthful leader: %s", out)
	}

	// backup requires KV nodes and must produce a valid manifest.
	backupDir := t.TempDir() + "/backup"
	code, out, errOut = run(append(append([]string{}, base...),
		"-format", "json", "-nodes", nodesArg, "backup", "-out", backupDir)...)
	if code != 0 {
		t.Fatalf("backup = code %d err %q", code, errOut)
	}
	var bak struct {
		Dir        string `json:"dir"`
		SnapshotTS uint64 `json:"snapshot_ts"`
		Keys       int64  `json:"keys"`
		Bytes      int64  `json:"bytes"`
		Ranges     int    `json:"ranges"`
	}
	if err := json.Unmarshal([]byte(out), &bak); err != nil {
		t.Fatalf("backup json: %v (%q)", err, out)
	}
	if bak.Dir != backupDir || bak.SnapshotTS == 0 || bak.Keys < 3 || bak.Ranges != 1 {
		t.Fatalf("backup result = %s", out)
	}
	manifest, err := backup.LoadManifest(backupDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}
	if manifest.TotalKeys != bak.Keys || manifest.TotalBytes != bak.Bytes || len(manifest.Ranges) != bak.Ranges {
		t.Fatalf("manifest totals = keys %d bytes %d ranges %d; output = %s",
			manifest.TotalKeys, manifest.TotalBytes, len(manifest.Ranges), out)
	}
	if _, err := backup.Verify(context.Background(), backupDir); err != nil {
		t.Fatalf("backup verify: %v", err)
	}
}

func startE2ECluster(t *testing.T) (metaLeader string, kvAddrs []string, kvServers []*kvserver.Server) {
	t.Helper()
	ctx := context.Background()
	kvAddrs = []string{e2eFreeAddr(t), e2eFreeAddr(t), e2eFreeAddr(t)}
	metaAddrs := []string{e2eFreeAddr(t), e2eFreeAddr(t), e2eFreeAddr(t)}

	metas := make([]*metaserver.Server, 0, len(metaAddrs))
	for i, addr := range metaAddrs {
		peers := make([]string, 0, len(metaAddrs)-1)
		for j, p := range metaAddrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		ms, err := metaserver.New(metaserver.Config{
			Addr: addr, ID: addr, Peers: peers, DataDir: t.TempDir(), Replicated: true,
			Ranges:             []meta.Range{{ID: 1, Replicas: kvAddrs, Version: 1}},
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			Logger:             e2eLogger(),
		})
		if err != nil {
			t.Fatalf("new meta %s: %v", addr, err)
		}
		if err := ms.Start(ctx); err != nil {
			t.Fatalf("start meta %s: %v", addr, err)
		}
		metas = append(metas, ms)
		t.Cleanup(func() { _ = ms.Stop() })
	}
	metaLeader = waitE2EMetaLeader(t, metaAddrs)

	kvServers = make([]*kvserver.Server, 0, len(kvAddrs))
	for i, addr := range kvAddrs {
		peers := make([]string, 0, len(kvAddrs)-1)
		for j, p := range kvAddrs {
			if i != j {
				peers = append(peers, p)
			}
		}
		srv, err := kvserver.New(kvserver.Config{
			ID: addr, RaftAddr: addr, DataDir: t.TempDir(), Peers: peers, RangeID: 1,
			ElectionTimeoutMin: 120 * time.Millisecond,
			ElectionTimeoutMax: 240 * time.Millisecond,
			HeartbeatInterval:  25 * time.Millisecond,
			CacheSize:          8 << 20, MemTableSize: 4 << 20,
			Logger: e2eLogger(),
		})
		if err != nil {
			t.Fatalf("new kv %s: %v", addr, err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatalf("start kv %s: %v", addr, err)
		}
		kvServers = append(kvServers, srv)
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Stop(stopCtx)
		})
	}
	waitE2EGroupLeader(t, kvServers, 1)
	return metaLeader, kvAddrs, kvServers
}

func e2eFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func e2eLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func waitE2EMetaLeader(t *testing.T, addrs []string) string {
	t.Helper()
	clients := make([]pbv2.MetaServiceClient, 0, len(addrs))
	for _, addr := range addrs {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial meta %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		clients = append(clients, pbv2.NewMetaServiceClient(cc))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range clients {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			resp, err := c.Leader(ctx, &pbv2.LeaderRequest{})
			cancel()
			if err == nil && resp.GetLeader() != "" {
				return resp.GetLeader()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no meta leader elected among %v", addrs)
	return ""
}

func waitE2EGroupLeader(t *testing.T, servers []*kvserver.Server, rangeID uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, srv := range servers {
			node, err := srv.GroupForID(rangeID)
			if err == nil && node.Status().Role == raft.Leader {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("range %d did not get a leader", rangeID)
}
