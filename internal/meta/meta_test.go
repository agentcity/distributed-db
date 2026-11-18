package meta_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"distributed-db/internal/meta"
)

func metaTestMustRange(t *testing.T, c *meta.Cluster, key []byte) *meta.Range {
	t.Helper()
	r, err := c.RangeForKey(key)
	if err != nil {
		t.Fatalf("RangeForKey(%q): %v", key, err)
	}
	return r
}

func metaTestMustSplit(t *testing.T, c *meta.Cluster, id uint64, at string) (*meta.Range, *meta.Range) {
	t.Helper()
	left, right, err := c.Split(id, []byte(at))
	if err != nil {
		t.Fatalf("Split(%d, %q): %v", id, at, err)
	}
	return left, right
}

func metaTestCheckContiguous(t *testing.T, ranges []meta.Range) {
	t.Helper()
	if len(ranges) == 0 {
		t.Fatal("empty range map")
	}
	if ranges[0].Start != nil {
		t.Fatalf("first range starts at %q, want -inf", ranges[0].Start)
	}
	for i := 1; i < len(ranges); i++ {
		if !bytes.Equal(ranges[i-1].End, ranges[i].Start) {
			t.Fatalf("gap/overlap between range %d end %q and range %d start %q",
				i-1, ranges[i-1].End, i, ranges[i].Start)
		}
		if bytes.Compare(ranges[i-1].Start, ranges[i].Start) >= 0 {
			t.Fatalf("ranges not sorted at index %d: %q then %q", i, ranges[i-1].Start, ranges[i].Start)
		}
	}
	if ranges[len(ranges)-1].End != nil {
		t.Fatalf("last range ends at %q, want +inf", ranges[len(ranges)-1].End)
	}
}

func TestMetaNewInitialRange(t *testing.T) {
	c := meta.New()
	snap := c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() has %d ranges, want 1", len(snap))
	}
	r := snap[0]
	if r.ID != 1 {
		t.Fatalf("initial range ID = %d, want 1", r.ID)
	}
	if r.Start != nil || r.End != nil {
		t.Fatalf("initial range = [%q, %q), want [nil, nil)", r.Start, r.End)
	}
	if r.Version != 1 {
		t.Fatalf("initial range version = %d, want 1", r.Version)
	}
	if len(r.Replicas) != 0 || r.Leader != "" {
		t.Fatalf("initial placement = replicas %v leader %q, want empty", r.Replicas, r.Leader)
	}

	for _, key := range [][]byte{nil, {}, []byte("a"), {0}, {0xff}} {
		if got := metaTestMustRange(t, c, key); got.ID != 1 {
			t.Fatalf("RangeForKey(%q).ID = %d, want 1", key, got.ID)
		}
	}
}

func TestMetaRangeForKeyBoundaries(t *testing.T) {
	c := meta.New()
	_, mid := metaTestMustSplit(t, c, 1, "m")
	_, last := metaTestMustSplit(t, c, mid.ID, "t")

	tests := []struct {
		key  []byte
		want uint64
	}{
		{nil, 1},
		{[]byte{}, 1},
		{[]byte("a"), 1},
		{[]byte("l\xff"), 1},
		{[]byte("m"), mid.ID},
		{[]byte("m\x00"), mid.ID},
		{[]byte("s"), mid.ID},
		{[]byte("t"), last.ID},
		{[]byte("t\x00"), last.ID},
		{[]byte("zzz"), last.ID},
		{[]byte{0xff}, last.ID},
	}
	for _, tt := range tests {
		if got := metaTestMustRange(t, c, tt.key); got.ID != tt.want {
			t.Errorf("RangeForKey(%q).ID = %d, want %d", tt.key, got.ID, tt.want)
		}
	}

	metaTestCheckContiguous(t, c.Snapshot())
}

func TestMetaRangeForKeyByteOrder(t *testing.T) {
	c := meta.New()
	_, high := metaTestMustSplit(t, c, 1, string([]byte{0x80}))

	tests := []struct {
		key  []byte
		want uint64
	}{
		{nil, 1},
		{[]byte{}, 1},
		{[]byte{0x00}, 1},
		{[]byte{0x7f, 0xff}, 1},
		{[]byte{0x80}, high.ID},
		{[]byte{0x80, 0x00}, high.ID},
		{[]byte{0xff}, high.ID},
	}
	for _, tt := range tests {
		if got := metaTestMustRange(t, c, tt.key); got.ID != tt.want {
			t.Errorf("RangeForKey(%x).ID = %d, want %d", tt.key, got.ID, tt.want)
		}
	}
}

func TestMetaSplit(t *testing.T) {
	c := meta.New()
	before := c.Snapshot()[0]

	at := []byte("m")
	left, right, err := c.Split(1, at)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if left.ID != before.ID {
		t.Errorf("left range ID = %d, want original ID %d", left.ID, before.ID)
	}
	if right.ID == left.ID || right.ID == 0 {
		t.Errorf("right range ID = %d, want a fresh non-zero ID", right.ID)
	}
	if left.Version != before.Version+1 || right.Version != before.Version+1 {
		t.Errorf("split versions = %d/%d, want %d", left.Version, right.Version, before.Version+1)
	}
	if left.Start != nil || !bytes.Equal(left.End, []byte("m")) {
		t.Errorf("left = [%q, %q), want [nil, m)", left.Start, left.End)
	}
	if !bytes.Equal(right.Start, []byte("m")) || right.End != nil {
		t.Errorf("right = [%q, %q), want [m, nil)", right.Start, right.End)
	}
	if !bytes.Equal(metaTestMustRange(t, c, []byte("m")).Start, []byte("m")) {
		t.Error("lookup at split point did not land on the right range")
	}
	if got := metaTestMustRange(t, c, []byte("a")).ID; got != left.ID {
		t.Errorf("key before split point maps to range %d, want %d", got, left.ID)
	}
	if got := metaTestMustRange(t, c, []byte("m")).ID; got != right.ID {
		t.Errorf("key at split point maps to range %d, want %d", got, right.ID)
	}
	metaTestCheckContiguous(t, c.Snapshot())

	// Split must copy the caller's boundary slice.
	at[0] = 'z'
	snap := c.Snapshot()
	if !bytes.Equal(snap[0].End, []byte("m")) || !bytes.Equal(snap[1].Start, []byte("m")) {
		t.Fatalf("Split retained caller slice: boundaries are %q and %q", snap[0].End, snap[1].Start)
	}

	_, third := metaTestMustSplit(t, c, right.ID, "t")
	if third.ID == right.ID || third.ID == left.ID {
		t.Fatalf("third range ID %d collides with existing IDs", third.ID)
	}
	metaTestCheckContiguous(t, c.Snapshot())
}

func TestMetaSplitInvalid(t *testing.T) {
	c := meta.New()
	left, right := metaTestMustSplit(t, c, 1, "m")
	before := c.Snapshot()

	tests := []struct {
		name string
		id   uint64
		at   []byte
	}{
		{"unknown range", 999, []byte("a")},
		{"at nil start", left.ID, nil},
		{"at empty", left.ID, []byte{}},
		{"at start", left.ID, []byte("m")},
		{"beyond end", left.ID, []byte("z")},
		{"at right start", right.ID, []byte("m")},
		{"at right start empty", right.ID, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, r, err := c.Split(tt.id, tt.at)
			if err == nil {
				t.Fatalf("Split(%d, %q) = %+v, %+v, nil; want error", tt.id, tt.at, l, r)
			}
			if l != nil || r != nil {
				t.Fatalf("failed Split returned ranges: %+v, %+v", l, r)
			}
		})
	}

	after := c.Snapshot()
	if len(after) != len(before) {
		t.Fatalf("failed splits changed range count from %d to %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || !bytes.Equal(before[i].Start, after[i].Start) ||
			!bytes.Equal(before[i].End, after[i].End) {
			t.Fatalf("failed split mutated range %d", i)
		}
	}
}

func TestMetaMerge(t *testing.T) {
	c := meta.New()
	_, mid := metaTestMustSplit(t, c, 1, "m")
	_, last := metaTestMustSplit(t, c, mid.ID, "t")

	merged, err := c.Merge(mid.ID, last.ID)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.ID != mid.ID {
		t.Errorf("merged ID = %d, want left ID %d", merged.ID, mid.ID)
	}
	if !bytes.Equal(merged.Start, []byte("m")) || merged.End != nil {
		t.Errorf("merged = [%q, %q), want [m, nil)", merged.Start, merged.End)
	}
	if merged.Version <= mid.Version || merged.Version <= last.Version {
		t.Errorf("merged version = %d, want greater than %d/%d", merged.Version, mid.Version, last.Version)
	}
	if got := metaTestMustRange(t, c, []byte("m")).ID; got != merged.ID {
		t.Errorf("lookup m = %d, want %d", got, merged.ID)
	}
	if got := metaTestMustRange(t, c, []byte("zzz")).ID; got != merged.ID {
		t.Errorf("lookup zzz = %d, want %d", got, merged.ID)
	}
	metaTestCheckContiguous(t, c.Snapshot())
	if len(c.Snapshot()) != 2 {
		t.Fatalf("range count after merge = %d, want 2", len(c.Snapshot()))
	}

	whole, err := c.Merge(1, merged.ID)
	if err != nil {
		t.Fatalf("Merge whole keyspace: %v", err)
	}
	if whole.ID != 1 || whole.Start != nil || whole.End != nil {
		t.Fatalf("whole merge = %+v, want ID 1 [nil, nil)", whole)
	}
	if whole.Version <= merged.Version {
		t.Errorf("whole merge version = %d, want > %d", whole.Version, merged.Version)
	}
	if len(c.Snapshot()) != 1 {
		t.Fatalf("range count after full merge = %d, want 1", len(c.Snapshot()))
	}
	if got := metaTestMustRange(t, c, []byte("anything")).ID; got != 1 {
		t.Errorf("lookup after full merge = %d, want 1", got)
	}
}

func TestMetaMergeInvalid(t *testing.T) {
	c := meta.New()
	_, mid := metaTestMustSplit(t, c, 1, "m")
	_, last := metaTestMustSplit(t, c, mid.ID, "z")
	before := c.Snapshot()

	tests := []struct {
		name        string
		left, right uint64
	}{
		{"self", mid.ID, mid.ID},
		{"unknown left", 999, mid.ID},
		{"unknown right", mid.ID, 999},
		{"reversed", last.ID, mid.ID},
		{"not adjacent", 1, last.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := c.Merge(tt.left, tt.right); err == nil {
				t.Fatalf("Merge(%d, %d) = %+v, nil; want error", tt.left, tt.right, got)
			}
		})
	}

	after := c.Snapshot()
	if len(after) != len(before) {
		t.Fatalf("failed merge changed range count from %d to %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || !bytes.Equal(before[i].Start, after[i].Start) ||
			!bytes.Equal(before[i].End, after[i].End) {
			t.Fatalf("failed merge mutated range %d", i)
		}
	}
}

func TestMetaUpdateLeader(t *testing.T) {
	c := meta.New()
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}
	if got := metaTestMustRange(t, c, nil).Leader; got != "n1" {
		t.Fatalf("leader = %q, want n1", got)
	}
	if got := c.Snapshot()[0].Leader; got != "n1" {
		t.Fatalf("snapshot leader = %q, want n1", got)
	}

	_, right := metaTestMustSplit(t, c, 1, "m")
	if err := c.UpdateLeader(right.ID, "n2"); err != nil {
		t.Fatalf("UpdateLeader new range: %v", err)
	}
	if got := metaTestMustRange(t, c, []byte("m")).Leader; got != "n2" {
		t.Fatalf("right leader = %q, want n2", got)
	}
	if got := metaTestMustRange(t, c, []byte("a")).Leader; got != "n1" {
		t.Fatalf("left leader = %q, want n1", got)
	}

	if err := c.UpdateLeader(999, "n3"); err == nil {
		t.Fatal("UpdateLeader for unknown range returned nil error")
	}
	if err := c.UpdateLeader(right.ID, ""); err != nil {
		t.Fatalf("UpdateLeader clear: %v", err)
	}
	if got := metaTestMustRange(t, c, []byte("m")).Leader; got != "" {
		t.Fatalf("cleared leader = %q, want empty", got)
	}
}

func TestMetaSnapshotDeepCopy(t *testing.T) {
	c := meta.New()
	_, right := metaTestMustSplit(t, c, 1, "m")
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}
	_ = right

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot length = %d, want 2", len(snap))
	}
	snap[0].ID = 12345
	snap[0].Start = []byte("hacked")
	snap[0].End = []byte("hacked")
	snap[0].Leader = "evil"
	snap[0].Version = 999
	snap[1].Start[0] = 'X'
	snap[1].End = []byte("end")
	snap[1].Replicas = append(snap[1].Replicas, "evil")
	snap[1].Leader = "evil"
	snap = append(snap, meta.Range{ID: 777})

	fresh := c.Snapshot()
	if len(fresh) != 2 {
		t.Fatalf("fresh Snapshot length = %d, want 2", len(fresh))
	}
	if fresh[0].ID != 1 || fresh[0].Start != nil || !bytes.Equal(fresh[0].End, []byte("m")) ||
		fresh[0].Leader != "n1" || fresh[0].Version != 2 {
		t.Fatalf("first range mutated through snapshot: %+v", fresh[0])
	}
	if !bytes.Equal(fresh[1].Start, []byte("m")) || fresh[1].End != nil {
		t.Fatalf("second range bounds mutated through snapshot: [%q, %q)", fresh[1].Start, fresh[1].End)
	}
	if fresh[1].Leader != "" || len(fresh[1].Replicas) != 0 {
		t.Fatalf("second range placement mutated through snapshot: %+v", fresh[1])
	}

	// A range returned by RangeForKey is a copy as well.
	got := metaTestMustRange(t, c, []byte("m"))
	got.Start[0] = 'Y'
	got.Leader = "evil"
	if again := metaTestMustRange(t, c, []byte("m")); !bytes.Equal(again.Start, []byte("m")) || again.Leader != "" {
		t.Fatalf("RangeForKey exposed internal state: %+v", again)
	}
}

func TestMetaSnapshotSorted(t *testing.T) {
	c := meta.New()
	_, mid := metaTestMustSplit(t, c, 1, "m")
	metaTestMustSplit(t, c, mid.ID, "t")
	metaTestMustSplit(t, c, 1, "f")

	snap := c.Snapshot()
	metaTestCheckContiguous(t, snap)
	if len(snap) != 4 {
		t.Fatalf("range count = %d, want 4", len(snap))
	}
	want := []string{"", "f", "m", "t"}
	for i, r := range snap {
		if string(r.Start) != want[i] {
			t.Errorf("range %d starts at %q, want %q", i, r.Start, want[i])
		}
	}
}

func TestMetaAllocIDUniqueConcurrent(t *testing.T) {
	c := meta.New()
	const workers = 8
	const perWorker = 1000

	ids := make(chan uint64, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ids <- c.AllocID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[uint64]struct{}, workers*perWorker)
	for id := range ids {
		if id == 0 || id == 1 {
			t.Fatalf("AllocID returned reserved ID %d", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("unique IDs = %d, want %d", len(seen), workers*perWorker)
	}
}

func TestMetaHeartbeat(t *testing.T) {
	c := meta.New()
	if err := c.Heartbeat("n1", []uint64{1}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := c.Heartbeat("n1", nil); err != nil {
		t.Fatalf("Heartbeat empty ranges: %v", err)
	}
	if err := c.Heartbeat("", []uint64{1}); err == nil {
		t.Fatal("Heartbeat with empty node returned nil error")
	}
	if err := c.Heartbeat("n1", []uint64{1, 999}); err == nil {
		t.Fatal("Heartbeat with unknown range returned nil error")
	}

	_, right := metaTestMustSplit(t, c, 1, "m")
	if err := c.Heartbeat("n2", []uint64{1, right.ID}); err != nil {
		t.Fatalf("Heartbeat after split: %v", err)
	}
	if _, err := c.Merge(1, right.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := c.Heartbeat("n2", []uint64{right.ID}); err == nil {
		t.Fatal("Heartbeat for merged-away range returned nil error")
	}
}

func TestMetaConcurrentAccess(t *testing.T) {
	c := meta.New()
	left, right := metaTestMustSplit(t, c, 1, "m")
	keys := [][]byte{nil, {}, []byte("a"), []byte("m"), []byte("z"), {0xff}}

	const workers = 8
	const perWorker = 200
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if id := c.AllocID(); id <= 1 {
					errCh <- fmt.Errorf("bad allocated ID %d", id)
					return
				}
				start, count := c.AllocTS(i%3 + 1)
				if count <= 0 || start == 0 {
					errCh <- fmt.Errorf("bad TSO reservation (%d, %d)", start, count)
					return
				}
				if _, err := c.RangeForKey(keys[i%len(keys)]); err != nil {
					errCh <- err
					return
				}
				_ = c.Snapshot()
				if err := c.UpdateLeader(left.ID, "n1"); err != nil {
					errCh <- err
					return
				}
				if err := c.Heartbeat("n1", []uint64{left.ID, right.ID}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestSetRangesBootstrap(t *testing.T) {
	c := meta.New()
	err := c.SetRanges([]meta.Range{
		{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"n1", "n2", "n3"}},
		{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{"n4", "n5", "n6"}},
	})
	if err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	r, err := c.RangeForKey([]byte("a"))
	if err != nil || r.ID != 1 {
		t.Fatalf("RangeForKey(a) = %+v, %v", r, err)
	}
	r, err = c.RangeForKey([]byte("z"))
	if err != nil || r.ID != 2 {
		t.Fatalf("RangeForKey(z) = %+v, %v", r, err)
	}
	if got := c.AllocID(); got <= 2 {
		t.Fatalf("AllocID after SetRanges = %d, want > 2", got)
	}
}

func TestSetRangesRejectsGapsAndDuplicates(t *testing.T) {
	cases := [][]meta.Range{
		{{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"n1"}}},
		{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"n1"}},
			{ID: 2, Start: []byte("n"), End: nil, Replicas: []string{"n2"}},
		},
		{
			{ID: 1, Start: nil, End: []byte("m"), Replicas: []string{"n1"}},
			{ID: 1, Start: []byte("m"), End: nil, Replicas: []string{"n2"}},
		},
		{
			{ID: 1, Start: nil, End: []byte("m")},
			{ID: 2, Start: []byte("m"), End: nil, Replicas: []string{"n2"}},
		},
	}
	for i, tc := range cases {
		if err := meta.New().SetRanges(tc); err == nil {
			t.Errorf("case %d: SetRanges accepted invalid map", i)
		}
	}
}

func metaTestLeaderRanges() []meta.Range {
	return []meta.Range{{
		ID: 1, Start: nil, End: nil,
		Replicas: []string{"n1", "n2", "n3"}, Version: 1,
	}}
}

func TestMetaRefreshLeaderEpoch(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges(metaTestLeaderRanges()); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}

	if err := c.RefreshLeader(1, "n1", 1); err != nil {
		t.Fatalf("RefreshLeader: %v", err)
	}
	r := metaTestMustRange(t, c, nil)
	if r.Leader != "n1" || r.LeaderEpoch != 1 || !r.LeaderKnown() || r.LeaderStale() {
		t.Fatalf("range after refresh = %+v", r)
	}

	// Epoch 0 means "no epoch assertion" and is accepted conservatively.
	if err := c.RefreshLeader(1, "n2", 0); err != nil {
		t.Fatalf("RefreshLeader zero epoch: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "n2" || r.LeaderEpoch != 1 {
		t.Fatalf("zero epoch refresh = %+v", r)
	}

	// An epoch that does not match the descriptor is stale. The previous hint
	// is cleared rather than published as current.
	err := c.RefreshLeader(1, "n1", 2)
	if !errors.Is(err, meta.ErrStaleLeader) {
		t.Fatalf("RefreshLeader error = %v, want ErrStaleLeader", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("stale refresh left hint %+v, want cleared", r)
	}

	// A stale epoch must clear an existing hint even when a leader is passed.
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}
	if err := c.RefreshLeader(1, "n2", 99); !errors.Is(err, meta.ErrStaleLeader) {
		t.Fatalf("RefreshLeader stale error = %v, want ErrStaleLeader", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("stale epoch kept hint %+v, want cleared", r)
	}

	if err := c.RefreshLeader(1, "n2", 1); err != nil {
		t.Fatalf("RefreshLeader current epoch: %v", err)
	}
	if err := c.ClearLeader(1); err != nil {
		t.Fatalf("ClearLeader: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("ClearLeader left hint %+v", r)
	}
	if err := c.RefreshLeader(999, "n1", 1); err == nil {
		t.Fatal("RefreshLeader unknown range returned nil error")
	}
}

func TestMetaSetRangesClearsStaleLeader(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges(metaTestLeaderRanges()); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}

	// Omitting the leader on an epoch change clears the old hint.
	ranges := metaTestLeaderRanges()
	ranges[0].Version = 2
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges epoch change: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("epoch change kept stale hint %+v", r)
	}

	// A hint from the previous epoch is cleared even when it names the same
	// leader, unless the caller explicitly re-stamps it.
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}
	ranges = metaTestLeaderRanges()
	ranges[0].Version = 3
	ranges[0].Leader = "n1"
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges unstamped same leader: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("unstamped hint survived epoch change: %+v", r)
	}

	// An explicit stale epoch is also rejected and cleared.
	if err := c.UpdateLeader(1, "n2"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}
	ranges = metaTestLeaderRanges()
	ranges[0].Version = 4
	ranges[0].Leader = "n2"
	ranges[0].LeaderEpoch = 3
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges explicit stale epoch: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("explicit stale epoch kept hint %+v", r)
	}
}

func TestMetaSetRangesPreservesLiveLeader(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges(metaTestLeaderRanges()); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}

	// A read-modify-write round trip that omits Leader on the same version
	// must not lose the hint.
	ranges := metaTestLeaderRanges()
	ranges[0].Leader = ""
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges round trip: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "n1" || r.LeaderEpoch != 1 {
		t.Fatalf("round trip lost hint: %+v", r)
	}

	// A same-version update preserves or replaces the hint without needing an
	// explicit epoch.
	ranges = metaTestLeaderRanges()
	ranges[0].Leader = "n2"
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges same version: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "n2" || r.LeaderEpoch != 1 {
		t.Fatalf("same version hint = %+v", r)
	}

	// A group-preserving membership change carries the same leader forward at
	// the new version only when it re-stamps the hint at that version.
	ranges = metaTestLeaderRanges()
	ranges[0].Leader = "n2"
	ranges[0].LeaderEpoch = 2
	ranges[0].Version = 2
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges same leader new version: %v", err)
	}
	r := metaTestMustRange(t, c, nil)
	if r.Leader != "n2" || r.LeaderEpoch != 2 || r.LeaderStale() {
		t.Fatalf("same leader across version = %+v", r)
	}

	// A leader that is no longer a replica is cleared.
	ranges = metaTestLeaderRanges()
	ranges[0].Replicas = []string{"n2", "n3"}
	ranges[0].Leader = "n1"
	ranges[0].Version = 3
	if err := c.SetRanges(ranges); err != nil {
		t.Fatalf("SetRanges leader removed: %v", err)
	}
	if r := metaTestMustRange(t, c, nil); r.Leader != "" || r.LeaderEpoch != 0 {
		t.Fatalf("non-replica leader kept: %+v", r)
	}
}

func TestMetaSplitMergeLeaderHints(t *testing.T) {
	c := meta.New()
	if err := c.SetRanges(metaTestLeaderRanges()); err != nil {
		t.Fatalf("SetRanges: %v", err)
	}
	if err := c.UpdateLeader(1, "n1"); err != nil {
		t.Fatalf("UpdateLeader: %v", err)
	}

	left, right, err := c.Split(1, []byte("m"))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if left.Leader != "n1" || left.LeaderEpoch != left.Version || left.LeaderStale() {
		t.Fatalf("left hint after split = %+v", left)
	}
	if right.Leader != "" || right.LeaderEpoch != 0 {
		t.Fatalf("new right range invented leader %+v", right)
	}

	merged, err := c.Merge(1, right.ID)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Leader != "n1" || merged.LeaderEpoch != merged.Version || merged.LeaderStale() {
		t.Fatalf("merged hint = %+v", merged)
	}
}

func TestMetaRangeHealth(t *testing.T) {
	r := meta.Range{
		ID: 1, Replicas: []string{"n1", "n2"}, Leader: "n1",
		LeaderEpoch: 7, Version: 7,
	}
	h := r.Health()
	if h.ReplicaCount != 2 || !h.LeaderKnown || h.LeaderEpoch != 7 || h.Epoch != 7 || h.Stale {
		t.Fatalf("health = %+v", h)
	}
	r.Version = 8
	h = r.Health()
	if !h.Stale || h.LeaderEpoch == h.Epoch {
		t.Fatalf("stale health = %+v", h)
	}
}
