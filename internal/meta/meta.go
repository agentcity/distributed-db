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
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// Range describes one contiguous slice of the key space [Start, End).
//
// A nil Start means -inf and a nil End means +inf. Replicas is the placement
// for the range and Leader is the node currently serving it. Version is the
// range descriptor version; Split and Merge increment it.
type Range struct {
	ID       uint64
	Start    []byte // inclusive; nil means -inf
	End      []byte // exclusive; nil means +inf
	Replicas []string
	Leader   string
	Version  uint64
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

	right := &Range{
		ID:       c.AllocID(),
		Start:    cloneBytes(at),
		End:      cloneBytes(old.End),
		Replicas: cloneStrings(old.Replicas),
		Leader:   old.Leader,
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

	c.ranges[li] = merged
	c.ranges = slices.Delete(c.ranges, ri, ri+1)

	return cloneRange(merged), nil
}

// UpdateLeader records leader as the current leader of the range with the
// given ID. An empty leader clears the recorded leader.
func (c *Cluster) UpdateLeader(id uint64, leader string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.rangeIndexLocked(id)
	if idx < 0 {
		return fmt.Errorf("meta: range %d not found", id)
	}
	c.ranges[idx].Leader = leader
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
