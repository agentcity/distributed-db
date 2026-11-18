package version

import (
	"strings"
	"testing"
	"time"
)

func TestRegistryRecordPreservesFirstSeenAndSorts(t *testing.T) {
	reg := NewRegistry()
	base := time.Unix(1000, 0)
	reg.now = func() time.Time { return base }

	rep, err := Evaluate(Current, CurrentFeatures, PeerInfo{Version: Version{2, 0, 0}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	reg.Record(rep.PeerRecord("b-node"))
	reg.Record(rep.PeerRecord("a-node"))

	base = base.Add(5 * time.Second)
	rep2, err := Evaluate(Current, CurrentFeatures, PeerInfo{Version: Current, Features: CurrentFeatures})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	reg.Record(rep2.PeerRecord("b-node"))

	if got := reg.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
	rec, ok := reg.Lookup("b-node")
	if !ok {
		t.Fatal("Lookup(b-node) = false")
	}
	if !rec.FirstSeen.Equal(time.Unix(1000, 0)) {
		t.Fatalf("FirstSeen = %s, want preserved 1000s", rec.FirstSeen)
	}
	if !rec.LastSeen.Equal(base) {
		t.Fatalf("LastSeen = %s, want %s", rec.LastSeen, base)
	}
	if rec.PeerVersion != Current {
		t.Fatalf("PeerVersion = %s, want current", rec.PeerVersion)
	}

	snap := reg.Snapshot()
	if len(snap) != 2 || snap[0].Addr != "a-node" || snap[1].Addr != "b-node" {
		t.Fatalf("Snapshot order = %+v, want [a-node b-node]", snap)
	}
	// Mutating the snapshot must not mutate the registry.
	snap[0].Warnings = append(snap[0].Warnings, "mutated")
	if got, _ := reg.Lookup("a-node"); strings.Contains(strings.Join(got.Warnings, "; "), "mutated") {
		t.Fatal("Snapshot aliased registry warnings")
	}

	latest, ok := reg.Latest()
	if !ok || latest.Addr != "b-node" {
		t.Fatalf("Latest() = %+v, %t; want b-node", latest, ok)
	}
}

func TestRegistryNilSafety(t *testing.T) {
	var reg *Registry
	reg.Record(PeerRecord{Addr: "x"})
	if _, ok := reg.Lookup("x"); ok {
		t.Fatal("nil Registry Lookup returned a record")
	}
	if reg.Len() != 0 {
		t.Fatal("nil Registry Len != 0")
	}
	if snap := reg.Snapshot(); snap != nil {
		t.Fatalf("nil Registry Snapshot = %+v, want nil", snap)
	}
	if _, ok := reg.Latest(); ok {
		t.Fatal("nil Registry Latest returned a record")
	}
}

func TestPeerRecordString(t *testing.T) {
	rep, err := Evaluate(Current, CurrentFeatures, PeerInfo{Version: Version{2, 0, 0}, Features: FeatureTxnAtomic})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	rec := rep.PeerRecord("peer-1")
	rec.FirstSeen = time.Unix(1, 0)
	rec.LastSeen = time.Unix(2, 0)
	got := rec.String()
	for _, want := range []string{"peer-1", Current.String(), "2.0.0", "compatible=true", "older-minor"} {
		if !strings.Contains(got, want) {
			t.Fatalf("PeerRecord.String() = %q, missing %q", got, want)
		}
	}
}
