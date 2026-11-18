// Package meta implements the cluster metadata service: the range map,
// placement information and the timestamp oracle (TSO) used by the v2 KV
// layer.
//
// The range map is a sorted, contiguous partition of the key space. The first
// range starts at -inf (a nil Start) and the last ends at +inf (a nil End).
// Range boundaries are compared lexicographically as raw bytes.
package meta

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// ErrStaleLeader is returned by RefreshLeader when the caller's epoch does not
// match the range's current descriptor version. The stale hint is cleared
// before the error is returned, so callers may safely ignore the error when
// leader discovery is best-effort.
var ErrStaleLeader = errors.New("meta: stale leader epoch")

// Range describes one contiguous slice of the key space [Start, End).
//
// A nil Start means -inf and a nil End means +inf. Replicas is the placement
// for the range and Leader is the node currently serving it. Version is the
// range descriptor version; Split and Merge increment it.
//
// Leader is a hint, not a safety-critical part of the range map: it is learned
// from a live KV Status/leader redirect and may lag the actual Raft leader.
// LeaderEpoch records the descriptor Version at which the hint was learned. An
// empty Leader means "unknown" and is never invented. A hint whose epoch no
// longer matches the current Version is stale: RefreshLeader and SetRanges
// clear it instead of carrying it forward, while UpdateLeader re-stamps it at
// the current Version because the caller just observed a live leader.
type Range struct {
	ID       uint64
	Start    []byte // inclusive; nil means -inf
	End      []byte // exclusive; nil means +inf
	Replicas []string
	Leader   string
	// LeaderEpoch is the Version at which Leader was learned. Zero means the
	// hint predates epochs (or no leader is known).
	LeaderEpoch uint64 `json:"leader_epoch,omitempty"`
	Version     uint64
}

// LeaderKnown reports whether a leader hint is recorded. It never probes the
// KV replicas; use a liveness probe for a verified answer.
func (r Range) LeaderKnown() bool { return r.Leader != "" }

// LeaderStale reports whether the recorded hint was learned for an older
// descriptor version. Such a hint must not be presented as the current leader.
func (r Range) LeaderStale() bool {
	return r.Leader != "" && r.LeaderEpoch != 0 && r.LeaderEpoch != r.Version
}

// RangeHealth is the metadata-side health view of a range. LiveVoters is not
// populated here because liveness requires probing the KV replicas; callers
// that have a live view (for example v2client) fill it in.
type RangeHealth struct {
	ReplicaCount int      `json:"replica_count"`
	LiveVoters   []string `json:"live_voters,omitempty"`
	LeaderKnown  bool     `json:"leader_known"`
	LeaderEpoch  uint64   `json:"leader_epoch,omitempty"`
	Epoch        uint64   `json:"epoch"`
	Stale        bool     `json:"stale"`
}

// Health returns the metadata-only health view of r. It is cheap and does not
// contact any replica.
func (r Range) Health() RangeHealth {
	return RangeHealth{
		ReplicaCount: len(r.Replicas),
		LeaderKnown:  r.LeaderKnown(),
		LeaderEpoch:  r.LeaderEpoch,
		Epoch:        r.Version,
		Stale:        r.LeaderStale(),
	}
}

// Cluster is an in-memory range map, placement table and TSO. It is safe for
// concurrent use.
type Cluster struct {
	mu     sync.RWMutex
	ranges []*Range

	nextID atomic.Uint64

	tsMu         sync.Mutex
	lastTS       uint64
	lastPhysical uint64
	lastLogical  uint64

	hbMu       sync.Mutex
	heartbeats map[string]heartbeat
}

// New returns a cluster with a single range [nil, nil) with ID 1.
func New() *Cluster {
	c := &Cluster{
		ranges: []*Range{{
			ID:      1,
			Start:   nil,
			End:     nil,
			Version: 1,
		}},
		heartbeats: make(map[string]heartbeat),
	}
	// ID 1 belongs to the initial range; hand out IDs starting at 2.
	c.nextID.Store(1)
	return c
}

// SetRanges replaces the whole range map with a validated, sorted,
// contiguous set. It is used by the metadata bootstrap and by tests.
//
// Leader hints are preserved only while they remain credible. A hint is
// dropped when:
//   - it is not a member of the incoming replica set;
//   - the caller attaches an explicit LeaderEpoch that does not match the
//     incoming Version (a stale hint);
//   - the descriptor Version changes and the hint was not explicitly stamped
//     at the new Version by setting LeaderEpoch to that Version.
//
// This makes an epoch change a default invalidation: a group-preserving
// operation that knows leadership did not move (split left, merge left,
// AddReplica/RemoveReplica on a non-leader) must set LeaderEpoch to the new
// Version to carry the hint forward. An empty incoming Leader on an unchanged
// Version preserves the existing hint so a read-modify-write round trip does
// not lose it.
func (c *Cluster) SetRanges(ranges []Range) error {
	if len(ranges) == 0 {
		return fmt.Errorf("meta: SetRanges requires at least one range")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	oldByID := make(map[uint64]Range, len(c.ranges))
	for _, r := range c.ranges {
		oldByID[r.ID] = *r
	}

	seen := make(map[uint64]struct{}, len(ranges))
	out := make([]*Range, 0, len(ranges))
	var maxID uint64
	for i := range ranges {
		r := ranges[i]
		if r.ID == 0 {
			return fmt.Errorf("meta: range %d has zero id", i)
		}
		if _, ok := seen[r.ID]; ok {
			return fmt.Errorf("meta: duplicate range id %d", r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Version == 0 {
			r.Version = 1
		}
		if i == 0 {
			if r.Start != nil && len(r.Start) != 0 {
				return fmt.Errorf("meta: first range must start at -inf")
			}
			r.Start = nil
		} else {
			prev := ranges[i-1]
			if r.Start == nil || len(r.Start) == 0 || bytes.Compare(r.Start, prev.End) != 0 {
				return fmt.Errorf("meta: range %d is not adjacent to the previous range", r.ID)
			}
		}
		if i == len(ranges)-1 {
			if r.End != nil && len(r.End) != 0 {
				return fmt.Errorf("meta: last range must end at +inf")
			}
			r.End = nil
		} else if r.End == nil || len(r.End) == 0 || bytes.Compare(r.Start, r.End) >= 0 {
			return fmt.Errorf("meta: range %d has invalid bounds", r.ID)
		}
		if len(r.Replicas) == 0 {
			return fmt.Errorf("meta: range %d has no replicas", r.ID)
		}
		old, hadOld := oldByID[r.ID]
		leader, leaderEpoch := normalizeLeaderHint(r, old, hadOld)
		clone := Range{
			ID: r.ID, Start: cloneBytes(r.Start), End: cloneBytes(r.End),
			Replicas: cloneStrings(r.Replicas),
			Leader:   leader, LeaderEpoch: leaderEpoch, Version: r.Version,
		}
		out = append(out, &clone)
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	c.ranges = out
	c.nextID.Store(maxID)
	return nil
}

// normalizeLeaderHint decides whether an incoming leader hint may be recorded
// at the incoming descriptor version. See SetRanges for the policy.
func normalizeLeaderHint(in Range, old Range, hadOld bool) (string, uint64) {
	if in.Leader == "" {
		if hadOld && old.Version == in.Version && old.Leader != "" &&
			slices.Contains(in.Replicas, old.Leader) {
			return old.Leader, old.LeaderEpoch
		}
		return "", 0
	}
	if !slices.Contains(in.Replicas, in.Leader) {
		return "", 0
	}
	// An epoch supplied by the caller is authoritative. A mismatch means the
	// hint belongs to an older descriptor and must not be published.
	if in.LeaderEpoch != 0 {
		if in.LeaderEpoch != in.Version {
			return "", 0
		}
		return in.Leader, in.LeaderEpoch
	}
	if !hadOld {
		return in.Leader, in.Version
	}
	if old.Version == in.Version {
		epoch := old.LeaderEpoch
		if epoch == 0 {
			epoch = in.Version
		}
		return in.Leader, epoch
	}
	// The descriptor version changed and the caller did not stamp the hint at
	// the new version. A group-preserving operation (split, merge, membership
	// change) must set LeaderEpoch to the new Version to carry the hint
	// forward; otherwise the hint belongs to the previous epoch and is stale.
	// If the old map had no hint there is nothing stale to clear, so initial
	// placement (for example Config.Ranges) is still accepted.
	if old.Leader == "" {
		return in.Leader, in.Version
	}
	return "", 0
}

// RangeForKey returns the range that contains key. A nil key is the smallest
// possible key, and nil range bounds mean -inf/+inf respectively.
func (c *Cluster) RangeForKey(key []byte) (*Range, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, r := range c.ranges {
		if r.Start != nil && bytes.Compare(key, r.Start) < 0 {
			continue
		}
		if r.End != nil && bytes.Compare(key, r.End) >= 0 {
			continue
		}
		return cloneRange(r), nil
	}
	return nil, fmt.Errorf("meta: no range covers key %q", key)
}

// Split divides the range with the given ID at at, which must lie strictly
// inside the range. The left range keeps the original ID, the right range gets
// a fresh ID, and both descriptors get a new version.
func (c *Cluster) Split(id uint64, at []byte) (*Range, *Range, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.rangeIndexLocked(id)
	if idx < 0 {
		return nil, nil, fmt.Errorf("meta: range %d not found", id)
	}
	old := c.ranges[idx]
	if bytes.Compare(at, old.Start) <= 0 || (old.End != nil && bytes.Compare(at, old.End) >= 0) {
		return nil, nil, fmt.Errorf("meta: split point %q is not strictly inside range %d [%q, %q)",
			at, id, old.Start, old.End)
	}

	left := cloneRange(old)
	left.End = cloneBytes(at)
	left.Version = old.Version + 1
	// The left range keeps the original Raft group, so a live leadership hint
	// remains meaningful; re-stamp it at the new descriptor version.
	if left.Leader != "" {
		left.LeaderEpoch = left.Version
	}

	// The right range is a brand-new group. Its leader is unknown even when the
	// parent's leader is still a replica, so no hint is copied.
	right := &Range{
		ID:       c.AllocID(),
		Start:    cloneBytes(at),
		End:      cloneBytes(old.End),
		Replicas: cloneStrings(old.Replicas),
		Version:  old.Version + 1,
	}

	c.ranges[idx] = left
	c.ranges = slices.Insert(c.ranges, idx+1, right)

	return cloneRange(left), cloneRange(right), nil
}

// Merge combines two adjacent ranges that have identical replica sets. The
// left range keeps its ID, absorbs the right range's key space and gets a new
// version.
func (c *Cluster) Merge(left, right uint64) (*Range, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if left == right {
		return nil, fmt.Errorf("meta: cannot merge range %d with itself", left)
	}
	li := c.rangeIndexLocked(left)
	if li < 0 {
		return nil, fmt.Errorf("meta: left range %d not found", left)
	}
	ri := c.rangeIndexLocked(right)
	if ri < 0 {
		return nil, fmt.Errorf("meta: right range %d not found", right)
	}
	if ri != li+1 {
		return nil, fmt.Errorf("meta: ranges %d and %d are not adjacent", left, right)
	}

	l, r := c.ranges[li], c.ranges[ri]
	if !bytes.Equal(l.End, r.Start) {
		return nil, fmt.Errorf("meta: ranges %d and %d have inconsistent boundaries", left, right)
	}
	if !slices.Equal(l.Replicas, r.Replicas) {
		return nil, fmt.Errorf("meta: ranges %d and %d have different replicas", left, right)
	}

	merged := cloneRange(l)
	merged.End = cloneBytes(r.End)
	if r.Version > merged.Version {
		merged.Version = r.Version
	}
	merged.Version++
	// The left range's Raft group absorbs the right range, so only the left
	// leader hint can still be meaningful. Re-stamp it at the merged version;
	// a right-side-only hint is not carried over.
	if merged.Leader != "" {
		merged.LeaderEpoch = merged.Version
	} else {
		merged.LeaderEpoch = 0
	}

	c.ranges[li] = merged
	c.ranges = slices.Delete(c.ranges, ri, ri+1)

	return cloneRange(merged), nil
}

// UpdateLeader records leader as the current leader of the range with the
// given ID at the range's current descriptor version. An empty leader clears
// the recorded leader. Passing a node that is not a replica is allowed (a
// range may not have a placement yet), but callers should normally pass a
// value learned from a live KV Status response.
func (c *Cluster) UpdateLeader(id uint64, leader string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.rangeIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("meta: range %d not found", id)
	}
	r := c.ranges[idx]
	r.Leader = leader
	if leader == "" {
		r.LeaderEpoch = 0
	} else {
		r.LeaderEpoch = r.Version
	}
	return nil
}

// RefreshLeader records a leader hint that was learned at epoch. When epoch is
// non-zero and does not match the range's current Version, the hint is stale:
// any recorded leader is cleared and ErrStaleLeader is returned. This is the
// explicit path meta uses when a KV Status probe or a leader redirect reveals
// the range leader.
func (c *Cluster) RefreshLeader(id uint64, leader string, epoch uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.rangeIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("meta: range %d not found", id)
	}
	r := c.ranges[idx]
	if epoch != 0 && epoch != r.Version {
		r.Leader = ""
		r.LeaderEpoch = 0
		return fmt.Errorf("%w: range %d leader epoch %d, current %d",
			ErrStaleLeader, id, epoch, r.Version)
	}
	r.Leader = leader
	if leader == "" {
		r.LeaderEpoch = 0
	} else {
		r.LeaderEpoch = r.Version
	}
	return nil
}

// ClearLeader forgets the recorded leader hint for a range. It is idempotent.
func (c *Cluster) ClearLeader(id uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.rangeIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("meta: range %d not found", id)
	}
	c.ranges[idx].Leader = ""
	c.ranges[idx].LeaderEpoch = 0
	return nil
}

// AllocID returns a new cluster-unique range ID. The initial range's ID (1) is
// never returned.
func (c *Cluster) AllocID() uint64 {
	return c.nextID.Add(1)
}

// Snapshot returns a point-in-time copy of the range map, sorted by Start.
// The returned ranges and their slices are deep copies and may be modified by
// the caller without affecting the cluster.
func (c *Cluster) Snapshot() []Range {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Range, len(c.ranges))
	for i, r := range c.ranges {
		out[i] = *cloneRange(r)
	}
	return out
}

func (c *Cluster) rangeIndexLocked(id uint64) int {
	for i, r := range c.ranges {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func cloneRange(r *Range) *Range {
	if r == nil {
		return nil
	}
	clone := *r
	clone.Start = cloneBytes(r.Start)
	clone.End = cloneBytes(r.End)
	clone.Replicas = cloneStrings(r.Replicas)
	return &clone
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
