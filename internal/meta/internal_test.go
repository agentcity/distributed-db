package meta

import (
	"testing"
	"time"
)

func TestMetaInternalMergeReplicasMustMatch(t *testing.T) {
	c := New()
	left, right, err := c.Split(1, []byte("m"))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}

	c.mu.Lock()
	c.ranges[0].Replicas = []string{"n1", "n2"}
	c.ranges[1].Replicas = []string{"n1", "n3"}
	c.mu.Unlock()

	if _, err := c.Merge(left.ID, right.ID); err == nil {
		t.Fatal("Merge with different replicas returned nil error")
	}

	c.mu.Lock()
	c.ranges[1].Replicas = []string{"n1", "n2"}
	c.mu.Unlock()
	merged, err := c.Merge(left.ID, right.ID)
	if err != nil {
		t.Fatalf("Merge with identical replicas: %v", err)
	}
	if len(merged.Replicas) != 2 || merged.Replicas[0] != "n1" || merged.Replicas[1] != "n2" {
		t.Fatalf("merged replicas = %v", merged.Replicas)
	}
}

func TestMetaInternalMergeInconsistentBoundaries(t *testing.T) {
	c := New()
	left, right, err := c.Split(1, []byte("m"))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	c.mu.Lock()
	c.ranges[1].Start = []byte("x")
	c.mu.Unlock()

	if _, err := c.Merge(left.ID, right.ID); err == nil {
		t.Fatal("Merge with inconsistent boundaries returned nil error")
	}
}

func TestMetaInternalHeartbeatRecord(t *testing.T) {
	c := New()
	ids := []uint64{1}
	before := time.Now()
	if err := c.Heartbeat("node-1", ids); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	ids[0] = 99

	c.hbMu.Lock()
	hb, ok := c.heartbeats["node-1"]
	c.hbMu.Unlock()
	if !ok {
		t.Fatal("heartbeat was not recorded")
	}
	if len(hb.ranges) != 1 || hb.ranges[0] != 1 {
		t.Fatalf("recorded ranges = %v, want [1] (caller slice must be copied)", hb.ranges)
	}
	if hb.lastSeen.Before(before) || hb.lastSeen.After(time.Now()) {
		t.Fatalf("lastSeen = %v, want between %v and now", hb.lastSeen, before)
	}
	firstSeen := hb.lastSeen

	if err := c.Heartbeat("node-1", []uint64{1, 1}); err != nil {
		t.Fatalf("second Heartbeat: %v", err)
	}
	c.hbMu.Lock()
	hb = c.heartbeats["node-1"]
	c.hbMu.Unlock()
	if len(hb.ranges) != 2 || hb.ranges[0] != 1 || hb.ranges[1] != 1 {
		t.Fatalf("updated ranges = %v, want [1 1]", hb.ranges)
	}
	if hb.lastSeen.Before(firstSeen) {
		t.Fatalf("lastSeen moved backwards: %v then %v", firstSeen, hb.lastSeen)
	}
}

func TestMetaInternalSnapshotCopiesReplicas(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.ranges[0].Replicas = []string{"n1", "n2"}
	c.mu.Unlock()

	snap := c.Snapshot()
	snap[0].Replicas[0] = "evil"
	snap[0].Replicas = append(snap[0].Replicas, "extra")

	fresh := c.Snapshot()
	if len(fresh[0].Replicas) != 2 || fresh[0].Replicas[0] != "n1" || fresh[0].Replicas[1] != "n2" {
		t.Fatalf("replicas aliased through Snapshot: %v", fresh[0].Replicas)
	}
}
