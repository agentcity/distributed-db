package meta_test

import (
	"testing"

	"distributed-db/internal/meta"
)

func TestExportRestoreRoundTrip(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges([]meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"n1", "n2", "n3"}},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{"n4", "n5", "n6"}},
	}); err != nil {
		t.Fatalf("set ranges: %v", err)
	}
	start, n := c.AllocTS(5)
	if n != 5 {
		t.Fatalf("AllocTS count = %d", n)
	}
	c.AdvanceTS(start + 4)
	state := c.Export()

	other := meta.New()
	if err := other.Restore(state); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := other.Export()
	if len(got.Ranges) != 2 || got.LastTS != state.LastTS || got.NextID != state.NextID {
		t.Fatalf("restored state = %+v, want %+v", got, state)
	}
	next, _ := other.AllocTS(1)
	if next <= start+4 {
		t.Fatalf("AllocTS after restore = %d, want > %d", next, start+4)
	}
}

func TestAdvanceTSNeverGoesBackwards(t *testing.T) {
	c := meta.New()
	c.AdvanceTS(100)
	c.AdvanceTS(50)
	if got := c.Export().LastTS; got != 100 {
		t.Fatalf("LastTS = %d, want 100", got)
	}
}

func TestRestoreLeaderEpoch(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges([]meta.Range{{
		ID: 1, Replicas: []string{"n1", "n2"}, Version: 1,
	}}); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}

	other := meta.New()
	if err := other.Restore(c.Export()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	r, err := other.RangeForKey(nil)
	if err != nil {
		t.Fatalf("RangeForKey: %v", err)
	}
	if r.Leader != "n1" || r.LeaderEpoch != 1 || r.LeaderStale() {
		t.Fatalf("restored range = %+v", r)
	}

	// A snapshot whose hint epoch predates its descriptor version is stale
	// and must not be restored as a current leader.
	state := c.Export()
	state.Ranges[0].Version = 2
	if err := other.Restore(state); err != nil {
		t.Fatalf("Restore stale: %v", err)
	}
	r, err = other.RangeForKey(nil)
	if err != nil {
		t.Fatalf("RangeForKey after restore: %v", err)
	}
	if r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("stale snapshot restored hint %+v", r)
	}
}
