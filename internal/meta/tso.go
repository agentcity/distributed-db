package meta

import "time"

// Timestamps have the form physical milliseconds << 18 | logical counter.
const (
	logicalBits = 18
	logicalMask = uint64(1<<logicalBits) - 1
)

// AllocTS reserves n consecutive timestamps and returns the first one and the
// number reserved. Positive n always yields count == n.
//
// Timestamps are strictly increasing across all concurrent callers, and the
// range [start, start+count) never overlaps a range returned by another call.
// Non-positive n returns (0, 0).
func (c *Cluster) AllocTS(n int) (start uint64, count int) {
	if n <= 0 {
		return 0, 0
	}

	c.tsMu.Lock()
	defer c.tsMu.Unlock()

	now := uint64(time.Now().UnixMilli()) << logicalBits

	// Keep both the physical and logical fields as close to the wall clock as
	// possible while never going backwards. The stored lastTS also protects
	// callers from a clock that jumps backwards.
	if now > c.lastTS {
		c.lastPhysical = now >> logicalBits
		c.lastLogical = 0
	} else {
		c.lastLogical++
		if c.lastLogical > logicalMask {
			c.lastPhysical++
			c.lastLogical = 0
		}
	}

	// Keep an entire reservation within one physical millisecond when it fits
	// in the 18-bit logical field. Larger reservations are still contiguous
	// and monotonic; the carry simply moves into the physical field.
	if uint64(n-1) > logicalMask-c.lastLogical {
		c.lastPhysical++
		c.lastLogical = 0
	}

	start = c.lastPhysical<<logicalBits | c.lastLogical
	if uint64(n-1) > ^uint64(0)-start {
		// The uint64 timestamp space is exhausted; this cannot happen with a
		// real millisecond clock, but never wrap around.
		return 0, 0
	}
	last := start + uint64(n-1)
	c.lastTS = last
	// Derive the fields from lastTS so large reservations that carry into the
	// physical part stay consistent with future allocations.
	c.lastPhysical = last >> logicalBits
	c.lastLogical = last & logicalMask

	return start, n
}
