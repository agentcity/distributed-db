package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"distributed-db/internal/pb"
)

func TestStatusJSON(t *testing.T) {
	c := newTestCoordinator(t, Config{Shards: map[string][]string{
		"shard-b": {"n3:7001", "n4:7001"},
		"shard-a": {"n1:7001", "n2:7001"},
	}}, nil)

	// Seed one fresh and one expired cache entry: only the fresh one is a
	// cached leader.
	c.leadersMu.Lock()
	c.leaders["shard-a"] = cachedLeader{addr: "n1:7001", expiresAt: time.Now().Add(time.Minute)}
	c.leaders["shard-b"] = cachedLeader{addr: "n3:7001", expiresAt: time.Now().Add(-time.Minute)}
	c.leadersMu.Unlock()

	if _, err := c.ReportLoad(context.Background(), &pb.LoadReportRequest{
		NodeId: "n2:7001", Shard: "shard-a", Load: 0.75, Ops: 123,
	}); err != nil {
		t.Fatalf("ReportLoad: %v", err)
	}
	if _, err := c.ReportLoad(context.Background(), &pb.LoadReportRequest{
		NodeId: "n3:7001", Shard: "shard-b", Load: 0.25, Ops: 10,
	}); err != nil {
		t.Fatalf("ReportLoad: %v", err)
	}
	// A second report for the same node/shard replaces the first sample.
	if _, err := c.ReportLoad(context.Background(), &pb.LoadReportRequest{
		NodeId: "n3:7001", Shard: "shard-b", Load: 0.5, Ops: 20,
	}); err != nil {
		t.Fatalf("ReportLoad: %v", err)
	}

	rec := doRequest(t, c, http.MethodGet, "/v1/status", nil, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var got coordinatorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v (body %s)", err, rec.Body)
	}

	wantShards := map[string][]string{
		"shard-a": {"n1:7001", "n2:7001"},
		"shard-b": {"n3:7001", "n4:7001"},
	}
	if !reflect.DeepEqual(got.Shards, wantShards) {
		t.Fatalf("shards = %v, want %v", got.Shards, wantShards)
	}
	if !reflect.DeepEqual(got.Leaders, map[string]string{"shard-a": "n1:7001"}) {
		t.Fatalf("leaders = %v, want only fresh shard-a entry", got.Leaders)
	}

	if len(got.Loads) != 2 {
		t.Fatalf("loads = %v, want 2 samples", got.Loads)
	}
	if got.Loads[0].Node != "n2:7001" || got.Loads[0].Shard != "shard-a" || got.Loads[0].Load != 0.75 || got.Loads[0].Ops != 123 {
		t.Fatalf("unexpected first load sample: %+v", got.Loads[0])
	}
	if got.Loads[1].Node != "n3:7001" || got.Loads[1].Shard != "shard-b" || got.Loads[1].Load != 0.5 || got.Loads[1].Ops != 20 {
		t.Fatalf("unexpected second load sample: %+v", got.Loads[1])
	}
	now := time.Now()
	for _, sample := range got.Loads {
		if sample.UpdatedAt.IsZero() || now.Sub(sample.UpdatedAt) > time.Minute {
			t.Fatalf("bad sample timestamp: %v", sample.UpdatedAt)
		}
	}
}

func TestStatusEmptyCollectionsAreJSONObjects(t *testing.T) {
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, nil)
	rec := doRequest(t, c, http.MethodGet, "/v1/status", nil, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if string(raw["leaders"]) != "{}" {
		t.Fatalf("leaders = %s, want {}", raw["leaders"])
	}
	if string(raw["loads"]) != "[]" {
		t.Fatalf("loads = %s, want []", raw["loads"])
	}
}

func TestGRPCStatusAndReportLoad(t *testing.T) {
	c := newTestCoordinator(t, Config{Shards: map[string][]string{
		"shard-b": {"n3:7001"},
		"shard-a": {"n1:7001"},
	}}, nil)

	resp, err := c.Status(context.Background(), &pb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp.GetId() != "coordinator" || resp.GetRole() != "coordinator" {
		t.Fatalf("unexpected status identity: %+v", resp)
	}
	if resp.GetShard() != "shard-a,shard-b" {
		t.Fatalf("status shard = %q, want sorted names", resp.GetShard())
	}

	load, err := c.ReportLoad(context.Background(), &pb.LoadReportRequest{
		NodeId: "n1:7001", Shard: "shard-a", Load: 0.1, Ops: 1,
	})
	if err != nil || !load.GetSuccess() {
		t.Fatalf("ReportLoad: resp = %v, err = %v", load, err)
	}
	if got := c.statusSnapshot().Loads; len(got) != 1 || got[0].Node != "n1:7001" {
		t.Fatalf("stored loads = %+v", got)
	}
}

func TestGRPCGetNotFoundPassthrough(t *testing.T) {
	clients := fakeClients{
		"n1:7001": {
			status: leaderStatus("n1:7001"),
			get: func(context.Context, *pb.GetRequest) (*pb.GetResponse, error) {
				return &pb.GetResponse{Found: false, Leader: "n1:7001"}, nil
			},
		},
	}
	c := newTestCoordinator(t, Config{Shards: map[string][]string{"s": {"n1:7001"}}}, clients)
	resp, err := c.Get(context.Background(), &pb.GetRequest{Key: "missing"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.GetFound() || resp.GetError() != "" {
		t.Fatalf("unexpected Get response: %+v", resp)
	}
	if _, err := c.Get(context.Background(), &pb.GetRequest{}); err == nil {
		t.Fatal("Get with empty key should fail")
	}
}
