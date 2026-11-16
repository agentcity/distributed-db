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
